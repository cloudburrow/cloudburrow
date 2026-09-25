package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Tink is a test-only dependency: the compat suite drives Cloud KMS through
// tink-go-gcpkms as a client, and the server's crypto is the standard
// library (#416; docs/upstream-evaluation.md, Cloud KMS amendment). The
// binary must never link it.
func TestTheBinaryDoesNotLinkTink(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "tink-crypto") {
			t.Errorf("cmd/cloudburrow links %s", pkg)
		}
	}
}
