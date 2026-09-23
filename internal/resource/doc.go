// Package resource parses and formats Google resource names and applies
// project and location scoping, and holds the shared shapes every name is
// checked against — the project-ID rule among them.
//
// It is the shared place for name syntax rather than the only one: a rule that
// belongs to one service lives with that service (a Cloud Run service ID in the
// Run adapter, a project's creation rule in Resource Manager). What belongs
// here is the syntax more than one service parses, so that collision and
// traversal tests have a single surface to cover for it.
package resource
