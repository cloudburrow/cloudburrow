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
  minutes, then hold.
- Secret Manager keeps each secret in a Kubernetes Secret and reads it with `kubectl`, so its calls
  take a few hundred milliseconds; see [compatibility.md](compatibility.md).
