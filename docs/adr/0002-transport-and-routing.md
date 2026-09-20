# ADR-0002: One listener per service surface; gRPC multiplexed

- Status: **Superseded by [ADR-0005](0005-kubernetes-foundation-and-upstream-reuse.md)**
- Date: 2026-09-20
- Issue: #1 (implemented by #5)


> **Superseded.** The text below is preserved unedited as the record of why this
> was decided at the time. See [ADR-0005](0005-kubernetes-foundation-and-upstream-reuse.md)
> for what replaced it.

## Context

CloudBurrow serves several API surfaces across two protocols. The obvious design is a single
port with a reverse-proxy-style router that inspects each request and dispatches by path
prefix and content type. It presents one endpoint to users, which is appealing.

Two facts make it unworkable.

**The Go Cloud Storage client requires HTTP and gRPC on different ports.** The client reads
`STORAGE_EMULATOR_HOST` for its HTTP endpoint and a separate `STORAGE_EMULATOR_HOST_GRPC` for
gRPC, and its source states that "when using a local emulator, HTTP and gRPC must use
different ports." Whatever we might prefer, a client we must satisfy has already decided this.

**REST path spaces collide.** Cloud Storage owns `/storage/v1/`, which is distinctive. Cloud
Run's Admin API owns `/v2/{name=projects/*/locations/*/services/*}` — a bare `/v2/` prefix.
Cloud Tasks v2 and Pub/Sub v1 REST have similarly generic shapes. Sharing one listener would
mean disambiguating generic prefixes by guesswork, and issue #5 explicitly requires explicit
routing over ambiguous fallback. A router that guesses wrong returns a plausible-looking
response from the wrong service, which is worse than a clean failure.

## Decision

Bind **one listener per service surface**, with a configurable port for each.

**gRPC is the single exception** and is multiplexed onto one port, because fully-qualified
protobuf service names (`google.pubsub.v1.Publisher`, `google.cloud.tasks.v2.CloudTasks`) are
globally unique. Dispatch is unambiguous by construction rather than by convention, so the
objection above does not apply.

Default ports, all loopback, all overridable individually or by base offset:

| Port | Surface | Protocol |
|---|---|---|
| 9000 | Control: health, readiness, admin | HTTP |
| 9001 | Cloud Storage JSON API v1 | HTTP |
| 9002 | Cloud Run Admin API v2 | HTTP |
| 9003 | Cloud Tasks REST v2 (if implemented) | HTTP |
| 9004 | Pub/Sub REST v1 (if implemented) | HTTP |
| 9010 | gRPC: Pub/Sub, Cloud Tasks, Storage v2 | gRPC |

Any port may be set to `0` for an OS-assigned free port. This is required, not a convenience:
issue #10 needs parallel test instances, and fixed ports serialize the test suite.

## Consequences

**Costs.** Users configure more than one endpoint, and the getting-started documentation is
correspondingly longer. More listeners mean more startup and shutdown paths to get right, and
a port map to keep documented and in sync.

**Benefits.** No ambiguous routing anywhere, so an unknown method produces a protocol-correct
`UNIMPLEMENTED` or 404 from a known surface rather than a fabricated success from the wrong
one. The Go storage client's split requirement is satisfied by the general design instead of
by a special case. Surfaces can be enabled selectively, so an instance that needs neither
Docker nor Cloud Run simply does not bind that port. Port-0 support makes parallel
compatibility tests straightforward.

**Obligations.** A started instance must report its resolved ports — on stdout and on the
control port — because with port 0 in use the caller cannot know them in advance. The port
table in `architecture.md` §4.1 and this ADR must not drift from the implemented defaults.
