# Support matrix

This file is the authoritative statement of what CloudBurrow supports.

**Three separate things are tracked separately**, because conflating them is how a tool
over-promises:

| Dimension | Question | Where |
|---|---|---|
| **GCP API compatibility** | Does an official Google SDK call behave correctly? | §Services below |
| **Native Kubernetes portability** | Do ordinary manifests, Helm charts and operators work? | §Native Kubernetes |
| **Knative feature coverage** | Which Cloud Run v2 behaviors does the adapter actually map? | §Cloud Run |

A component being adopted from upstream does not make its operations supported. **Upstream
behavior still has to be demonstrated through an official SDK** before it is promoted here. It is not a roadmap
and not an aspiration. If an operation is not listed as `Verified` here, CloudBurrow does not
support it, regardless of what any other document, README, or commit message says.

## Status values

| Status | Meaning |
|---|---|
| `Planned` | Not implemented. May return `UNIMPLEMENTED`, or may not be routed at all. |
| `Partial` | Implemented with documented gaps. The gap is named in the Notes column; an unexplained `Partial` is a documentation bug. |
| `Verified` | Implemented **and** exercised by a merged test that drives it through an official Google client library. |

**The promotion rule.** An operation moves off `Planned` only in a PR that adds a test using
an official Google SDK — `cloud.google.com/go/...`, `google-cloud-*` for Python, and so on.
A curl invocation, a handwritten HTTP request, or an internal unit test is not sufficient
evidence, because those test our understanding of the API rather than the client's.

**Control plane vs data plane.** These are tracked separately because they fail
independently, and conflating them is the standard way an emulator overstates itself.
Creating a Cloud Run service and receiving a well-formed `Service` response says nothing
about whether a container ran. A service is described as supported only when both columns are
`Verified`.

**Backing component** is recorded per service, from the
[upstream reuse audit](upstream-evaluation.md). Inherited limitations are listed with the
service — an upstream gap is still our gap as far as a user is concerned.

**Verified** rows below are backed by `test/compat`, run against a live `cloudburrow up` on
2026-09-20. Everything else remains `Planned`: exercised-by-hand is not evidence, and an
operation a test merely touches in cleanup is not asserted.

## Native Kubernetes portability

Tested by #29, independently of any GCP API. A cluster that answers GCP calls is not
automatically a cluster your manifests run on, so this is never inferred from the sections
below.

| Capability | Status | Notes |
|---|---|---|
| `kubectl apply` of ordinary manifests | **Verified** | `internal/cluster` integration test deploys a Deployment and waits for Available. |
| Helm chart install | Planned | |
| Custom resources and operators | Planned | |
| PersistentVolumeClaims | Partial | A PVC is created and bound for the storage backend, but survival of data across a restart is not yet asserted by a test. |
| **Unmodified GKE manifests** | **Not promised** | Endpoint configuration and local overlays legitimately differ. No universal claim is made. |

---

## Cloud Storage — JSON API v1

**Backing component:** `fake-gcs-server` v1.56.1 (integrate + adapt).
Contract: Cloud Storage JSON API v1.
Client endpoint override: `STORAGE_EMULATOR_HOST` (Go, Python — see architecture §4.2).

**Inherited limitation:** signed-URL signatures are **not verified** — a request with a bogus
`X-Goog-Signature` returned HTTP 200. Not a tool for testing signing correctness.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `buckets.insert` | **Verified** | `TestStorageBucketLifecycle` |
| `buckets.get` | **Verified** | `TestStorageBucketLifecycle` |
| `buckets.list` | Planned | Not exercised. |
| `buckets.delete` | **Verified** | `TestStorageBucketLifecycle`, including `ErrBucketNotExist` afterwards. Rejection of non-empty buckets is **not** covered. |
| `buckets.patch` / `buckets.update` | Planned | |
| `objects.list` | **Verified** | `TestStorageListWithPrefix` covers prefix **and** delimiter, asserting both the direct children and the synthetic `dir/sub/` prefix. |
| `objects.get` (metadata) | **Verified** | `TestStorageObjectRoundTrip` |
| `objects.delete` | **Verified** | `TestStorageObjectRoundTrip`, including `ErrObjectNotExist` afterwards. |
| `objects.copy` / `objects.rewrite` | Planned | Not exercised. `rewrite` is chunked and token-driven; not a `copy` alias. |
| `objects.compose` | **Verified** | `TestStorageCompose` |
| Bucket/object IAM methods | Planned | Non-goal for the first release. Will stub or return `UNIMPLEMENTED`; behavior to be recorded here, not assumed. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| Simple upload (`uploadType=media`) | **Verified** | `TestStorageObjectRoundTrip` |
| Multipart upload (`uploadType=multipart`) | Planned | Not separately exercised. |
| Resumable upload (`uploadType=resumable`) | **Verified** | `TestStorageResumableUploadAndRangedRead` writes 8 MiB in 256 KiB chunks. **Interruption and resume are not covered.** |
| Download | **Verified** | `TestStorageObjectRoundTrip` |
| Ranged download | **Verified** | `TestStorageResumableUploadAndRangedRead`, byte-exact. |
| Generation / metageneration preconditions | **Verified** | `TestStorageGenerationPreconditions`: `DoesNotExist` on an existing object and a stale `GenerationMatch` are both refused with HTTP 412, and a matched precondition advances the generation. |
| Signed URLs | Planned | **Signatures will not be verified.** A signed URL is accepted on shape alone. Not a tool for testing signing correctness. |
| CRC32C / MD5 validation | Planned | Clients verify these; wrong checksums surface as client-side corruption errors. |

## Cloud Storage — gRPC (`google.storage.v2`)

Deferred. Not part of the first release; see architecture §10. Requires a separate port from
the JSON API (`STORAGE_EMULATOR_HOST_GRPC`).

| Operation | Status | Notes |
|---|---|---|
| All | Planned | Deferred until the JSON API surface is `Verified`. |

---

## Pub/Sub — `google.pubsub.v1`

**Backing component:** Google `cloud-pubsub-emulator` 0.8.35 (integrate).
Contract: `google/pubsub/v1/pubsub.proto`. Client endpoint override: `PUBSUB_EMULATOR_HOST`.

**Inherited limitations**, measured in #24:

- **State is not persisted.** A topic was `NotFound` after restart even with `--data-dir`.
  Pub/Sub is effectively memory-only, and CloudBurrow will not imply otherwise.
- **IAM methods return `Unimplemented`.** The emulator announces this at startup.
- The emulator describes itself as a "fake" that "may be incomplete or differ from the real
  system."

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `Publisher.CreateTopic` | **Verified** | `TestPubSubTopicLifecycle` |
| `Publisher.GetTopic` | **Verified** | `TestPubSubTopicLifecycle`, including `NotFound` for an absent topic. |
| `Publisher.ListTopics` | Planned | Not exercised. |
| `Publisher.DeleteTopic` | Planned | Called during cleanup but not asserted, so not claimed. |
| `Subscriber.CreateSubscription` | **Verified** | Pull configuration only; push is not covered. |
| `Subscriber.GetSubscription` / `ListSubscriptions` | Planned | |
| `Subscriber.DeleteSubscription` | Planned | |
| `Subscriber.UpdateSubscription` | Planned | |
| Schema service | Planned | Likely out of scope for the first release. |
| Snapshots / `Seek` | Planned | Likely out of scope for the first release. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| `Publisher.Publish` | **Verified** | `TestPubSubPublishAndPull`, data and attributes both asserted. |
| `Subscriber.Pull` | **Verified** | `TestPubSubPublishAndPull` |
| `Subscriber.StreamingPull` | **Verified** | `TestPubSubStreamingPull` delivers 20 distinct messages through the SDK's default `Receive` path. |
| `Acknowledge` | **Verified** | `TestPubSubPublishAndPull` |
| `ModifyAckDeadline` | Planned | |
| Ack-deadline expiry and redelivery | Planned | At-least-once. Duplicates are possible by design. |
| Push delivery to HTTP endpoint | Planned | Must reach Cloud Run services by their container-network address. |
| Ordering keys | Planned | |
| Dead-letter topics | Planned | |

---

## Cloud Tasks — `google.cloud.tasks.v2`

**Backing component:** none — implemented by CloudBurrow. No official emulator exists and no
viable community implementation was found (#24).
Contract: `google/cloud/tasks/v2/cloudtasks.proto`.
**No emulator environment variable exists.** Callers must set an explicit endpoint and
disable authentication in client options. This is a documented ergonomic limit.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `CreateQueue` | Planned | |
| `GetQueue` / `ListQueues` | Planned | |
| `DeleteQueue` / `UpdateQueue` | Planned | |
| `PauseQueue` / `ResumeQueue` / `PurgeQueue` | Planned | |
| `CreateTask` | Planned | Including `scheduleTime`. |
| `GetTask` / `ListTasks` / `DeleteTask` | Planned | |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| HTTP target dispatch | Planned | |
| App Engine target dispatch | Planned | Out of scope; no App Engine runtime. Will report unsupported rather than silently dropping tasks. |
| Scheduled execution at `scheduleTime` | Planned | Uses the injected clock. |
| Retry with backoff | Planned | Cloud Tasks retry config, distinct from Pub/Sub redelivery. |
| `RunTask` (forced immediate run) | Planned | |
| Rate limits / concurrency caps | Planned | |

---

## Cloud Run — `google.cloud.run.v2`

**Backing component:** Knative Serving v1.23.0, behind a CloudBurrow-owned Cloud Run v2
adapter. Contract: `google/cloud/run/v2/*.proto`. Admin API is REST. No emulator environment
variable. Requires a local Kubernetes cluster; its absence is a clear capability error, not a
silent degradation.

> **Knative is not Cloud Run.** It is the closest available model. No blanket claim is made
> that it reproduces Cloud Run semantics. Revision traffic and scaling behavior are mapped and
> tested explicitly in #30; whatever is not tested there is not claimed here.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `CreateService` | Planned | Long-running operation. |
| `GetService` / `ListServices` | Planned | |
| `UpdateService` / `DeleteService` | Planned | |
| `GetRevision` / `ListRevisions` | Planned | |
| Operations (`google.longrunning`) | Planned | Pending, completed, and failed states must all be reachable. |
| IAM methods | Planned | Non-goal. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| Pull image and start container | Planned | Prebuilt images only. No source builds. |
| Readiness detection | Planned | |
| Request routing to container | Planned | |
| Injected environment (endpoints, `PORT`) | Planned | Container-network addresses, not host loopback. |
| Logs | Planned | |
| Stop and cleanup | Planned | Only CloudBurrow-owned containers, identified by instance labels. |
| Autoscaling / scale-to-zero | Planned | Non-goal. Fixed single container. |
| Traffic splitting across revisions | Planned | Non-goal beyond acceptance-workflow needs. |
| Jobs / Executions | Planned | Out of scope for the first release. |

---

## Cross-cutting

| Concern | Status | Notes |
|---|---|---|
| Google-style error bodies and gRPC status codes | **Verified** | `internal/apierror`: one cause renders a consistent gRPC code and HTTP status, plus the JSON envelope a Google client parses. Not yet wired into a served surface. |
| Pagination with deterministic ordering | **Verified** | `internal/paging`: full-walk coverage proves no duplicates or gaps; invalid and cross-listing tokens are rejected. Not yet wired into a served surface. |
| Long-running operations | **Verified** | `internal/lro`: pending, succeeded and failed are all reachable and terminal states are final. Not yet wired into a served surface. |
| Project isolation | **Verified** | `TestPubSubProjectIsolation`; `internal/resource` makes it structural — identical IDs in different projects or locations produce different keys by construction. |
| REST and gRPC transport | **Verified** | `internal/transport`: unknown REST paths return a Google 404 envelope and unknown gRPC methods return `Unimplemented`; request sizes are bounded and unknown JSON fields rejected. No service is registered on it yet. |
| Scheduling, retry and backoff over an injected clock | **Verified** | `internal/sched`: retry timing is proven by advancing virtual time, never by sleeping. Concurrent duplicate attempts are prevented, and shutdown drains in-flight work. |
| Metadata storage, memory and durable modes | **Verified** | `internal/store`: both modes share one test body; durable state survives restart, a second instance is refused, a failed commit leaves state unchanged, and unsafe keys are rejected. |
| Resource-name parsing and traversal safety | **Verified** | `internal/resource` rejects `..`, encoded separators, NUL and path separators in IDs. |
| Reset / seed / event inspection | Planned | Issue #18. Admin API, loopback-only. |
| Go SDK compatibility harness | **Verified** | `test/compat`. Refuses non-loopback endpoints and fails outright if cloud credentials are present in the environment. |
| Local cluster lifecycle (up/status/stop/reset/delete) | **Verified** | `internal/cluster` integration tests. |
| Cluster ownership isolation | **Verified** | Prefix enforced at construction and re-checked on delete; namespace reset requires `cloudburrow.dev/owned=true`. |
| Python SDK compatibility harness | Planned | Go only so far. |
| Java / Node SDK support | Planned | Endpoint-override mechanism not yet verified against client source. No support claimed. |
