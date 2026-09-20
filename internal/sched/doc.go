// Package sched provides cancellable background work, due-time scheduling, and
// bounded retry and backoff over an injected clock, so that tests advance
// virtual time instead of sleeping. Retry policy is per service, not shared.
//
// Not implemented yet; see issue #8 and docs/architecture.md.
package sched
