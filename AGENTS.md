# Agent instructions

**This file is the canonical instruction source for all AI coding agents working in this
repository.** `CLAUDE.md` refers here and adds nothing of its own. If you change how agents
should work, change this file.

CloudBurrow is a local Google Cloud emulator. Its value depends entirely on behaving the way
the real services behave, so the overriding rule is: **never claim support you have not
demonstrated.** An emulator that quietly returns plausible responses is worse than one that
returns a clear `UNIMPLEMENTED`, because the first sends the user to debug their own correct
code.

---

## Before you write code

1. **Read the issue you are working on, in full, including its acceptance criteria.**
2. **Read every issue it depends on.** Dependencies are listed in the issue body. If a
   dependency is unmerged, either build on its branch or stop and say so — do not reimplement
   its work.
3. **Read [`docs/architecture.md`](docs/architecture.md)** for module boundaries, routing,
   state, and auth posture, and [`docs/compatibility.md`](docs/compatibility.md) for the
   current honest status of every operation.
4. **Read the relevant ADRs** in [`docs/adr/`](docs/adr/). They record why things are the way
   they are; a decision you disagree with gets a new ADR, not a silent reversal.
5. **Use the official API definitions as the contract** —
   [googleapis/googleapis](https://github.com/googleapis/googleapis) and the published service
   documentation. Not another emulator's behavior, and not recollection of how an API works.

## Scope

**One issue, one reviewable PR.** Stay inside the issue's acceptance criteria.

- Work that turns out to be independent gets its own issue and its own PR. Say so rather than
  folding it in.
- Do not fix unrelated problems you notice in passing. Note them; open an issue.
- Do not add dependencies, restructure packages, or change decisions from an ADR as a side
  effect of unrelated work.
- Respect the package boundaries in `architecture.md` §3. In particular: **services under
  `internal/service/` must not import each other.** Cross-service needs go through a narrow
  interface declared by the consumer and wired in `internal/lifecycle`.

## Testing

**Behavioral tests, not tests of your own implementation details.** A test that mirrors the
structure of the code it tests passes for the wrong reasons and fails whenever the code is
refactored.

- Table-driven tests for protocol serialization and failure cases, written against the
  published API contract.
- **An operation is only "supported" when an official Google SDK drives it.** Curl, a
  handwritten HTTP request, or an internal unit test is not evidence of compatibility — it
  tests our understanding of the API, not the client's. This is the promotion rule in
  `docs/compatibility.md` and it is not negotiable.
- **Never sleep to wait for time to pass.** Use the injected clock (`internal/sched`) and
  advance virtual time. A sleeping test is slow, flaky, and usually wrong.
- Tests must use unique project and resource IDs and ephemeral ports so they can run in
  parallel.
- Cover the failure paths: malformed names, missing resources, duplicates, invalid arguments,
  cancellation, and restart recovery. Error mapping is part of the contract.
- Separate tests that need Docker from tests that do not, and document both commands.

## Reporting honestly

This is the part that matters most, and the part most easily skipped under pressure.

- **If something does not work, say so in the PR.** Partial implementations are welcome;
  partial implementations described as complete are not.
- **Update `docs/compatibility.md` in the same PR** that changes what is supported. Move rows
  off `Planned` only with SDK-driven test evidence. A `Partial` row must name its gap.
- **Unimplemented means `UNIMPLEMENTED`.** Never return a fabricated success, an empty list
  standing in for a real result, or a plausible-looking stub, to make a client proceed.
- **Do not weaken a test to make it pass.** If a test fails, either the code is wrong or the
  test encoded a wrong expectation — decide which, and say which in the PR.
- **Do not claim you ran something you did not run.** Paste real command output.

## Pull requests

Include:

- **What is implemented**, and explicitly what is not.
- **Exact validation commands and their real output.**
- **Compatibility limitations** discovered along the way.
- `Closes #N` — **after confirming the actual issue number**, which is not always the number
  in the issue title.

Move the project board item to Done only after merge and acceptance.

## Commands

See the [Makefile](Makefile). `make check` runs what CI runs.

| Command | Purpose |
|---|---|
| `make build` | Build the binary into `bin/` |
| `make fmt` | Format; `make fmt-check` fails instead of writing |
| `make vet` | `go vet` |
| `make test` | Unit tests |
| `make test-race` | Unit tests with the race detector |
| `make test-integration` | Tests requiring Docker (build tag `integration`) |
| `make test-compat` | Official-SDK compatibility tests (build tag `compat`) |
| `make check` | `fmt-check` + `vet` + `test-race` |

## Conventions

- Go, pinned at the minimum version in `go.mod`. Standard library first; a new dependency
  needs a justification in the PR.
- Generated code is never edited by hand and stays segregated from handwritten behavior
  (issue #4).
- Errors are wrapped with `%w` and carry enough context to locate the failure.
- Exported identifiers are documented. Comments explain *why*, not *what*.
- No credentials, tokens, or real project identifiers in the repository — including in tests
  and fixtures.
