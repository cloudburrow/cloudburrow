# ADR-0005: Kubernetes foundation and reuse-first components

- Status: Accepted
- Date: 2026-09-20
- Issue: #25 (depends on the evidence in #24)
- **Supersedes:** [ADR-0001](0001-independent-go-implementation.md),
  [ADR-0002](0002-transport-and-routing.md),
  [ADR-0003](0003-state-and-persistence.md)
- **Amends:** [ADR-0004](0004-local-access-and-no-authentication.md)

Historical ADR text is preserved unedited. This document records what changed and why, so
the earlier reasoning stays readable rather than being rewritten into agreement.

## Context

ADR-0001 chose an independent Go implementation of all four MVP services, with Docker used
directly to run Cloud Run containers. The [upstream reuse audit](../upstream-evaluation.md)
(#24) measured what that decision was actually buying, and two findings undercut it:

1. **Google already ships a working Pub/Sub emulator**, and it passes every data-plane probe
   we care about, including StreamingPull — the hardest part of the surface. Rewriting it in
   Go would spend significant effort to arrive at something we would then have to prove
   equivalent.
2. **A maintained community Cloud Storage implementation already gets the hard parts right.**
   `fake-gcs-server` returns correct `412 conditionNotMet` for generation preconditions,
   handles resumable uploads and compose, and survives restart. Preconditions in particular
   are what make concurrent writes correct and are the first thing a shallow implementation
   omits.

Separately, driving Docker directly to run Cloud Run workloads reimplements — badly — what an
orchestrator already does: scheduling, readiness, service discovery, restart, and resource
limits. Knative Serving is the upstream that maps most closely onto Cloud Run's model, and it
requires Kubernetes.

ADR-0001's stated cost was "more code before anything works." The audit showed that cost was
larger than assumed and the benefit smaller.

## Decision

**Kubernetes is the foundation. Reuse is the default. CloudBurrow owns integration, the local
developer experience, and only the compatibility behavior that is demonstrably missing
upstream.**

Concretely:

1. **A local Kubernetes cluster is the runtime.** `kind` is the initial and only reference
   provider. A second provider is not added until one is demonstrably needed — supporting two
   from the start doubles the test matrix before either is proven.
2. **Emulator backends run as workloads in that cluster**, reached through Kubernetes
   Services, with persistence through PVCs where the backend supports it.
3. **Cloud Run v2 requests go through an explicit adapter to Knative Serving.** The adapter is
   ours; the execution engine is not.
4. **The native Kubernetes API is exposed directly.** `kubectl`, Helm and operators work
   against the cluster without going through CloudBurrow.
5. **Not everything is written in Go.** Go remains the language of the CLI, adapters and
   owned services. An upstream component being Java, or having a clock we cannot inject, is
   an integration constraint — not grounds for rewriting it.
6. **Dependencies are pinned by immutable identity** (digest, checksum, or commit) in one
   central inventory. Mutable tags are for discovery only, never for a reproducible artifact.

### Per-service outcome

| Service | Approach | Component |
|---|---|---|
| Pub/Sub | Integrate | Google `cloud-pubsub-emulator` |
| Cloud Storage | Build to spec (*amended by #486*; `fake-gcs-server` until the cut-over, #519) | none viable: see the [evaluation amendment](../upstream-evaluation.md#amendment-cloud-storage-is-built-not-reused-485) |
| Cloud Tasks | Build | none viable found |
| Cloud Run | Adapt onto upstream | Knative Serving |

### Pinned reference combination

| Component | Version | Basis |
|---|---|---|
| kind | v0.33.0 | Latest release |
| Kubernetes node image | `kindest/node` v1.36.4 | Published by kind v0.33.0 |
| Knative Serving | v1.23.0 | Latest release |
| Knative networking | net-kourier v1.23.0 | Matches Serving minor |

The Kubernetes version is not a guess. Knative Serving v1.23.0 vendors
`DefaultKubernetesMinVersion = "v1.34.0"` in `knative.dev/pkg/version`, and kind v0.33.0
publishes node images for v1.34.11, v1.35.8, v1.36.4 and v1.37.0. v1.36.4 sits comfortably
above the enforced minimum without being the newest release, since Knative states a minimum
but no upper bound, and running ahead of what upstream tests is a poor default.

**This combination has been stood up and verified end to end** (2026-09-20; evidence in
[local-verification.md](../local-verification.md)): the cluster came up in 37.6 s on
Kubernetes v1.36.4, all four Knative Serving deployments reached Available, a Knative Service
became Ready in 4.6 s and answered HTTP 200 through Kourier from the host, and the full
upload → publish → worker → result workflow passed driven by official SDKs. #28 remains the
issue that installs this as a product feature rather than by hand.

**Resource budget, measured not projected.** The full stack — kind, Knative Serving, Kourier,
both emulator backends and one workload, 19 pods — used **1.5 GiB** resident in the node
container with **2125 m CPU / 1370 Mi** in aggregate pod requests. Knative's own guidance is
3 CPU / 3 GB for a local quickstart, which the measurement is consistent with.

## What this changes from the superseded ADRs

**ADR-0001 (independent Go implementation) — superseded.** The contract source is unchanged:
`googleapis` protos and published API documentation remain authoritative, and divergence from
an official client is still always our bug. What changes is who writes the server. Reuse is
now the default, and building requires a demonstrated gap.

**ADR-0002 (one listener per service surface) — superseded.** Its core reasoning still holds
and is *why* the Kubernetes model works: surfaces stay separated, and the Go storage client's
requirement that HTTP and gRPC use different ports is still satisfied. But separation now
comes from distinct Kubernetes Services rather than a fixed host port map. The host-side port
table in the old architecture no longer describes anything real.

**ADR-0003 (two state modes, single-instance data-directory ownership) — superseded.**
CloudBurrow no longer owns one data directory containing all state, so single-instance
ownership of it is moot. Persistence is now **per backend**, through PVCs, and must be
reported honestly for each one. The audit makes this urgent rather than theoretical:
**Google's Pub/Sub emulator does not persist state even when given `--data-dir`** — a topic
was `NotFound` after restart. We cannot inherit a durability guarantee the backend does not
have, and we will not imply one.

**ADR-0004 (no authentication, loopback default) — amended, not superseded.** CloudBurrow
still performs no authentication, still validates no credentials, and still never reads
application default credentials. What changes is the shape of exposure, since a cluster is
not a single loopback process. See the ownership and privilege rules below.

## Ownership, isolation and privilege

A tool that creates and deletes clusters can destroy work that is not its own. These rules
are constraints, not guidance:

- **CloudBurrow acts only on clusters it created**, identified by its own name prefix and
  labels.
- **It never changes the global current kubecontext.** All operations use an explicit
  kubeconfig and an explicit context. A developer's `kubectl` must behave exactly the same
  before and after CloudBurrow runs.
- **It never mutates or deletes clusters, namespaces or resources it does not own.**
- **Cluster-node privileges are distinct from application-pod privileges.** The kind node
  container is necessarily privileged; that says nothing about what an application pod gets.
  Application pods are not privileged by default and do not receive host mounts or a Docker
  socket.
- **Destructive operations are explicit.** Reset and cluster deletion are separate, named,
  and never implied by `stop`.

## Consequences

**Costs, stated plainly.**

- **Docker plus a Kubernetes cluster is a much heavier prerequisite** than one Go binary.
  Startup moves from sub-second to cluster-provisioning time, and the memory floor rises by
  gigabytes. This is the real price of the decision.
- **We inherit upstream limitations we cannot fix**, including Pub/Sub's lack of persistence
  and its `Unimplemented` IAM methods.
- **More moving parts to version together.** kind, Kubernetes, Knative and the networking
  layer must be upgraded as a coordinated set, which is why #31 exists.
- **Knative is not Cloud Run.** It is the closest available model, not an equivalent one.

**What we get.** Real container orchestration instead of a hand-rolled Docker driver. Native
`kubectl`/Helm/operator support as a first-class capability. Google's own Pub/Sub behavior
rather than our approximation of it. Far less code to write, and correspondingly less to be
wrong about.

**Obligations this creates.**

- **No blanket claim that Knative reproduces Cloud Run semantics.** Revision traffic and
  scaling behavior are mapped and tested explicitly in #30; whatever is not tested is not
  claimed.
- **No promise that an unmodified GKE manifest runs unchanged.** Endpoint configuration and
  local overlays legitimately differ from production, and the support matrix separates native
  Kubernetes portability from GCP API compatibility so neither is mistaken for the other.
- **The testing rule changes for external components.** Owned scheduling logic keeps
  deterministic injected-clock tests. Upstream components get **bounded readiness and event
  polling with explicit deadlines**, because another process's clock cannot be advanced. This
  replaces the previous blanket prohibition on sleeping, which was written when every
  component was ours.
- **Pure Go units stay testable without a cluster.** Config, adapters and resource-name
  handling must not require Kubernetes to run their unit tests.

## Deferred

GKE-specific APIs, Google-managed load balancing, storage and identity, full IAM enforcement,
Cloud Run Jobs, and source builds. Each remains deferred unless separately scoped.
