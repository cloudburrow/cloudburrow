// Package compat holds black-box compatibility tests that drive CloudBurrow
// through official Google Cloud SDKs.
//
// These tests are the only evidence that promotes an operation off "Planned"
// in docs/compatibility.md. Tests written against handwritten HTTP requests
// belong elsewhere: they verify our reading of an API, not a real client's.
//
// Tests here are guarded by the "compat" build tag and run via
// `make test-compat`. The harness itself is issue #10; no tests exist yet.
package compat
