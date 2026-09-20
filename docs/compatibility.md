# Supported-operation matrix

This file is the authoritative statement of what CloudBurrow supports. It is not a roadmap
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

Every row below is `Planned`. Nothing is implemented yet.

---

## Cloud Storage — JSON API v1

Primary surface for the MVP. Contract: Cloud Storage JSON API v1.
Client endpoint override: `STORAGE_EMULATOR_HOST` (Go, Python — see architecture §4.2).

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `buckets.insert` | Planned | |
| `buckets.get` | Planned | |
| `buckets.list` | Planned | |
| `buckets.delete` | Planned | Must reject non-empty buckets. |
| `buckets.patch` / `buckets.update` | Planned | |
| `objects.list` | Planned | Prefix and delimiter behavior is the hard part; needs explicit coverage. |
| `objects.get` (metadata) | Planned | |
| `objects.delete` | Planned | |
| `objects.copy` / `objects.rewrite` | Planned | `rewrite` is chunked and token-driven; not a `copy` alias. |
| `objects.compose` | Planned | |
| Bucket/object IAM methods | Planned | Non-goal for the first release. Will stub or return `UNIMPLEMENTED`; behavior to be recorded here, not assumed. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| Simple upload (`uploadType=media`) | Planned | |
| Multipart upload (`uploadType=multipart`) | Planned | |
| Resumable upload (`uploadType=resumable`) | Planned | Session URLs, chunk offsets, and interruption/resume. Default path for large objects in most clients. |
| Download | Planned | |
| Ranged download | Planned | Range requests and partial content. |
| Generation / metageneration preconditions | Planned | `ifGenerationMatch` and friends; required for correct concurrent-write tests. |
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

Contract: `google/pubsub/v1/pubsub.proto`. Client endpoint override: `PUBSUB_EMULATOR_HOST`.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `Publisher.CreateTopic` | Planned | |
| `Publisher.GetTopic` / `ListTopics` | Planned | |
| `Publisher.DeleteTopic` | Planned | |
| `Subscriber.CreateSubscription` | Planned | Both pull and push configurations. |
| `Subscriber.GetSubscription` / `ListSubscriptions` | Planned | |
| `Subscriber.DeleteSubscription` | Planned | |
| `Subscriber.UpdateSubscription` | Planned | |
| Schema service | Planned | Likely out of scope for the first release. |
| Snapshots / `Seek` | Planned | Likely out of scope for the first release. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| `Publisher.Publish` | Planned | |
| `Subscriber.Pull` | Planned | |
| `Subscriber.StreamingPull` | Planned | The default path for the Go and Python clients. Bidirectional streaming, flow control, and lifecycle. Harder than `Pull` and cannot be skipped. |
| `Acknowledge` | Planned | |
| `ModifyAckDeadline` | Planned | |
| Ack-deadline expiry and redelivery | Planned | At-least-once. Duplicates are possible by design. |
| Push delivery to HTTP endpoint | Planned | Must reach Cloud Run services by their container-network address. |
| Ordering keys | Planned | |
| Dead-letter topics | Planned | |

---

## Cloud Tasks — `google.cloud.tasks.v2`

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

Contract: `google/cloud/run/v2/*.proto`. Admin API is REST. No emulator environment variable.
Requires Docker; unavailable Docker is a clear capability error, not a silent degradation.

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
| Google-style error bodies and gRPC status codes | Planned | Issue #6. |
| Pagination with deterministic ordering | Planned | Invalid tokens must be rejected, not ignored. |
| Long-running operations | Planned | |
| Project isolation | Planned | Same resource ID in two projects must not collide. |
| Reset / seed / event inspection | Planned | Issue #18. Admin API, loopback-only. |
| Go SDK compatibility harness | Planned | Issue #10. |
| Python SDK compatibility harness | Planned | Issue #10. |
| Java / Node SDK support | Planned | Endpoint-override mechanism not yet verified against client source. No support claimed. |
