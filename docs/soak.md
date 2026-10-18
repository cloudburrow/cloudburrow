# Soak testing

`test/soak` drives one running instance with the official Go clients at a steady, realistic mix
for a long time (30 minutes by default), and reports what drifted. It is behind the `soak` build
tag, so `go test ./...`, `make check` and every CI job leave it out; it is run by hand, against an
instance of your own, before a release or after a change to a service's hot path.

## What it runs

Every group runs at once, against one instance, at a fixed rate: a worker that falls behind delays
its next call rather than stacking calls up, so a slow service shows as latency, not as a queue
inside the harness. 21 clients in all; the harness refuses to open more than 32.

| Group | Workers and rate | What it checks at the end |
|-------|------------------|---------------------------|
| storage | 4 workers × 4 calls/s, each over a ring of 8 object names it overwrites: uploads of 1 KB (50%), 64 KB (25%), 1 MB (20%) and 10 MB (5%) objects (45% of calls), downloads that compare the bytes (40%), listings (15%) | every download's length and CRC-32 |
| pubsub | 2 publishers × 20 messages/s; a subscription read by streaming pull (`Subscriber.Receive`), and one by `Pull` and `Acknowledge` twice a second | every acknowledged publish reached both subscriptions; redeliveries beyond one in a thousand (at-least-once allows some) |
| tasks | 2 workers × 10 HTTP tasks/s, at a loopback target that answers 200 | every task delivered, none twice |
| secretmanager | 2 workers × 5 calls/s over one secret: `AccessSecretVersion` of latest (90%), `AddSecretVersion` (5%), `ListSecretVersions` (5%) | |
| firestore | 3 workers × 10 calls/s over 200 documents: gets (50%), sets (40%), a range query (10%); and one listener on the collection | the listener still sees a write within 10 s |
| bigquery | 10 streamed rows twice a second; a `COUNT(*)` or a `GROUP BY` query every 2 s | the table holds exactly the rows streamed |
| logging | 2 writers × 10 `LogSync` entries/s; one reader of the last minute's entries every 2 s | a last entry reads back |

Each call is timed, and the latencies are kept per window (1 minute by default): the report gives
p50, p95 and p99 for each window, and names a percentile that grew by half, and by more than 20 ms,
between the first window and the last complete one. Any error is reported with its gRPC or HTTP
code and a sample message, and fails the test, as does a lost or duplicated delivery, a container
restart, or a stale listener.

Every 30 s the harness also samples:

- the host `cloudburrow up` process (which serves Tasks, Secret Manager and Logging in process, and
  fronts the rest): its resident memory (`ps`), threads (`ps -M`) and open files (`lsof`);
- each pod container, read from its cgroup on the kind node with `docker exec` and `crictl`, so no
  metrics-server is needed: anonymous memory (page cache left out), tasks (threads) and the open
  files of every process in it, and its restart count;
- the state directory's size.

Neither the front nor the pods serve pprof, so goroutines are not counted directly; threads and
memory stand in for them.

## Running it

Start an instance of your own with the services, on ports of your own:

```sh
make build
state=$PWD/soak-state
bin/cloudburrow up --detach --name soak-1 --port-base 29400 --state-dir "$state" \
  --services storage,pubsub,tasks,secretmanager,firestore,bigquery,logging
```

`up --detach` prints the process ID. Then export the endpoints (never print them: they include the
admin token) and run:

```sh
eval "$(scripts/compat-env.sh --only STORAGE,PUBSUB,TASKS,SECRETS,FIRESTORE,BIGQUERY,BIGQUERY_PROJECT,LOGGING \
  --name soak-1 --state-dir "$state" 2>/dev/null)"
CLOUDBURROW_SOAK_PID=<the pid up printed> \
CLOUDBURROW_SOAK_NODE=cloudburrow-soak-1-control-plane \
CLOUDBURROW_SOAK_STATE_DIR="$state" \
CLOUDBURROW_SOAK_REPORT=soak.json \
  go test -tags soak -count=1 -timeout 50m -v ./test/soak/
```

Then `bin/cloudburrow stop --name soak-1 --state-dir "$state"` and
`kind delete cluster --name cloudburrow-soak-1`.

| Variable | Default | Meaning |
|----------|---------|---------|
| `CLOUDBURROW_SOAK_DURATION` | `30m` | how long the load runs; the end-of-run checks take up to 3 minutes more |
| `CLOUDBURROW_SOAK_GROUPS` | every group | comma-separated groups to run, such as `pubsub,tasks` |
| `CLOUDBURROW_SOAK_RATE` | `1` | multiplier on every worker's rate |
| `CLOUDBURROW_SOAK_WINDOW` | `1m` | the latency window |
| `CLOUDBURROW_SOAK_REPORT` | none | where to write the JSON report: every group's windows and every sample |
| `CLOUDBURROW_SOAK_PID` | none | the `up` process to sample |
| `CLOUDBURROW_SOAK_NODE` | none | the kind node container to sample, `cloudburrow-<name>-control-plane` |
| `CLOUDBURROW_SOAK_STATE_DIR` | none | the state directory to measure |

A group whose endpoint is not exported does not run. The harness refuses an endpoint that is not
loopback, and refuses to start with Google credentials in the environment.

## Reading the report

The test log ends with a line per operation (ops, errors, the first and the last window's
percentiles), the host process's and each container's figures at the first and the last sample,
and the delivery counts of Pub/Sub, Tasks and BigQuery. The JSON report has every window and every
sample, for a graph.

Expected, and not a leak:

- Logging keeps the last 20,000 entries; at 20 entries a second the store fills in about 17 minutes,
  so `logging.read` (which filters every entry kept) is slower in the last window than in the first.
- The JVM emulators (Pub/Sub, Firestore) and the BigQuery emulator grow their heaps for the first
  minutes. Firestore's then holds; Pub/Sub's keeps growing by the messages its subscriptions keep
  until they are deleted, about 2 MiB a minute at the soak's rate (#1603, below), and the heap
  bound (`--emulator-heap-mib`) is what stops it.
- Secret Manager keeps each secret in a Kubernetes Secret and reads it with `kubectl`, so its calls
  take a few hundred milliseconds; see [compatibility.md](compatibility.md).

## Results

The first run (2026-10-17, #1594): every group for 30 minutes on one instance of an Apple silicon
Mac shared with other work, `CLOUDBURROW_SOAK_RATE=1`. No call failed. It found three leaks, fixed
in #1594 and measured again in a 15-minute run after:

- **Tasks delivered twice:** 340 of 33,141 tasks that answered 200 arrived twice. A dispatch pass's
  list of due tasks went stale while it ran; a task whose attempt ended meanwhile was started again.
  After: 0 of 17,971.
- **Tasks tombstones never expired**, so the store grew to 10 MB in 30 minutes and `CreateTask`'s
  p50 went from 6 to 74 ms (every change writes the whole store). After: the state directory held
  at 17–20 KB, and `CreateTask`'s p50 at 6–10 ms.
- **Logging** allocated its whole 20,000-entry store again at each write once full; it now drops the
  oldest in place.

| Group | Ops (30 min) | Errors | p50 / p99, first minute | p50 / p99, last minute | Memory, start → end |
|-------|-------------:|-------:|-------------------------|------------------------|---------------------|
| storage (upload) | 12,548 | 0 | 9.1 / 142 ms | 11.7 / 166 ms | pod 30 → 64 MiB |
| storage (download) | 10,949 | 0 | 2.0 / 90 ms | 2.5 / 212 ms | |
| pubsub (publish) | 71,513 | 0 | 12.7 / 24 ms | 13.0 / 41 ms | emulator 288 → 390 MiB |
| pubsub (publish to streaming receipt) | 71,513 | 0 lost, 0 redelivered | 39 / 65 ms | 40 / 76 ms | |
| tasks (create) | 33,141 | 340 delivered twice | 6.4 / 71 ms | 73.5 / 210 ms | in `up` |
| secretmanager (access) | 10,399 | 0 | 217 / 390 ms | 300 / 639 ms | in `up` |
| firestore (set) | 21,412 | 0 | 3.9 / 23 ms | 3.6 / 29 ms | emulator 368 → 542 MiB |
| bigquery (insert) | 3,410 | 0, 34,100 rows counted | 101 / 269 ms | 139 / 442 ms | emulator 91 → 174 MiB |
| logging (write) | 35,865 | 0 | 0.6 / 2.2 ms | 0.6 / 7.0 ms | in `up` |

The `up` process stayed at 120–260 MiB, 29–35 threads and about 65 open files; no container
restarted; each front stayed at 25–35 MiB. BigQuery's queries slow as the table grows (21 → 99 ms
p50 for the `GROUP BY` over 34,000 rows), and Logging's reads as its store fills, both expected.
Secret Manager's latency grows with the versions a secret holds, and the JVM emulators' memory
kept rising slowly; both are followed up in #1596 (Secret Manager also refuses a secret's 905th version).

### The Java emulators' memory (#1603)

A 60-minute run of `pubsub,firestore` on `land/111` found Pub/Sub's container growing about
4 MiB a minute for the whole hour, with Firestore's levelling off; the emulators then ran with no
`-Xmx` and no container limit, so the JVM's heap could grow to a quarter of the kind node's memory
(about 6 GiB). To tell lazy collection from a leak, the same soak ran again for 30 minutes with the
heap bounded at 256 MiB (`JAVA_TOOL_OPTIONS=-Xmx256m`, with `-Xlog:gc` and a live-object histogram,
`-XX:+PrintClassHistogram` and SIGQUIT, every 5 minutes, as the image has no `jcmd` or `jmap`).
It passed: 71,995 messages published, none lost or redelivered on either subscription, no errors,
no restarts.

Container memory (anonymous, MiB), by minute of the soak:

| Minute | 0 | 5 | 10 | 15 | 20 | 25 | 30 |
|---|---|---|---|---|---|---|---|
| Pub/Sub, unbounded (#1603's run) | 200 | 320 | 340 | 355 | 373 | 396 | 411 |
| Pub/Sub, `-Xmx256m` | 218 | 275 | 279 | 294 | 313 | 332 | 349 |
| Firestore, unbounded (#1603's run) | 258 | 407 | 420 | 448 | 449 | 451 | 455 |
| Firestore, `-Xmx256m` | 220 | 363 | 371 | 390 | 393 | 394 | 394 |

Live heap after a full collection (MiB), by minute of the JVM's uptime, which ran a minute and a
half ahead of the soak; the last column is after the soak deleted its topic and subscriptions:

| Uptime | 3 | 9 | 14 | 19 | 24 | 29 | 34 |
|---|---|---|---|---|---|---|---|
| Pub/Sub live heap | 16 | 27 | 37 | 47 | 60 | 69 | 10 |
| Pub/Sub `PubsubMessage` objects live | 6,189 | 20,699 | 32,909 | 45,119 | 57,330 | 69,542 | 1 |
| Firestore live heap | 20 | 23 | 24 | 25 | 26 | 27 | |

- **Firestore is lazy collection**: its live heap stays small (20 → 27 MiB over 26 minutes, under
  0.3 MiB a minute) while its container held 400-470 MiB unbounded, heap the JVM had grown and
  not given back.
- **Pub/Sub is a leak in the emulator**, not in how CloudBurrow drives it: its live heap grows
  2 MiB a minute, and the number of live `PubsubMessage` objects is the number of messages
  published so far. Every message stays in each subscription's backlog (the emulator's
  `MessageBacklog`, whose acknowledgement only marks it) until the subscription is deleted; the
  front holds none of them, as it is another container. See
  [compatibility.md](compatibility.md#pubsub--googlepubsubv1).
- **Filled, the bounded emulator stops**: with 256 MiB, 175,000 acknowledged 1 KB messages on one
  subscription filled the heap, and the emulator answered nothing (every `Publish` timed out, its
  pod not ready) until deleted. With `-XX:+ExitOnOutOfMemoryError` it exited instead and its
  container restarted, empty, within seconds.

So `up` now gives each Java emulator (Pub/Sub, Firestore, Datastore) `-Xmx512m
-XX:+ExitOnOutOfMemoryError`, a memory request of 512 MiB and a limit of 1 GiB
(`--emulator-heap-mib`, [configuration.md](configuration.md#settings)): enough for about 550,000 of
the soak's messages on a pair of subscriptions, 3.8 hours at its 40 a second, before Pub/Sub
restarts. The Pub/Sub and Firestore compatibility tests pass under it.

