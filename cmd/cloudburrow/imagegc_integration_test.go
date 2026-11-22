//go:build integration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// TestStopLeavesNoInstanceImages is #2152 end to end: every `up` built a
// new 113 MB storage image and nothing removed it. One instance is started,
// stopped, and started and stopped again. Each up must use the one storage
// image whose tag is the embedded binary's content hash, and each stop must
// leave no image or volume labelled for the instance behind.
func TestStopLeavesNoInstanceImages(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "cloudburrow")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	docker := func(args ...string) []string {
		t.Helper()
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
		}
		return strings.Fields(string(out))
	}
	arch := strings.Join(docker("version", "--format", "{{.Server.Arch}}"), "")
	binary, err := storageimage.Binary(arch)
	if err != nil {
		t.Skipf("no embedded storage server for linux/%s (make storage-binaries): %v", arch, err)
	}
	tag := storageimage.Tag(binary)
	// Another instance or worktree may already have built this very image;
	// then this instance reuses it, builds none, and must not remove it.
	preexisting := len(docker("image", "ls", "-q", tag)) > 0

	name := fmt.Sprintf("gc-%d", time.Now().UnixNano()%1e6)
	// Below Linux's ephemeral range, as TestTwoInstancesRunSideBySideWithAPortBase
	// explains; CLOUDBURROW_TEST_PORT_BASE pins it on a shared machine.
	base := 20000 + int(time.Now().UnixNano()%110)*100
	if v, err := strconv.Atoi(os.Getenv("CLOUDBURROW_TEST_PORT_BASE")); err == nil {
		base = v
	}
	flags := []string{"--name", name, "--state-dir", t.TempDir(), "--port-base", strconv.Itoa(base),
		"--services", "storage", "--mode", "ephemeral"}
	t.Cleanup(func() {
		_ = exec.Command(bin, append([]string{"delete"}, flags...)...).Run()
		_ = exec.Command("kind", "delete", "cluster", "--name", dockergc.ClusterName(name)).Run()
	})
	cb := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", args[0], err, out)
		}
		return string(out)
	}
	labelled := func(extra ...string) []string {
		return docker(append([]string{"image", "ls", "-q", "--no-trunc", "--filter", "label=" + dockergc.Label(name)}, extra...)...)
	}

	var firstID string
	for round := 1; round <= 2; round++ {
		cb("up", "--detach", "--detach-timeout", "15m")
		ids := docker("image", "ls", "-q", "--no-trunc", tag)
		if len(ids) != 1 {
			t.Fatalf("up %d: %d images tagged %s, want exactly one", round, len(ids), tag)
		}
		storage := labelled("--filter", "reference="+storageimage.Repository)
		switch {
		case preexisting && len(storage) != 0:
			t.Errorf("up %d rebuilt the storage image another instance had built: %v", round, storage)
		case !preexisting && (len(storage) != 1 || storage[0] != ids[0]):
			t.Errorf("up %d: storage images labelled for %s = %v, want exactly %s", round, name, storage, ids[0])
		}
		if round == 1 {
			firstID = ids[0]
		} else if preexisting && ids[0] != firstID {
			t.Errorf("the shared storage image changed between ups: %s, then %s", firstID, ids[0])
		}

		stop := cb("stop")
		if left := labelled(); len(left) != 0 {
			t.Errorf("stop %d left %d image(s) labelled %s: %v\n%s", round, len(left), dockergc.Label(name), left, stop)
		}
		if vols := docker("volume", "ls", "-q", "--filter", "label="+dockergc.Label(name)); len(vols) != 0 {
			t.Errorf("stop %d left volumes labelled %s: %v", round, dockergc.Label(name), vols)
		}
		if gone := len(docker("image", "ls", "-q", tag)) == 0; gone == preexisting {
			t.Errorf("after stop %d the storage image is gone=%v; it was there before the test: %v", round, gone, preexisting)
		}
	}
}
