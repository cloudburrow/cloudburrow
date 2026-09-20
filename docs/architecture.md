# CloudBurrow MVP Architecture and Compatibility Contract

Status: accepted for the first release cycle
Applies to: milestones `01 - Foundation`, `02 - Working GCP services`, `03 - Usable MVP`

This document defines what CloudBurrow is, what it promises to callers, and where the
boundaries between its parts lie. It is the contract that issues #2 through #20 implement
against. Where this document and an implementation disagree, one of the two is a bug.

> **Nothing described here is implemented yet.** Every operation in
> [`compatibility.md`](compatibility.md) is marked `Planned` until a merged PR demonstrates it
> passing a test written against an official Google client library. Do not read this document
> as a description of working software.

---

## 1. Goal and scope

CloudBurrow is a single local process that speaks enough of the Google Cloud APIs that an
application built with official Google Cloud SDKs can run end to end on a developer machine
with no GCP project, no credentials, and no network egress.

The first release targets four services:

| Service | Why it is in the MVP |
|---|---|
| Cloud Storage | The most common dependency; the entry point of the acceptance workflow. |
| Pub/Sub | Carries events between components without a broker. |
| Cloud Tasks | Covers deferred and retried HTTP work, which has no official emulator at all. |
| Cloud Run | Executes application containers, which is what makes the workflow real rather than a set of mocked APIs. |

### 1.1 The acceptance workflow

The first release is judged by one workflow, defined here and verified by issue #19. A
developer must be able to:

1. **Upload** an object to a CloudBurrow bucket using an official Cloud Storage client.
2. **Publish** an event describing that object to a CloudBurrow Pub/Sub topic.
3. **Run** a worker — a container deployed as a Cloud Run service — that receives the event
   by push delivery or by a Cloud Tasks dispatch, and reads the uploaded object back out of
   Cloud Storage.
4. **Save** the worker's result as a new Cloud Storage object, and read that result from the
   host with an official client.

Every step uses an official Google SDK against a local endpoint. If any step needs a
CloudBurrow-specific client, the release has failed its own acceptance test.

### 1.2 Non-goals for the first release

These are excluded deliberately. They are not "not yet scheduled"; they are out of contract,
and an issue that starts implementing one has drifted.

- **Full IAM enforcement.** No policy evaluation, no principals, no `setIamPolicy` semantics.
  IAM-shaped methods either return `UNIMPLEMENTED` or a permissive stub, and the matrix says
  which. CloudBurrow is not a tool for testing whether your permissions are correct.
- **Autoscaling and revision traffic behavior.** Cloud Run services run a fixed local
  container. No scale-to-zero, no concurrency-driven replica counts, no traffic splitting
  across revisions beyond what the acceptance workflow needs.
- **Source builds.** No Cloud Build, no buildpacks, no `gcloud run deploy --source`. Callers
  supply a prebuilt image reference.
- **GKE, BigQuery, and Firestore.** Out of scope entirely for the first release.
- **Billing, quotas, org policy, VPC-SC, and audit logging.**
- **Production durability.** CloudBurrow is a development tool. Its state format carries no
  compatibility guarantee across versions before 1.0, and it is not a backup target.

---

## 2. Design position

### 2.1 Independent implementation, official contracts

CloudBurrow is written from scratch in Go. It does not fork `fake-gcs-server`, the
`gcloud beta emulators` implementations, or any other existing emulator. It takes its
protocol definitions from [googleapis/googleapis](https://github.com/googleapis/googleapis)
at a pinned revision, and its behavioral expectations from the published service
documentation. See [ADR-0001](adr/0001-independent-go-implementation.md).

The practical consequence: **the contract is the proto and the published API reference, not
another emulator's behavior.** When CloudBurrow and some other emulator differ, that is not
automatically a CloudBurrow bug. When CloudBurrow and the official client library differ,
it always is.

### 2.2 Resource management is separated from execution

Every service splits into two independently testable halves:

- The **control plane** stores and returns resource metadata: buckets, topics,
  subscriptions, queues, services, revisions. It is CRUD over a metadata store, and it can be
  fully correct while nothing actually runs.
- The **data plane** does the work: transferring object bytes, delivering messages,
  dispatching tasks, running containers.

This split matters because it is the most common way an emulator lies. Creating a Cloud Run
service and getting a well-formed `Service` back proves nothing about whether a container
started. The compatibility matrix tracks the two halves in separate columns for exactly this
reason, and a control-plane-only implementation is never described as supporting a service.

---

## 3. Process and module boundaries

One process, one lifecycle coordinator, several listeners.

```
cmd/cloudburrow/           CLI entry point; flag parsing only, no behavior
internal/
  config/                  Configuration model, precedence, validation
  lifecycle/               Startup ordering, readiness, cancellation, shutdown
  transport/
    rest/                  HTTP listeners, JSON encoding, upload/download protocols
    grpc/                  gRPC server, interceptors, reflection
  apierror/                Google-style errors; one cause -> gRPC status + JSON body
  resource/                Resource-name parsing and formatting, project/location scoping
  paging/                  Deterministic ordering, page tokens
  lro/                     Long-running operation tracking
  store/                   Metadata store abstraction: memory mode and durable mode
  blob/                    Object payload storage, separate from metadata
  sched/                   Clock injection, due-time scheduling, retry/backoff, workers
  runtime/docker/          Container runtime adapter
  service/
    storage/               Cloud Storage (JSON API v1; gRPC v2 later)
    pubsub/                Pub/Sub
    tasks/                 Cloud Tasks
    run/                   Cloud Run
  admin/                   Local-only control API: seed, reset, event inspection
test/
  compat/                  Black-box tests using official SDKs (Go, then Python)
```

Dependency rules, enforced by review and later by a lint check:

1. `internal/service/*` may depend on the shared primitives (`store`, `blob`, `sched`,
   `resource`, `apierror`, `paging`, `lro`, `runtime`). **Services may not import each
   other.** The acceptance workflow crosses service boundaries, so the temptation is real —
   Pub/Sub push needs to reach a Cloud Run URL, and Cloud Storage events need to reach
   Pub/Sub. Those crossings go through narrow interfaces declared by the *consumer* and wired
   in `lifecycle`, never through a direct import.
2. `transport/*` may not contain service behavior. It converts wire formats to and from
   service calls. A protocol adapter that decides what a request means is misplaced.
3. Nothing outside `runtime/docker` knows Docker exists.
4. `cmd/` contains no logic worth testing.

---

## 4. Endpoints, routing, and SDK configuration

This is the part most likely to be got wrong, because it depends on client library behavior
rather than on the API definitions.

### 4.1 Routing decision: one port per service surface

CloudBurrow binds **separate listeners per service surface** rather than multiplexing
everything behind one port with prefix matching. See
[ADR-0002](adr/0002-transport-and-routing.md).

There are two forcing reasons:

- The Go Cloud Storage client requires HTTP and gRPC on **different ports**. This is not a
  style preference; it is stated in the client source, which uses `STORAGE_EMULATOR_HOST` for
  the HTTP endpoint and a separate `STORAGE_EMULATOR_HOST_GRPC` for gRPC, with the comment
  that "when using a local emulator, HTTP and gRPC must use different ports."
- REST path spaces collide. The Cloud Storage JSON API owns `/storage/v1/`, but the Cloud Run
  Admin API owns `/v2/{name=projects/*/locations/*/services/*}` — a generic `/v2/` prefix
  that would force ambiguous fallback routing if it shared a listener with other REST
  surfaces.

gRPC is the exception and is multiplexed onto a single port, because fully-qualified
protobuf service names are globally unique, so dispatch is unambiguous by construction.

Default port map, all configurable, all bound to loopback:

| Port | Surface | Protocol |
|---|---|---|
| 9000 | Control: health, readiness, admin API | HTTP |
| 9001 | Cloud Storage JSON API v1 | HTTP |
| 9002 | Cloud Run Admin API v2 | HTTP |
| 9003 | Cloud Tasks REST v2 (if implemented) | HTTP |
| 9004 | Pub/Sub REST v1 (if implemented) | HTTP |
| 9010 | Pub/Sub, Cloud Tasks, Cloud Storage v2 | gRPC |

Ports are configurable individually and as a base offset, and every port may be set to `0`
to request an OS-assigned free port — required so that compatibility tests can run in
parallel (issue #10). A started instance reports its resolved ports on the control port and
on stdout.

### 4.2 Client configuration is per-language and per-service, and it is not uniform

**Do not assume a single `CLOUDBURROW_HOST` variable will work.** Each client library decides
for itself whether an emulator override exists and what it means. The verified state today:

| Client | Service | Mechanism | Notes |
|---|---|---|---|
| Go | Storage | `STORAGE_EMULATOR_HOST` | Scheme optional; client prepends `http://` when absent and appends the `storage/v1/` path itself. |
| Go | Storage (gRPC) | `STORAGE_EMULATOR_HOST_GRPC` | Scheme stripped; must be a different port from the HTTP endpoint. |
| Go | Pub/Sub | `PUBSUB_EMULATOR_HOST` | Sets endpoint, insecure transport, and disables auth via a client hook. |
| Python | Storage | `STORAGE_EMULATOR_HOST` | **Scheme required** — the client uses the value verbatim. Ranks below an explicit `client_options.api_endpoint` and above `API_ENDPOINT_OVERRIDE`. |
| Go/Python | Cloud Tasks | **None exists** | No emulator environment variable. Callers must pass an explicit endpoint and disable auth in client options. |
| Go/Python | Cloud Run | **None exists** | Same as Cloud Tasks. |
| Java, Node | All | **Unverified** | Not yet confirmed against client source. Out of scope for the first harness, and no support is claimed. |

Two consequences the documentation must carry, because they will otherwise be discovered as
bugs by users:

- The Go and Python storage clients disagree about whether `STORAGE_EMULATOR_HOST` includes a
  scheme. Published setup instructions must include the scheme, since that form is accepted
  by both.
- **Cloud Tasks and Cloud Run cannot be pointed at CloudBurrow by environment variable at
  all.** They require explicit client options in application code. This is a real ergonomic
  limit of the approach, not something CloudBurrow can paper over, and issue #20's
  documentation must show the explicit-endpoint form for those two services.

### 4.3 Addressing between host and containers

Three distinct addresses exist for the same emulator, and conflating them is the most likely
source of "works from my terminal, fails in the container" reports:

- **Host to emulator:** `127.0.0.1:<port>`.
- **Container to emulator:** the loopback address inside a container is the container itself.
  Containers reach CloudBurrow at `host.docker.internal` on Docker Desktop, and on Linux at
  the gateway address of the container's network, which the runtime adapter discovers and
  injects. CloudBurrow therefore also binds an address reachable from the container network
  when container execution is enabled, and this widens exposure beyond loopback — see §6.
- **Emulator to container:** the container's mapped port, discovered by the runtime adapter
  after start (issue #9), never assumed from the image.

Environment variables injected into application containers use the container-to-emulator
form. The values differ from what the host uses, and the acceptance workflow must exercise
both directions.

---

## 5. Projects, locations, and resource naming

- Projects are **namespaces created implicitly on first use.** There is no project admin API
  and no project existence check. Any syntactically valid project ID works.
- Resource isolation by project is mandatory and tested: `projects/a/topics/t` and
  `projects/b/topics/t` are different resources that must not collide in the store or on
  disk (issue #6).
- Locations are validated for syntax but not against a list of real regions. The default is
  `us-central1`. Requests for a location that is merely unusual succeed; requests for a
  malformed one fail with `INVALID_ARGUMENT`.
- Resource names follow the proto `google.api.resource` annotations exactly. Parsing and
  formatting live in `internal/resource` and nowhere else, so that the collision and
  traversal tests have a single place to cover.

Bucket names are the exception: Cloud Storage buckets are globally namespaced in GCP, not
project-scoped. CloudBurrow keeps buckets global **within an instance**, which matches client
expectations, and relies on per-instance isolation rather than per-project isolation to keep
parallel tests from colliding.

---

## 6. Local authentication and exposure

CloudBurrow accepts no cloud credentials and performs no authentication.

- **No credential validation.** `Authorization` headers are ignored if present. CloudBurrow
  never verifies a signature, never contacts Google, and never reads application default
  credentials. A test that appears to authenticate has not.
- **Default bind is `127.0.0.1`.** The default configuration is unreachable from other
  machines.
- **Binding to a non-loopback address requires an explicit flag** and emits a warning at
  startup that names the exposure. Because an unauthenticated service with a writable data
  directory and a Docker socket is a serious liability on a shared network, this is opt-in,
  loud, and documented as unsafe.
- **Container execution widens exposure by necessity** (§4.3) and this is called out at
  startup when it happens.
- **Admin endpoints are always loopback-only**, on the control port, regardless of the bind
  setting, and are refused on service ports. Reset and seed destroy data; they are never
  reachable from the container network or the LAN.
- Signed URLs, when implemented, are accepted without signature verification. The matrix says
  so, since a passing signed-URL test would otherwise imply a guarantee that does not exist.

---

## 7. State, reset, and shutdown

Two modes, selected at startup:

- **Memory mode** (default for tests): all metadata and payloads in process. Leaves no
  durable application state behind on exit. The default for the compatibility harness, so
  tests cannot contaminate each other.
- **Durable mode** (default for `cloudburrow up`): metadata in an embedded store, object
  payloads as files, under a single data directory.

Rules, implemented in issue #7:

- A data directory is **owned by one running instance.** Ownership is claimed by a lock, and
  a second instance pointed at a live directory refuses to start rather than corrupting it.
- Metadata updates that must agree with a payload write are committed atomically; a crash
  mid-upload must not leave a readable object with the wrong bytes or generation.
- Object names are untrusted input and may never escape the data directory, including through
  traversal, encoded separators, and Unicode normalization tricks.
- **Reset** cancels scheduled work first, then deletes state — never the reverse, since a
  live worker would otherwise recreate state after deletion. Reset is admin-only and
  loopback-only.
- **Shutdown** on SIGINT/SIGTERM stops accepting work, cancels workers, drains within a
  bounded timeout, closes listeners, and flushes durable state. Exceeding the timeout is
  reported on exit rather than hidden.
- Readiness reports actual initialized services. An instance whose mandatory startup work
  failed is never ready, and reporting ready while degraded is a bug, not a convenience.

---

## 8. Background work ownership

Message delivery, task dispatch, and retries are background activity, and unowned goroutines
are how emulators come to hang on shutdown and flake in tests.

- The **lifecycle coordinator owns all workers.** Services register work; they do not spawn
  detached goroutines.
- All scheduling goes through an **injected clock** (issue #8) so tests advance virtual time
  instead of sleeping. A test that sleeps to wait for a retry is a defect.
- **Retry policy is per service, not shared.** Pub/Sub redelivery after ack-deadline
  expiry and Cloud Tasks retry with backoff are different mechanisms with different
  configuration surfaces, and forcing them through one policy would misrepresent both.
- Delivery is **at-least-once**, matching the real services. Duplicates are possible and the
  documentation says so rather than implying exactly-once.
- Persisted jobs recover on restart in durable mode; in-flight attempts at crash time may be
  redelivered.

---

## 9. Supported-operation matrix

The authoritative list lives in [`compatibility.md`](compatibility.md), with a row per
operation and separate control-plane and data-plane status.

The rule governing it: **an operation moves off `Planned` only when a merged PR includes a
test that exercises it through an official Google client library.** Not a curl command, not
an internal unit test, not a hand-built request. Anything else is a claim about our own code
rather than about compatibility, and the matrix exists to prevent exactly that substitution.

---

## 10. Open questions

Carried deliberately, to be closed by the issues named.

1. **Embedded metadata store choice** — decided in issue #7 with its own ADR. Requires atomic
   multi-key commits and single-writer ownership.
2. **Cloud Storage gRPC (`google.storage.v2`)** — the JSON API v1 is the primary surface used
   by clients by default and is the MVP target. gRPC storage is deferred until the JSON
   surface passes its tests.
3. **REST surfaces for Pub/Sub and Cloud Tasks** — the SDKs use gRPC, so REST is speculative
   value. Ports are reserved in §4.1; implementation is not committed.
4. **Cloud Storage object-change notifications into Pub/Sub** — the acceptance workflow
   publishes explicitly from the application, so automatic notifications are not required for
   the first release. Whether to add them is deferred until the workflow passes.
