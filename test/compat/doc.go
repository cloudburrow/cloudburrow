// Package compat holds black-box compatibility tests that drive CloudBurrow
// through official Google Cloud SDKs.
//
// These tests are the only evidence that promotes an operation off "Planned"
// in docs/compatibility.md. Tests written against handwritten HTTP requests
// belong elsewhere: they verify our reading of an API, not a real client's.
//
// Tests here are guarded by the "compat" build tag and run via
// `make test-compat`. The exception is target_test.go, the unit tests for
// the check that the CLI and gcloud reach the harness's instance (#927),
// which need no instance and run with every `go test ./...`.
package compat
