package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinuxCLILinksNoPurego (#1729): the CGO_ENABLED=0 Linux CLI links no
// github.com/ebitengine/purego. With cgo off, purego links its fakecgo
// runtime shim and imports libc dynamically, so the release CLIs, built
// CGO_ENABLED=0 and meant to be statically linked (docs/install.md), would
// come out "dynamically linked" while the release check, which reads only
// the recorded CGO_ENABLED=0, still passed. gen2brain/avif and
// gen2brain/heic import it to load a system libavif or libheif; CloudBurrow
// vendors only avif's WebAssembly path (internal/avif) for that reason.
func TestLinuxCLILinksNoPurego(t *testing.T) {
	if testing.Short() {
		t.Skip("lists the CLI's dependencies with the go command")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		cmd := exec.Command("go", "list", "-deps", "./cmd/cloudburrow")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("linux/%s: go list -deps: %v\n%s", arch, err, out)
		}
		deps := strings.Fields(string(out))
		if len(deps) < 100 {
			t.Fatalf("linux/%s: go list -deps listed only %d packages", arch, len(deps))
		}
		for _, p := range deps {
			if p == "github.com/ebitengine/purego" || strings.HasPrefix(p, "github.com/ebitengine/purego/") {
				t.Errorf("linux/%s: the CGO_ENABLED=0 CLI links %s, which makes it dynamically linked", arch, p)
			}
		}
	}
}
