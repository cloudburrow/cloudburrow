# ADR-0003: Two state modes, single-instance data-directory ownership

- Status: **Superseded by [ADR-0005](0005-kubernetes-foundation-and-upstream-reuse.md)**
- Date: 2026-09-20
- Issue: #1 (implemented by #7)


> **Superseded.** The text below is preserved unedited as the record of why this
> was decided at the time. See [ADR-0005](0005-kubernetes-foundation-and-upstream-reuse.md)
> for what replaced it.

## Context

CloudBurrow has two user groups with incompatible needs. Automated tests want isolation,
speed, and no residue: a test that leaves state behind contaminates the next one. Developers
running a local stack want their buckets and topics to survive a restart, or the emulator is
useless for day-to-day work.

Object payloads and resource metadata also have genuinely different shapes. Metadata is many
small records needing atomic multi-key updates. Payloads are arbitrarily large byte streams
needing ranged reads and resumable writes. One mechanism serving both would serve one badly.

## Decision

**Two modes, selected at startup.**

- *Memory mode* — all metadata and payloads in process; nothing durable is written.
  The default for the compatibility harness.
- *Durable mode* — metadata in an embedded store, payloads as files, under one data
  directory. The default for `cloudburrow up`.

Both satisfy one storage interface, so services are written once and tested in both.

**Metadata and payloads are separate subsystems**: `internal/store` for metadata,
`internal/blob` for payloads. The embedded metadata store is chosen in issue #7 under its own
ADR; it must support atomic multi-key commits and single-writer ownership.

**A data directory is owned by exactly one running instance**, claimed by a lock. A second
instance pointed at a live directory refuses to start.

## Consequences

**The ownership rule is not negotiable.** Two instances sharing a data directory produce
interleaved metadata writes and corrupted objects, and the resulting failures surface later
as unrelated, unreproducible bugs. Refusing to start is hostile in the moment and correct in
every other respect. It is also easy to hit accidentally — a forgotten background instance,
or a test suite that defaults to the same path — so the error message must name the holding
instance and the path.

**Atomicity spans both subsystems.** An object write touches payload bytes and metadata, and
a crash in between must not leave a readable object with the wrong bytes or a stale
generation. Metadata is committed only after payload bytes are durable; a payload with no
committed metadata is unreferenced garbage, collected on the next start. The reverse ordering
would produce readable objects with wrong content, which is the failure mode that actually
misleads users.

**Object names are untrusted input.** They are attacker-shaped even in a development tool:
path traversal, encoded separators, absolute paths, and Unicode normalization must not
produce a write outside the data directory. Payload files are therefore stored under
content-derived or escaped names, never under the raw object name.

**Costs.** Every service is implemented against an abstraction rather than against a concrete
store, and both modes need test coverage. Memory mode can hide durability bugs, so
restart-recovery tests must run in durable mode specifically.

**No format stability before 1.0.** The on-disk layout may change between releases without
migration. CloudBurrow is a development tool and not a backup target; the README and release
notes must say so plainly rather than letting users infer durability guarantees from the word
"durable".
