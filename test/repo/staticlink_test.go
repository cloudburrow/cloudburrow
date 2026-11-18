package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinuxBinariesNeverImportPurego (#1722): github.com/ebitengine/purego
// makes a CGO_ENABLED=0 Linux binary dynamically linked against libc (it
// asks for an ELF interpreter to dlopen through), so the released CLI and
// the storage image's binary would no longer run on a system without glibc.
// go.264 imports it for its Linux hardware encoders, which is why
// third_party/go264 is go.264 copied without them (a replace directive
// would break go install ...@latest); AVIF is decoded with
// gen2brain/gav1d (#1736), pure Go, rather than gen2brain/avif, whose
// loader of a system libavif imports it. No package these Linux builds
// compile may import it, without any build tag.
func TestLinuxBinariesNeverImportPurego(t *testing.T) {
	if testing.Short() {
		t.Skip("lists the dependencies of the CLI")
	}
	root := filepath.Join("..", "..")
	for _, arch := range []string{"amd64", "arm64"} {
		for _, pkg := range []string{"./cmd/cloudburrow", "./cmd/cloudburrow-storage"} {
			cmd := exec.Command("go", "list", "-deps", pkg)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOFLAGS=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps %s (linux/%s): %v\n%s", pkg, arch, err, out)
			}
			for _, dep := range strings.Fields(string(out)) {
				if dep == "github.com/ebitengine/purego" || strings.HasPrefix(dep, "github.com/ebitengine/purego/") {
					t.Errorf("%s for linux/%s imports %s, which links libc dynamically; find the importer with go mod why -m github.com/ebitengine/purego", pkg, arch, dep)
					break
				}
			}
		}
	}
}
