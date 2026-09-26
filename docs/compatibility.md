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
| `UNVERIFIED` (an error code) | The call is Verified, but an error code the test asserts is one **Google does not document**. It is implemented as the most plausible code and marked `// unverified:` in the test, so [docs/coverage](coverage/README.md) lists it and a row that relies on it says `(error code UNVERIFIED)`. It stays so until a recorded observation pins it; see [test/compat/README.md](../test/compat/README.md#unverified-error-codes). |

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

### What CI does not run, and why

Every merge runs `test/compat` against a live instance. A test that skips there is not evidence,
so each one that still skips is listed here with its reason (#346).

| Tests | Why they skip in the main compat run |
|---|---|
| `TestDatastoreAcrossRestart`, `TestMemorystoreAcrossRestart`, `TestCloudSQLMySQLAcrossRestart`, `TestKMSAcrossRestart` | They read what an earlier process wrote. They run, and must pass twice each, in CI's restart probe after `stop`/`up` (persistent mode) and `up --mode ephemeral`. |
| `TestGenerateContentThroughTheOfficialSDK`, `TestStreamGenerateContentThroughTheOfficialSDK`, `TestGenerationNeverUsesApplicationDefaultCredentials` | Local AI needs a 2.59 GB model download and minutes of CPU inference. The decision is that it does not run in per-PR CI. The Local AI and Vertex AI generation rows come from runs on a developer machine, dated in [generation.md](generation.md) (2026-09-21) and [local-ai.md](local-ai.md). They are the only Verified rows whose evidence is not re-run on every merge. |

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
The **builtin Cloud Storage server** (#485, `--storage-backend builtin`, not yet usable with `up`) is built to Google's spec in steps. **Buckets** (#490) are served: insert, get, list (prefix, pages of at most 1000), patch as a JSON merge patch (an omitted field stays, `null` clears it, a `null` label removes that key), update, delete, and metageneration with `ifMetagenerationMatch`/`NotMatch` (412 `conditionNotMet`; 304 for a read). `location`, `storageClass`, `labels`, `versioning`, `defaultEventBasedHold` and `softDeletePolicy` are kept; any other settable field with a value is **refused by name with 400** until its issue lands, never dropped with 200. A non-empty bucket delete is 409 (UNVERIFIED for the JSON API). **Objects** (#491): media and multipart uploads (`uploadType=media`, `multipart`; a metadata-only POST is 400 `wrongUrlForUpload`), with `Content-MD5`, `x-goog-hash` and the metadata's `md5Hash`/`crc32c` verified and a mismatch refused; `ifGenerationMatch` (including 0, "only if absent") and the other three preconditions; metadata reads, `alt=media`, `/download/storage/v1/…`, and XML `GET`/`HEAD /{bucket}/{object}` with the `x-goog-*` headers; one byte range (`a-b`, `a-`, `-N`; 206, or 416 with `Content-Range: bytes */size`); and a new generation, metageneration 1, on every finalize. `TestStorageObjectRoundTrip`, `TestStorageChecksums` and `TestMultipartUploadStoresMetadataWithContent` pass against it. **Object metadata** (#492): patch (merge; a `null` metadata key removes it) and update (replace), each advancing the metageneration and keeping the generation and bytes (fake-gcs-server reported `"1"` forever); `customTime` only moves forward and cannot be removed, as documented; `storageClass`, holds and retention are refused by name here. Preconditions combine with AND on the live generation unless one is named: `ifGeneration(Not)Match`, `ifMetageneration(Not)Match` (412, or 304 for a failed NotMatch on a read), `If-Match` (412) and `If-None-Match` (304 on reads), and for the XML API `x-goog-if-generation-match`, `x-goog-if-metageneration-match`, `If-Modified-Since` (304) and `If-Unmodified-Since` (412). `TestStorageGenerationPreconditions` and `TestObjectMetadataUpdate` pass against it. **Listing** (#494): `prefix`, `delimiter` with synthetic `prefixes` (which count toward `maxResults`), `includeTrailingDelimiter`, `startOffset`/`endOffset`, `matchGlob` (`**`, `*`, `?`, `[…]`, `[!…]`, `{a,b}`), and pages of at most 1000 in byte order; `versions=true` and `softDeleted=true` are 501 until #498 and #499. `TestStorageListWithPrefix` passes against it. **The official Go client retries every 5xx, 501 included, until its deadline**, so a method not built yet costs a client its timeout rather than failing at once. The existing `TestStorageBucketLifecycle` and `TestStorageBucketsList`, plus `TestStorageBucketPatchKeepsOmittedFields` and `TestStorageBucketMetagenerationPreconditions`, run against it in CI through the official Go client (`check` job).

| `buckets.get` | **Verified** | `TestStorageBucketLifecycle` |
| `buckets.list` | **Verified** | `TestStorageBucketsList` |
| `buckets.delete` | **Verified** | `TestStorageBucketLifecycle`. `TestStorageNonEmptyBucketDelete` confirms a non-empty bucket is refused with HTTP 412 `conditionNotMet`. No Google page states the JSON API's status for this (UNVERIFIED); the XML API documents 409 `BucketNotEmpty`, which the builtin server will mirror (#485). |
| `buckets.patch` / `buckets.update` | **Partial** | `TestBucketUpdateChangesMetadata`, through `Bucket.Update`: `defaultEventBasedHold` is kept and re-read rather than trusted from the response. `versioning` is kept only in ephemeral mode. The filesystem backend of persistent mode refuses it with HTTP 500 "fs storage type does not support versioning yet". fake-gcs-server **accepts and silently discards** labels, storage class, CORS, website, retention policy and lifecycle rules, on create as well as on patch (#321, #374). A patch also **resets** each field it keeps when the patch omits it, where Cloud Storage would leave it unchanged. The test pins both limitations. |
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
| `GetIamPolicy`, `SetIamPolicy` on a queue | ***Stored, not enforced*** | `TestQueueIamPolicyIsStoredNotEnforced` (#366, [ADR-0006](adr/0006-iam-policy-surface.md)): bindings set through the official client read back with a new etag, and a stale etag is **ABORTED**. **No RPC consults a stored policy**: a binding grants and denies nothing, and dispatch is unaffected. Conditions and audit configs are **UNIMPLEMENTED** with the field named. A version 3 policy without conditions is accepted. The policy is kept on the queue, so it goes with `DeleteQueue`, is cleared by `/admin/reset` and is captured by `state save`. |
| `TestIamPermissions` on a queue | ***Stored, not enforced*** | Same test: returns **every** requested permission, the literal truth when nothing is enforced. |
| JSON API (`/v2/…/queues`) | **Partial** | On the same port as gRPC, for REST clients such as Terraform (#366). It serves create, get, list, delete, pause, resume and purge for queues, and their IAM methods, as a transcoding of the gRPC service. `PATCH` (UpdateQueue) and every task method over JSON are **UNIMPLEMENTED**; use gRPC for tasks. Unknown JSON fields are refused. Unit-tested (`TestRESTQueueAndIamPolicy`) and driven by Terraform (below). |

### Data plane

| Operation | Status | Notes |
|---|---|---|
| HTTP target dispatch | **Verified** | `TestTasksHTTPDispatchCarriesCloudTasksHeaders`: a task created with the official client reaches a host HTTP server with its method, body and headers, and with `X-CloudTasks-QueueName`, `-TaskName`, `-TaskRetryCount` and `-TaskExecutionCount`. The names are **short IDs**, as the service sends them; full resource names were sent until #276. Non-2xx is retried, matching the real service including 4xx. **`X-CloudTasks-TaskETA`, `-TaskPreviousResponse` and `-TaskRetryReason` are not set.** |
| App Engine target dispatch | **Verified unsupported** | `TestTasksUnsupportedOperationsAreHonest`: returns `Unimplemented` rather than accepting a task that would never be dispatched. |
| Scheduled execution at `scheduleTime` | **Verified** | `TestTasksScheduleTimeIsHonoured`: a task scheduled 3s ahead is not delivered early, and is delivered within 3s of its time. The dispatcher polls every 200ms, so delivery can trail the time by that much. |
| Retry with backoff | **Verified** | `TestTasksAFailingTargetIsRetriedPerRetryConfig`: a target answering 500 is retried after `minBackoff`, then after the doubled delay capped at `maxBackoff`, and dispatch stops at `maxAttempts`. **`maxDoublings` is modelled** as the service documents it: double that many times, then grow linearly (`TestBackoffGrowsLinearlyAfterMaxDoublings`, `TestTheWorkerReschedulesOnTheLinearPhase`). Each `RetryConfig` field defaults on its own, as in the API, and `maxAttempts: -1` is unlimited. Driven by the queue's own `RetryConfig`, distinct from Pub/Sub redelivery. Exhausted tasks are dropped, as the real service does. |
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
> encryption keys, binary authorization, `template.executionEnvironment`,
> `template.sessionAffinity` and traffic splitting all return `Unimplemented` with the field
> named. A caller who set one of these and had it silently dropped would believe it took
> effect.
>
> Secret-backed environment is **not** in that list and was listed there in error:
> `secretKeyRef` is mapped and tested — see the Secret Manager section's
> `TestCloudRunRevisionReadsASecretManagerSecret`, which also asserts the payload does not
> appear in the deployed Knative Service.

### Control plane

| Operation | Status | Notes |
|---|---|---|
| `CreateService` | **Verified** | `TestRunServiceLifecycle`: deploys a real container through the official SDK and returns a long-running operation. |
| `GetService` | **Verified** | `TestRunServiceLifecycle`, including `NotFound` for an absent service. |
| `ListServices` | **Verified** | `TestRunServiceLifecycle`. Only CloudBurrow-owned Services are listed. |
| `DeleteService` | **Verified** | Refuses to delete a Knative Service CloudBurrow did not create. |
| `UpdateService` | **Verified** | `TestRunUpdateService` (#300): changing the image and an env var applies the Knative template, which cuts a new revision. The operation completes only when Knative has observed the new generation and **that** revision is ready, not while the old revision still reports Ready. The URL then serves the new version, and `ListRevisions` returns both revisions, newest first. A failed new revision fails the operation with the revision's own reason. The whole service is replaced, as `gcloud run deploy` and Terraform send it; a partial `update_mask` is UNIMPLEMENTED rather than honoured partly. `allow_missing` creates and `validate_only` applies nothing. |
| Traffic | **Partial** | Only 100% to the latest revision is accepted. Any split, a pinned revision, a percentage below 100 or a tag is UNIMPLEMENTED naming `traffic` (tested on create and update). |
| `GetRevision`, `ListRevisions`, `DeleteRevision` | **Verified** | `TestRunRevisions` (#299), official `run.RevisionsClient` against Knative in CI. Knative Revisions are mapped onto `run.v2.Revision`: the name under the caller's service, the service, the generation (Knative's `configurationGeneration`: 1 for the first deploy), create time, uid, containers (image, env, ports), concurrency and every condition. A new service has exactly one revision; an unknown one is NOT_FOUND. **Deleting a revision that serves traffic is refused** with FAILED_PRECONDITION, as Cloud Run refuses it; a retired revision is deleted. Revision fields Knative has no counterpart for (scaling, VPC access, encryption, execution environment) are absent rather than invented. |
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
| Requests per instance (`maxInstanceRequestConcurrency`) | Partial | Mapped to Knative's `containerConcurrency` and readable on the console's Configuration tab. **Not tested under load** — proving a concurrency limit needs sustained traffic. |
| Request timeout (`template.timeout`) | Partial | Mapped to Knative's `timeoutSeconds` and asserted by `TestTimeoutReachesKnative`. It was previously **dropped in silence**, so a service deployed with a ten-minute timeout got Knative's default and failed at five with nothing to explain it. The mapping is asserted; the resulting behaviour under a long request is not. |
| Resource limits (`container.resources.limits`) | Partial | CPU and memory limits reach the manifest. Whether the kubelet enforces them identically to Cloud Run is not claimed. |
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
| Datastore | Cloud SDK `cloud-datastore-emulator`, as Firestore in Datastore mode | **Verified** | `TestDatastoreEntityCRUD`: entity CRUD, a filtered query, and a transaction. `TestDatastoreQueriesSeeAFreshWrite`: 50 queries in a row, each made right after its write, all see it. The emulator runs in Firestore-in-Datastore mode, which is strongly consistent like a Datastore database in Google Cloud today. It does not reproduce legacy Datastore's eventually consistent non-ancestor queries. Left at its default (`--consistency=0.9`), about one query in ten missed a fresh write (#371). |
| Bigtable | Cloud SDK `bigtable` (`cbtemulator`) | **Verified** | `TestBigtableTableAndRows`: table and column family creation, row write, read, and a filtered scan |
| Spanner | `cloud-spanner-emulator` 1.5.58 (own image, digest-pinned) | **Verified, host and pod** | `TestSpannerSchemaAndQuery`: instance, database, DDL, write, read, SQL query. `TestSpannerReadWriteTransactionIsAtomic`: a read-modify-write transfer, and an aborted transaction leaving nothing behind. `TestSpannerDatabasesAreIsolated`. `TestSpannerFromInsideAPod`: the same client from a Job, over cluster DNS. |

**All four are in-memory.** CloudBurrow provisions no volume for them and `status` lists them
as never surviving a restart. Nothing here is persistent, whatever `--mode` says.

For Spanner this is now **measured rather than quoted**: `TestSpannerStateDoesNotSurviveARestart`
writes a row, restarts the pod, waits for the endpoint to come back, and reads —

```
after the restart the data is gone: spanner: code = "NotFound",
desc = "Instance not found: projects/.../instances/cb-durab"
```

The test fails rather than passes if the endpoint does not return, because an unreachable
address is not evidence about durability.

**Datastore keeps its data in persistent mode, measured (#307).** In persistent mode the
emulator's on-disk store is on a PersistentVolumeClaim, and the emulator's JVM is the container's
main process. The Cloud SDK emulator writes its store only when it shuts down cleanly, and both
`gcloud beta emulators datastore start` and the `cloud_datastore_emulator` script run the JVM as
a child that never receives the pod's SIGTERM, so under the wrapper an entity written before a
pod restart was lost. In ephemeral mode it runs through the wrapper with `--no-store-on-disk`,
as it always did. `TestDatastoreSurvivesAPodRestart` writes an entity with the Go client,
deletes the pod, and reads every entity written so far back from the new one, three times over.
A pod that is killed rather than stopped (OOM, node loss) still loses what was written since it
started. CI's restart probe reads a probe entity after `stop` and `up` in persistent mode, and
finds none after `up --mode ephemeral`. `status` reports `volume` for Datastore, and for every
other service, only in persistent mode. Firestore and Bigtable are still documented from Google's description,
not measured.

### Generated coverage

[docs/coverage](coverage/README.md) is generated from the proto descriptors, not written by hand.
It lists every RPC of Cloud Tasks, Secret Manager, Cloud Run v2, Pub/Sub and the Storage JSON
API with its status, and links each Verified one to the compat test that proves it. CI fails when
it is stale. Where this document and that one differ, the generated one names the test, or its
absence.

### BigQuery is a community emulator

`--services bigquery` runs [`goccy/bigquery-emulator`](https://github.com/goccy/bigquery-emulator)
0.8.1 (MIT), digest-pinned for linux/amd64 and linux/arm64. **Google publishes no BigQuery
emulator**, so this is the one opt-in backend that is neither Google's nor the real engine. It runs
ZetaSQL, so it accepts GoogleSQL, but the only fidelity claimed is what is listed below.

| Capability | Status | Evidence |
|---|---|---|
| Datasets and tables: create, read metadata, delete | **Verified** | `TestBigQueryDatasetTableInsertAndQuery`, official Go client. The schema reads back as created. A deleted table and dataset answer 404 afterwards. |
| Streaming inserts (`tabledata.insertAll`) | **Verified** | Same test: four rows through `Inserter().Put`. |
| `SELECT` with `WHERE`, `GROUP BY`, `ORDER BY` | **Verified** | Same test: aggregates checked value by value. |
| Storage Read API, Arrow | **Verified** | `TestBigQueryStorageReadAPIStreamsArrowRows`: a read session over the second tunnel streams all four rows as Arrow batches, through the official Storage Read client. |
| Storage Read API, Avro | **Not supported** | Measured: the emulator fails an Avro session for **any project ID containing a hyphen**, because it uses the project as an Avro namespace. |
| Go client accelerated reads (`Client.EnableStorageReadClient`) | **Not supported** | Measured: `cloud.google.com/go/bigquery` v1.85.0 panics decoding the emulator's responses. Use the default REST reads, or the Storage Read client directly. |
| DML (`UPDATE`, `DELETE`, `INSERT … SELECT`) | **Not verified** | An `UPDATE` worked in a REST probe. Not SDK-tested, so not claimed. |
| Scripting (`DECLARE`, multi-statement) | **Not verified** | A `DECLARE` worked in a REST probe. Not SDK-tested, so not claimed. |
| BQML (`CREATE MODEL` …) | **Not supported** | Measured: `Statement not supported: CreateModelStatement`. |
| Projects | **One only** | The emulator serves the project it is started with, which is the instance's default. Every other project is 404 "project … is not found" (`TestBigQueryOtherProjectsAreNotFound`). |
| Persistence | **None, measured** | A dataset created before a container restart was gone after it. CloudBurrow provisions no volume, whatever `--mode` says. |
| Emulator environment variable | **None exists** | No official client reads one. `cloudburrow env` exports `CLOUDBURROW_BIGQUERY_ENDPOINT`, which code passes to `option.WithEndpoint`; see configuration.md. |

### State snapshots

`cloudburrow state save <file>` and `state load <file>`, over `POST /admin/state/export` and
`/admin/state/import` on the loopback control port. The archive is a gzipped tar whose first entry
is a versioned `manifest.json`, naming every enabled service as captured or not, with the reason.
A service is captured **record for record from the store it keeps its state in**, so it comes back
exactly: a queue's retry configuration and state, a task's schedule and dispatch count, a disabled
secret version. This holds whichever backend the instance uses: memory, a durable file, or
Kubernetes Secrets for Secret Manager.

| Service | Status | Evidence |
|---|---|---|
| Cloud Tasks | **Verified** | `TestStateSaveResetLoadRestoresEverything` (CLI, official SDKs, CI instance) and `TestStateRoundTripsTasksSecretsAndProjects`, record-identical in ephemeral and persistent modes |
| Secret Manager | **Verified** | Same tests: every version and payload restored. In CI the backend is Kubernetes Secrets. **The archive holds secret values in plain form**: the CLI warns, and writes it `0600`. |
| Project registry | **Verified** | Same tests: a project created through the console is restored |
| Cloud Storage | **Verified** | `TestStateRestoresStorage`: buckets, objects with their bytes, content type and metadata, a 10 MiB object streamed through the archive, and notification configurations, restored with matching CRC32C. Objects stream into and out of the archive and are never held whole in memory, and a restored object whose CRC32C differs fails the load. Restoring goes through the bare backend, so it fires no notifications. Bucket labels, location and storage class are saved, but the backend discards them (see Seeding). |
| Pub/Sub | **Not captured** | Google's emulator has no export, and keeps nothing across a restart either |
| Cloud Run | **Not captured** | Services are Knative objects in the cluster; redeploy them from their images |
| Cloud KMS | **Not captured** | Key material is not exported. Keys recreated from their definitions get new material, so ciphertext produced before a `state load` cannot be decrypted after it (`TestStateManifestSaysKMSIsNotCaptured`, #388) |
| Cloud SQL (PostgreSQL) | **Verified** | `TestStateRestoresCloudSQL` (CLI, `pgx`, CI instance): a table's rows come back exactly after they were changed, and a database created since the save is gone. Saved with `pg_dump -Fc` for each application database and loaded with `pg_restore --exit-on-error`, both run **inside the server's pod** through the instance's kubeconfig (#311). Nothing is exposed beyond the existing tunnel. The archive holds the dumps and no kubeconfig or cluster credential (`TestPostgresSnapshotArchiveHoldsNoClusterCredential`, and checked on the real archive). Roles are not captured, because the server has only the one it was initialised with. `reset` does not touch PostgreSQL; a load drops and recreates every application database. |
| Cloud SQL for MySQL | **Not captured** | A real MySQL: use `mysqldump` |
| Memorystore | **Not captured** | A real Valkey: use its own `BGSAVE`, or the append-only file on its volume |
| Firestore, Datastore, Bigtable, Spanner, BigQuery | **Not captured** | In-memory emulators with no export |

**Load replaces.** Loading resets each captured service to exactly the archive's contents, in one
atomic commit per service, so nothing created since the save survives. **Refused before anything
is touched:** an archive whose manifest version is unknown (`TestARefusedArchiveLoadsNothing`),
one in another format, one that is not an archive, and one holding a service this instance does
not run.

### Python client libraries

The Go column is the rest of this document. The Python column is **Verified only where a pytest
case in `test/compat-python` exists**, run against a live instance in compat CI with the official
clients pinned in `requirements.lock`. Anything not listed is not claimed for Python.

| Operation | Go | Python | Python evidence |
|---|---|---|---|
| Storage: bucket create / get / list / delete | Verified | **Verified** | `test_bucket_and_object_crud_with_a_resumable_upload` |
| Storage: object upload, download, list, delete | Verified | **Verified** | same |
| Storage: resumable upload | Verified | **Verified** | same (a 256 KiB chunk size forces the resumable protocol) |
| Pub/Sub: topic and subscription create / delete | Verified | **Verified** | `test_publish_and_pull_with_ack` |
| Pub/Sub: publish, pull, acknowledge, no redelivery after ack | Verified | **Verified** | same |
| Cloud Tasks: queue and task create / get / delete | Verified | **Verified** | `test_queue_and_task_create_get_delete` |
| Secret Manager: secret create, version add, access by number and `latest` | Verified | **Verified** | `test_secret_version_add_and_access` |
| Every snippet in [examples/python.md](examples/python.md) | — | **Verified** | `test_examples.py` runs each block as written |

**Configuration.** Storage and Pub/Sub need nothing but `cloudburrow env`. The Python Storage
client needs the scheme in `STORAGE_EMULATOR_HOST`, which the Go client does not, and `env` exports
the form both accept. Cloud Tasks and Secret Manager need an explicit plaintext channel (see
[credentials.md](credentials.md#python-clients-for-cloud-tasks-and-secret-manager)).

**Guards.** The suite refuses to run unless `GOOGLE_APPLICATION_CREDENTIALS` is the generated
fixture and default credentials resolve to it. Every connection through Python sockets must be
loopback, which covers the HTTP clients, and so must every gRPC channel target. A violation
fails the whole session even if the test that caused it caught the error. `test_guard.py` proves
both guards by running sessions that break them. gRPC connects from C, which a Python guard
cannot see, so for gRPC it is the channel target that is checked, not the socket.

### Cloud SQL is not an emulator

| Capability | Status | Notes |
|---|---|---|
| A real SQL server, locally | **Verified** | PostgreSQL 17.11 in the cluster, digest-pinned, reached with an ordinary driver. A plain `psql` client created a table and inserted rows with nothing of CloudBurrow's involved. |
| Durability | **Verified** | The only opt-in backend with a volume, because it is the only one that is a real database rather than an in-memory emulator. |
| MySQL, locally (`cloudsql-mysql`) | **Verified** | `TestCloudSQLMySQLDataPlane` (#297): MySQL 8.4.11, digest-pinned, reached with `go-sql-driver/mysql` from the host and with the `mysql` client from a pod through `cloudsql-mysql.cloudburrow.svc.cluster.local:3306`; `cloudburrow reset` drops every non-system database. A generated per-instance password, exported by `env` and never printed by `status`. |
| MySQL durability | **Verified, measured** | CI reads back a row written before `stop` after `up` in persistent mode, and finds none after `up --mode ephemeral`. |
| MySQL in the console | **Not supported** | The schema browser reads PostgreSQL's catalogue only. |
| **The Cloud SQL Admin API** | **Not implemented** | No instances, connection names, IAM database authentication, backups, replicas or Auth Proxy path. There is no `sqladmin` endpoint. |
| Google-published component | **No** | The only one here that is not. Google publishes no Cloud SQL emulator — `gcloud emulators` ships firestore and spanner, and the sole Cloud SQL component is `cloud-sql-proxy`, which connects to a real instance in GCP. |

An application that talks SQL works. An application that calls the Cloud SQL Admin API does
not, and nothing here implies otherwise. See [cloudsql.md](cloudsql.md) and
[#121](https://github.com/cloudburrow/cloudburrow/issues/121).

### Memorystore is not an emulator

| Capability | Status | Notes |
|---|---|---|
| A real Redis-compatible server, locally | **Verified** | `TestMemorystoreDataPlane` (#296), with `github.com/redis/go-redis/v9` and nothing of CloudBurrow's: SET/GET, MULTI/EXEC, PUBLISH/SUBSCRIBE and Lua EVAL against the host endpoint, and GET from a pod through `memorystore.cloudburrow.svc.cluster.local:6379`. Valkey 8.1.10, digest-pinned. The host address comes from `REDIS_HOST`/`REDIS_PORT` as `cloudburrow env` exports them. |
| Durability | **Verified, measured** | CI stops and starts the persistent compat instance and `TestMemorystoreAcrossRestart` reads back a key written before the stop; brought up again in `--mode ephemeral`, the same test finds no key. Persistent mode is an append-only file on a PVC, fsynced every second, so up to a second of writes can be lost on a crash; ephemeral mode writes nothing to disk. |
| **The Memorystore admin API (`redis.googleapis.com`)** | **Not implemented** | No instances, no `gcloud redis instances create`, no AUTH strings, TLS, maintenance, replicas, export/import or IAM. There is no admin endpoint. |
| Google-published component | **No** | Google publishes no Memorystore emulator. Valkey is the server Memorystore for Valkey runs; Memorystore for Redis runs Redis, with which Valkey is protocol-compatible. |
| Authentication | **None** | No password, as for Cloud SQL: nothing in CloudBurrow authenticates a request, and the host endpoint is loopback only. An application that sends `AUTH` will get an error. |

An application that talks RESP works. An application that calls the Memorystore admin API does
not, and nothing here implies otherwise. See [memorystore.md](memorystore.md) and
[#296](https://github.com/cloudburrow/cloudburrow/issues/296).

**Bigtable needs network on first start.** Its emulator is the one Cloud SDK emulator absent
from the published emulators image, so the component is installed when the container starts.

Not covered, and not claimed: instance and cluster administration for Bigtable, Firestore
indexes and security rules, Datastore composite indexes, Spanner dialects other than
GoogleSQL, and backup/restore for any of them.

### Host endpoints survive a backend restart

| Capability | Status | Notes |
|---|---|---|
| Advertised host port after a pod restart | **Verified** | `TestHostEndpointSurvivesABackendRestart` restarts the pod and requires the printed address to both accept a connection **and carry a request**. Each tunnel binds a pod the forwarder chooses itself: the newest one that is Ready and **not being deleted**. `kubectl port-forward svc/…` could bind a pod that was terminating but still Ready during a rollout, such as `up --mode ephemeral` replacing a persistent pod, and connections through it hung (#381). |

This was broken until the restart criterion exposed it. `kubectl port-forward` binds one pod
and exits when it goes away, and the forwarder started it once and watched nothing — so a
crash, an OOM kill, an eviction or a rollout left the advertised endpoint dead for the life of
the instance, while the pod was `Running`, the service existed, and the startup banner still
printed the address. Worse than a refusal: with supervision removed the port still **accepted
connections** and carried nothing, so a client's `connect()` succeeded and every request timed
out.

The tunnel is now supervised and re-established on the **same** host port — the address has
already been printed, may be in an application's configuration, and for storage is baked into
the backend's advertised download URL, so reconnecting elsewhere would be a different kind of
broken. `Restarts()` counts re-establishments, because surviving a restart and never noticing
one are different states.

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

**Not re-run in CI.** The model download and CPU inference are too heavy for per-PR CI, so the
Verified rows in this section and in Vertex AI generation come from runs on a developer machine
on 2026-09-21 (see [What CI does not run](#what-ci-does-not-run-and-why)).

| Capability | Status | Notes |
|---|---|---|
| Model catalogue with verified provenance | **Verified** | Publisher recorded per artifact; a community conversion is never reported as Google-published. |
| Gated-artifact handling | **Verified** | Refused without credentials, with an actionable message. |
| Disk preflight, checksum verification, atomic cache, recovery | **Verified** | `internal/localai` |
| **Linux generation runtime** | **Verified** | `//runtime/engine:litert_lm_main` built from source at commit `02e5030` and run **from the shipped image**: **78.43 tok/s prefill, 31.98 tok/s decode, 0.39 s to first token** over a 144-token generation, CPU backend, warm cache. No LiteRT-LM release publishes a Linux artifact; Linux is nevertheless a documented, supported build target, and the gap is packaging rather than capability. |
| **Linux embedding runtime** | **Runs; no model to run** | `//runtime/engine:embedding_litert_lm_main` builds, ships in the runtime image, loads models and initialises its engine. Every obtainable model is then rejected — see [embeddings.md](embeddings.md). |
| **Text generation** | **Verified** | On `litert-community/gemma-4-E2B-it.litertlm` (2.59 GB, ungated). |
| **A Google-published runnable model** | **Not available** | Every one is `gated: manual`. The model that works is a **community** conversion and is labelled as one everywhere it appears. |
| **Embeddings** | **Blocked on the model export, not the runtime and not access** | The earlier statement — *every embedding artifact is gated* — was **wrong**: three are ungated. They fail for another reason. One ships with no tokenizer at all; rebuilt correctly with Google's own `litert-lm-builder` it reaches `Input tensor bytes must be 4 but got 2048`, as does an independent bundle from a different publisher. The conversions export a fixed `[1,512]` encoder signature and the engine requires a dynamic one. No flag bridges it. See [embeddings.md](embeddings.md). |
| Output quality | **Not claimed** | The measured run answered a factual question wrongly. That is a small quantised model's quality; CloudBurrow does not present model output as correct. |
| Benchmarks | **Measured, single sample** | One run on one machine (Apple M4 Max, arm64, CPU, container). Not a benchmark, and no comparison is drawn from them. Two conditions change them substantially: a **cold** XNNPACK cache raises init from 0.31 s to 3.27 s, and a **short** answer collapses decode speed (6.89 tok/s over two tokens on the same binary) because the first token's fixed cost is averaged over the generation. |
| Pinned artifact checksums | **Absent** | The Google artifacts are gated, so they could not be downloaded and hashed. The code supports pinning; the status reports the absence rather than implying verification. |
| Runtime distribution | **Built locally** | No image is published. `deploy/litert-lm/Dockerfile` builds it: **6m17s measured** from a cold cache on an M4 Max with 16 CPUs, once, producing a 274 MB image. Debian 13 is the base because Abseil needs C++20 `<source_location>`, which Debian 12's default clang 14 lacks. |
| Runtime image completeness | **Verified** | The image is run, not merely built. `litert_lm_main` links `libGemmaModelConstraintProvider.so` — shipped prebuilt upstream, found via a `RUNPATH` into Bazel's output tree — so a multi-stage build that copies only the binary produces an image that builds cleanly and fails on first run. The prebuilt libraries are staged and registered with `ldconfig`. |
| Catalogue artifact filenames | **Verified against the live API** | `TestCatalogArtifactsExistUpstream` (tag `upstream`) lists each repository and asserts the named file is in it, and HEADs the resolve URL for entries marked ungated. It was written after the community entry was found naming `gemma-4-E2B-it-int4.litertlm`, a filename inferred from the `google/` repositories' convention, which returns **404**. |

## Vertex AI generation (local)

A **subset** of `generateContent`, served by the runtime CloudBurrow builds. Not a Vertex
emulator and not a Gemini replica. See [generation.md](generation.md) for the exact surface.

| Capability | Status | Notes |
|---|---|---|
| `generateContent` through the official SDK | **Verified** | `TestRealGenerationThroughTheOfficialSDK`, `google.golang.org/genai` v1.71.0 pointed at a local endpoint, real runtime, real model. |
| `streamGenerateContent` (SSE) | **Verified** | 12 events over 741 ms through the SDK's own stream iterator. A unit test additionally asserts the first event arrives *while generation is still running*, so streaming cannot regress into buffering. |
| Both SDK path shapes | **Verified** | Vertex `/v1beta1/projects/{p}/locations/{l}/publishers/google/models/{m}:verb` and Gemini-API `/v1beta/models/{m}:verb`, taken from what the SDK actually sends. |
| No silent model substitution | **Verified** | A request for another model returns 404 naming what is served; `modelVersion` reports what ran; aliases must be configured by hand. |
| Generation options | **None supported** | Every one is refused with a message naming it. `maxOutputTokens` is refused on measurement: the runtime accepts it and ignores it — 309 decoded tokens at limits of 8, 40 and unset. |
| Tools, safety settings, multimodal input, multi-turn, `countTokens` | **Refused** | Each returns an error rather than being ignored. `safetySettings` in particular: **no filtering happens here**, so accepting the field would imply it does. |
| Token accounting | **Absent** | `usageMetadata` is never emitted. Nothing here measures tokens. |
| Prompt/output separation | **Verified** | The runtime echoes the prompt before generating, and echoes a multi-line prompt across lines, so forwarding from the marker returned the caller's own prompt as generated text. The echo is now skipped by matching rather than counting, which stays correct if a future runtime stops echoing. Covered against a simulated runtime in both directions and against the real one by `TestRealMultiLinePromptIsNotEchoedBack`. |
| Cancellation | **Verified** | A cancelled request kills the runtime container by name. Found by testing: killing the `docker run` client left the container generating, because it is a client for work in the daemon and the runtime ignores `SIGTERM`. |
| Output quality, safety, or equivalence to Gemini | **Not claimed** | No test asserts what the model says. |
| Embeddings | **Blocked on the model export** | Not served. See [embeddings.md](embeddings.md): ungated artifacts do exist, and the runtime rejects them. |

### Console playground

| Capability | Status | Notes |
|---|---|---|
| Real inference from the browser | **Verified** | Driven through Chromium against the real runtime; 12 lines generated, streamed. |
| Streaming in the UI | **Verified** | Output grew in **6 separate steps over 1628–2209 ms**, so it is not one late write. |
| Uses the same API as the SDK | **Verified** | A relay to the identical Vertex path and body; asserted by test, including that no generation option is added. |
| Readiness | **Verified** | A live probe requiring a 404 for an unserved model — proves answering *and* routing without loading a model. |
| Model identity and provenance | **Verified** | Model, publisher and a notice that a community conversion is not a Gemini result, shown where the output appears. |
| Refused options listed | **Verified** | All thirteen, held in step with the API by `TestPlaygroundRefusalsMatchTheAPI`. |
| Cancellation | **Verified** | Stops the stream and the container; recorded as **Cancelled**, not Failed — a distinction only browser use exposed. |
| History | **In memory, bounded** | 20 entries, never written to storage, never leaves the browser. |
| Off by default | **Verified** | Not advertised, not navigable, and the rest of the console unaffected. |
| Model download/load/unload, embedding view, prediction UI | **Not implemented** | See [playground.md](playground.md) §7. |
| Pixel parity with Vertex reference screens | **Not claimed** | Structural only; no reference screenshots exist. |

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
| IAM Credentials `generateAccessToken` for impersonation | **Verified** | `TestImpersonatedTokenUsedWithACloudBurrowClient` (#303): a `google.golang.org/api/impersonate` token source, served by the metadata server, drives an official Storage client against CloudBurrow. A transport guard fails any request not bound for loopback. **The token is local and not validated; no permission is checked.** Go's impersonate package takes no endpoint override, so its client needs a transport that routes `iamcredentials.googleapis.com` locally ([credentials.md](credentials.md#service-account-impersonation-iam-credentials)). |
| IAM Credentials `generateIdToken` | **Verified** | Unit-tested through the official `impersonate.IDTokenSource`: a JWT with the requested audience and the impersonated account's email, verified against the local `/certs`. It will **not** verify against Google's certificates. |
| IAM Credentials `signJwt` | **Implemented** | Signs the payload's claims with the instance key. Unit-tested over REST. |
| IAM Credentials `signBlob` | **Unimplemented** | UNIMPLEMENTED, tested: a local signature would make signed URLs that no Google service accepts. |
| `cloudburrow env` shell/JSON/plain export | **Verified** | `TestEnvExportsEveryClientVariable`. Exports the client-library variables *and* the `gcloud` endpoint overrides, because `gcloud` ignores the former. |
| ADC fixture usable by Google's auth library | **Verified** | `TestOfficialAuthLibraryUsesTheLocalFixture`: `google.FindDefaultCredentials` mints a token through the fixture. The test fails if a `ya29.` token appears, which would mean it reached Google with the developer's real credentials. |
| Token exchange stays local | **Verified** | Same test. The fixture's `token_uri`, `auth_uri` and cert URLs all point at the instance. |
| Metadata server contract (`/`, project, zone, email, token, identity, scopes, aliases) | **Verified** | `TestMetadataServerAnswersTheContract` and `internal/metadata`. |
| `Metadata-Flavor: Google` required and echoed | **Verified** | Enforced even though the tokens grant nothing: a local server accepting what a real one rejects teaches code to fail in production. |
| ID token is a valid RS256 JWT | **Verified** | Signed with the instance key and verifiable against `/certs`. |
| **ID token verifies against Google's certificates** | **Not supported** | Google did not issue it. Tokens are structurally valid and self-consistent; signature verification against Google's public certs is **not** claimed. |
| **Token validation of any kind** | **Not supported** | No signature, expiry, audience, scope or assertion is ever checked. The token endpoint checks only that an assertion was *sent*, so a client silently sending none finds out. |
| **IAM, scopes, per-resource permissions** | **Not supported** | Every caller can do everything. |
| `cloudburrow gcloud-setup` / `gcloud-teardown` | **Verified where gcloud is installed** | `TestGcloudSetupConfiguration` (#305): through the configuration alone, `gcloud storage ls` and `gcloud pubsub topics list` reach CloudBurrow. The default configuration is unchanged, and teardown removes the configuration and is safe to repeat. Only the storage and pubsub overrides are written, and `cloudkms = http://host:port/` when Cloud KMS is enabled on a known port (#426, `TestGcloudKMS`); `env` exports the same as `CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDKMS`. |
| `gcloud storage ls` | **Verified** | With `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE` and a local token. |
| `gcloud pubsub topics create` / `list` | **Verified** | With `CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB`. |
| **`gcloud` honouring `*_EMULATOR_HOST`** | **Not supported (upstream)** | `gcloud` ignores them and goes to the real service, which **leaves the machine**. Inherited limitation, recorded rather than worked around; `cloudburrow env` exports the overrides too. |
| Terraform `google` provider create / read / destroy | **Verified** | `TestTerraformAppliesAndDestroysThroughTheWrapper`: `google_storage_bucket`, `google_pubsub_topic`, `google_secret_manager_secret`, `google_secret_manager_secret_iam_member` (#365), `google_cloud_tasks_queue` and `google_cloud_tasks_queue_iam_member` (#366; IAM stored, not enforced) applied through `cloudburrow terraform` with `hashicorp/google` ~> 8.0 on Terraform 1.16.4, read back through the official SDKs, destroyed, and confirmed gone. A second `plan` after the apply is clean for the Secret Manager and Cloud Tasks resources. **A bucket does not plan clean:** fake-gcs-server does not return fields such as `versioning` and `soft_delete_policy`, so the provider plans to replace it (#374). The wrapper sets `storage_custom_endpoint`, `pubsub_custom_endpoint`, `secret_manager_custom_endpoint`, `cloud_tasks_custom_endpoint`, the Resource Manager and Billing endpoints, `project` and a fixture `access_token`. `terraform init` still downloads the provider once. |
| Terraform `google_kms_key_ring`, `google_kms_crypto_key`, `google_kms_crypto_key_version`, `google_kms_key_ring_iam_member`, `google_kms_crypto_key_iam_member`, `google_kms_crypto_key_iam_binding` | **Verified** (IAM stored, not enforced) | `TestTerraformKMS` (#425, #430), through `cloudburrow terraform` with `kms_custom_endpoint`, egress blocked: apply, the IAM bindings read back through the official client (one on the ring, two on the key), a clean plan covering them, a label change sent as `PATCH ?updateMask=labels` and planned clean again, and destroy. **Destroy does what the provider does on Google:** the key ring is only removed from state, and the crypto key is not deleted, but every version is destroyed (DESTROY_SCHEDULED, 30 days out); no DELETE is sent. The iam_member and iam_binding destroys remove their member from both policies. `_iam_policy` resources and `google_kms_key_ring_iam_binding` are not tested and not claimed. Re-applying the same names afterwards is ALREADY_EXISTS, as on Google. |
| Terraform, other services' endpoints | **Not set** | `cloudburrow terraform` sets an endpoint only for a service with a Verified row here, and names the enabled services it left out: their resources would otherwise be sent to real Google. |
| Terraform resources beyond Storage, Pub/Sub, Secret Manager, Cloud Tasks queues and the Cloud KMS resources above | Planned | Untested is untested. |
| Metadata server reachable from inside the cluster | **Not supported** | It binds loopback. A pod's loopback is the pod. |

## Cloud Scheduler — `google.cloud.scheduler.v1`

**Built, not reused** (#302). Google publishes no Cloud Scheduler emulator. It runs in the CLI
process, as Cloud Tasks does, and is opt-in: `--services scheduler`, on `--port-scheduler`
(default `9008`). `env` exports `CLOUDBURROW_SCHEDULER_ENDPOINT`. No client library reads it, so
pass it to `option.WithEndpoint`. Per-method status is generated in
[coverage/scheduler.md](coverage/scheduler.md).

| Claim | Status | Notes |
|---|---|---|
| Create, get, list, pause, resume, run, delete | **Verified** | `TestSchedulerHTTPAndPubSubJobs`, with `cloud.google.com/go/scheduler/apiv1` against the CI instance. |
| `RunJob` delivers at once to an HTTP target | **Verified** | The same test, against an `httptest` server. Deliveries carry `X-CloudScheduler`, `X-CloudScheduler-JobName` and `X-CloudScheduler-ScheduleTime`, and `User-Agent: Google-Cloud-Scheduler`. A 2xx response is success. HTTP targets are called from the host, so they must be reachable from it. |
| Pub/Sub targets | **Verified** | The same test: the job's data and attributes reach a subscription on the local emulator, published through the official client. A job whose topic does not exist fails its attempt, as on the real service. |
| Cron schedules in IANA time zones | **Verified** | Unit-tested through the official client with a fake clock. Unix-cron with five fields, ranges, steps and names. Zones come from Go's embedded tzdata, so they work on hosts with no zoneinfo. Invalid expressions and unknown zones are INVALID_ARGUMENT. |
| Paused jobs do not fire; resumed ones do | **Verified** | Unit-tested through the official client with a fake clock. Resuming restarts from the next slot; runs missed while paused are not made up. |
| Retries | **Verified** | Unit-tested: `retry_count` retries on the Cloud Tasks backoff schedule, fed from `min_backoff_duration`, `max_backoff_duration` and `max_doublings`, and bounded by `max_retry_duration`. The last attempt's outcome is in `status` and `last_attempt_time`. |
| `UpdateJob` | **Implemented** | Unit-tested. An empty mask replaces every settable field; the mask paths are `description`, `schedule`, `time_zone`, the target, `retry_config` and `attempt_deadline`. |
| App Engine targets | **Unimplemented** | UNIMPLEMENTED, tested: there is no App Engine locally. |
| OAuth and OIDC tokens on HTTP targets | **Unimplemented** | UNIMPLEMENTED, tested. CloudBurrow mints no tokens, and a target that checked one would receive nothing it could verify. |
| Persistence | **Implemented** | Jobs follow `--mode`, in a store under the state directory. `/admin/reset` clears them. `state save` does not capture them yet. |
## Cloud Logging — `google.logging.v2` (write and read)

**Built, not reused** (#304). Google publishes no Logging emulator. It runs in the CLI process
and is opt-in: `--services logging`, on `--port-logging` (default `9009`). `env` exports
`CLOUDBURROW_LOGGING_ENDPOINT` for `option.WithEndpoint`. Per-method status is generated in
[coverage/logging.md](coverage/logging.md).

| Claim | Status | Notes |
|---|---|---|
| `WriteLogEntries`, `ListLogEntries`, `ListLogs` | **Verified** | `TestLoggingWriteAndRead`, with `cloud.google.com/go/logging` and `logadmin` against the CI instance: structured entries are written, then read back with `logName` and `severity >= WARNING`. |
| API-written entries in the Logs Explorer | **Verified** | The same test finds them in the console's `/api/logs`, under the log's name and the project. Severities are folded onto the console's four levels. |
| Unsupported filters | **Verified** | INVALID_ARGUMENT naming the term, never a silent match: tested with `jsonPayload.event = "boot"`. |
| `DeleteLog` | **Implemented** | Removes one log's entries. |
| The store | **Implemented** | Bounded at 20,000 entries (the oldest go first), in memory in every mode, cleared by `/admin/reset`. It keeps nothing across a restart. |
| `TailLogEntries`, `ListMonitoredResourceDescriptors` | **Unimplemented** | UNIMPLEMENTED. |
| Sinks, exclusions, buckets, views, log-based metrics | **Not served** | `ConfigServiceV2` and `MetricsServiceV2` are not registered, so every call is UNIMPLEMENTED. |
| Parents other than `projects/{project}` | **Unimplemented** | Organizations, folders and billing accounts are INVALID_ARGUMENT. |

**The filter grammar ListLogEntries accepts:**

```
filter = term { [ "AND" ] term }
term   = field op value
field  = logName | severity | resource.type | timestamp
op     = "=" | "!=" | ">" | ">=" | "<" | "<="   (logName and resource.type take = and != only)
value  = a bare word, or a double-quoted string
```

Terms are ANDed, and an explicit `AND` is optional. `severity` compares by level:
`DEFAULT < DEBUG < INFO < NOTICE < WARNING < ERROR < CRITICAL < ALERT < EMERGENCY`.
`timestamp` values are RFC 3339. Everything else is refused with INVALID_ARGUMENT naming the
term: `OR`, `NOT`, parentheses, the `:` has-operator, functions, and any other field such as
`jsonPayload.*`, `labels.*` or `textPayload`. `logadmin` appends its own
`timestamp >= "…"` term, which this grammar accepts.

## Cloud KMS — `google.cloud.kms.v1`

**Built, not reused** (#309): Google publishes no Cloud KMS emulator, and each third-party one departs from Google's documented behaviour ([upstream-evaluation.md](upstream-evaluation.md#amendment-cloud-kms-is-built-not-reused-309)).
It runs in the CLI process and is opt-in: `--services kms`, on `--port-kms` (default `9018`).
`env` exports `CLOUDBURROW_KMS_ENDPOINT` for `option.WithEndpoint`. Per-method status is
generated in [coverage/kms.md](coverage/kms.md).
**One port serves gRPC and JSON** (#414), as `cloudkms.googleapis.com` does: an HTTP/2 request with an `application/grpc` content type goes to gRPC, and everything else goes to the JSON router. The router's paths are Google's own bindings, read from the `google.api.http` annotations of every `google.cloud.kms.v1` service plus the `cloudkms_v1.yaml` mixins (Locations, IAMPolicy, Operations). A bound path that is not transcoded yet answers **501 UNIMPLEMENTED** with an AIP-193 envelope that includes `status`, and a path Google does not bind answers 404. `:encrypt` and `:decrypt` are transcoded to the same server methods gRPC runs (#415), with protojson bodies: unknown fields are refused, CRCs are accepted as JSON strings or numbers, int64 fields are written as strings, and a malformed body is refused without quoting it. **Over REST, a code reaches a Go caller by its HTTP status**: gax maps HTTP 400 to INVALID_ARGUMENT, so FAILED_PRECONDITION (HTTP 400, as on Google) arrives as INVALID_ARGUMENT. The envelope's `status` still says FAILED_PRECONDITION, and the compat tests read it from there. A path that matches no Google binding is 404 over REST, where gRPC says INVALID_ARGUMENT: a malformed name, for example, or a version-name `:decrypt`. `TestKMSJSONIsServedOnTheSamePort` pins the routing. The six reads are transcoded too (#422): GetKeyRing, ListKeyRings, GetCryptoKey, ListCryptoKeys, GetCryptoKeyVersion and ListCryptoKeyVersions, the path giving `name` or `parent` to the same parsers gRPC uses, so a malformed segment is the same INVALID_ARGUMENT (`TestKMSResources` runs its reads as `grpc` and `rest` subtests; `TestKMSReadsAgreeOverREST`; `TestRESTReads`). **One query-parameter policy covers every transcoded method.** Request fields the path does not bind are query parameters, in camelCase or snake_case; whether Google accepts snake_case is UNVERIFIED. `alt=json` (or `$alt`), `prettyPrint` and `$.xgafv` are accepted; `$alt=json;enum-encoding=int`, which the GAPIC REST client sends on every call, makes enums numbers (UNVERIFIED for Google). Any other `alt`, and `fields`/`$fields`, are UNIMPLEMENTED; **any other parameter is INVALID_ARGUMENT naming it** (code UNVERIFIED), unlike the Cloud Tasks and Secret Manager JSON APIs, which ignore unknown parameters, because a dropped KMS parameter would look as though it took effect. The creates are transcoded too (#423): CreateKeyRing, CreateCryptoKey, CreateCryptoKeyVersion and `:updatePrimaryVersion`. The new resource is the body (for `:updatePrimaryVersion`, the whole request, and a body `name` that differs from the path is INVALID_ARGUMENT, UNVERIFIED for Google). An empty body is an empty message, which is what Terraform's key ring create sends. Unknown body fields are INVALID_ARGUMENT (UNVERIFIED) and are never echoed; `skipInitialVersionCreation` takes `true` or `false`; a duplicate is HTTP 409, ALREADY_EXISTS; a verb nothing binds is 404 (`TestKMSResources`' `create-grpc`/`create-rest` subtests, `TestKMSCreatesOverREST`, `TestRESTCreates`, which replays Terraform's, gcloud's and the GAPIC client's bodies). The lifecycle is transcoded too (#424): `PATCH` UpdateCryptoKey and UpdateCryptoKeyVersion, with `updateMask` a protojson FieldMask of lowerCamelCase paths converted to proto paths before the server's mask checks (a path that does not convert is INVALID_ARGUMENT), and `:destroy` and `:restore`, which take `{}` or no body. A body `name` that differs from the path is INVALID_ARGUMENT (UNVERIFIED). DeleteCryptoKey and DeleteCryptoKeyVersion return long-running operations and stay 501. `TestKMSUpdateCryptoKey`, `TestKMSUpdateCryptoKeyVersion`, `TestKMSDestroyCryptoKeyVersion` and `TestKMSRestoreCryptoKeyVersion` run every case as `grpc` and `rest` subtests; `TestKMSLifecycleOverRESTWire` and `TestRESTLifecycle` cover the wire. The IAMPolicy mixin is transcoded too (#429): `GET :getIamPolicy`, `POST :setIamPolicy` and `POST :testIamPermissions` on key rings and crypto keys, the path's resource winning over one in the body, with the same query-parameter policy; on import jobs and EKM they stay 501 (`TestRESTIam`, `TestKMSIamPolicyIsStoredNotEnforced`). JSON requests appear in `/admin/events`; fault injection is not interposed on them.

| Claim | Status | Notes |
|---|---|---|
| Key rings: create, get, list | **Verified** | `TestKMSResources`, with `cloud.google.com/go/kms/apiv1` against the CI instance. Key rings cannot be deleted here, since the API has no DeleteKeyRing RPC. Whether Google can delete an empty key ring is **UNVERIFIED**: [resource-hierarchy](https://cloud.google.com/kms/docs/resource-hierarchy) says key rings cannot be deleted, and [delete-kms-resources](https://cloud.google.com/kms/docs/delete-kms-resources) conflicts with it. |
| Crypto keys: create, get, list | **Verified** | The same test. `ENCRYPT_DECRYPT` with `GOOGLE_SYMMETRIC_ENCRYPTION` at `SOFTWARE` protection only. A new key gets version 1 as its primary unless `skip_initial_version_creation` is set. `purpose` is required; UNSPECIFIED is INVALID_ARGUMENT (error code UNVERIFIED). `destroy_scheduled_duration` defaults to 30 days and accepts whole seconds from 24h to 120d; anything else is INVALID_ARGUMENT (error code UNVERIFIED). The default is echoed in every response, and whether Google echoes it is UNVERIFIED (`TestKMSCreateCryptoKeyFields`, #399). |
| Versions: create, get, list; primary: update | **Verified** | The same test: a new version does not become primary until `UpdateCryptoKeyPrimaryVersion` says so. `TestKMSGetCryptoKeyVersion` (#395) gets each version and checks its name, `create_time`, algorithm, SOFTWARE protection and ENABLED state; a version number the key does not have is NOT_FOUND (error code UNVERIFIED). Versions are listed by number. Only an ENABLED version can become the primary; a DISABLED, DESTROY_SCHEDULED or DESTROYED target is FAILED_PRECONDITION (error code UNVERIFIED; `TestKMSPrimaryMustBeEnabled`, #401). Google also refuses this call on a key whose purpose is not ENCRYPT_DECRYPT, but that check cannot be reached here, because every key is ENCRYPT_DECRYPT. A version starts ENABLED, or DISABLED when `crypto_key_version.state` asks for it (`TestKMSVersionCanStartDisabled`). Any other initial state is INVALID_ARGUMENT (error code UNVERIFIED). Each version's state is stored, and `destroy_time` and `destroy_event_time` appear only in DESTROY_SCHEDULED and DESTROYED (#398). |
| Key material storage | Storage fact | Not an API claim. **Not a security boundary** ([ADR-0004](adr/0004-local-access-and-no-authentication.md)): key material is stored unencrypted, readable by anyone with access to the managed namespace, and `up` says so (#391). `TestKMSResources` counts the ring, key and versions as owned Secrets (`cloudburrow.dev/service=kms`) in the managed namespace. Each version's key material is 256 random bits. No RPC returns it. It is also kept out of error text (#390). Measured on kind: when `kubectl apply -f -` fails (invalid label, bad base64 in `data`, unreachable server), kubectl did not echo the Secret's data, but it does quote an invalid field's value. So every kubectl error is scrubbed of the record in raw, base64 and hex form, and of any long encoded run, before it is wrapped. |
| `/admin/reset` | **Verified** (error code UNVERIFIED) | The same test: after `reset?service=kms` the key ring is NOT_FOUND and no KMS Secrets remain. `project=` confines it to one project: its rings, keys and versions go and another project's ring still reads (`TestKMSProjectResetClearsOnlyThatProject`, #387); a store failure part-way is reported as a failed reset. No KMS page states that a missing resource is NOT_FOUND. |
| Asymmetric, MAC and raw purposes, other algorithms, HSM/EXTERNAL/EXTERNAL_VPC protection, rotation schedules (`rotation_period`, `next_rotation_time`), `import_only`, `crypto_key_backend` | **Unimplemented** | UNIMPLEMENTED at create, the message naming the field. Each is tested through the official client against the CI instance (`TestKMSOutOfScopeKeyOptionsAreUnimplemented`, #396). Automatic rotation is not implemented; rotate by creating a version and making it primary. |
| Import jobs | **Unimplemented** | `CreateImportJob` is UNIMPLEMENTED, tested. |
| `Encrypt` (gRPC and REST) | **Verified** (error codes UNVERIFIED) | Real gcloud too: `TestGcloudKMSEncryptDecrypt` (#427) round-trips files with and without AAD with integrity verification on, so gcloud requires the verified CRC32C flags, including for the crc32c("") = 0 it sends without AAD; ciphertext crosses between gcloud and the gRPC client both ways; `--version 2` ciphertext decrypts after the primary is disabled, and fails once version 2 is. `TestKMSEncrypt` (#411, #415), with the official client over gRPC **and** `NewKeyManagementRESTClient` against the CI instance, every case run on both. By CryptoKey name it uses the primary; by CryptoKeyVersion name it uses exactly that version, as the proto implies (UNVERIFIED). Request CRC32Cs are checked whenever sent, even as 0: a mismatch is INVALID_ARGUMENT naming the field, and the response sets `ciphertext_crc32c` and the `verified_*` flags. Plaintext is required, and plaintext and AAD are each at most 64KiB (the limit is documented, the code is UNVERIFIED). A missing primary or a version that is not ENABLED is FAILED_PRECONDITION (error code UNVERIFIED). **The ciphertext is CloudBurrow's own format**, AES-256-GCM in a version-id envelope, and is **not interchangeable with Google's**: ciphertext from Cloud KMS does not decrypt here, and ciphertext from here does not decrypt in Cloud KMS. |
| `Decrypt` (gRPC and REST) | **Verified** (error codes UNVERIFIED) | Real gcloud too: `TestGcloudKMSEncryptDecrypt` (#427) round-trips files with and without AAD with integrity verification on, so gcloud requires the verified CRC32C flags, including for the crc32c("") = 0 it sends without AAD; ciphertext crosses between gcloud and the gRPC client both ways; `--version 2` ciphertext decrypts after the primary is disabled, and fails once version 2 is. `TestKMSDecrypt` (#412, #415), every case over gRPC and over the REST client. The state rules are proven by `TestKMSDecryptFollowsTheVersionLifecycle` (#413): one ciphertext is refused while its version is DISABLED, DESTROY_SCHEDULED and restored-but-DISABLED, and opens again once re-enabled. `TestDecryptIsRefusedOnceDestroyed` shows it is refused for good once DESTROYED. The name is a CryptoKey; the server picks the version from the ciphertext, without trying keys. Any ENABLED version decrypts, so ciphertext from before a rotation still opens, with `used_primary` false. A DISABLED, DESTROY_SCHEDULED or DESTROYED version is FAILED_PRECONDITION (error code UNVERIFIED). A wrong AAD, a tampered ciphertext and another key's ciphertext all get one INVALID_ARGUMENT that does not say which check failed (error code UNVERIFIED). So does a CryptoKeyVersion name or an empty ciphertext. Request CRC32Cs are checked before opening; a mismatch is INVALID_ARGUMENT naming the field (documented). The response sets `plaintext_crc32c` and `used_primary`. |
| Tink envelope encryption (tink-go-gcpkms) | **Verified** | `TestKMSTinkEnvelopeEncryption` (#416), over both of tink-go-gcpkms's transports, gRPC and REST. The remote AEAD round-trips with associated data; Tink sends both CRCs and requires both to be verified. An envelope sealed with an AES-256-GCM DEK still opens after the key's primary rotates. A wrong associated data and a tampered ciphertext fail. Tink is a test-only dependency: the server never links it. |
| Every other cryptographic RPC (Raw, asymmetric, MAC, `Decapsulate`, `GenerateRandomBytes`, import, export, retired resources, deletion) | **Unimplemented** | UNIMPLEMENTED, through the official client against the CI instance: `TestKMSUnimplementedContract` (#396) calls every method the coverage registry lists as unimplemented, and fails if one has no request in its table or the table names one that is no longer unimplemented. |
| `UpdateCryptoKey` | **Verified** (error codes UNVERIFIED) | `TestKMSUpdateCryptoKey` (#405). `update_mask` is required. `labels` replaces the map, validated against Google's label rules; `version_template` accepts only the GOOGLE_SYMMETRIC_ENCRYPTION / SOFTWARE template every key has. Rotation and `key_access_justifications_policy` are UNIMPLEMENTED. An immutable field (`purpose`, `destroy_scheduled_duration`, `import_only`, `crypto_key_backend`), an output-only one or an unknown path is INVALID_ARGUMENT naming the path. |
| `UpdateCryptoKeyVersion` | **Verified** (error code UNVERIFIED) | `TestKMSUpdateCryptoKeyVersion` (#400) moves a version ENABLED to DISABLED and back. `update_mask` must contain `state`, and only ENABLED and DISABLED are allowed as targets; anything else is INVALID_ARGUMENT. A DESTROY_SCHEDULED or DESTROYED version is FAILED_PRECONDITION. Disabling the primary keeps it as the primary, now DISABLED. `external_protection_level_options` is UNIMPLEMENTED. |
| `DestroyCryptoKeyVersion` | **Verified** (error code UNVERIFIED) | `TestKMSDestroyCryptoKeyVersion` (#402). An ENABLED or DISABLED version becomes DESTROY_SCHEDULED, with `destroy_time` set to now plus the key's `destroy_scheduled_duration`. Its material is kept, so it can be restored. A version already scheduled or destroyed is FAILED_PRECONDITION. The primary may be destroyed and then reads as DESTROY_SCHEDULED; whether Google refuses that is **UNVERIFIED**. The move to DESTROYED at `destroy_time` is #403. |
| Automatic DESTROYED at `destroy_time` | **Implemented** | `TestAScheduledVersionIsDestroyedAtItsDestroyTime` drives the official client in-process on a fake clock (#403). Not Verified, because the CI instance cannot wait the 24h minimum. Every read computes the state from the clock, so the version is DESTROYED the moment `destroy_time` passes. A background sweep then stores that and **removes the key material** from its Secret, and it runs at start too, so a version that fell due while CloudBurrow was down is DESTROYED on restart. A DESTROYED version stays listed and can never be restored. `destroy_event_time` equals `destroy_time` here; Google's may be later. |
| `RestoreCryptoKeyVersion` | **Verified** (error code UNVERIFIED) | `TestKMSRestoreCryptoKeyVersion` (#404). A DESTROY_SCHEDULED version whose `destroy_time` is still ahead becomes DISABLED, with `destroy_time` cleared and its material kept; re-enable it with `UpdateCryptoKeyVersion`. Any other state, including a version whose `destroy_time` has passed, is FAILED_PRECONDITION. |
| `GetIamPolicy`, `SetIamPolicy` on a key ring or crypto key | ***Stored, not enforced*** | `TestKMSIamPolicyIsStoredNotEnforced` (#428, [ADR-0006](adr/0006-iam-policy-surface.md) as amended by #421), on a ring and on a key through the official client: bindings read back with a new etag, a stale etag is **ABORTED**, a condition is **UNIMPLEMENTED** naming it, a version 3 policy without conditions is accepted, and `ResourceIAM` round-trips; real gcloud too (`TestGcloudKMSIam`, #431). **No RPC consults a stored policy: Encrypt and Decrypt work for a caller no binding names.** Nothing is inherited: a key's policy is its own, never merged with its ring's. The policy is kept on the ring or key record, so it has the record's persistence, is cleared by `/admin/reset` and `reset?project=`, and, like the rest of KMS, is not captured by `state save`. On a missing ring or key, Get and Set are NOT_FOUND (UNVERIFIED). **Over gRPC and REST** (#429): every case runs as `grpc` and `rest` subtests with the same codes; over JSON, `:getIamPolicy` is GET only, as Google binds it, so POST is 404, and `options.requestedPolicyVersion` is a query parameter. |
| `TestIamPermissions` on a key ring or crypto key | ***Stored, not enforced*** | Same test: returns **every** requested permission on a ring or key that exists. On a well-formed name that does not exist it returns an **empty set**, as Google documents (cloudkms_v1.yaml:59-64; UNVERIFIED), where Cloud Tasks answers NOT_FOUND. |
| IAM on import jobs and EKM | **Unimplemented** | Tested (`TestKMSOtherServicesAreUnimplemented`, `TestKMSIamPolicyIsStoredNotEnforced`): CloudBurrow serves no import jobs, `ekmConfig` or `ekmConnections`, so IAM on them is UNIMPLEMENTED naming the resource type, never an empty policy. |
| Locations | **Unimplemented** | Tested (the same test): ListLocations and GetLocation are UNIMPLEMENTED. Any well-formed location ID is accepted in resource names. |
| EKM, Autokey, HSM management | **Unimplemented** | Tested (the same test): EkmService, Autokey, AutokeyAdmin and HsmManagement are not registered on the port, so each answers UNIMPLEMENTED; `TestTheKMSPortRegistersOnlyKeyManagementService` lists what is. |
| `version_view` / `view` on lists | **Verified** | `TestKMSFullViewAddsNothingForSoftwareKeys` (#406). FULL is accepted and adds nothing, because it adds only `attestation`, which exists for HSM versions, and every version here is SOFTWARE. An undefined value is INVALID_ARGUMENT (error code UNVERIFIED). |
| `order_by` on lists | **Verified** (error code UNVERIFIED) | `TestKMSOrderByName` (#407). `name` and `name desc` are accepted on ListKeyRings, ListCryptoKeys and ListCryptoKeyVersions; versions order by number. Anything else is INVALID_ARGUMENT naming the value, and a page token is bound to its order. CloudBurrow's choices, where Google's behaviour is **UNVERIFIED**: the default order is ascending by name, the default `page_size` is 100 and the maximum is 1000. |
| `filter` on lists | **Partial** | The documented grammar is parsed whole: `field op value`, NOT or `-`, AND (or a space), OR (which binds tighter than AND, as documented) and parentheses. **ListCryptoKeyVersions** filters on `state` with `=` and `!=` (`TestKMSVersionStateFilter`, #408). **ListKeyRings and ListCryptoKeys** filter on `name` with `=`, `!=` and `:` (case-insensitive substring), and ListCryptoKeys also filters on `labels.<key>` with `=` and `:` (`TestKMSNameAndLabelFilters`, #409). `name` compares the full resource name, which is UNVERIFIED against Google. Key rings have no labels. Matches are counted before paging, so `total_size` is the number matched. Every other field or operator, on any list, is UNIMPLEMENTED naming it, never ignored. A filter that does not parse is INVALID_ARGUMENT (error code UNVERIFIED). |
| Resource names | **Verified** (error code UNVERIFIED) | Every implemented RPC parses its `name` or `parent` before any lookup (#394). A malformed name is INVALID_ARGUMENT naming the field; a well-formed name that does not exist is NOT_FOUND. Google documents neither code for KMS. Unit-tested for every RPC (`TestMalformedNamesAreInvalidArgument`), and `TestKMSNamesAreValidatedBeforeLookup` checks GetCryptoKey against the CI instance. **Locations follow the rule the other services use**: lowercase words joined by single hyphens, so `1abc` and `us--east1` are refused. |
| `gcloud kms keyrings` / `keys` / `keys versions` | **Verified where gcloud is installed** | `TestGcloudKMS` (#426), through `gcloud-setup`'s configuration alone: `keyrings create` and `list`; `keys create --purpose encryption --labels`, `list` and `describe` (ENCRYPT_DECRYPT, GOOGLE_SYMMETRIC_ENCRYPTION, the label); `versions create` and `list`; `versions disable`, `enable`, `destroy` and `restore`; and `keys set-primary-version`, each checked through the official gRPC client. gcloud uses the JSON API for every kms command. `keyrings delete` is not tested: its DELETE is 501. |
| `gcloud kms keyrings add-iam-policy-binding`, `kms keys add-iam-policy-binding`, `kms keys get-iam-policy` | ***Stored, not enforced*** | `TestGcloudKMSIam` (#431), through `gcloud-setup`'s configuration alone: bindings added on a ring and on a key read back through the official gRPC client; `get-iam-policy --format=json` shows the binding and an etag; `--condition` exits non-zero naming UNIMPLEMENTED and condition, and nothing conditional is stored; a second add for another member keeps the first, the etag read-modify-write working through a real tool. |
| Terraform | **Verified** | `TestTerraformKMS` (#425): key rings, keys and versions apply, plan clean, update labels and destroy through `cloudburrow terraform` over the JSON API; see the Terraform section. |
| Durability | **Verified, measured** | CI's restart probe (#418): `TestKMSRestartSetup` runs alone just before `stop` and leaves a key with an ENABLED primary v1, a DISABLED v2, a DESTROY_SCHEDULED v3 and a v1 ciphertext. After `stop` and `up` in persistent mode, `TestKMSAcrossRestart` decrypts the ciphertext to the original plaintext and finds the states, `destroy_time`, the primary and the next version number (4) unchanged. Brought up again with `--mode ephemeral`, it requires the ring to be NOT_FOUND (error code UNVERIFIED). The KMS Secrets live in the managed namespace, which an ephemeral `up` keeps, so once the cluster answers and before `up` is ready, an ephemeral run deletes every KMS Secret not written by itself (each write carries the run's epoch label), and a persistent run deletes what an ephemeral run wrote (#481; #418 measured the ring surviving before that). Without a cluster, `TestCiphertextSurvivesReopeningADurableStore` decrypts across a close and reopen of the durable store. |
| Project-number parents | **Not supported** | `projects/123456/...` is rejected with INVALID_ARGUMENT (error code UNVERIFIED). Whether Google accepts a project number in a KMS parent is UNVERIFIED. |

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
| `GetIamPolicy`, `SetIamPolicy` on `projects/*/secrets/*` | ***Stored, not enforced*** | `TestSecretIamPolicyIsStoredNotEnforced` (#365, [ADR-0006](adr/0006-iam-policy-surface.md)): bindings set through the official client read back with a new etag, and a stale etag is **ABORTED**. **No RPC consults a stored policy**, so a binding grants and denies nothing here. Conditions and audit configs are **UNIMPLEMENTED** with the field named. A version 3 policy without conditions is accepted, and is read back as version 1. Policies are kept on the secret, so they follow `--mode`, go with `DeleteSecret`, are cleared by `/admin/reset` and are captured by `state save`. |
| `TestIamPermissions` on a secret | ***Stored, not enforced*** | Same test: returns **every** requested permission, the literal truth when nothing is enforced. A test that asserts a principal *lacks* a permission fails here rather than passing falsely. |

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

## Resource Manager — `google.cloud.resourcemanager.v3` (Projects only)

**v3 Projects** (#298), over gRPC and the v3 REST paths, and **v1 REST
`projects.list/get/create/delete`** (#301), which `gcloud projects` and Terraform's
`google_project` call, are served on `--port-resourcemanager` (default `9007`) from the **same
project registry the console lists**. Per-method status for v3 is generated in
[coverage/resourcemanager.md](coverage/resourcemanager.md).

| Claim | Status | Notes |
|---|---|---|
| Create, get, search, update labels, delete | **Verified** | `TestResourceManagerV3Projects`, with `cloud.google.com/go/resourcemanager/apiv3` against the CI instance. Create, Update and Delete return **completed** long-running operations, since each change is made before the call returns. Update accepts `display_name` and `labels` in the mask and refuses anything else. |
| One store with the console | **Verified** | The same test: a project created through the API is in the console's project list, one created in the console is found by `SearchProjects`, and an API delete removes it from the console. |
| `ListProjects` | **Implemented, lists nothing** | It lists a parent's children, and no project here has a parent: there are no folders or organizations. `folders/…` and `organizations/…` answer UNIMPLEMENTED, and an empty parent is INVALID_ARGUMENT, as in Google's API. Use `SearchProjects`. |
| `SearchProjects` query | **Partial** | Space-separated `key:value` terms, all of which must match: `id`, `projectId`, `name`, `displayName`, `state`, `parent` and `labels.KEY`. `OR`, wildcards and any other key are INVALID_ARGUMENT, never ignored. |
| Delete | **Differs** | Google keeps a deleted project in `DELETE_REQUESTED` for 30 days. Here the operation's project says `DELETE_REQUESTED`, but the registration is removed at once, and `UndeleteProject` is UNIMPLEMENTED. Resources created under the ID are not touched. |
| `GetIamPolicy`, `SetIamPolicy`, `TestIamPermissions`, `MoveProject`, `UndeleteProject` | **Unimplemented** | UNIMPLEMENTED, tested in-process and against the CI instance. |
| Folders, Organizations, Liens, TagKeys, TagValues, TagBindings | **Not served** | Not registered, so every call is UNIMPLEMENTED; `FoldersClient.GetFolder` and `ListFolders` are tested. |
| v1 `projects.list`, `get`, `create`, `delete`, and `operations.get` | **Verified** | `TestResourceManagerV1Projects`, using `google.golang.org/api/cloudresourcemanager/v1`: a project created through v1 is visible through v3 and in the console. `create` returns a completed operation that `operations.get` also answers. `list` supports v1 filter terms (`id`, `name`, which is the display name, `labels.KEY` and `lifecycleState`), with trailing wildcards; `OR` and other fields are INVALID_ARGUMENT. `projectNumber` is a stable number derived from the ID, not a real project's. |
| Every other v1 method | **Unimplemented** | `update`, `undelete`, the IAM methods, org policy, liens, folders and organizations all return the UNIMPLEMENTED envelope. |
| `gcloud projects list` | **Verified where gcloud is installed** | `TestGcloudProjectsList` runs gcloud with `CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER` and a local access-token file; it is skipped when gcloud is absent. |
| Terraform `google_project` | **Verified** | `TestTerraformGoogleProject`: create and destroy through `cloudburrow terraform`, with `deletion_policy = "DELETE"` (the provider's default, `PREVENT`, refuses destroy by design). The wrapper points `resource_manager_custom_endpoint` and `cloud_billing_custom_endpoint` here. `apply` and `destroy` run with `HTTPS_PROXY` pointed at a closed port, so a provider call to any Google endpoint the wrapper failed to override fails the test instead of leaving the machine. |
| Cloud Billing `projects.getBillingInfo` | **Partial** | Served only because `google_project` reads it on every refresh. It always says billing is disabled, since there is no billing locally. No other Cloud Billing method is served. |
| Endpoint variables | **Verified** | `env` exports `CLOUDBURROW_RESOURCEMANAGER_ENDPOINT` for `option.WithEndpoint`, and `CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER` for gcloud. gcloud's `projects` commands use v1, which this does not serve. |

## Fault injection — `/admin/faults`

Rules on the loopback-only control port (#306) make calls to the services CloudBurrow serves
itself fail or slow down: Cloud Tasks, Secret Manager, the Cloud Run adapter and Cloud KMS
(#392, `TestKMSFaultInjectionAgainstTheSDKRetry`). That lets a
client's retry and deadline handling be exercised with the SDK it actually uses.

```sh
curl -X POST http://<control>/admin/faults -d '{"service":"secretmanager","method":"AccessSecretVersion","code":"UNAVAILABLE","count":2}'
curl http://<control>/admin/faults              # list, with each rule's injected and remaining counts
curl -X DELETE http://<control>/admin/faults    # all rules; ?id=fault-1 for one
```

| Field | Meaning |
|---|---|
| `service` | `tasks`, `secretmanager`, `run` or `kms`; required |
| `method` | a glob over the method name, `AccessSecretVersion` or `Get*`; default every method |
| `project` | only calls whose resource is under `projects/{project}` |
| `probability` | 0 to 1, default 1 |
| `code` or `httpStatus` | the gRPC code by name, or an HTTP status mapped to its code as Google maps them; default `UNAVAILABLE` |
| `latencyMs` | delay before the call. With no code or status, the call then proceeds; with one, it then fails |
| `count` | faults to inject before the rule stops; 0 means no limit |
| `seed` | makes a probabilistic rule reproducible: the same seed gives the same sequence of faulted and passed calls |

| Claim | Status | Notes |
|---|---|---|
| The SDK's retry absorbs injected faults | **Verified** | `TestFaultInjectionAgainstTheSDKRetry`, against the CI instance: two UNAVAILABLE faults on `AccessSecretVersion`, and the official client succeeds on its third attempt. `/admin/events` shows both (`kind=fault`). |
| Latency against a deadline | **Verified** | Unit-tested with the official Cloud Tasks client: a 500ms latency rule fails a 200ms-deadline `GetQueue` with DeadlineExceeded. |
| Reproducible probability | **Verified** | Unit-tested: `seed` 42 at `probability` 0.5 gives the same 20-call sequence twice. |
| Storage, Pub/Sub and the opt-in emulators | **Refused** | 400. They are reached through a port-forward to the upstream emulator, so CloudBurrow never sees their requests and a rule would never apply. |
| Secret Manager's JSON API | **Not interposed** | Rules apply to gRPC calls. REST requests to Secret Manager pass through untouched. |
| Clearing | **Verified** | `DELETE /admin/faults` and `/admin/reset` (scoped to the services named) clear rules; unit-tested. |

Faults are returned as gRPC statuses with the requested code, which is how Google's gRPC errors
reach a client.

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
| Object upload, download, preview and delete | **Verified** | `TestConsoleStorageObjects` (#295), through the official storage client against fake-gcs-server: a console upload reads back through the SDK with the same bytes and CRC32C; a 10 MiB download is byte-identical; an HTML object previews as `text/plain` with a CSP and `nosniff`; an upload over the Settings limit (default 32 MiB) is refused with the limit named and leaves no object; a console delete removes the object. `TestDownloadIsStreamed` shows the download reaches the client before the provider's reader has finished. Previews are limited to 1 MiB, and only PNG, JPEG, GIF and WebP render as images. The upload limit lives in the running console and resets to the default on restart. |
| Pub/Sub actions: create subscription, publish, pull | **Verified** | `TestConsolePubSubActions` (#294), on a topic's page, each through the official `pubsub/v2` client against the emulator. **Create subscription** takes an ID, a push endpoint (empty for pull) and an ack deadline, and the SDK reads back what was set. **Publish** takes a body, attributes and an ordering key, and an SDK subscriber receives the same data and attributes. **Pull and ack** leaves nothing for the next SDK pull. **Pull without ack** sets the ack deadline to 0, so the SDK is redelivered the message; its copy says so and that delivery attempts go up, because Pub/Sub has no peek. Pulled messages are shown in the action's dialog and are never written to the operations ledger or the logs; the ledger records that each action ran. Push subscriptions cannot be pulled and say where their messages go. A delivery attempt count is shown only where Pub/Sub keeps one, on a subscription with a dead-letter policy. |
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
| **Request-rate, latency and resource metrics** | **Partial** | **Cloud Tasks, Secret Manager and the Cloud Run v2 adapter only**, the services CloudBurrow serves itself: every call is counted by service, method and canonical code, with a latency histogram, at `GET /metrics` on the loopback control port in the Prometheus text format (`TestCreateTaskCallsAreCounted`, which parses it with Prometheus's own parser). `/monitoring` charts request rate, error rate and p50/p95 latency per service. **Storage, Pub/Sub and the opt-in emulators are not measured**, since their traffic goes over a raw port-forward, and are labelled so, never charted at zero. Cloud Run workloads' own requests need Knative's queue-proxy metrics, which are off (#181). |
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
| Reset / seed / event inspection | Partial | `internal/admin`, served on the loopback-only control port and refused on service ports. **Reset covers Cloud Tasks, Cloud Storage (buckets, objects and notification configurations), Pub/Sub, Secret Manager and Cloud KMS**, narrowable with `service=` and, except for Storage, `project=` (`TestAProjectResetClearsOnlyThatProject`). A project-scoped reset that includes Storage is refused, because the backend cannot list buckets by project (`TestAProjectResetIncludingStorageIsRefused`). Pub/Sub state in a project CloudBurrow has never seen survives a full reset. **Seed covers Cloud Tasks, Cloud Storage, Pub/Sub and Secret Manager** from one document validated whole before anything is created (`TestOneSeedDocumentIsReadBackByTheOfficialSDKs`, `TestAnInvalidSeedDocumentSeedsNothing`). Pub/Sub fields the emulator is not known to honour are refused by name. **Bucket labels, location and storage class are refused: the backend discards them** (a bucket created with labels read back with none through the official client). **Events record every API call on the ports CloudBurrow serves itself** — Cloud Tasks, Secret Manager (gRPC and JSON) and the Cloud Run v2 adapter — with method, resource, project, canonical status code and duration, and never a payload or query string (`TestSecretManagerCallsAreRecordedWithoutTheirPayloads`). **Storage, Pub/Sub and the opt-in emulators are not recorded**: their traffic goes over a raw port-forward to an upstream process that CloudBurrow does not intercept. |
| Go SDK compatibility harness | **Verified** | `test/compat`. Refuses non-loopback endpoints and fails outright if cloud credentials are present in the environment. |
| Local cluster lifecycle (up/status/stop/reset/delete) | **Verified** | `internal/cluster` integration tests. |
| Workstation preflight (`cloudburrow doctor`) | **Verified** | `internal/doctor`: binaries, daemon reachability, memory, CPUs, disk and every port `up` would bind, each with a remedy. Only genuine blockers exit non-zero; an unmeasurable check reports `unknown`, never `ok`. On macOS and Windows the disk figure is the host volume backing the VM disk, and says so. |
| Diagnostics bundle (`cloudburrow diagnose`) | **Verified** | `test/compat/diagnose_test.go` creates a secret with a known payload, runs `diagnose` against the CI instance and searches every file of the bundle for that payload, the ADC fixture's private key and the kubeconfig's client key; none may appear. A stopped instance is covered by a unit test. Redaction is pattern-based (`console.Redact`) plus removal of pod env values; a credential an application logs in a form those patterns do not recognise is not caught, so read a bundle before sharing it. |
| Cluster ownership isolation | **Verified** | Prefix enforced at construction and re-checked on delete; namespace reset requires `cloudburrow.dev/owned=true`. |
| Python SDK compatibility harness | **Verified** | `test/compat-python`: pytest against the official Python clients, pinned by hash in `requirements.lock`, run by `make compat-python` in compat CI. See [Python client libraries](#python-client-libraries). |
| Java / Node SDK support | Planned | Endpoint-override mechanism not yet verified against client source. No support claimed. |
