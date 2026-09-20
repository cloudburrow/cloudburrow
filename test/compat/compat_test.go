//go:build compat

package compat

import "testing"

// TestHarnessNotImplemented fails deliberately rather than passing vacuously.
//
// A compatibility suite that reports success while containing no tests is the
// exact failure this project is trying to avoid: it would let `make
// test-compat` go green and imply SDK compatibility that has never been
// demonstrated. Issue #10 replaces this with a real harness.
func TestHarnessNotImplemented(t *testing.T) {
	t.Fatal("SDK compatibility harness is not implemented yet (issue #10); " +
		"no operation may be promoted off Planned in docs/compatibility.md")
}
