# Architecture decision records

Numbered, append-only. A decision is superseded by a new ADR rather than edited in place, so
that the reasoning behind a past choice stays readable. **Superseded text is never rewritten**
— read the superseding ADR for what changed and why.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-independent-go-implementation.md) | Independent Go implementation against official API contracts | **Superseded by [0005](0005-kubernetes-foundation-and-upstream-reuse.md)** |
| [0002](0002-transport-and-routing.md) | One listener per service surface; gRPC multiplexed | **Superseded by [0005](0005-kubernetes-foundation-and-upstream-reuse.md)** |
| [0003](0003-state-and-persistence.md) | Two state modes, single-instance data-directory ownership | **Superseded by [0005](0005-kubernetes-foundation-and-upstream-reuse.md)** |
| [0004](0004-local-access-and-no-authentication.md) | No authentication, loopback by default, admin separated | Accepted — **amended** by [0005](0005-kubernetes-foundation-and-upstream-reuse.md) |
| [0005](0005-kubernetes-foundation-and-upstream-reuse.md) | Kubernetes foundation and reuse-first components | Accepted |
| [0006](0006-iam-policy-surface.md) | IAM policy storage without enforcement, on Secret Manager and Cloud Tasks; ADR-0004's no-authorization clause reaffirmed | Accepted |

ADRs 0001–0003 describe the original single-process, all-custom-Go, direct-Docker design.
They were superseded after the [upstream reuse audit](../upstream-evaluation.md) (#24)
measured what that design cost and what upstream components already provide.

Expected next: the Cloud Run v2 → Knative mapping (#17/#30) and the Cloud Tasks
implementation approach (#15).
