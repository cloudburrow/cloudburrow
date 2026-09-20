# CLAUDE.md

Instructions for AI coding agents in this repository live in a single canonical file:

## → [AGENTS.md](AGENTS.md)

Read it before making any change. It is not duplicated here, because two copies of a rule
become two different rules.

Quick orientation, all of which AGENTS.md covers properly:

- Read your issue and its dependencies first, then `docs/architecture.md`.
- One issue, one PR. Do not widen scope.
- Never claim support that an official Google SDK has not demonstrated —
  see the promotion rule in `docs/compatibility.md`.
- Unimplemented behavior returns `UNIMPLEMENTED`, never a plausible stub.
- `make check` before opening a PR.
