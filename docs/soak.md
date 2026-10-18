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

- the goroutines of each process named in `CLOUDBURROW_SOAK_PPROF` (below), read from its pprof
  endpoint; where none is named, threads and memory stand in for them.

`up`, the storage server and every front (each a `cloudburrow-storage` command) serve the Go
runtime's profiles (`net/http/pprof`) when `CLOUDBURROW_DEBUG_PPROF` names a loopback address,
such as `127.0.0.1:6060`; it is unset, and nothing listens, by default. A heap profile and a
goroutine dump are then `go tool pprof http://<addr>/debug/pprof/heap` and
`curl 'http://<addr>/debug/pprof/goroutine?debug=1'`.

## Running it

Start an instance of your own with the services, on ports of your own:

```sh
make build
state=$PWD/soak-state
bin/cloudburrow up --detach --name soak-1 --port-base 29400 --state-dir "$state" \
  --services storage,pubsub,tasks,secretmanager,firestore,bigquery,logging
```

`up --detach` prints the process ID. To profile the Go processes as well, start `up` with
`CLOUDBURROW_DEBUG_PPROF=127.0.0.1:<port>` (a port outside the instance's), and turn it on in a
pod's container, then reach the pod's loopback with a port-forward:

```sh
export KUBECONFIG="$state/soak-1/kubeconfig"
kubectl -n cloudburrow set env deploy/storage -c storage CLOUDBURROW_DEBUG_PPROF=127.0.0.1:6060
kubectl -n cloudburrow set env deploy/bigquery -c front CLOUDBURROW_DEBUG_PPROF=127.0.0.1:6060
kubectl -n cloudburrow port-forward deploy/storage 29491:6060 &
```

Set it on one container of a pod only: the containers share the pod's loopback, and a second
listener on the same port is refused (the process goes on without it). `set env` restarts the
pod, so do it before the run. The BigQuery emulator itself is not a CloudBurrow process and serves
no profile.

Then export the endpoints (never print them: they include the
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
| `CLOUDBURROW_SOAK_PPROF` | none | comma-separated `name=127.0.0.1:<port>` pprof endpoints whose goroutines to sample, such as `host=127.0.0.1:29490,storage=127.0.0.1:29491` |

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
  minutes, then hold.
- Secret Manager keeps each secret in a Kubernetes Secret and reads it with `kubectl`, so its calls
  take a few hundred milliseconds; see [compatibility.md](compatibility.md).
- Storage keeps every overwritten generation under the bucket's soft delete policy (7 days by
  default), as Cloud Storage does: the storage workload's blobs come to about 20 GB an hour, and
  its metadata grows with them.
- BigQuery's tables grow with the rows streamed, and its queries over them slow; the emulator's
  heap runs well past its live size between collections (#1618).

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

### 90 minutes (#1609)

The second run (2026-10-17, #1609): every group for 90 minutes on one instance, on the same
shared Mac, `CLOUDBURROW_SOAK_RATE=1`, with `CLOUDBURROW_DEBUG_PPROF` on in `up`, the storage
server and the BigQuery front, a heap profile and goroutine dump of each every 10 minutes, and the
BigQuery engine's SQLite file and the storage server's metadata store measured with them. It asked,
of each growth the first run left unexplained, whether it was still growing at 90 minutes.

Memory is each container's anonymous memory (the `up` process's resident memory), at the sample
nearest each mark; latency is p50 / p99 of the minute ending there.

| | Start | 30 min | 60 min | 90 min | Verdict |
|---|---|---|---|---|---|
| storage pod | 31 MiB | 66 MiB | 199 MiB | 122 MiB | the store rewrote kept versions: fixed |
| storage upload | 10.2 / 307 ms | 7.0 / 82 ms | 23.9 / 650 ms | 24.0 / 307 ms | the same |
| storage download | 1.6 / 75 ms | 1.2 / 62 ms | 3.3 / 226 ms | 2.6 / 94 ms | |
| storage metadata (snapshot + log) | 1.0 MB | 19 MB | 47 MB | 152 MB | kept versions: expected |
| storage blobs | 0.2 GB | 10 GB | 20 GB | 29 GB | kept versions: expected |
| bigquery emulator | 99 MiB | 174 MiB | 283 MiB | 318 MiB | uncollected garbage, not a leak: #1618 |
| bigquery engine SQLite | 1 MB | 40 MB | 80 MB | 139 MB | rows and their change history: expected; the front's jobs: fixed |
| bigquery front | 28 MiB, 23 goroutines | 29 MiB, 23 | 33 MiB, 23 | 33 MiB, 24 | flat |
| bigquery insert | 107 / 300 ms | 122 / 257 ms | 262 / 775 ms | 315 / 764 ms | the front's jobs: fixed |
| bigquery `GROUP BY` | 44 / 60 ms | 109 / 123 ms | 288 / 704 ms | 426 / 539 ms | a table of 107,320 rows: expected |
| secretmanager access | 210 / 408 ms | 283 / 470 ms | 330 / 881 ms | 337 / 610 ms | versions kept as annotations: #1596 |
| `up` (Tasks, Secret Manager, Logging) | 290 MiB, 118 goroutines | 332 MiB, 115 | 279 MiB, 117 | 151 MiB, 117 | flat |
| logging read | 3.5 / 6 ms | 18.0 / 28 ms | 19.4 / 49 ms | 20.3 / 31 ms | the store full at 17 min: expected |
| tasks create | 6.3 / 99 ms | 6.1 / 43 ms | 8.6 / 158 ms | 7.3 / 61 ms | flat |
| pubsub emulator | 292 MiB | 396 MiB | 518 MiB | 638 MiB | still rising: #1619 |
| pubsub publish | 12.7 / 29 ms | 12.0 / 22 ms | 12.7 / 42 ms | 13.3 / 38 ms | flat |
| firestore emulator | 358 MiB | 427 MiB | 449 MiB | 461 MiB | levels off |
| firestore set | 4.2 / 23 ms | 2.5 / 13 ms | 3.5 / 38 ms | 4.3 / 24 ms | flat |

Pub/Sub delivered all 215,834 messages to both subscriptions with none redelivered, Tasks all
107,831 tasks once each, BigQuery counted the 107,320 rows streamed; no container restarted, and
the storage server's goroutines held at 42, the BigQuery front's at 23 and `up`'s at 115–118. The
only errors were Secret Manager's: 868 of 1,787 `AddSecretVersion` calls failed INTERNAL from the
secret's 905th version on (#1596).

What each growth was:

- **Storage: kept versions were rewritten at each overwrite (fixed).** The harness overwrites a
  ring of 32 names about 7 times a second, and under the bucket's default 7-day soft delete policy
  each overwritten generation is kept, soft-deleted, as Cloud Storage keeps it. The metadata store
  held a name's kept versions as one JSON array, which every overwrite read, wrote whole and logged
  again: by 300 versions an overwrite logged 350 KB, the store compacted every few seconds, and the
  heap profile put 82% of the server's allocations (3.8 GB in its first 10 minutes) in that write
  and the compactions' base64 encoding. The store now appends: its log records the version added
  alone, whatever the number kept before it, and versioned buckets' noncurrent versions the same
  way (`TestOverwriteLogsOnlyTheVersionItKeeps`, `TestMetadataStoreAppend`).
- **Storage: the kept versions themselves (expected).** About 0.9 KB of metadata and the object's
  bytes per overwrite, kept until the soft delete policy's 7 days pass, when `Sweep` drops them:
  29 GB of blobs by 90 minutes. A soak that should not keep them can give its bucket a soft delete
  retention of 0.
- **BigQuery: the front's own jobs were never deleted (fixed).** Each streamed `insertAll` runs an
  `INSERT` and a `COUNT(*)` of the front's own, so 11 minutes in the engine's jobs table held 2,793
  jobs no client can read beside the client's 350, growing without end, each a row every keyed read
  of the table visits. The front now deletes a job of its own once its `jobs.query` has answered it
  complete with every row (`TestTheFrontDeletesItsOwnFinishedJobs`).
- **BigQuery: the emulator's memory (not a leak; #1618).** Nearly all in one anonymous mapping,
  and not its database. On a standalone emulator of the same image, 6,300 queries in a loop took
  it to 2.3 GB, of which a collection after two idle minutes returned 0.7 GB, and under
  `GOMEMLIMIT=300MiB` 4,500 held it at 281 MB: garbage the collector reclaims, which the default
  GOGC lets run to several times the live heap (most likely because GoogleSQL's wasm memory counts
  as live, and the C++ objects it frees wait for finalizers).
- **BigQuery: the table (expected).** Queries read a table that grows by 20 rows a second, and the
  change history doubles what each insert writes.
- **Secret Manager (#1596).** Each secret's versions are annotations on one Kubernetes Secret,
  read with `kubectl` at each call; the latency follows the versions kept, and Kubernetes' 256 KiB
  cap on annotations refuses the 905th. #1596 moves each version to a Secret of its own.
- **Pub/Sub's JVM (#1619)** was still rising by 4 MiB a minute at 90 minutes, with no effect on
  latency or delivery; Firestore's levelled off by 60 minutes.

After the fixes, a 15-minute run of the storage, bigquery and secretmanager groups alone (with
#1596's branch merged in, locally, for Secret Manager), against the first run's figures at 15
minutes:

| | First run, 15 min | After, 15 min |
|---|---|---|
| storage pod | 47 MiB | 42 MiB |
| storage upload | 7.2 / 91 ms | 8.7 / 265 ms |
| storage metadata log, last record | up to about 1 MB | 2.4 KB |
| engine jobs table at 11 min | 2,793 of the front's, 350 of the client's | 0 of the front's, 362 of the client's |
| bigquery insert | 107 / 211 ms | 146 / 454 ms |
| bigquery `COUNT(*)` / `GROUP BY` p50 | 37 / 66 ms | 25 / 58 ms |
| bigquery emulator | 138 MiB | 142 MiB |
| secretmanager access | 240 / 383 ms | 1.2 / 97 ms, with #1596 |
| secretmanager add | 373 / 419 ms | 119 / 464 ms, with #1596, no errors |

Goroutines held: `up` 55–56, the storage server 16–17, the BigQuery front 21–23. A streamed insert
now costs two more calls to the engine (the deletions of its two jobs), which is the 15-minute
insert latency above; what it saves, a jobs table that grows with every insert, showed in the first
run only after 30 minutes. A longer run after the fixes is the next measurement.
