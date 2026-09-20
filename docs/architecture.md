# CloudBurrow architecture

Status: accepted for the first release cycle · Revised 2026-09-20 by #25
Supersedes the single-process, all-custom-Go, direct-Docker design recorded in
[ADR-0001](adr/0001-independent-go-implementation.md)–[ADR-0003](adr/0003-state-and-persistence.md).

This document is the contract the remaining issues implement against. Where it and an
implementation disagree, one of the two is a bug.

> **Nothing described here is implemented yet.** Every operation in
> [`compatibility.md`](compatibility.md) is `Planned` until a merged PR demonstrates it
> passing a test written against an official Google client library.

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
| Cloud Storage | Integrate + adapt | `fake-gcs-server` v1.56.1 |
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
- **Cloud Run Jobs and source builds.** Prebuilt images only.
- **BigQuery, Firestore, Spanner, Bigtable, Datastore.** Official emulators exist for several
  of these and are recorded as future extensions; they are not in this release.
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
   ├─ component install + readiness      │   │  fake-gcs-server       (Service) │
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

```
cmd/cloudburrow/           CLI entry point; argument dispatch only
internal/
  config/                  Configuration model, precedence, validation
  lifecycle/               Startup ordering, readiness, cancellation, bounded shutdown
  cluster/                 kind provider: create, delete, kubeconfig, ownership labels
  k8s/                     Typed client helpers, apply, wait-for-ready
  components/              Install and manage in-cluster backends
  adapter/
    pubsub/                Endpoint discovery, reset, persistence reporting
    storage/               Same, for the storage backend
    run/                   Cloud Run v2 -> Knative Serving mapping
  service/
    tasks/                 Cloud Tasks, implemented by us
  apierror/                Google-style errors; one cause -> gRPC status + JSON body
  resource/                Resource-name parsing, project/location scoping
  paging/  lro/            Pagination primitives; long-running operations
  sched/                   Injected clock, due-time scheduling, retry/backoff
  admin/                   Seed, reset, event inspection
test/
  upstream/                Probes measuring third-party components (tag: upstream)
  compat/                  Official-SDK compatibility tests (tag: compat)
  k8s/                     Native Kubernetes and Helm portability (tag: integration)
```

Rules, enforced by review:

1. **Adapters may not import each other.** Cross-service needs go through a narrow interface
   declared by the consumer and wired in `lifecycle`.
2. **Only `internal/cluster` and `internal/k8s` know Kubernetes exists.** Adapters speak to
   endpoints, not to pods.
3. **Pure Go units must be testable without a cluster.** Config, resource names, error
   mapping, paging and scheduling have unit tests that never touch Kubernetes. A change that
   makes them require a cluster is a design regression.
4. **`cmd/` contains no logic worth testing.**

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
| Java, Node | All | **Unverified** | No support claimed. |

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
the hard way: `fake-gcs-server` advertised `mediaLink: http://0.0.0.0:4443/...`, and because
the official storage client *follows* `mediaLink` on download, host-side reads failed while
in-cluster reads succeeded. One address cannot serve both audiences. The storage adapter must
therefore set the backend's public host to match the audience, or expose an address that
resolves identically inside and outside the cluster. Resolved in #26.

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
- **Cloud Storage persists** with a filesystem backend on a PVC — measured surviving restart.
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
- **Latest-candidate mode** — discovered and tested in isolation by #31. Never used for
  release artifacts until promoted into the verified set.

**A mutable tag is for discovery only, never a release pin.** An entry whose digest is `null`
is not yet reproducible, and the inventory lists those explicitly rather than implying
otherwise.

Reference combination — **stood up and verified end to end on 2026-09-20** (see
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
