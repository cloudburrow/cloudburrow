# CloudBurrow architecture

Status: accepted for the first release cycle · Revised 2026-09-20 by #25 · Module map and
rules revised 2026-09-27 by #588
Supersedes the single-process, all-custom-Go, direct-Docker design recorded in
[ADR-0001](adr/0001-independent-go-implementation.md)–[ADR-0003](adr/0003-state-and-persistence.md).

This document is the contract the remaining issues implement against. Where it and an
implementation disagree, one of the two is a bug.

> **This document is the design, not the support matrix.** Most of it is implemented. What
> works is recorded per operation in [`compatibility.md`](compatibility.md), where a row is
> `Verified` only when a merged test drives it through an official Google client, and
> summarised in [`status.md`](status.md). Anything here that is not built yet says so.

---

## 1. What CloudBurrow is

A local Kubernetes cluster that speaks Google Cloud APIs.

Applications built with official Google Cloud SDKs run end to end on a developer machine with
no GCP project, no credentials and no network egress — and the same cluster is a real
Kubernetes cluster, so `kubectl`, Helm charts and operators work against it directly.

Two distinct capabilities, deliberately not conflated:

- **GCP API compatibility** — Cloud Storage, Pub/Sub, Cloud Tasks and Cloud Run v2 answered
  locally for official SDKs.
- **Native Kubernetes portability** — ordinary manifests, Helm charts and operators applied to
  the cluster without going through CloudBurrow at all.

### 1.1 Reuse is the default

CloudBurrow does not rewrite working upstream components to keep everything in one language.
The [upstream reuse audit](upstream-evaluation.md) decided each service on measured evidence:

| Service | Approach | Component |
|---|---|---|
| Pub/Sub | Integrate | Google `cloud-pubsub-emulator` 0.8.35 |
| Cloud Storage | Build | CloudBurrow's own server, to the discovery document (#485; replaced an upstream emulator in #519) |
| Cloud Tasks | Build | no viable upstream found |
| Cloud Run | Adapt onto upstream | Knative Serving v1.23.0 |

Building instead of integrating requires a **specific unmet requirement and measured
evidence**. An upstream being Java, or having a clock we cannot inject, is an integration
constraint — not a reason to replace it.

### 1.2 The acceptance workflow

The release is judged by one workflow (#19):

1. **Upload** an object to Cloud Storage with an official client.
2. **Publish** an event describing it to Pub/Sub.
3. **Run** a worker — a Cloud Run service backed by Knative — that receives the event and
   reads the object.
4. **Save** the result as a new object, read back from the host with an official client.

Every step uses an official SDK. If any step needs a CloudBurrow-specific client, the release
has failed its own test.

### 1.3 Non-goals

Out of contract, not merely unscheduled:

- **Full IAM enforcement.** No policy evaluation. Google's Pub/Sub emulator returns
  `Unimplemented` for IAM methods and we do not paper over it.
- **GKE-specific APIs**, Google-managed load balancing, storage classes and identity.
- **Source builds.** Prebuilt images only. (Cloud Run Jobs were listed here; since #582 they
  run as Kubernetes batch Jobs, docs/compatibility.md.)
- **BigQuery beyond what its community emulator does.** Firestore, Spanner, Bigtable and
  Datastore now ship as opt-in Google emulators. BigQuery ships as `goccy/bigquery-emulator`,
  because Google publishes none, and inherits its limits (docs/compatibility.md).
- **Production durability.** CloudBurrow is a development tool. State format carries no
  compatibility guarantee before 1.0.
- **A promise that an unmodified GKE manifest runs unchanged.** Endpoint configuration and
  local overlays legitimately differ. See §7.

---

## 2. Shape of the system

```
 host                                    │  local Kubernetes cluster (kind)
 ────────────────────────────────────────┼──────────────────────────────────────────
  cloudburrow CLI                        │   ┌─ cloudburrow namespace ──────────┐
   ├─ cluster lifecycle (create/delete)  │   │  Pub/Sub emulator      (Service) │
   ├─ component install + readiness      │   │  Cloud Storage (ours)  (Service) │
   ├─ endpoint reporting                 │   │  Cloud Tasks (ours)    (Service) │
   └─ explicit kubeconfig, own context   │   │  Cloud Run v2 adapter  (Service) │
                                         │   └──────────────────────────────────┘
  kubectl / helm / operators ────────────┼──▶ Kubernetes API (direct, unmediated)
                                         │   ┌─ knative-serving ────────────────┐
                                         │   │  Knative Serving + net-kourier   │
                                         │   └──────────────────────────────────┘
```

**The host CLI owns**: cluster lifecycle, component installation, readiness aggregation,
endpoint discovery and reporting, and reset. It holds no application state.

**The cluster owns**: every service backend, its persistence, and all workload execution.

---

## 3. Module boundaries

The package set as of 2026-09-27, one line each, taken from each package's doc comment. A
row marked **Planned** names a package that does not exist yet.

| Path | Purpose |
|---|---|
| `cmd/cloudburrow/` | The CLI: argument parsing, and the wiring that builds every in-process service, the console, the admin API and their observers for `up`; also `env`, `terraform`, `gcloud-setup`, `logs`, `state` and the other subcommands. Tested in the package. |
| `cmd/cloudburrow-storage/` | The builtin Cloud Storage server alone: cross-built for Linux, embedded in the CLI, and run by the in-cluster storage Deployment (#514). `cloudburrow-storage bigquery-front` is BigQuery's validating front (#902) and `cloudburrow-storage pubsub-front` the Pub/Sub front (#873), each from the same image in its emulator's pod. |
| `internal/adapter/run/` | Cloud Run v2 API mapped onto Knative Serving (services, revisions) and Kubernetes batch Jobs (jobs, executions); refuses what it cannot map (ADR-0005). |
| `internal/admin/` | The loopback-only control API: seed, reset, event inspection. |
| `internal/apicontract/` | Pins the Google API contracts CloudBurrow implements against, and where each comes from. |
| `internal/apierror/` | One internal cause mapped to a Google-style gRPC status and JSON error body. |
| `internal/archtest/` | Tests only (#672): enforces rules 1–3 below against the module, each with an explicit list of today's exceptions. |
| `internal/bigqueryfront/` | The checks in front of the BigQuery emulator (#861), run in its pod by `cloudburrow-storage bigquery-front` (#902): IDs, schemas, DDL, jobs and `insertAll` rows are refused as BigQuery refuses them, with 400 or 409 and per-row `insertErrors`, and what the emulator would report done without doing is 501. |
| `internal/buildpacks/` | Turns source into a runnable image with Google Buildpacks and `pack`. |
| `internal/cluster/` | The local kind cluster: create, discover, stop, start, delete, with an explicit kubeconfig. |
| `internal/components/` | Installs and manages the in-cluster backends and Knative Serving from pinned manifests. |
| `internal/config/` | Configuration model, precedence (flags over environment over file over defaults), validation. |
| `internal/console/` | The local web console: a view over the same surfaces an SDK uses, never a store of its own. |
| `internal/doctor/` | Diagnoses a workstation before a cluster is created. |
| `internal/hooks/` | Runs the `ready.d` and `shutdown.d` lifecycle script directories (#285). |
| `internal/hostguard/` | Refuses HTTP requests whose Host header names a host CloudBurrow does not answer to: the DNS-rebinding defence on every HTTP listener (#676). |
| `internal/hostrelay/` | TCP relay that lets pods reach services the CLI serves on loopback (#575). |
| `internal/iampolicy/` | IAM policy storage without enforcement (ADR-0006). |
| `internal/images/` | Gets locally built images into the cluster without a registry. |
| `internal/k8s/` | The one kubectl Runner (#599): kubeconfig, context and namespace fixed at construction, typed not-found and forbidden errors, the ownership labels, long-running port-forwards, streamed exec and log following, and an interactive exec on a pseudo-terminal for the console's terminal (#781). Every kubectl CloudBurrow runs outside `internal/cluster` goes through it: Secret Manager, Cloud KMS, the Cloud Run adapter, `internal/netfwd`, `internal/images`, `internal/components`, and in `cmd/cloudburrow` the console's reads and log follower, `logs`, `diagnose`, the Cloud SQL snapshots and the cluster-host Service; and `internal/terminal`. |
| `internal/lifecycle/` | Startup ordering, readiness, bounded shutdown, ownership of background workers, and the narrow interfaces by which one service reaches another. |
| `internal/localai/` | Acquisition of local AI model artifacts, kept apart from their execution. |
| `internal/localhost/` | Dials any name under `.localhost` on loopback without a DNS lookup, for the dispatchers the CLI runs (#714). |
| `internal/lro/` | Long-running operations: pending, completed and failed. |
| `internal/metadata/` | A local GCE metadata server and the fixture credentials that point Google tooling at CloudBurrow. |
| `internal/metrics/` | Counts the calls CloudBurrow serves itself, in the Prometheus text format (#292). |
| `internal/netfwd/` | Publishes in-cluster Services on host loopback addresses (port-forward tunnels), with the Host check in front of the tunnels that carry HTTP (#725). |
| `internal/paging/` | Pagination with deterministic ordering; invalid page tokens refused. |
| `internal/prediction/` | The Vertex AI custom prediction container contract. |
| `internal/prefetch/` | Offline cache of the node, backend and Knative artifacts a first `up` downloads (#604). |
| `internal/pubsubfront/` | The front the Pub/Sub pod runs before Google's emulator (`cloudburrow-storage pubsub-front`), for gRPC and the REST API on one port: passes every call through, and enforces subscription expiration, which the emulator stores and never acts on (#873); relays the emulator's pushes so a successful one counts as activity, refuses exactly-once delivery on a push subscription, and reads and restores the expiration clocks and the projects named for `cloudburrow state` (#880), over REST as over gRPC (#908), routing each request by its content type, so REST over h2c is served (#909); keeps what the emulator does not store in a file on the pod's emptyDir, so a restart of the front alone loses nothing (#898). |
| `internal/resource/` | Google resource-name parsing and formatting, project and location scoping. |
| `internal/sched/` | Cancellable background work, due-time scheduling, retry and backoff over an injected clock. |
| `internal/service/kms/` | Cloud KMS, built by us (#309). |
| `internal/service/logging/` | Cloud Logging write and read (`LoggingServiceV2`), built by us (#304). |
| `internal/service/resourcemanager/` | The project registry, and the Resource Manager v3 and v1 Projects APIs over it (#298, #301). |
| `internal/service/scheduler/` | Cloud Scheduler, built by us (#302). |
| `internal/service/secrets/` | Secret Manager v1, built by us; with a cluster, versions are kept as Kubernetes Secrets in the workload namespace. |
| `internal/service/storage/` | Cloud Storage, built by us to Google's spec (#485), the only storage backend (#519). |
| `internal/service/tasks/` | Cloud Tasks queues, tasks and HTTP dispatch, built by us. |
| `internal/service/vertexai/` | The subset of Vertex AI `generateContent` the local runtime can perform. |
| `internal/storageimage/` | Builds the in-cluster Cloud Storage image from the embedded `cmd/cloudburrow-storage` binaries (#514), and says which architectures a CLI embeds (#686). |
| `internal/storageimage/storageimagetest/` | Tests only: stand-in embedded binaries, present, missing or placeholder (#686). |
| `internal/storageserver/` | Runs the builtin Cloud Storage server as a process. |
| `internal/store/` | Resource metadata storage: in-memory and durable modes, atomic multi-key commits, single-instance ownership of a data directory. |
| `internal/telemetry/` | OpenTelemetry traces of the requests CloudBurrow serves itself (#313). |
| `internal/terminal/` | The console's Cloud Shell-style terminal (#781): the pinned Cloud SDK pod in the instance's namespace, with the instance's pod environment and gcloud configuration and read-only kubectl, and the interactive `kubectl exec` into it, through the `internal/k8s` runner. |
| `internal/transport/grpc/` | The gRPC server CloudBurrow's own services run on: one listener, shared interceptors and message-size limit. |
| `internal/transport/rest/` | Shared HTTP plumbing for CloudBurrow's own REST surfaces: routing, size limits, JSON, Google-style error bodies. |
| `internal/trust/` | Which directories' configuration and hooks the developer has agreed to run (#598). |
| `internal/version/` | Build identification, injected at link time. |
| `tools/coverage/` | Generates per-service API coverage from the proto surface (#288); `make docs-check` fails when it is stale. |
| `tools/depcheck/` | Reports every `dependencies.json` component as candidate, current, skipped (with the reason) or unreachable; discovery only (#703). |
| `tools/docsmap/` | Fails `make docs-check` when this module map drifts from the tree, or a Console subject in compatibility.md is both Verified and Not supported (#588). |
| `tools/doclinks/` | Fails `make docs-check` on a relative Markdown link to a path that does not exist (#520). |
| `test/compat/` | Official Go SDK compatibility tests against a running instance (tag `compat`). |
| `test/compat-python/` | Official Python client suite (`make compat-python`). |
| `test/compat-node/` | Official Node.js client suite (`make compat-node`), run in compat CI's `storage` and `emulators` shards (#589). |
| `test/k8s/` | Native Kubernetes, Helm and Knative portability (tag `integration`). |
| `test/e2e/` | The acceptance workflow of §1.2 (tag `e2e`). |
| `test/upstream/` | Probes measuring third-party components (tag `upstream`). |
| `test/oracle/` | Differential oracles: Google's fakekms and storage-testbench (tag `oracle`). |
| `test/install/` | `scripts/install.sh`, the formula renderer and the release workflow, against a fake release. |
| `test/actionselftest/` | Runs after the setup-cloudburrow action, with only the environment it exported (#282). |
| `test/localai/` | Local generation against the real runtime and a real model (tag `localai`). |
| `test/repo/` | Checks over the repository's own files. |
| `test/tools/` | Test-only helpers (`pubsubfake`), never shipped. |

Rules:

1. **No service or adapter package depends on another's.** A package under
   `internal/service/` or `internal/adapter/` does not import, directly or transitively,
   another one there. A cross-service need goes through a narrow interface declared by the
   consumer and wired in `internal/lifecycle` or `cmd/cloudburrow`; the Cloud Run adapter's
   `SecretResolver` is the pattern. There is no exception: the retry schedule Cloud Scheduler
   shared with Cloud Tasks moved into `internal/sched` (#672).
2. **Only `internal/cluster` and `internal/k8s` exec kubectl.** ADR-0005 makes some packages
   Kubernetes-aware by design, and that is not a violation: `internal/adapter/run` translates
   Cloud Run into Knative Serving and batch Jobs, `internal/components` installs the in-cluster
   backends and Knative, and `internal/service/secrets` keeps versions as Kubernetes Secrets so
   Cloud Run revisions can reference them (Cloud KMS keeps its keys the same way). They may build Kubernetes objects, but reach
   the cluster through the one runner in `internal/k8s` (#599) instead of their own.
   Other service packages speak to endpoints, not to pods. The port-forward tunnels
   (`internal/netfwd`), image loading (`internal/images`) and the component installer
   (`internal/components`) use the same runner, and so does `cmd/cloudburrow`: the console's
   reads and log follower, `logs`, the Cloud SQL snapshots and the cluster-host Service. No
   package outside `internal/cluster` and `internal/k8s` execs kubectl, and
   `internal/archtest` fails on one that does; its allow-list is empty.
3. **`cmd/` borrows no service's kubectl helper.** Wiring in `cmd/cloudburrow` does not take
   its kubectl runner from a service or adapter package, and does not exec kubectl itself: it
   builds an `internal/k8s` runner for its own calls and hands one to each service (#599).
4. **Pure Go units must be testable without a cluster.** Config, resource names, error
   mapping, paging and scheduling have unit tests that never touch Kubernetes. A change that
   makes them require a cluster is a design regression.
5. **`cmd/cloudburrow` holds wiring and CLI behaviour, and tests it.** It is not a thin
   dispatcher: it builds every in-process service and implements the subcommands, and it is
   tested in the package (`go test ./cmd/cloudburrow`; the few tests that need a cluster are
   behind the `integration` tag). Logic that is not about the CLI or about
   wiring belongs in `internal/`.

Rules 1–3 are checked by `internal/archtest` (#672), each against an explicit list of today's
exceptions that fails when a new one appears or a listed one is gone; rules 4 and 5 are
enforced by review. `make docs-check` checks the table above against the tree
(`tools/docsmap`, #588): every listed path exists, every package under `internal/` is listed,
and a **Planned** row fails once its package lands.

---

## 4. Endpoints and SDK configuration

Separation of surfaces is unchanged in spirit from
[ADR-0002](adr/0002-transport-and-routing.md) — it now comes from distinct Kubernetes
Services rather than a fixed host port map.

Each backend is reached on the host through a stable local address that the CLI reports after
startup. Ports are not fixed constants: they are allocated and reported, which is what makes
parallel instances possible.

### 4.1 Client configuration is per-language and per-service

Verified against client source in #1 and unchanged by this revision:

| Client | Service | Mechanism | Notes |
|---|---|---|---|
| Go | Storage | `STORAGE_EMULATOR_HOST` | Scheme optional; client appends `storage/v1/`. |
| Go | Storage (gRPC) | `STORAGE_EMULATOR_HOST_GRPC` | Must differ from the HTTP endpoint. |
| Go / Python | Pub/Sub | `PUBSUB_EMULATOR_HOST` | Sets endpoint, insecure transport, disables auth. |
| Python | Storage | `STORAGE_EMULATOR_HOST` | **Scheme required** — value used verbatim. |
| Go / Python | Cloud Tasks | **None exists** | Explicit endpoint in client options. |
| Go / Python | Cloud Run | **None exists** | Explicit endpoint in client options. |
| Node | All | See [compatibility.md](compatibility.md#nodejs-client-libraries) | Tested by `test/compat-node` (#589); [credentials.md](credentials.md#nodejs-clients) gives each client's mechanism. |
| Java | All | **Untested, no plan** | No Java test exists and none is planned (#718). CloudBurrow makes no Java claim and documents no Java mechanism. |

Two consequences that must appear in user documentation, not as footnotes: Go and Python
disagree on whether `STORAGE_EMULATOR_HOST` includes a scheme (publish the form with a
scheme, which both accept), and **Cloud Tasks and Cloud Run cannot be redirected by
environment variable at all**.

### 4.2 Addressing

- **Host → service:** the reported local address.
- **In-cluster → service:** the Kubernetes Service DNS name. Workloads deployed into the
  cluster use this form, which differs from the host form.
- **Adapter → workload:** the Knative-assigned URL, discovered after readiness, never assumed.

Environment injected into application workloads uses the in-cluster form. The acceptance
workflow exercises both directions.

**A backend's advertised address is part of its configuration, not an afterthought.** Verified
the hard way (#26): the official storage client *follows* `mediaLink` on download, so a server
that advertises one fixed address serves one audience, and the upstream emulator used until #519
needed a second Deployment for in-cluster reads. CloudBurrow's own storage server builds
`mediaLink` and `selfLink` from each request's `Host`, so one Deployment answers the host,
through the tunnel, and the cluster, by its Service name (#514).

### 4.3 Local images must bypass tag resolution

Knative resolves image tags to digests by contacting the registry. A locally built image
loaded straight into the cluster has no registry, so the revision fails with
`failed to resolve image to digest: ... 401 Unauthorized`.

Images intended for local execution therefore use a registry prefix Knative skips
(`dev.local/`, `ko.local/`, `kind.local/`), or CloudBurrow runs a local registry. Verified:
the identical image failed as `cloudburrow-worker:verify` and succeeded as
`dev.local/cloudburrow-worker:verify`. Implemented in #26.

---

## 5. Cluster ownership and safety

A tool that creates and deletes clusters can destroy work that is not its own.

- CloudBurrow acts **only on clusters it created**, identified by name prefix and labels.
- It **never changes the global current kubecontext.** Every operation uses an explicit
  kubeconfig and context. A developer's `kubectl` behaves identically before and after.
- It **never mutates or deletes clusters, namespaces or resources it does not own.**
- **Cluster-node privileges are not application-pod privileges.** The kind node container is
  necessarily privileged; application pods are not, and receive no host mounts and no Docker
  socket by default.
- **Destructive operations are explicit and named.** `stop` does not delete. `reset` destroys
  state. `delete` destroys the cluster. None of the three implies another.

---

## 6. Lifecycle, state and persistence

| Operation | Meaning |
|---|---|
| `up` | Create the cluster if absent, install components, wait for readiness, report endpoints. |
| `status` | Report actual component readiness and resolved endpoints. |
| `stop` | Stop the cluster without destroying it. State that a backend persists survives. |
| `reset` | Destroy all CloudBurrow-managed state, keeping the cluster. Cancels work *before* deleting state. |
| `delete` | Destroy the cluster CloudBurrow created. |

**Persistence is per backend and must be reported truthfully.** This is not a formality:

- **Pub/Sub does not persist.** The audit measured a topic `NotFound` after restart *even
  with `--data-dir`*. We cannot inherit durability the backend lacks, and the CLI and
  compatibility matrix say so plainly rather than implying otherwise.
- **Cloud Storage persists** in persistent mode, in the server's own store on a PVC, and starts
  empty in ephemeral mode whatever is on disk (#512) — both measured across a restart.
- **Cloud Tasks** is ours; its persistence is our decision (#15/#16).

Readiness reflects what actually initialised. A component whose mandatory startup failed is
never reported ready.

---

## 7. Support matrix: three separate things

Conflating these is how a tool ends up over-promising. They are tracked separately in
[`compatibility.md`](compatibility.md):

1. **GCP API compatibility** — does an official SDK call behave correctly? Promoted only by an
   SDK-driven test.
2. **Native Kubernetes and Helm portability** — do ordinary manifests, charts and operators
   work? Tested by #29, independently of any GCP API.
3. **Knative feature coverage** — which Cloud Run v2 behaviors does the adapter actually map?
   Revision traffic and scaling are tested in #30.

**Knative is not Cloud Run.** It is the closest available model. No blanket claim is made
that it reproduces Cloud Run semantics, and untested behavior is not claimed at all.

---

## 8. Dependencies and reproducibility

[`dependencies.json`](../dependencies.json) is the single inventory. Every component records
its source, version, immutable identity, license, redistribution terms, update feed and
verification mechanism.

- **Verified mode** — the pinned set. All ordinary, release and offline builds use it, and
  need no network.
- **Latest-candidate mode** — discovery only. The scheduled Dependencies workflow runs
  `tools/depcheck`, which lists newer GitHub releases in the job summary; no CI job runs a
  suite against a candidate. A candidate is tested only when a reviewed pull request moves
  the pin, and that pull request's ordinary CI is the test. Never used for release artifacts
  until promoted into the verified set.

**A mutable tag is for discovery only, never a release pin.** An entry whose digest is `null`
is not yet reproducible, and the inventory lists those explicitly rather than implying
otherwise. Pulled images carry a digest and release YAMLs a per-file `manifests` hash; `up`
downloads each Knative manifest, checks its sha256 against the pin and applies it from stdin,
refusing one that differs (#597). `TestPinsMatchTheInventory` fails when a Go constant and the
inventory disagree. `TestDockerfilesPinTheInventory` does the same for the repository's
Dockerfiles: every `FROM` carries an inventory digest, and every download is checked against an
inventory checksum (#687).

Reference combination — **stood up and verified end to end on 2026-09-20** (see
[the 2026-09-20 record](https://github.com/cloudburrow/cloudburrow/blob/15762298ef4e164275ce769e87d5823649aa0758/docs/local-verification.md); the current macOS run is in
[`docs/local-verification.md`](local-verification.md)):

| Component | Version | Verified |
|---|---|---|
| kind | v0.33.0 | Cluster ready in 37.6 s |
| Kubernetes (`kindest/node`) | v1.36.4 | Server reports v1.36.4 |
| Knative Serving | v1.23.0 | All 4 deployments Available; no version complaint |
| net-kourier | v1.23.0 | Knative Service served HTTP 200 from the host |

Knative Serving v1.23.0 enforces `DefaultKubernetesMinVersion = "v1.34.0"`; upstream states no
maximum, so none is assumed. v1.36.4 satisfies it, confirmed by the controller starting
without the version check firing.

**Measured resource budget** for the full stack (kind + Knative + Kourier + both emulator
backends + one workload, 19 pods): **1.5 GiB** resident in the node container, **2125 m CPU
and 1370 Mi** in aggregate pod requests. That is the real floor a developer pays, and it is
substantially heavier than the superseded single-process design.

**Redistribution is not the same as licensing.** Google's Pub/Sub emulator is **not
established as open source** — the Apache-2.0 LICENSE inside its JAR belongs to bundled
dependencies, and its own classes have no published source. CloudBurrow therefore runs the
official digest-pinned image or directs the user to `gcloud components install`, and does not
vendor the binary.

---

## 9. Testing rules

- **Owned scheduling logic keeps an injected clock.** Retry, backoff and due-time behavior are
  tested deterministically by advancing virtual time.
- **External components get bounded polling.** Readiness and events are awaited with explicit
  deadlines, because another process's clock cannot be advanced. This replaces the earlier
  blanket prohibition on sleeping, which was written when every component was ours.
- **Unit tests must not require a cluster.** Cluster-dependent tests are tagged and separate.
- **An operation is supported only when an official SDK drives it.** Unchanged, and the reason
  `compatibility.md` exists.

---

## 10. Open questions

1. **Cloud Tasks has no upstream** — concluded from not finding one, which is weaker than the
   other audit findings. Re-tested before #15.
2. **Pub/Sub non-persistence**: surface honestly as memory-only, or reconstruct state
   CloudBurrow-side? Decided in #27.
3. **Knative + Kubernetes v1.36.4 is pinned but unverified**; #28 is the gate.
4. **Resource budget** for the full stack is measured in #28, not projected.
