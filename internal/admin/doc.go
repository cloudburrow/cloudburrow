// Package admin implements the local-only control API: seed, reset, and event
// inspection. It is served on the control port and refused on service ports,
// because reset destroys data and must be unreachable from the container
// network.
//
// Not implemented yet; see issue #18 and docs/architecture.md.
package admin
