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
| `kubectl apply` of ordinary manifests | **Verified** | `test/k8s`: Namespace, ConfigMap, Secret, multi-replica Deployment and Service applied directly, with `readyReplicas` asserted. |
| Helm chart install | **Verified** | `TestHelmChartInstalls` installs a chart and asserts the release reports `deployed`. |
| Custom resources and operators | **Verified** | `TestCustomResourceDefinition` establishes a CRD and reads back a custom resource — the operator pattern. |
| PersistentVolumeClaims | **Verified** | `TestPersistentVolumeClaimBindsAndPersists` writes from one pod and reads from **another**, which is what proves the volume persisted rather than the data living in one container. |
| Jobs | **Verified** | Used by the PVC test; `batch/v1` Jobs run to completion. |
| **Unmodified GKE manifests** | **Not promised** | Endpoint configuration and local overlays legitimately differ. No universal claim is made, and nothing here tests a GKE-specific API. |

---

## Cloud Storage — JSON API v1

**Backing component:** `fake-gcs-server` v1.56.1 (integrate + adapt).

**Two endpoints, same objects.** `-public-host` is a single process-wide value and the
official client's read path `/{bucket}/{object}` is matched against it, so one process can
serve reads to exactly one audience. CloudBurrow therefore runs a second deployment sharing
the same volume: `storage` for the developer's machine and `storage-internal` for workloads
inside the cluster. `cloudburrow up` prints both. They are separate processes over one
filesystem — fine for a development emulator, but not a concurrency guarantee.
Contract: Cloud Storage JSON API v1.
Client endpoint override: `STORAGE_EMULATOR_HOST` (Go, Python — see architecture §4.2).

**Inherited limitation:** signed-URL signatures are **not verified** — a request with a bogus
`X-Goog-Signature` returned HTTP 200. Not a tool for testing signing correctness.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `buckets.insert` | **Verified** | `TestStorageBucketLifecycle` |
| `buckets.get` | **Verified** | `TestStorageBucketLifecycle` |
| `buckets.list` | **Verified** | `TestStorageBucketsList` |
| `buckets.delete` | **Verified** | `TestStorageBucketLifecycle`. `TestStorageNonEmptyBucketDelete` confirms a non-empty bucket is refused with HTTP 412 `conditionNotMet`, matching the real service. |
| `buckets.patch` / `buckets.update` | **Verified** | `TestBucketUpdateChangesMetadata`, through `Bucket.Update`. The change is re-read rather than trusted from the response. |
| `objects.list` | **Verified** | `TestStorageListWithPrefix` covers prefix **and** delimiter, asserting both the direct children and the synthetic `dir/sub/` prefix. |
| `objects.get` (metadata) | **Verified** | `TestStorageObjectRoundTrip` |
| `objects.delete` | **Verified** | `TestStorageObjectRoundTrip`, including `ErrObjectNotExist` afterwards. |
| `objects.copy` | **Verified** | `TestStorageCopy`; the source survives. |
| `objects.rewrite` | **Verified** | `TestObjectCopyAndRewrite`, within a bucket and **across** buckets — the cross-bucket path is the one that actually uses rewrite. Content is compared after the copy. |
| `objects.compose` | **Verified** | `TestStorageCompose` |
| Bucket/object IAM methods | **Verified unsupported** | `TestBucketIAMIsRefusedRatherThanStubbed`: the call **fails** (404). It is not stubbed, because an empty policy reads as "no bindings" rather than "not implemented". |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| Simple upload (`uploadType=media`) | **Verified** | `TestStorageObjectRoundTrip` |
| Multipart upload (`uploadType=multipart`) | **Verified** | `TestMultipartUploadStoresMetadataWithContent`. Custom metadata and content type survive alongside the bytes — if the multipart body were parsed as content, the object would be wrong in a way a plain read would not reveal. |
| Resumable upload (`uploadType=resumable`) | **Verified** | `TestStorageResumableUploadAndRangedRead` writes 8 MiB in 256 KiB chunks. **Interruption and resume are not covered.** |
| Download | **Verified** | `TestStorageObjectRoundTrip` |
| Ranged download | **Verified** | `TestStorageResumableUploadAndRangedRead`, byte-exact. |
| Generation / metageneration preconditions | **Verified** | `TestStorageGenerationPreconditions`: `DoesNotExist` on an existing object and a stale `GenerationMatch` are both refused with HTTP 412, and a matched precondition advances the generation. |
| Signed URLs | Planned | **Signatures will not be verified.** A signed URL is accepted on shape alone. Not a tool for testing signing correctness. |
| CRC32C / MD5 validation | **Verified** | `TestStorageChecksums`: both are returned, and the client verifies on read, so a wrong value would fail the test. |
| `objects.patch` (metadata update) | **Verified** | `TestObjectMetadataUpdate`. The bytes are re-read afterwards: a metadata patch that rewrote content would corrupt an object silently. |

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
| `Publisher.ListTopics` | **Verified** | `TestPubSubListTopicsAndSubscriptions` |
| `Publisher.DeleteTopic` | **Verified** | `TestPubSubDeleteTopic`, including `NotFound` on a second delete rather than a silent success. |
| `Subscriber.CreateSubscription` | **Verified** | Pull configuration only; push is not covered. |
| `Subscriber.GetSubscription` | **Verified** | `TestSubscriptionGetUpdateDelete`. |
| `Subscriber.ListSubscriptions` | **Verified** | `TestPubSubListTopicsAndSubscriptions` |
| `Subscriber.DeleteSubscription` | **Verified** | Same test; a subsequent get returns `NOT_FOUND`. |
| `Subscriber.UpdateSubscription` | **Verified** | Same test, with a field mask. The change is re-read, because a response echoing the request proves nothing. |
| Schema service | Planned | Not exercised. The v2 Go client exposes no schema surface on `Client`, so driving it would need a separate generated client. |
| Snapshots / `Seek` | **Verified** | `TestSnapshotsAndSeekAreSupported` — create, get, seek-to-snapshot and delete. **The matrix was wrong**: a test written to confirm these were refused found they work. Whether unacked messages are genuinely replayed on seek is a stronger claim and is **not** made. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| `Publisher.Publish` | **Verified** | `TestPubSubPublishAndPull`, data and attributes both asserted. |
| `Subscriber.Pull` | **Verified** | `TestPubSubPublishAndPull` |
| `Subscriber.StreamingPull` | **Verified** | `TestPubSubStreamingPull` delivers 20 distinct messages through the SDK's default `Receive` path. |
| `Acknowledge` | **Verified** | `TestPubSubPublishAndPull` |
| `ModifyAckDeadline` | **Verified** | `TestPubSubRedeliveryAfterNack` |
| Ack-deadline expiry and redelivery | **Verified** | `TestPubSubRedeliveryAfterNack`: returning the deadline redelivers the message. At-least-once, so duplicates remain possible by design. |
| Push delivery to HTTP endpoint | **Verified** | The acceptance workflow pushes to a Cloud Run service at its cluster-local address. |
| Ordering keys | **Verified** | `TestPubSubOrderingKeys`: order preserved within a single key. Ordering *across* keys is not claimed. |
| Dead-letter topics | Partial | `TestDeadLetterPolicyIsRecorded`: the policy round-trips, topic and max attempts included. **Whether messages are actually routed to the dead-letter topic after the attempt limit is not tested and not claimed.** |

---

## Cloud Tasks — `google.cloud.tasks.v2`

**Backing component:** none — implemented by CloudBurrow. No official emulator exists and no
viable community implementation was found (#24).

**It runs in the CLI process, not the cluster**, since there is no upstream workload to
deploy. Its dispatch targets are therefore addresses reachable from the host. There is **no
emulator environment variable** in any official client, so the endpoint and insecure
credentials must be passed explicitly in client options — see `test/compat/tasks_test.go`
for the exact form.
Contract: `google/cloud/tasks/v2/cloudtasks.proto`.
**No emulator environment variable exists.** Callers must set an explicit endpoint and
disable authentication in client options. This is a documented ergonomic limit.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `CreateQueue` | **Verified** | `TestTasksQueueLifecycle`, via the official `cloudtasks/apiv2` client. Contract defaults are returned. |
| `GetQueue` | **Verified** | `TestTasksQueueLifecycle`, including `NotFound` for an absent queue. |
| `ListQueues` | **Verified** | `TestTasksListQueues`; pagination covered by unit tests. |
| `DeleteQueue` | **Verified** | Removes the queue's tasks too, so a recreated queue cannot inherit them. |
| `UpdateQueue` | Planned | Returns `Unimplemented`, asserted by `TestTasksUnsupportedOperationsAreHonest`. |
| `PauseQueue` / `ResumeQueue` | **Verified** | `TestTasksPauseAndResume`. A paused queue genuinely stops dispatching. |
| `PurgeQueue` | Partial | Implemented and unit-tested; not driven by an SDK test. |
| `CreateTask` | **Verified** | `TestTasksTaskLifecycle`. Name generation, method, body and headers all round-trip. |
| `GetTask` / `DeleteTask` | **Verified** | `TestTasksTaskLifecycle`. |
| `ListTasks` | Partial | Implemented; covered by gRPC unit tests, not by an SDK test. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| HTTP target dispatch | Partial | Implemented with the `X-CloudTasks-*` headers handlers read; non-2xx is retried, matching the real service including 4xx. Unit-tested end to end against a real HTTP server, but **not yet proven through an SDK test**, which would need a task to actually fire during a compat run. |
| App Engine target dispatch | **Verified unsupported** | `TestTasksUnsupportedOperationsAreHonest`: returns `Unimplemented` rather than accepting a task that would never be dispatched. |
| Scheduled execution at `scheduleTime` | Partial | Uses the injected clock; verified by advancing virtual time. Not yet served. |
| Retry with backoff | Partial | Driven by the queue's own `RetryConfig`, distinct from Pub/Sub redelivery. Exhausted tasks are dropped, as the real service does. **`maxDoublings` is not yet modelled.** |
| `RunTask` (forced immediate run) | **Verified unsupported** | Returns `Unimplemented` through the SDK. |
| Rate limits / concurrency caps | Planned | Stored and returned, but **not enforced**. |

---

## Cloud Run — `google.cloud.run.v2`

**Backing component:** Knative Serving v1.23.0, behind a CloudBurrow-owned Cloud Run v2
adapter. Contract: `google/cloud/run/v2/*.proto`. Admin API is REST. No emulator environment
variable. Requires a local Kubernetes cluster; its absence is a clear capability error, not a
silent degradation.

> **Knative is not Cloud Run.** It is the closest available model. No blanket claim is made
> that it reproduces Cloud Run semantics. Revision traffic and scaling behavior are mapped and
> tested explicitly in #30; whatever is not tested there is not claimed here.
>
> The adapter **refuses configuration it cannot map** — service accounts, VPC access, volumes,
> encryption keys, secret-backed environment and traffic splitting all return `Unimplemented`
> with the field named. A caller who set one of these and had it silently dropped would
> believe it took effect.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `CreateService` | **Verified** | `TestRunServiceLifecycle`: deploys a real container through the official SDK and returns a long-running operation. |
| `GetService` | **Verified** | `TestRunServiceLifecycle`, including `NotFound` for an absent service. |
| `ListServices` | **Verified** | `TestRunServiceLifecycle`. Only CloudBurrow-owned Services are listed. |
| `DeleteService` | **Verified** | Refuses to delete a Knative Service CloudBurrow did not create. |
| `UpdateService` | Planned | Returns `Unimplemented`. |
| `GetRevision` / `ListRevisions` | Planned | Revision names are surfaced on the Service, but the Revisions API is not served. |
| Operations (`google.longrunning`) | **Verified** | `GetOperation` backs the SDK's `op.Wait`. Pending, succeeded and failed are all reachable; a failed revision reports Knative's own message. |
| IAM methods | Planned | Non-goal. |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| Pull image and start container | **Verified** | Prebuilt images only. Local images are rewritten to `dev.local/` with `imagePullPolicy: Never`; untagged references are refused. |
| Readiness detection | **Verified** | Reported from Knative's `Ready` condition, including the failure message. A service still reconciling is pending, not failed. |
| Request routing to container | **Verified** | The acceptance workflow delivers a Pub/Sub push to a deployed Cloud Run service, which reads and writes Cloud Storage. |
| Injected environment (endpoints, `PORT`) | Partial | `env` is mapped and ordered deterministically and is used by the acceptance workflow; CloudBurrow does not yet inject its own endpoints automatically. |
| Logs | Planned | Not served through the Cloud Run API. |
| Stop and cleanup | **Verified** | Deletion refuses any Knative Service lacking `cloudburrow.dev/owned=true`. |
| Scale to keep an instance warm (`minInstanceCount`) | **Verified** | `TestMinScaleKeepsAnInstanceWarm`: a pod stays running with no traffic. |
| Scale to zero | **Verified** | `TestScaleToZeroHappensWithoutTraffic`, observed on Knative's own schedule. Cloud Run also scales to zero but on a different schedule; the timing is **not** claimed to match. |
| Max instances (`maxInstanceCount`) | Partial | `TestMaxScaleIsRecordedOnTheRevision` proves the annotation reaches the revision. **The cap is not tested under load** — proving a ceiling needs sustained concurrency that would make the suite slow and flaky. |
| Traffic to the latest revision | **Verified** | `TestSingleRevisionTakesAllTraffic`: 100% to the latest revision. |
| Traffic splitting across revisions | **Verified unsupported** | Reported as `Unimplemented` rather than silently ignored. |
| Service name validated before apply | **Verified** | An invalid name previously reached the cluster and came back as `Internal` — which says the fault is ours when it is the caller's, and buries the rule. Now `InvalidArgument` naming the rule. A cluster rejection is also classified as `InvalidArgument` rather than `Internal`, and carries the cluster's own message. |
| Jobs / Executions | Planned | Out of scope for the first release. |
| **Service URL reachable from a browser** | **Verified** | `cloudburrow up` publishes the Knative gateway on a host port (default `9080`) and names services `<service>.<namespace>.cloudburrow.localhost`. Verified with headless Chrome and `test/k8s/ingress_test.go`. **Loopback only**, HTTP only. The mapping is fixed at cluster creation, and `up` reports when a cluster predates it rather than leaving a port that silently refuses. |
| Service URL DNS resolution | Partial | `*.cloudburrow.localhost` resolves on the macOS system resolver and musl, **not** on bare glibc or **Go's pure resolver** (`CGO_ENABLED=0`), which forwards the query to the configured nameserver. The `Host`-header path needs no resolver and always works. Measured matrix in [networking.md](networking.md). |
| HTTPS to a service | **Not supported** | No certificate is issued. Publishing a port answering with a self-signed certificate would look like support for something that does not work. |

---

## Optional services

Opt-in, and **never started by default** — a developer should not pay memory for databases
they did not ask for. Enable with `--services firestore,datastore,bigtable,spanner`.

Each wraps a **Google-published emulator**; none is a reimplementation. Each has an official
emulator environment variable, so an application needs no code change to use it.

| Service | Backing component | Status | Evidence |
|---|---|---|---|
| Firestore | Cloud SDK `cloud-firestore-emulator` | **Verified** | `TestFirestoreDocumentCRUD`: document CRUD, a `>` query, and a transaction |
| Datastore | Cloud SDK `cloud-datastore-emulator` | **Verified** | `TestDatastoreEntityCRUD`: entity CRUD, a filtered query, and a transaction |
| Bigtable | Cloud SDK `bigtable` (`cbtemulator`) | **Verified** | `TestBigtableTableAndRows`: table and column family creation, row write, read, and a filtered scan |
| Spanner | `cloud-spanner-emulator` (own image, digest-pinned) | **Verified** | `TestSpannerSchemaAndQuery`: instance, database, DDL, a write, a read and a SQL query |

**All four are in-memory.** Google documents them that way, so CloudBurrow provisions no
volume for them and `status` lists them as never surviving a restart. Nothing here is
persistent, whatever `--mode` says.

**Bigtable needs network on first start.** Its emulator is the one Cloud SDK emulator absent
from the published emulators image, so the component is installed when the container starts.

Not covered, and not claimed: instance and cluster administration for Bigtable, Firestore
indexes and security rules, Datastore composite indexes, Spanner dialects other than
GoogleSQL, and backup/restore for any of them.

## Functions and source builds

Optional, and neither implies any management-API parity. See
[functions-and-builds.md](functions-and-builds.md).

| Capability | Status | Evidence |
|---|---|---|
| Functions Framework, `http` signature | **Verified** | Handler answers; a body-less request still returns 200 |
| Functions Framework, `cloudevent` signature | **Verified** | Google-schema CloudEvent reaches the handler with type, subject and data intact |
| CloudEvent error handling | **Verified** | A request without `ce-*` headers returns HTTP 400 |
| Google Buildpacks source build | **Verified** | Go source built with no Dockerfile; the image runs and answers |
| Build cache reuse | **Verified** | 15 layers reported reused on rebuild |
| Failed-build diagnostics | **Verified** | An invalid module path produced a precise, actionable error |
| **Cloud Functions management API** | **Not supported** | No `projects.locations.functions` surface exists. A working handler does not imply one. |
| **Eventarc trigger management** | **Not supported** | Nothing creates or routes triggers. |
| Languages other than Go | Planned | Google publishes other runtimes; untested is untested. |
| Native amd64 build host | Planned | Only the emulated arm64 path was tested. |

**The builder is `linux/amd64` only.** On arm64 it runs under emulation and emits an amd64
image, which needs an amd64-capable node. CloudBurrow reports this rather than letting it
surface as a scheduling failure.

## Local AI

**Text generation works**, on a runtime CloudBurrow builds from Google's source. See
[local-ai.md](local-ai.md), including §4 — which corrects this page's previous conclusion.

| Capability | Status | Notes |
|---|---|---|
| Model catalogue with verified provenance | **Verified** | Publisher recorded per artifact; a community conversion is never reported as Google-published. |
| Gated-artifact handling | **Verified** | Refused without credentials, with an actionable message. |
| Disk preflight, checksum verification, atomic cache, recovery | **Verified** | `internal/localai` |
| **Linux generation runtime** | **Verified** | `//runtime/engine:litert_lm_main` built from source at commit `02e5030` and run **from the shipped image**: **78.43 tok/s prefill, 31.98 tok/s decode, 0.39 s to first token** over a 144-token generation, CPU backend, warm cache. No LiteRT-LM release publishes a Linux artifact; Linux is nevertheless a documented, supported build target, and the gap is packaging rather than capability. |
| **Linux embedding runtime** | **Builds** | `//runtime/engine:embedding_litert_lm_main` builds (21.9 MB) and takes `--input_prompt --backend=cpu`. **Not exercised**, because no embedding model can be downloaded — see below. |
| **Text generation** | **Verified** | On `litert-community/gemma-4-E2B-it.litertlm` (2.59 GB, ungated). |
| **A Google-published runnable model** | **Not available** | Every one is `gated: manual`. The model that works is a **community** conversion and is labelled as one everywhere it appears. |
| **Embeddings** | **Blocked on the model, not the runtime** | Every embedding artifact is gated. `litert-community/embeddinggemma-300m` is `gated: auto` and returns `401 ... You must have access to it and be authenticated` without a token. |
| Output quality | **Not claimed** | The measured run answered a factual question wrongly. That is a small quantised model's quality; CloudBurrow does not present model output as correct. |
| Benchmarks | **Measured, single sample** | One run on one machine (Apple M4 Max, arm64, CPU, container). Not a benchmark, and no comparison is drawn from them. Two conditions change them substantially: a **cold** XNNPACK cache raises init from 0.31 s to 3.27 s, and a **short** answer collapses decode speed (6.89 tok/s over two tokens on the same binary) because the first token's fixed cost is averaged over the generation. |
| Pinned artifact checksums | **Absent** | The Google artifacts are gated, so they could not be downloaded and hashed. The code supports pinning; the status reports the absence rather than implying verification. |
| Runtime distribution | **Built locally** | No image is published. `deploy/litert-lm/Dockerfile` builds it: **6m17s measured** from a cold cache on an M4 Max with 16 CPUs, once, producing a 274 MB image. Debian 13 is the base because Abseil needs C++20 `<source_location>`, which Debian 12's default clang 14 lacks. |
| Runtime image completeness | **Verified** | The image is run, not merely built. `litert_lm_main` links `libGemmaModelConstraintProvider.so` — shipped prebuilt upstream, found via a `RUNPATH` into Bazel's output tree — so a multi-stage build that copies only the binary produces an image that builds cleanly and fails on first run. The prebuilt libraries are staged and registered with `ldconfig`. |
| Catalogue artifact filenames | **Verified against the live API** | `TestCatalogArtifactsExistUpstream` (tag `upstream`) lists each repository and asserts the named file is in it, and HEADs the resolve URL for entries marked ungated. It was written after the community entry was found naming `gemma-4-E2B-it-int4.litertlm`, a filename inferred from the `google/` repositories' convention, which returns **404**. |

## Vertex AI custom prediction

Google's **serving contract**, on CloudBurrow's **own runtime**. See
[prediction.md](prediction.md) for the decision and its reasoning.

| Capability | Status | Notes |
|---|---|---|
| `AIP_HTTP_PORT` / `AIP_HEALTH_ROUTE` / `AIP_PREDICT_ROUTE` honoured | **Verified** | `TestPredictionEndpointServesTheVertexContract` deploys with **non-default** routes and asserts the defaults return 404, so the environment is proven plumbed through rather than coincidentally matching. Names checked against `google.cloud.aiplatform.constants.prediction` 2.1.3. |
| Health route reports readiness | **Verified** | Same test. Any 2xx is healthy, as Vertex defines it; a redirect is not. |
| `{"instances":[...]}` → `{"predictions":[...]}` | **Verified** | Same test, decoded through `internal/prediction` types rather than string matching. |
| One prediction per instance enforced | **Verified** | `prediction.ValidateResponse`; a mismatched count is a 500, not a mis-attributed result. |
| `parameters` passed to the container | **Verified** | `TestPredictionDeadlineBoundsASlowPrediction` drives the fixture's delay through it. |
| Malformed input refused | **Verified** | Empty instances, wrong instance type, unknown field and invalid JSON all return 400. |
| Caller deadline bounds a slow prediction | **Verified** | A 10s prediction fails a 2s caller, and the endpoint remains usable afterwards. |
| Endpoint deletion is complete | **Verified** | `TestPredictionEndpointDeletionIsComplete`; `GetService` then returns `NotFound`. |
| Startup failure reported with the container's output | **Verified** | `TestPredictionStartupFailureIsReported`. The adapter prefers Knative's `ConfigurationsReady` message over the top-level `does not have any ready Revision`. |
| **Startup-failure latency** | **Inherited limitation** | **602s, measured.** Knative declares a revision failed only after its `progress-deadline` (default `600s`), and Cloud Run v2 exposes no field that shortens it. Until then the endpoint reports `PENDING`. |
| `AIP_STORAGE_URI` artifact download | **Not supported** | Recognised as a contract variable; nothing fetches from GCS. The offline path downloads no artifacts and loads no credentials. |
| `LocalModel.deploy_to_local_endpoint` | **Declined by design** | It runs `containers.run(detach=True)` outside the cluster — unowned by readiness, reset or delete. Execution goes through the owned runtime instead. |
| `LocalModel.build_cpr_model` | Available, not invoked | CloudBurrow accepts any image honouring the contract; it does not build one for you. |
| Vertex model registry, `Endpoint`, `DeployedModel`, `PredictionService` | **Not supported** | No Vertex management surface is served at all, so a client fails to connect rather than receiving a stub. |
| Model Garden, tuning, batch prediction | **Not supported** | Out of scope. |
| **GPU / accelerators** | **Not supported** | CPU only, on every platform. NVIDIA is untested and nothing configures it. On Apple Silicon a Linux container has no Metal device, so no configuration achieves it — **NVIDIA container flags do not establish Metal acceleration**. |

The host-advertised URI is the cluster ingress address and the Knative gateway is not
published on a host port, so a host-side caller reaches an endpoint through a port-forward
with the service in the `Host` header. In-cluster callers use the cluster-local name
directly. Both are documented in [prediction.md](prediction.md#reaching-the-endpoint).

## Credentials, metadata and external tooling

**CloudBurrow authenticates nothing.** These exist so that tools which insist on credentials
can run offline. See [credentials.md](credentials.md).

| Capability | Status | Notes |
|---|---|---|
| `cloudburrow env` shell/JSON/plain export | **Verified** | `TestEnvExportsEveryClientVariable`. Exports the client-library variables *and* the `gcloud` endpoint overrides, because `gcloud` ignores the former. |
| ADC fixture usable by Google's auth library | **Verified** | `TestOfficialAuthLibraryUsesTheLocalFixture`: `google.FindDefaultCredentials` mints a token through the fixture. The test fails if a `ya29.` token appears, which would mean it reached Google with the developer's real credentials. |
| Token exchange stays local | **Verified** | Same test. The fixture's `token_uri`, `auth_uri` and cert URLs all point at the instance. |
| Metadata server contract (`/`, project, zone, email, token, identity, scopes, aliases) | **Verified** | `TestMetadataServerAnswersTheContract` and `internal/metadata`. |
| `Metadata-Flavor: Google` required and echoed | **Verified** | Enforced even though the tokens grant nothing: a local server accepting what a real one rejects teaches code to fail in production. |
| ID token is a valid RS256 JWT | **Verified** | Signed with the instance key and verifiable against `/certs`. |
| **ID token verifies against Google's certificates** | **Not supported** | Google did not issue it. Tokens are structurally valid and self-consistent; signature verification against Google's public certs is **not** claimed. |
| **Token validation of any kind** | **Not supported** | No signature, expiry, audience, scope or assertion is ever checked. The token endpoint checks only that an assertion was *sent*, so a client silently sending none finds out. |
| **IAM, scopes, per-resource permissions** | **Not supported** | Every caller can do everything. |
| `gcloud storage ls` | **Verified** | With `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE` and a local token. |
| `gcloud pubsub topics create` / `list` | **Verified** | With `CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB`. |
| **`gcloud` honouring `*_EMULATOR_HOST`** | **Not supported (upstream)** | `gcloud` ignores them and goes to the real service, which **leaves the machine**. Inherited limitation, recorded rather than worked around; `cloudburrow env` exports the overrides too. |
| Terraform `google` provider create / read / destroy | **Verified** | `google_storage_bucket` and `google_pubsub_topic`, full lifecycle, with `storage_custom_endpoint` / `pubsub_custom_endpoint` and `access_token`. `terraform init` still downloads the provider once. |
| Terraform resources beyond Storage and Pub/Sub | Planned | Untested is untested. |
| Metadata server reachable from inside the cluster | **Not supported** | It binds loopback. A pod's loopback is the pod. |

## Secret Manager — `google.cloud.secretmanager.v1`

An **owned implementation**: the upstream audit (#24) found no official Secret Manager
emulator. Measured against the published contract, never against another emulator.

> **It is not a secret store.** CloudBurrow authenticates nothing, so anything written here
> is readable by any caller that can reach the endpoint. It exists so an application whose
> code fetches configuration from Secret Manager can run locally. See
> [credentials.md](credentials.md).

Served on one port over **both gRPC and JSON**, as Google's own endpoint is.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `CreateSecret` | **Verified** | `TestSecretLifecycle`. Duplicate is `ALREADY_EXISTS`. |
| `GetSecret` | **Verified** | Missing is `NOT_FOUND`. |
| `ListSecrets` | **Verified** | Paged, ordered by name, scoped per project. |
| `UpdateSecret` | **Verified** | `labels` and `annotations` only. An **empty update mask is refused** rather than treated as "replace everything", which would silently clear every label. A mask naming an immutable field is refused rather than ignored. |
| `DeleteSecret` | **Verified** | Removes every version with it: an orphaned version could otherwise be listed by a later secret with the same ID. |
| Replication config | Partial | `automatic` and `user-managed` are **recorded and returned**, not enforced — there is one local store either way. Recorded rather than rewritten so a caller reads back what they set. |
| **`Secret.expire_time` / `ttl`** | **Not supported** | Nothing expires a secret. |
| **`Secret.rotation`** | **Not supported** | No rotation is scheduled. |
| **`Secret.topics`** | **Not supported** | No Pub/Sub event is published on a version change. |
| **Regional secrets** (`projects/*/locations/*/secrets/*`) | **Not supported** | Only the global name shape is served. |
| **IAM policy** (`GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions`) | **Not supported** | Returns `UNIMPLEMENTED`; there is no IAM. |

### Version plane

| Operation | Status | Notes |
|---|---|---|
| `AddSecretVersion` | **Verified** | Version numbers increment and are **never reused**, including after a destroy — reuse would let a stale reference resolve to different bytes. |
| `AccessSecretVersion` | **Verified** | Returns the **concrete** version name even when asked for `latest`, so a client can record which bytes it got. |
| `GetSecretVersion` | **Verified** | Metadata stays readable for a disabled or destroyed version. |
| `ListSecretVersions` | **Verified** | Newest first, across page boundaries, with a full-walk test proving no duplicates or gaps. |
| `EnableSecretVersion` / `DisableSecretVersion` | **Verified** | A disabled version is `FAILED_PRECONDITION` on access, **not** `NOT_FOUND`: it exists, and saying otherwise sends a caller looking for a creation bug instead of an enable call. |
| `DestroySecretVersion` | **Verified** | Terminal. The payload is **cleared, not flagged**, so a destroyed version cannot leak what it held; re-enabling is refused. |
| `latest` alias | **Verified** | The most recently **created** version, per `GetSecretVersionRequest.Name`. Deliberately not "the most recently enabled": a disabled latest fails rather than silently returning older bytes. |
| Payload limit (64 KiB) and empty payload | **Verified** | Both `INVALID_ARGUMENT`. |
| **Client-side encryption / CMEK** | **Not supported** | Payloads are stored as given. |

### Transport

| Concern | Status | Notes |
|---|---|---|
| gRPC and JSON on one port | **Verified** | One `Store` behind both, so there is one implementation of the contract rather than two that drift. gRPC is served through `grpc.Server.ServeHTTP` behind `h2c`, which upstream documents as lower-performance than a dedicated listener — worth it to match the real service's shape locally. |
| Google error envelope over REST | **Verified** | `{"error":{"code":404,...,"reason":"notFound"}}`. |
| Custom methods (`:addVersion`, `:access`, `:enable`, `:disable`, `:destroy`) | **Verified** | `net/http` cannot express a wildcard followed by a literal in one path segment, so the verb is captured with the ID and split by the handler. An unrecognised verb is `UNIMPLEMENTED`, not 404. |
| Kubernetes-backed storage | **Verified** | One GCP secret is one Kubernetes Secret: payloads in `data["v1"]`, metadata in annotations, labelled `cloudburrow.dev/owned=true`. Applied **server-side**, because a client-side apply copies every payload into `last-applied-configuration` — an annotation `kubectl describe` prints. |
| `secretKeyRef` in a Cloud Run revision | **Verified** | `TestCloudRunRevisionReadsASecretManagerSecret`: the container reports the payload, and the test also asserts the payload is **not** in the deployed Knative Service — the revision reads it from the cluster rather than having it templated in. |
| `latest` in a `secretKeyRef` | **Verified, with a pinned result** | Kubernetes has no `latest` key, so the alias is resolved to a concrete version **at deployment**. A running revision does not pick up a version added later. Cloud Run behaves the same way for environment variables. |
| Reference to a disabled or destroyed version | **Verified refused** | `FAILED_PRECONDITION` at deploy time. Accepting it would produce a pod that fails to start with Kubernetes complaining about a missing key — a message pointing nowhere near the cause. |
| **Secret volume mounts** | **Not supported** | Only `secretKeyRef` environment variables are mapped. `template.volumes` is still refused. |
| Namespace | — | Secrets live in the **workload** namespace, not the managed one, because `secretKeyRef` cannot cross namespaces. `cloudburrow reset` removes them **by ownership label**, so it can never touch an object CloudBurrow did not create. |
| **Project ID validation differs from Cloud Run** | Known inconsistency | Secret Manager accepts any valid resource ID; the Cloud Run adapter enforces GCP's 6-30 character project rule. The same project string can therefore be valid for one and not the other. Recorded rather than silently changed. |

## Cloud Storage — notifications to Pub/Sub

**Detection is upstream, routing is ours.** fake-gcs-server 1.56.1 already publishes object
mutations to Pub/Sub with the official message shape, so nothing here reimplements event
detection — see [upstream-evaluation.md](upstream-evaluation.md). What it lacks is
per-configuration routing: it takes one topic for the whole server.

| Capability | Status | Notes |
|---|---|---|
| `notificationConfigs` insert / get / list / delete | **Verified** | `TestNotificationConfigsCRUDThroughTheSDK`, driven by the official Storage client. Delete returns 204 with no body, as the service does. |
| Upload delivers `OBJECT_FINALIZE` | **Verified** | `TestObjectUploadDeliversANotification`: uploaded with the Storage SDK, received with the Pub/Sub SDK. |
| Standard attributes | **Verified** | `eventType`, `bucketId`, `objectId`, `objectGeneration`, `payloadFormat`, `eventTime`, plus `notificationConfig` naming the configuration that delivered it. |
| Payload is the Storage Object resource | **Verified** | `kind: storage#object` with `name`, `bucket`, `generation`, `size`. |
| Event-type filter | **Verified** | An empty list means **all** types, as the API defines it. Treating it as "none" would make a configuration created with defaults silently deliver nothing. |
| `object_name_prefix` filter | **Verified** | `TestNotificationFiltersAreHonoured`. |
| `custom_attributes` | **Verified** | Applied, and **cannot overwrite a standard attribute** — a configuration that set `eventType` would make the message lie about what happened. |
| `payload_format: NONE` | **Verified** | Attributes only, no body. |
| Fan-out to several configurations | **Verified** | One mutation reaches every matching configuration, each on its own topic. |
| Service-qualified topic form | **Verified** | The official client sends `//pubsub.googleapis.com/projects/{p}/topics/{t}` and parses it back. Both spellings are accepted and one canonical form stored, so the same delivery cannot be registered twice by changing only the spelling. |
| Duplicate configuration | **Verified refused** | `ALREADY_EXISTS`. Two identical registrations would deliver every event twice, which looks like the backend duplicating rather than the caller registering twice. |
| **`OBJECT_ARCHIVE`** | **Not supported** | Accepted in a configuration and never fires: the backend has no object versioning, so nothing can be archived. |
| **`OBJECT_DELETE` / `OBJECT_METADATA_UPDATE`** | Partial | The backend is configured to emit them and the router delivers them, but only `OBJECT_FINALIZE` is covered by a test. Untested is untested. |
| **Ordering and at-least-once semantics** | **Not claimed** | Delivery is a local republish with no retry: a message that cannot be routed is acked, counted and reported, never redelivered. Redelivering a permanent failure would loop forever. |
| **In-cluster writes** | **Verified by construction** | Both storage deployments publish. The hook is on the API call, not the filesystem, so each reports only what it served — enabling one would silently drop every event from the audience it does not serve. |
| Notification state across a restart | **Not durable in ephemeral mode** | Configurations follow the instance's mode. The internal event topic lives in the Pub/Sub emulator, which the audit measured losing state across a restart regardless. |

The `notificationConfigs` API is served by a handler **in front of** the storage tunnel, on
the same host port as the rest of the Storage API — an official client sends everything to
one endpoint, so a second port would be a shape no Google endpoint has. Requests that are not
notification calls are forwarded to the backend unchanged, with the original `Host` preserved
because the backend matches its download path against it.

## Console

The requirement is a console that looks and behaves like the Google Cloud console. The
checklist it is judged against is [console-parity.md](console-parity.md).

| Claim | Status | Notes |
|---|---|---|
| Parity specification | **Complete** | Screen-by-screen checklist, routes, accessibility and viewport rules, scoped to supported operations, with dated provenance for every structural claim. |
| **Visual parity with the GCP console** | **Partial — unverifiable today** | **No authorized read-only console session was available, so no reference screenshots exist.** Structural parity (pages, navigation paths, control labels) is specified from dated public documentation. Pixel metrics — spacing, type scale, palette, row heights — are **not specified and not claimed**, because inventing them is what #43 forbids. |
| Console served by `cloudburrow up` | **Verified** | `--port-console`, default `9090`, reported in the startup block. Assets are embedded in the binary; a test asserts no reference to any CDN or web-font host, so the console works with no network. |
| Shell: toolbar, navigation, project selector, search, notifications, settings | **Verified** | Rendered and asserted in headless Chrome. |
| Light / Dark / Same as device, **without a page reload** | **Verified** | The documented console behaviour. |
| Dashboard shows live instance state | **Verified** | Instance, readiness, cluster, Kubernetes version, namespace, mode, endpoints and per-service availability, each read at request time. Nothing cached. |
| Resource lists read live state | **Verified** | Buckets via the Storage JSON API, topics via the official Pub/Sub client, queues and secrets from the **same in-process stores the services serve**, Cloud Run and workloads from the cluster. No second store exists. |
| Loading / empty / error states | **Verified** | Three distinct states. A provider failure renders as an **error with its cause**, never an empty table — an empty table says "you have none" and sends a developer to debug their own code. |
| Deep links and project scoping | **Verified** | Every route renders when opened directly; `?project=` reaches the provider. |
| Same-origin protection | **Verified** | Cross-site and cross-origin requests to `/api` are refused. Loopback is reachable from any page the browser has open, so binding loopback is not by itself protection. |
| No cluster credentials or Docker socket exposed | **Verified by construction** | The browser receives JSON only; the kubeconfig never leaves the process. |
| Create buckets, topics and queues from the console | **Verified** | `TestConsoleCreatedBucketIsVisibleToTheOfficialSDK`, `TestConsoleCreatedTopicIsVisibleToTheOfficialSDK`. Every mutation goes through the **same API an SDK client calls**, so a console-created resource is indistinguishable from an SDK-created one — and the bucket test writes an object to it to prove it is a real bucket, not a record of one. |
| Delete from the console | **Verified** | Buckets, topics, queues and secrets. A delete with no name is refused rather than guessing. |
| Queue actions: pause, resume, purge | **Verified** | `TestConsoleQueueActionsFollowState`: the offered actions follow the queue's state, so a paused queue is not offered "Pause". An action that would do nothing is indistinguishable from one that is broken. |
| Destructive actions name their target | **Verified** | `purge` and every delete are marked destructive and confirmed by name. "Are you sure" with no subject is how the wrong resource gets deleted. |
| Form validation matches the API | **Verified** | The form is described by the **backend**, including the pattern the API enforces, so a field only appears when the service can accept it and an obvious mistake needs no round trip. |
| Failures show the service's own message | **Verified** | `TestConsoleReportsTheServiceMessageOnFailure`. The gRPC transport envelope is stripped while the status code is kept: the code says whose mistake it is, the envelope says nothing. |
| Unscoped create refused | **Verified** | A resource in the wrong project is worse than one that was not created. |
| Operations are tracked | **Verified** | Every mutation appears in the notifications panel while in flight and carries its terminal state. Nothing reports success before the API says so. |
| **Object upload / download / listing** | **Not supported** | Buckets only. The parity spec's rule applies: there is no object browser rather than one that cannot upload. |
| **Subscriptions, publishing and pulling messages** | **Not supported** | Topics only. |
| **Creating tasks, and attempt history** | **Not supported** | Queues only. |
| Deploy a Cloud Run service from the console | **Verified** | Through the **Cloud Run v2 adapter**, not by applying a Knative manifest — applying directly would let the console accept a configuration the API refuses, which is the console inventing support. The deploy **waits for readiness**: a service reported created and never ready is the failure the console exists to make visible. Verified end to end, including invoking the deployed service through the ingress. |
| Delete a Cloud Run service from the console | **Verified** | Through the same adapter, waiting for the operation. |
| Kubernetes pods, services, jobs and events | **Verified** | Scoped, **read-only**, across namespaces, each row showing whether CloudBurrow owns it. Events are what turn "the pod is Pending" into a reason. |
| Ownership shown per object | **Verified** | A developer can tell what CloudBurrow created from what they created. |
| **GKE cluster metadata** | **Not supported** | This is a kind cluster. No node pools, autopilot or release channels are presented, because presenting them would be fabricating exactly what the issue forbids. |
| **Creating Kubernetes workloads from manifests** | **Not supported** | Read-only by design: a console that could apply arbitrary manifests would create workloads CloudBurrow does not track and cannot clean up. |
| Logs Explorer, live | **Verified** | Real pod logs followed from the cluster, streamed over **Server-Sent Events** (#88). Verified in a real browser: 88 live rows, `live` state, pause control, severity filter. |
| Pause / resume with buffering | **Verified** | Resume shows what happened while paused rather than skipping it — the lines you paused to read are usually next to the ones you need. |
| Reconnect and resume | **Verified** | Every event carries an id; a reconnecting client sends `Last-Event-ID` and the **gap** is replayed rather than everything, which would make a reconnect look like a flood of new activity. |
| Filters: severity, source, resource, operation, text | **Verified** | Applied to both the backlog and the live stream. |
| **Credential redaction** | **Verified** | On the way **in**, not on the way out: an entry stored with a token in it has already been written somewhere a later change might expose. Covers `ya29.` and `cbl_` tokens, `Authorization`, api-key/password/secret/token assignments, PEM private keys and JWTs. Operation failure causes are redacted too. |
| Message truncation | **Verified** | 2 KiB, marked. An application logging a whole request body would otherwise push out everything that explains it. |
| Bounded retention | **Verified** | 2000 entries in memory. Nothing is persisted: keeping logs would make CloudBurrow responsible for data it never promised to keep. |
| **System namespaces are not followed** | **By design** | Measured: the control plane, kourier and Knative's own components were **90% of the buffer**, pushing out the lines that explain a developer's failure. Only `default` and the managed namespace are followed; the Events screen and `kubectl logs` still reach the rest. |
| Cloud Tasks attempt history | **Verified** | Each dispatch attempt is logged with its outcome. A queue that says "1 task" tells a developer nothing; the attempt says why it is still there. |
| Activity: operations with states and causes | **Verified** | PENDING / SUCCEEDED / FAILED from the **backend's verdict**, never from the console's optimism. A failed operation links to its own logs. |
| **Severity of container logs is inferred** | Partial | Container logs carry no structured severity, so it is a heuristic over the words applications conventionally use. Used for filtering, never to claim an application said something it did not. |
| **Billing, quota, SLA or cost metrics** | **Not supported** | Nothing of the kind is displayed. There is no billing here to report. |
| **Request-rate, latency and resource metrics** | **Not supported** | Not measured, so not shown. A chart nothing produced is the worst thing a console can display. |
| **Detail screens, traffic splitting** | **Not supported** | Lists only. |
| **Pagination** | **Not supported** | Lists are filterable and complete, not paged. Recorded rather than faked with controls that do nothing. |
| **Bucket listing is not scoped by project** | **Inherited limitation** | The storage backend accepts the project parameter and returns every bucket. The screen **says so** rather than presenting the rows under a project heading. |
| Console walkthrough on a fresh stack | **Verified** | [console-verification.md](console-verification.md): bucket and topic created from the browser and cross-checked outside it, all four states, keyboard and focus behaviour, project isolation, offline assets. |
| Form validation actually runs in a browser | **Verified** | It did **not** before this walkthrough. An HTML `pattern` is compiled with the RegExp `v` flag, where a literal `-` in a character class must be escaped; an unescaped one makes the pattern invalid and the browser then **silently skips validation**. Every create form had shipped that way. A regression test now compiles each shipped pattern under the `v` rule. |
| Subscription creation from the UI | **Not supported** | The console offers topics only. The acceptance criterion asks for it; it is recorded as absent rather than walked. |
| Visual regression against reference fixtures | **Not possible today** | No reference screenshots exist, so there is nothing to diff against — a threshold measured against our own output would only prove the console still looks like itself. This is what keeps visual fidelity at `Partial`. |
| Model catalogue screen | **Verified** | Shows the real catalogue with publisher, access, modality, licence, runtime and a **per-model** reason it is unavailable — the reasons differ, and one blanket message would hide that. |
| **Generation, embedding and prediction playground** | **Not supported** | #39 found no viable runtime. There is **no playground at all**, not a playground with a disabled Run button: a disabled button suggests the feature is one configuration change away, and it is not. |
| Gemma results labelled as Gemini | **Cannot occur** | Nothing produces results, and no screen uses the word. |

**Visual fidelity and API compatibility are separate claims with separate evidence**, and
neither implies the other. A screen that renders correctly proves nothing about the API
behind it, and a verified API proves nothing about the screen.

## Cross-cutting

| Concern | Status | Notes |
|---|---|---|
| Google-style error bodies and gRPC status codes | **Verified** | `internal/apierror`: one cause renders a consistent gRPC code and HTTP status, plus the JSON envelope a Google client parses. Not yet wired into a served surface. |
| Pagination with deterministic ordering | **Verified** | `internal/paging`: full-walk coverage proves no duplicates or gaps; invalid and cross-listing tokens are rejected. Not yet wired into a served surface. |
| Long-running operations | **Verified** | `internal/lro`: pending, succeeded and failed are all reachable and terminal states are final. Not yet wired into a served surface. |
| Project isolation | **Verified** | `TestPubSubProjectIsolation`; `internal/resource` makes it structural — identical IDs in different projects or locations produce different keys by construction. |
| REST and gRPC transport | **Verified** | `internal/transport`: unknown REST paths return a Google 404 envelope and unknown gRPC methods return `Unimplemented`; request sizes are bounded and unknown JSON fields rejected. No service is registered on it yet. |
| Background workers registered after startup | **Verified** | A worker registered after `Start` used to sit in the list and never run, because `Start` snapshotted the list once. **Cloud Tasks dispatch was wired into `up` and never dispatched anything.** Found while building the Logs Explorer: a deliberately failing task showed `dispatches=0` after minutes. Now started immediately, sharing the coordinator's lifetime. |
| Scheduling, retry and backoff over an injected clock | **Verified** | `internal/sched`: retry timing is proven by advancing virtual time, never by sleeping. Concurrent duplicate attempts are prevented, and shutdown drains in-flight work. |
| Metadata storage, memory and durable modes | **Verified** | `internal/store`: both modes share one test body; durable state survives restart, a second instance is refused, a failed commit leaves state unchanged, and unsafe keys are rejected. |
| Resource-name parsing and traversal safety | **Verified** | `internal/resource` rejects `..`, encoded separators, NUL and path separators in IDs. |
| Installation and release packaging | Partial | `docs/install.md`, `docs/status.md` and a release workflow that builds and checksums four platforms. **No release has been cut**, so the publish path is untested. |
| Reset / seed / event inspection | Partial | `internal/admin`, served on the loopback-only control port and refused on service ports. Reset and seed cover Cloud Tasks; the upstream-backed services are not yet wired in, and event recording has no producers yet. |
| Go SDK compatibility harness | **Verified** | `test/compat`. Refuses non-loopback endpoints and fails outright if cloud credentials are present in the environment. |
| Local cluster lifecycle (up/status/stop/reset/delete) | **Verified** | `internal/cluster` integration tests. |
| Workstation preflight (`cloudburrow doctor`) | **Verified** | `internal/doctor`: binaries, daemon reachability, memory, CPUs, disk and every port `up` would bind, each with a remedy. Only genuine blockers exit non-zero; an unmeasurable check reports `unknown`, never `ok`. On macOS and Windows the disk figure is the host volume backing the VM disk, and says so. |
| Cluster ownership isolation | **Verified** | Prefix enforced at construction and re-checked on delete; namespace reset requires `cloudburrow.dev/owned=true`. |
| Python SDK compatibility harness | Planned | Go only so far. |
| Java / Node SDK support | Planned | Endpoint-override mechanism not yet verified against client source. No support claimed. |
