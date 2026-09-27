package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// maxTrackedBytes is the largest file the repository tracks. Nothing here
// needs more; the build outputs that were committed (a 20 MB coverage binary
// and 38 MB of embedded storage servers) were each far over it (#585).
const maxTrackedBytes = 5 << 20

// TestNoLargeTrackedFiles (#585): every clone pays for a committed binary,
// it is useless on other platforms, and one built on a developer's machine
// lands on main unreviewed. Build outputs are made by `make build` and the
// release, and ignored.
func TestNoLargeTrackedFiles(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			continue // deleted in the working tree
		}
		if info.Size() > maxTrackedBytes {
			t.Errorf("%s is %d bytes; a tracked file over %d is a build output or a fixture that should be generated", f, info.Size(), maxTrackedBytes)
		}
	}
}
