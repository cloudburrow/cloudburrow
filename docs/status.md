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
| **Resource Manager** | CloudBurrow itself (#298, #301) | Always served, not chosen with `--services`. v3 Projects create, get, search, update labels and delete over gRPC and REST, and v1 `projects.list/get/create/delete`, from the same project registry the console lists; `gcloud projects list` and Terraform's `google_project` work. Folders, the local organization, tags and liens are served too, and IAM policies are stored, not enforced (#1294, #1368); a deleted project stays `DELETE_REQUESTED` for 30 days and can be undeleted |

Opt-in, with `--services`:

| Service | Backed by | State |
|---|---|---|
| **Firestore** | Google's own emulator | Documents and collections through the official SDK |
| **Datastore** | Google's own emulator | Entities and kinds through the official SDK |
| **Bigtable** | Google's own emulator | Tables, column families and rows through the official SDK |
| **Spanner** | Google's own emulator | Instances, databases, DDL and queries through the official SDK |
| **Cloud Scheduler** | CloudBurrow itself (#302) | Jobs with cron schedules in IANA time zones: create, get, list, pause, resume, run and delete, to HTTP and Pub/Sub targets and update, with retries. Survives a restart in persistent mode. App Engine targets are delivered to a local stand-in with `--appengine-url` (#1447). OIDC and OAuth tokens are minted locally, signed with the instance's own key (#1243). HTTP targets are called from the host |
| **Cloud Logging** | CloudBurrow itself (#304) | `WriteLogEntries`, `ListLogEntries` with a documented filter subset, `ListLogs`, `DeleteLog`; entries appear in the console's Logs Explorer. Also sinks (delivered to their destinations), exclusions, buckets and views, log-based metrics and `TailLogEntries`. In memory in every mode, bounded at 20,000 entries |
| **Cloud KMS** | CloudBurrow itself (#309) | Key rings, symmetric, asymmetric and MAC keys and versions, import jobs, Autokey and EKM connections, over gRPC and JSON. **Not a security boundary** — key material is stored unencrypted |
| **Cloud SQL** | PostgreSQL in the cluster; MySQL 8.4 as `cloudsql-mysql` (#297) | Google publishes no Cloud SQL emulator, so each instance is a real PostgreSQL or MySQL server, reached with an ordinary driver, the Cloud SQL connectors or the Auth Proxy. The **Cloud SQL Admin API** is served over them on its own port (`CLOUDBURROW_SQLADMIN_ENDPOINT`, #1301, #1330): 74 of its 80 methods are Verified ([coverage](coverage/sqladmin.md)), among them instances, databases, users, backups, read replicas, high availability, clones, imports and exports. IAM database authentication is 501 — see [cloudsql.md](cloudsql.md) |

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
| `gcloud tasks` | **Verified** | `test/compat/gcloudsecrets_test.go` (`TestGcloudTasks`): queues and tasks over JSON (#591); `RunTask` is verified over gRPC and JSON, `gcloud tasks run` itself is not tested |
| `gcloud scheduler` | **Verified** | `test/compat/gcloudschedulerlogging_test.go` (`TestGcloudScheduler`): jobs through `gcloud-setup`, with `--location` (#591) |
| `gcloud logging` | **Verified** | `test/compat/gcloudschedulerlogging_test.go` (`TestGcloudLogging`): write, read, list logs and delete a log (#591) |
| `gcloud projects list` | **Verified** | `test/compat/resourcemanagerv1_test.go` (`TestGcloudProjectsList`) |
| `gcloud pubsub` | **Verified** | `test/compat/gcloudpubsub_test.go` (`TestGcloudPubSub`): topics create, list, publish and delete; subscriptions create, update, describe, pull and delete |
| `gcloud run` | **Verified** | `TestGcloudRunServicesList`, `TestGcloudRunDeployImage`, `TestGcloudRunJobs` |
| **gsutil** | **Verified** | `test/compat/gcloudstorage_test.go` (`TestGsutilJSONAndHMACXML`) |
| **Terraform** (`hashicorp/google` 8.4.0, Terraform 1.16.6) | **Verified** for the resources compatibility.md lists | `test/compat/terraform_test.go`, `terraformstorage_test.go`, `terraform_kms_test.go`, `terraform_run_test.go`, and `resourcemanagerv1_test.go` (`TestTerraformGoogleProject`) |
| **OpenTofu** 1.12.6 (`cloudburrow terraform --binary tofu`) | **Verified** for the modules its tests apply | `test/compat/terraform_test.go` (`TestTofuAppliesAndDestroysThroughTheWrapper`) and `terraform_scheduler_test.go` (`TestTofuSchedulerAndSubscription`), in the `storage` shard |
| **Tink** (`tink-go` with `tink-go-gcpkms`) | **Verified** | `test/compat/kmstink_test.go` (`TestKMSTinkEnvelopeEncryption`), over gRPC and REST |
| **Node.js** (official Cloud client libraries) | **Verified** for the rows compatibility.md lists | `test/compat-node/storage.test.mjs`, `pubsub.test.mjs`, `tasks.test.mjs`, `secrets.test.mjs`, `firestore.test.mjs` and `examples.test.mjs`, run by `make compat-node` in compat CI's `storage` and `emulators` shards, green since the #759 train (#589) |
| Java, .NET, Ruby, PHP | Untested | No test; no Java suite is planned (#718) |
| Firebase SDKs, Spring Cloud GCP | Untested | No test |
| `bq` | **Verified** | `mk`, `insert`, `query` and `rm`, as compatibility.md's BigQuery section lists |
| Pulumi | Untested | No test |

## Shipped platforms

The release workflow builds four archives. What CI runs on each:

| Archive | Unit tests and build (`check`) | Cluster and SDK suites | Release smoke install |
|---|---|---|---|
| **linux/amd64** | **Verified** (`ubuntu-latest`) | **Verified** (`ubuntu-latest`: `integration` and every compat shard) | **Verified** (`ubuntu-latest`: install, `version`, `doctor`); the Homebrew formula is added from the next release |
| **darwin/arm64** | **Verified** (`macos-latest`) | Untested in CI | **Verified** (`macos-latest`: install, `version`, `doctor`, the Homebrew formula) |
| **darwin/amd64** | Untested: cross-built only | Untested | In the smoke matrix from the next release (`macos-15-intel`: install, `version`, `doctor`, the Homebrew formula); no tagged release has run it yet |
| **linux/arm64** | **Verified** nightly (`ubuntu-24.04-arm`: `make check` and `make build`, first run 2026-09-27, run 36331200218) | **Verified** nightly in `.github/workflows/arm64.yml`: `up` on an arm64 kind cluster, then 8 Storage and 4 Pub/Sub official-SDK tests, all passed on the first run (2026-09-27, run 36331200218) | In the smoke matrix from the next release (`ubuntu-24.04-arm`: install, `version`, `doctor`, the Homebrew formula); no tagged release has run it yet |

The macOS/arm64 cluster run under [Platforms](#platforms) is `make verify-local` on a Mac, dated
in [local-verification.md](local-verification.md), not CI. The smoke install runs only when a
release is tagged, on one native runner per archive; `scripts/smoke-release.sh` fails if the
installed binary's `cloudburrow version` does not report the runner's platform, and
`test/install/release_smoke_matrix_test.go` (`TestReleaseSmokeRunsEveryPublishedArchiveNatively`)
fails if an archive the build publishes has no smoke runner. By hand on 2026-09-27, the v0.1.0
formula installed and passed `brew test` and `brew audit --strict` on linux/arm64 (the
`homebrew/brew` image, Homebrew 4.6.20, under Docker Desktop on macOS/arm64); Homebrew calls
linux/arm64 a Tier 2 platform. The cluster and SDK subset runs on linux/arm64 nightly (#689).

## What will not work, and why

Worth reading before you hit these. Every remaining gap, grouped by service with why it is open
and a rough size, is in [gaps.md](gaps.md).

- **IAM policies are stored, not enforced, apart from five named checks.**
  [ADR-0006](adr/0006-iam-policy-surface.md) adds policy *storage*: Secret Manager (#365),
  Cloud Tasks (#366), Cloud KMS key rings and keys (#428), Cloud Storage buckets and ACLs (#504,
  #1233), Cloud Run services and jobs (#1216), BigQuery tables, routines and row access policies
  (#1231), Pub/Sub (#1367, served by CloudBurrow's front over Google's emulator), Resource
  Manager's projects, folders and organization (#1368), Artifact Registry (#1304), Spanner,
  Bigtable and tags (#1486) and Secure Source Manager (#1527) store policies, and no call of
  theirs reads one but for BigQuery's below. Four exceptions check a token this instance minted (by SHA-256, against its
  own log of minted tokens) and the roles, custom roles and conditions of the project's stored
  policy: Memorystore for Redis Cluster's and Valkey's IAM auth (#1614), Firestore with MongoDB
  compatibility's token and SCRAM sign-ins and each command's permissions (#1738), and IAM
  Credentials' `generateAccessToken`, `generateIdToken`, `signBlob` and `signJwt`, and the IAM
  API's `SignBlob` and `SignJwt`, on a service account the IAM API created (#1747, #1751), and
  BigQuery's per-project IAM of a caller with a minted token: jobs (with the creator rule),
  datasets, tables, routines, models, row access policies, table and routine policies, views
  and authorized views, with folder and organization inheritance and conditions, tag conditions
  on datasets and tables, job creators kept across a restart of the front, custom roles in table
  and routine policies, and stored procedures' bodies (#1767, #1774, #1780; `group:` and
  `domain:` members still grant nothing, [gaps.md](gaps.md)). The instance's own account is not such an account and is not checked
  ([credentials.md](credentials.md)); a BigQuery request without a minted token is served as it.
  The fifth is BigQuery remote models' connection permissions (#1765): `CREATE MODEL … REMOTE
  WITH CONNECTION` and each query of a remote model need `bigquery.connections.use` on the
  connection, and its service account Vertex AI User on the model's project. These checks prove that this instance applies its own
  rule, not that a policy is right on Google. **Do not use CloudBurrow to test whether your
  permissions are correct.**
- **Cloud KMS is not a security boundary, and has no IAM enforcement.** Key material sits
  unencrypted in Kubernetes Secrets. IAM policies on key rings and keys are stored, never
  enforced ([ADR-0006](adr/0006-iam-policy-surface.md), #428): a binding neither grants nor
  denies Encrypt or Decrypt. They are served over gRPC and JSON, and
  Terraform's `google_kms_key_ring_iam_member`, `google_kms_crypto_key_iam_member` and
  `google_kms_crypto_key_iam_binding` apply and destroy (#430). HSM and EKM keys are served with no
  real HSM or external key manager behind them: this instance acts as both.
- **Pub/Sub state does not survive a restart** — its emulator loses topics even with
  `--data-dir`. Measured, not assumed.
- **Cloud Storage methods not built answer 501 `notImplemented`** naming the method — object
  IAM (`objects.getIamPolicy`, `setIamPolicy`, `testIamPermissions`) — and the official Go client
  retries a 501 until its deadline. The gRPC `google.storage.v2` API is served on the same
  port (#1321); what it cannot serve answers `UNIMPLEMENTED`.
  ACLs are stored and returned, never enforced (#1233).
- **Cloud Run configuration we cannot map is refused, not ignored**, with the field named — for
  example `encryptionKeyRevocationAction`, and VPC access or an encryption key on a worker pool or
  an instance. You will see an error rather than silently wrong behaviour. Volumes and the service
  account are mapped (#1346); VPC access, encryption keys, binary authorization, the execution
  environment and session affinity are stored and returned with no effect, as compatibility.md
  says row by row.
- **Knative is not Cloud Run.** Min-instances is verified; max-instances, concurrency, the
  request timeout and resource limits reach the manifest but are not tested under load.
  `UpdateService` redeploys as a new revision (#300); the Revisions API serves Get, List and
  Delete (#299). Traffic splitting and tags are mapped to Knative's traffic block (#1224). Jobs and
  Executions run as Kubernetes batch Jobs (#582), with the same container mapping, over gRPC and
  JSON, so `gcloud run jobs` and Terraform reach them.
- **Cloud Tasks' `max_burst_size` is CloudBurrow's own pick.** Rate limits are enforced (#716):
  `max_dispatches_per_second` through a per-queue token bucket, `max_concurrent_dispatches`
  per queue (#579). Google does not document how it picks the burst; CloudBurrow uses one
  second of the rate, so a queue on Google may burst differently.
- **No GKE management APIs.** Firestore, Datastore, Bigtable and Spanner
  ship as opt-in emulators (above) rather than being absent, and BigQuery as an opt-in
  community emulator (#277) that serves one project only; source builds work
  through Google Buildpacks (#33) without implying the Cloud Build API. The Functions Framework
  and Buildpacks rows are Verified by a dated on-demand CI run (2026-09-27, [run 36357529139](https://github.com/cloudburrow/cloudburrow/actions/runs/36357529139) of
  `functions.yml`, amd64), not by every merge; the emulated arm64 build path is tried by hand only.
- **The admin API needs the instance's admin token** (#553): `Authorization: Bearer` with the
  contents of `<state-dir>/<name>/admin-token`, because on Docker Desktop a cluster workload can
  reach the host's loopback ports. Health, readiness and metrics stay open.
- **`/admin/events` and `/metrics` record only the services CloudBurrow observes** — Cloud Tasks,
  Secret Manager, the Cloud Run adapter, Cloud KMS, Cloud Scheduler, Cloud Logging, Resource Manager
  and the builtin Cloud Storage server, and Pub/Sub, Bigtable, Spanner, Firestore and Datastore,
  whose calls the CLI scrapes from the front in each emulator's pod (#1266, #1267, #1288). BigQuery,
  Cloud SQL and Memorystore have no call observer, and `/metrics` reports them as
  `cloudburrow_service_measured 0` (#600).
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

- Nothing at present: `DeleteTopic` (`TestPubSubDeleteTopic`) and a resumable upload's
  resume after an interrupted chunk (`test_resumable_upload_resumes_after_interruption`, #516),
  once listed here, are now Verified.

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
`up --detach` after `stop`, restarting the existing cluster. Since #1610 a warm start waits for the
cluster's Service network, then replaces CoreDNS's and Knative's pods, which the restarted node had
started before that network worked, and the controller manager runs without leader election: five
warm starts of the default profile on macOS/arm64 took 26 to 32 s (mean 29 s), from 23 to 53 s
(mean 35 s) before. Memory is read 30 s after ready. `up` prints the slowest components once it is ready, and `/readyz` reports
each one's time (`timing`). Re-run the workflow to measure a change.

## Platforms

The cluster and SDK suites run on **Linux/amd64** in CI on every merge, and ran on
**macOS/arm64** (Docker Desktop) on 2026-09-27 with `make verify-local`: 378 passed and one
failed, a stopped Cloud Run instance that did not come back up, since fixed by retrying Knative's
ConfigMap patches while its webhook comes back
([local-verification.md](local-verification.md#2026-09-27-macosarm64-docker-desktop)); linux/arm64 runs a cluster and SDK subset nightly, and darwin/amd64 releases are only smoke-installed
([Shipped platforms](#shipped-platforms)). The pinned node image publishes both architectures.

**Windows is unsupported outside WSL2, and untested inside it**: the CLI does not compile for
Windows and no release has a Windows archive ([install.md](install.md#prerequisites)). On macOS,
whether a release's binaries are signed and notarized, and what to do with one that is not, is in
[Gatekeeper](install.md#gatekeeper).
