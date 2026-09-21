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
| **Cloud Run** | Knative Serving | Create, get, list, delete services; deploys real containers |

## What will not work, and why

Worth reading before you hit these:

- **No IAM, anywhere.** No policy evaluation, no service-account identity. Google's Pub/Sub
  emulator returns `Unimplemented` for IAM methods and we do not paper over it. **Do not use
  CloudBurrow to test whether your permissions are correct.**
- **Pub/Sub state does not survive a restart** — its emulator loses topics even with
  `--data-dir`. Measured, not assumed.
- **Signed URL signatures are not verified.** A signed URL is accepted on shape alone, so
  this cannot test signing correctness.
- **Cloud Run configuration we cannot map is refused, not ignored** — service accounts, VPC
  access, volumes, encryption keys, secret-backed env and traffic splitting all return
  `Unimplemented` naming the field. You will see an error rather than silently wrong
  behaviour.
- **Knative is not Cloud Run.** Scaling annotations are mapped but their behaviour is
  untested (#30). `UpdateService` and the Revisions API are not served.
- **Cloud Tasks does not enforce rate limits**, and `maxDoublings` is not modelled.
- **No Cloud Run Jobs, no source builds, no GKE APIs, no BigQuery/Firestore/Spanner.**
- **`/admin/events` returns an empty list.** The recorder works, but nothing records events in
  the serving path yet.

## Deliberately unclaimed

Some things are implemented and tested but **not** marked `Verified`, because no official SDK
has driven them:

- `PurgeQueue`, `ListTasks` (Cloud Tasks) — unit-tested only
- `DeleteTopic` (Pub/Sub) — called during test cleanup, never asserted
- `buckets.list`, `objects.copy`/`rewrite`, multipart upload — not exercised
- Resumable upload **interruption and resume** — the happy path is verified, recovery is not

The distinction is the point: called is not the same as verified.

## Platforms

Verified on **macOS/arm64** and **Linux/amd64** (CI runs the cluster and SDK suites on Linux
every merge). The pinned node image publishes both architectures.
