# What actually works

A single page answering "can I use this yet?", so nobody has to infer it from a matrix.

**Read [`compatibility.md`](compatibility.md) for the per-operation detail.** Every
`Verified` row there names the test that proves it, and every test drives an **official
Google SDK**. Nothing is promoted on the strength of a curl command.

## The short answer

**The acceptance workflow passes end to end**: upload an object, publish an event, a Cloud Run
worker receives it and reads the object, writes a result, and the result is read back — every
step through an official SDK, including inside the worker. CI runs it on every merge against a
fresh instance, and the job fails if it skips rather than passes (`official SDK compatibility
(acceptance)`, #596).

If that is the shape of what you are building, CloudBurrow can run it today.

**State survives a restart in persistent mode, and is measured.** CI stops and starts each
compat shard's instance and reads back what was written before the stop, then brings it up
with `--mode ephemeral` and requires it gone: Cloud Storage (the in-cluster server and its PVC),
Cloud Tasks, Cloud Scheduler and Secret Manager in `official SDK compatibility (storage)`,
Cloud KMS in `(served)`, and Memorystore, Cloud SQL for MySQL and Datastore in `(emulators)`
(#596). Pub/Sub is the exception, below.

## Per service

| Service | Backed by | State |
|---|---|---|
| **Cloud Storage** | CloudBurrow itself (#485) | Buckets, objects, versioning, soft delete, retention and holds, lifecycle, CORS, IAM (stored), HMAC keys, the XML API and multipart uploads, **signed URLs verified**, notifications |
| **Pub/Sub** | Google's own emulator | Topics, subscriptions, publish, pull, **StreamingPull**, push delivery |
| **Cloud Tasks** | CloudBurrow itself | Queues, tasks, pause/resume, HTTP dispatch with retry |
| **Cloud Run** | Knative Serving | Create, get, list, delete services; deploys real containers; env from Secret Manager |
| **Secret Manager** | CloudBurrow itself | Secrets and versions, enable/disable/destroy, access, over gRPC and JSON. **Not a secret store** — nothing is authenticated |
| **Resource Manager** | CloudBurrow itself (#298, #301) | Always served, not chosen with `--services`. v3 Projects create, get, search, update labels and delete over gRPC and REST, and v1 `projects.list/get/create/delete`, from the same project registry the console lists; `gcloud projects list` and Terraform's `google_project` work. No folders, organizations or IAM; a delete takes effect at once |

Opt-in, with `--services`:

| Service | Backed by | State |
|---|---|---|
| **Firestore** | Google's own emulator | Documents and collections through the official SDK |
| **Datastore** | Google's own emulator | Entities and kinds through the official SDK |
| **Bigtable** | Google's own emulator | Tables, column families and rows through the official SDK |
| **Spanner** | Google's own emulator | Instances, databases, DDL and queries through the official SDK |
| **Cloud Scheduler** | CloudBurrow itself (#302) | Jobs with cron schedules in IANA time zones: create, get, list, pause, resume, run and delete, to HTTP and Pub/Sub targets (`UpdateJob` is unit-tested only), with retries. Survives a restart in persistent mode. No App Engine targets and no OIDC or OAuth tokens. HTTP targets are called from the host |
| **Cloud Logging** | CloudBurrow itself (#304) | `WriteLogEntries`, `ListLogEntries` with a documented filter subset, `ListLogs`, `DeleteLog`; entries appear in the console's Logs Explorer. In memory in every mode, bounded at 20,000 entries. No sinks, exclusions, buckets, log-based metrics or tailing |
| **Cloud KMS** | CloudBurrow itself (#309) | Key rings, symmetric keys and versions, over gRPC; Encrypt and Decrypt also over JSON. **Not a security boundary** — key material is stored unencrypted |
| **Cloud SQL** | PostgreSQL in the cluster; MySQL 8.4 as `cloudsql-mysql` (#297) | **A local SQL database, not the Cloud SQL Admin API.** Google publishes no Cloud SQL emulator, so this is a real PostgreSQL reached with an ordinary driver. No instances, connection names, IAM database authentication, backups or replicas, and no `sqladmin` endpoint — see [#121](https://github.com/cloudburrow/cloudburrow/issues/121) |

## Languages and tools

Verified means a test in the repository drives the client or tool against a running instance,
and CI runs that test. A Go compat test that skips in every shard fails CI's `every compat test ran in a
shard` job, so the gcloud and gsutil tests below, which skip where the tool is absent, do run.

| Language or tool | State | Evidence |
|---|---|---|
| **Go** (official Cloud client libraries) | **Verified** | `test/compat/*_test.go`, for example `storage_test.go`, `pubsub_test.go`, `tasks_test.go`, `secrets_test.go`, `run_test.go`, `kms_test.go`, `scheduler_test.go`, `logging_test.go` and `resourcemanager_test.go`, across CI's five compat shards |
| **Python** (official Cloud client libraries) | **Verified** for Cloud Storage, Pub/Sub, Cloud Tasks and Secret Manager | `test/compat-python/test_storage.py`, `test_pubsub.py`, `test_tasks.py` and `test_secrets.py`, run by `make compat-python` in the `storage` shard. `test_kms.py`, `test_firestore.py`, `test_datastore.py`, `test_bigtable.py`, `test_spanner.py` and `test_bigquery.py` exist but skip in CI, whose Python run does not enable those services |
| `gcloud storage` | **Verified** | `test/compat/gcloudstorage_test.go`, `test/compat/gcloudsetup_test.go` |
| `gcloud kms` | **Verified** | `test/compat/gcloudkms_test.go`, `gcloudkms_iam_test.go`, `gcloudkms_crypto_test.go` |
| `gcloud secrets` | **Verified** | `test/compat/gcloudsecrets_test.go` (`TestGcloudSecrets`) |
| `gcloud tasks` | **Verified** | `test/compat/gcloudsecrets_test.go` (`TestGcloudTasks`): queues and tasks over JSON (#591); `gcloud tasks run` answers 501 |
| `gcloud scheduler` | **Verified** | `test/compat/gcloudschedulerlogging_test.go` (`TestGcloudScheduler`): jobs through `gcloud-setup`, with `--location` (#591) |
| `gcloud logging` | **Verified** | `test/compat/gcloudschedulerlogging_test.go` (`TestGcloudLogging`): write, read, list logs and delete a log (#591) |
| `gcloud projects list` | **Verified** | `test/compat/resourcemanagerv1_test.go` (`TestGcloudProjectsList`) |
| `gcloud pubsub` | **Partial** | `topics list` only, in `test/compat/gcloudsetup_test.go` |
| Other `gcloud` command families (`run`, `scheduler`, `logging`, ...) | Untested | No test |
| **gsutil** | **Verified** | `test/compat/gcloudstorage_test.go` (`TestGsutilJSONAndHMACXML`) |
| **Terraform** (`hashicorp/google` ~> 8.0, Terraform 1.16.4) | **Verified** for the resources compatibility.md lists | `test/compat/terraform_test.go`, `terraformstorage_test.go`, `terraform_kms_test.go`, `terraform_run_test.go`, and `resourcemanagerv1_test.go` (`TestTerraformGoogleProject`) |
| **Tink** (`tink-go` with `tink-go-gcpkms`) | **Verified** | `test/compat/kmstink_test.go` (`TestKMSTinkEnvelopeEncryption`), over gRPC and REST |
| **Node.js** (official Cloud client libraries) | **Partial** | `test/compat-node/storage.test.mjs`, `pubsub.test.mjs`, `tasks.test.mjs`, `secrets.test.mjs`, `firestore.test.mjs` and `examples.test.mjs`, run by `make compat-node` in compat CI's `storage` and `emulators` shards, but **not yet passed in CI** (#589) |
| Java, .NET, Ruby, PHP | Untested | No test |
| Firebase SDKs, Spring Cloud GCP | Untested | No test |
| Pulumi, OpenTofu, `bq` | Untested | No test |

## Shipped platforms

The release workflow builds four archives. What CI runs on each:

| Archive | Unit tests and build (`check`) | Cluster and SDK suites | Release smoke install |
|---|---|---|---|
| **linux/amd64** | **Verified** (`ubuntu-latest`) | **Verified** (`ubuntu-latest`: `integration` and every compat shard) | **Verified** (`ubuntu-latest`: install, `version`, `doctor`) |
| **darwin/arm64** | **Verified** (`macos-latest`) | Untested in CI | **Verified** (`macos-latest`: install, `version`, `doctor`, the Homebrew formula) |
| **darwin/amd64** | Untested: cross-built only | Untested | Untested |
| **linux/arm64** | Untested: cross-built only | Untested | Untested |

The macOS/arm64 cluster run under [Platforms](#platforms) was by hand, on the machine in
[local-verification.md](local-verification.md), not in CI. The smoke install runs only when a
release is tagged.

## What will not work, and why

Worth reading before you hit these:

- **No IAM enforcement, anywhere.** No policy evaluation, no service-account identity. Google's
  Pub/Sub emulator returns `Unimplemented` for IAM methods and we do not paper over it.
  [ADR-0006](adr/0006-iam-policy-surface.md) adds policy *storage*, never enforcement: Secret
  Manager (#365), Cloud Tasks (#366) and Cloud KMS key rings and keys (#428) have it, and so do
  Cloud Storage buckets (#504). **Do not use CloudBurrow to test whether your
  permissions are correct.**
- **Cloud KMS is not a security boundary, and has no IAM enforcement.** Key material sits
  unencrypted in Kubernetes Secrets. IAM policies on key rings and keys are stored, never
  enforced ([ADR-0006](adr/0006-iam-policy-surface.md), #428): a binding neither grants nor
  denies Encrypt or Decrypt. They are served over gRPC and JSON, and
  Terraform's `google_kms_key_ring_iam_member`, `google_kms_crypto_key_iam_member` and
  `google_kms_crypto_key_iam_binding` apply and destroy (#430). There is no HSM, EKM or Autokey. Symmetric
  encryption only.
- **Pub/Sub state does not survive a restart** — its emulator loses topics even with
  `--data-dir`. Measured, not assumed.
- **Cloud Storage methods not built answer 501 `notImplemented`** naming the method — ACLs,
  object IAM, managed folders beyond an empty list, folders, caches, `bulkRestore` and the
  gRPC `google.storage.v2` API — and the official Go client retries a 501 until its deadline.
- **Cloud Run configuration we cannot map is refused, not ignored** — service accounts, VPC
  access, volumes, encryption keys, binary authorization, execution environment, session
  affinity and traffic splitting all return `Unimplemented` naming the field. You will see an
  error rather than silently wrong behaviour. (Secret-backed env used to be listed here in
  error: it is mapped and verified.)
- **Knative is not Cloud Run.** Min-instances is verified; max-instances, concurrency, the
  request timeout and resource limits reach the manifest but are not tested under load.
  `UpdateService` redeploys as a new revision (#300); the Revisions API serves Get, List and
  Delete (#299). Traffic splitting is not mapped. Jobs and Executions run as Kubernetes batch Jobs
  (#582), with the same container mapping, over gRPC only.
- **Cloud Tasks does not enforce rate limits.**
- **No GKE management APIs.** Firestore, Datastore, Bigtable and Spanner
  ship as opt-in emulators (above) rather than being absent, and BigQuery as an opt-in
  community emulator (#277) that serves one project only; source builds work
  through Google Buildpacks (#33) without implying the Cloud Build API.
- **The admin API needs the instance's admin token** (#553): `Authorization: Bearer` with the
  contents of `<state-dir>/<name>/admin-token`, because on Docker Desktop a cluster workload can
  reach the host's loopback ports. Health, readiness and metrics stay open.
- **`/admin/events` and `/metrics` record only the services CloudBurrow observes** — Cloud Tasks,
  Secret Manager, the Cloud Run adapter, Cloud KMS, Cloud Scheduler, Cloud Logging, Resource Manager
  and the builtin Cloud Storage server. Pub/Sub and the opt-in emulators are reached over a raw
  port-forward to an upstream process, so their calls are not observed, and `/metrics` reports them
  as `cloudburrow_service_measured 0` (#600).
- **The request log, tracing and fault injection cover the same in-process gRPC services** —
  Cloud Tasks, Secret Manager, the Cloud Run adapter, Cloud KMS, Cloud Scheduler, Cloud Logging and
  Resource Manager (#600): `cloudburrow logs --service <name>` shows their request lines, each call
  is a span when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, and `/admin/faults` accepts rules for them
  when they are enabled. JSON API requests are neither request-logged, traced nor faulted, and
  Storage, Pub/Sub and the opt-in emulators are outside all three: their requests appear in their
  own servers' logs, which `cloudburrow logs` reads.

## Deliberately unclaimed

Some things are implemented and tested but **not** marked `Verified`, because no official SDK
has driven them:

- `PurgeQueue` (Cloud Tasks) — unit-tested only
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

**Windows is unsupported outside WSL2, and untested inside it**: the CLI does not compile for
Windows and no release has a Windows archive ([install.md](install.md#prerequisites)). On macOS,
whether a release's binaries are signed and notarized, and what to do with one that is not, is in
[Gatekeeper](install.md#gatekeeper).
