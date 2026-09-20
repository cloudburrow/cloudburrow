# Architecture decision records

Numbered, append-only. A decision is superseded by a new ADR rather than edited in place, so
that the reasoning behind a past choice stays readable.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-independent-go-implementation.md) | Independent Go implementation against official API contracts | Accepted |
| [0002](0002-transport-and-routing.md) | One listener per service surface; gRPC multiplexed | Accepted |
| [0003](0003-state-and-persistence.md) | Two state modes, single-instance data-directory ownership | Accepted |
| [0004](0004-local-access-and-no-authentication.md) | No authentication, loopback by default, admin separated | Accepted |

Expected next: the embedded metadata store choice (issue #7) and the code-generation
strategy (issue #4).
