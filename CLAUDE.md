# CLAUDE.md

Instructions for AI coding agents in this repository live in a single canonical file:

## → [AGENTS.md](AGENTS.md)

Read it before making any change. It is not duplicated here, because two copies of a rule
become two different rules.

Quick orientation, all of which AGENTS.md covers properly:

- **The architecture was revised (#25): Kubernetes foundation, reuse-first, Knative for Cloud
  Run. ADR-0001 to ADR-0003 are superseded — read
  [ADR-0005](docs/adr/0005-kubernetes-foundation-and-upstream-reuse.md) before implementing.**
- Read your issue and its dependencies first, then `docs/architecture.md`.
- Reuse upstream by default; building requires a demonstrated gap with evidence.
- One issue, one PR. Do not widen scope.
- Never claim support that an official Google SDK has not demonstrated —
  see the promotion rule in `docs/compatibility.md`.
- Unimplemented behavior returns `UNIMPLEMENTED`, never a plausible stub.
- An inherited upstream limitation is still our limitation — record it in `compatibility.md`.
- `make check` before opening a PR.
