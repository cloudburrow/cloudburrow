// Package blob stores object payloads, separately from metadata, supporting
// ranged reads and resumable writes. Object names are untrusted input and must
// never resolve outside the data directory.
//
// Not implemented yet; see issue #7 and docs/architecture.md.
package blob
