# ADR-0001: Independent Go implementation against official API contracts

- Status: Accepted
- Date: 2026-09-20
- Issue: #1

## Context

CloudBurrow needs server implementations of four Google Cloud APIs. There are three ways to
get them, and the choice constrains everything downstream.

1. **Fork or vendor existing emulators.** `fake-gcs-server` covers much of the Cloud Storage
   JSON API. The `gcloud beta emulators` Pub/Sub emulator is a supported Java implementation.
   Neither Cloud Tasks nor Cloud Run has an official local emulator at all.
2. **Orchestrate existing emulators** as child processes behind one CLI.
3. **Implement independently in Go**, using the published API definitions as the contract.

Option 1 gives the fastest start on two of four services, but yields a codebase in two
languages with two upstreams, inherits each project's behavioral quirks as de facto contract,
and still leaves Cloud Tasks and Cloud Run — the two services that actually require
scheduling and container execution — to be written from scratch. Option 2 avoids the fork but
makes process lifecycle, state reset, and the shared clock nearly impossible: a child
emulator cannot have its time advanced by our tests, and controllable time is a hard
requirement for testing retries without sleeping.

## Decision

Implement all four services independently in Go, taking protocol definitions from
[googleapis/googleapis](https://github.com/googleapis/googleapis) at a pinned revision and
behavioral expectations from the published service documentation.

A corollary that carries real weight: **the contract is the proto and the published API
reference, not another emulator's observed behavior.** A divergence from another emulator is
not automatically a bug. A divergence from an official client library always is.

## Consequences

**Accepted costs.** More code before anything works, and Cloud Storage in particular is a
large surface — resumable uploads, rewrite tokens, preconditions and compose are each
non-trivial. We rebuild behavior other projects already have. The first release is slower.

**What we get.** One language and one build. A single injected clock across all services,
which makes deterministic retry and redelivery tests possible at all. Uniform state
ownership, so reset and restart are coherent rather than four separate mechanisms. Uniform
error mapping. No upstream licensing or divergence to manage, and no inherited quirks
promoted to contract by accident.

**Obligations this creates.**

- The compatibility matrix is mandatory, not decorative. Writing our own implementation means
  our belief about an API is untested until an official client exercises it, so the
  SDK-driven promotion rule in `compatibility.md` is the only thing separating support from
  assumption.
- Generated code stays segregated and unedited (issue #4), so a contract refresh is a
  regeneration rather than a merge.
- Upstream licenses and notices for `googleapis` definitions must be preserved.

## Alternatives reconsidered later

Vendoring a Cloud Storage implementation remains plausible if the JSON API data plane proves
disproportionately expensive. It would be a scoped dependency for one service, not a change
to this decision, and would require its own ADR.
