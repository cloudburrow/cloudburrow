// Package docker adapts container execution: image pull, create, start,
// readiness, logs, port discovery, stop, and removal. It labels every resource
// it creates so that cleanup touches only containers CloudBurrow owns. Nothing
// outside this package knows Docker exists.
//
// Not implemented yet; see issue #9 and docs/architecture.md.
package docker
