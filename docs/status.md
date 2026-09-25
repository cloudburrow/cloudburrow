# What actually works

A single page answering "can I use this yet?", so nobody has to infer it from a matrix.

**Read [`compatibility.md`](compatibility.md) for the per-operation detail.** Every
`Verified` row there names the test that proves it, and every test drives an **official
Google SDK**. Nothing is promoted on the strength of a curl command.

## The short answer

**The acceptance workflow passes end to end**: upload an object, publish an event, a Cloud Run
worker receives it and reads the object, writes a result, and the result is read back — every
step through an official SDK, including inside the worker.

If that is the shape of what you are building, CloudBurrow can run it today.

## Per service

| Service | Backed by | State |
|---|---|---|
| **Cloud Storage** | `fake-gcs-server` | Buckets, objects, prefix listing, resumable upload, ranged reads, **generation preconditions**, compose |
| **Pub/Sub** | Google's own emulator | Topics, subscriptions, publish, pull, **StreamingPull**, push delivery |
| **Cloud Tasks** | CloudBurrow itself | Queues, tasks, pause/resume, HTTP dispatch with retry |
| **Cloud Run** | Knative Serving | Create, get, list, delete services; deploys real containers; env from Secret Manager |
| **Secret Manager** | CloudBurrow itself | Secrets and versions, enable/disable/destroy, access, over gRPC and JSON. **Not a secret store** — nothing is authenticated |

Opt-in, with `--services`:

| Service | Backed by | State |
|---|---|---|
| **Firestore** | Google's own emulator | Documents and collections through the official SDK |
| **Datastore** | Google's own emulator | Entities and kinds through the official SDK |
| **Bigtable** | Google's own emulator | Tables, column families and rows through the official SDK |
| **Spanner** | Google's own emulator | Instances, databases, DDL and queries through the official SDK |
| **Cloud SQL** | PostgreSQL in the cluster; MySQL 8.4 as `cloudsql-mysql` (#297) | **A local SQL database, not the Cloud SQL Admin API.** Google publishes no Cloud SQL emulator, so this is a real PostgreSQL reached with an ordinary driver. No instances, connection names, IAM database authentication, backups or replicas, and no `sqladmin` endpoint — see [#121](https://github.com/cloudburrow/cloudburrow/issues/121) |

## What will not work, and why

Worth reading before you hit these:

- **No IAM enforcement, anywhere.** No policy evaluation, no service-account identity. Google's
  Pub/Sub emulator returns `Unimplemented` for IAM methods and we do not paper over it.
  [ADR-0006](adr/0006-iam-policy-surface.md) adds policy *storage*, never enforcement: Secret
  Manager has it (#365), and Cloud Tasks is next (#366). **Do not use CloudBurrow to test whether your
  permissions are correct.**
- **Pub/Sub state does not survive a restart** — its emulator loses topics even with
  `--data-dir`. Measured, not assumed.
- **Signed URL signatures are not verified.** A signed URL is accepted on shape alone, so
  this cannot test signing correctness.
- **Cloud Run configuration we cannot map is refused, not ignored** — service accounts, VPC
  access, volumes, encryption keys, binary authorization, execution environment, session
  affinity and traffic splitting all return `Unimplemented` naming the field. You will see an
  error rather than silently wrong behaviour. (Secret-backed env used to be listed here in
  error: it is mapped and verified.)
- **Knative is not Cloud Run.** Min-instances is verified; max-instances, concurrency, the
  request timeout and resource limits reach the manifest but are not tested under load.
  `UpdateService` redeploys as a new revision (#300); the Revisions API serves Get, List and
  Delete (#299). Traffic splitting is not mapped.
- **Cloud Tasks does not enforce rate limits.**
- **No Cloud Run Jobs, no GKE management APIs.** Firestore, Datastore, Bigtable and Spanner
  ship as opt-in emulators (above) rather than being absent, and BigQuery as an opt-in
  community emulator (#277) that serves one project only; source builds work
  through Google Buildpacks (#33) without implying the Cloud Build API.
- **`/admin/events` records only the services CloudBurrow serves itself** — Cloud Tasks, Secret
  Manager and the Cloud Run adapter. Storage, Pub/Sub and the opt-in emulators are reached over a
  raw port-forward to an upstream process, so their calls are not observed.

## Deliberately unclaimed

Some things are implemented and tested but **not** marked `Verified`, because no official SDK
has driven them:

- `PurgeQueue`, `ListTasks` (Cloud Tasks) — unit-tested only
- `DeleteTopic` (Pub/Sub) — called during test cleanup, never asserted
- `buckets.list`, `objects.copy`/`rewrite`, multipart upload — not exercised
- Resumable upload **interruption and resume** — the happy path is verified, recovery is not

The distinction is the point: called is not the same as verified.

## Footprint

Measured by `make footprint` (`scripts/footprint.sh`) in the `footprint` workflow on
2026-09-24: GitHub Actions `ubuntu-latest`, Linux x86_64, Docker with 4 CPUs and 15988 MiB.
One run, not an average, and not a promise about another machine.

| Profile | Cold start | Warm start | Node container memory | Pods' working set |
| --- | --- | --- | --- | --- |
| Default (storage, pubsub, tasks, run, secretmanager) | 85 s | 30 s | 1321 MiB | 842 MiB |
| Every opt-in service except logging | 96 s | 32 s | 2535 MiB | 1845 MiB |

Cold start is `up --detach` for a new instance: creating the cluster and pulling images (the
second profile ran after the first, so images they share were already pulled). Warm start is
`up --detach` after `stop`, restarting the existing cluster. Memory is read 30 s after ready. `up` prints the slowest components once it is ready, and `/readyz` reports
each one's time (`timing`). Re-run the workflow to measure a change.

## Platforms

Verified on **macOS/arm64** and **Linux/amd64** (CI runs the cluster and SDK suites on Linux
every merge). The pinned node image publishes both architectures.
