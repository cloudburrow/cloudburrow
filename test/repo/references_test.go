// Package repo holds checks over the repository's own files.
package repo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retired names an upstream component that no longer backs anything:
// fake-gcs-server, replaced by CloudBurrow's own Cloud Storage server
// (#485, #519).
var retired = regexp.MustCompile(`(?i)fake-?gcs|fsouza`)

// TestNoFakeGCSServerReferences (#519): no tracked file outside docs/ names
// fake-gcs-server. The docs keep its history (ADRs, the upstream evaluation,
// measurements); code, configuration, manifests and tests do not.
func TestNoFakeGCSServerReferences(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	self := "test/repo/references_test.go"
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f == "" || f == self || strings.HasPrefix(f, "docs/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue // deleted in the working tree
		}
		if bytes.IndexByte(b, 0) >= 0 {
			continue // binary
		}
		for i, line := range strings.Split(string(b), "\n") {
			if retired.MatchString(line) {
				t.Errorf("%s:%d names fake-gcs-server: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
