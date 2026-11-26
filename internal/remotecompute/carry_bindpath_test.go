package remotecompute

import (
	"path/filepath"
	"testing"
)

// TestLocalBindPathMapsDockerDesktopsHostMount: Docker Desktop on macOS
// reports a bind mount of /private/var/... as /host_mnt/private/var/...,
// which does not exist on the Mac, so a carry read nothing and the target's
// tar failed with "short read" (TestStandInMoveRoundTripKeepsData on a Mac).
func TestLocalBindPathMapsDockerDesktopsHostMount(t *testing.T) {
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := localBindPath("/host_mnt" + dir); got != dir {
		t.Errorf("localBindPath(/host_mnt%s) = %q, want %q", dir, got, dir)
	}
	if got := localBindPath(dir); got != dir {
		t.Errorf("an existing path changed: %q", got)
	}
	missing := "/host_mnt/no/such/cloudburrow/path"
	if got := localBindPath(missing); got != missing {
		t.Errorf("a path that is nowhere changed: %q", got)
	}
}
