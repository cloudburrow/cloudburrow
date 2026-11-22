package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The storage image's tag is the embedded binary's content hash, so the
// binary must be reproducible: built with the commit, its time or the build
// date in it, every `make build` gave `up` a new 113 MB image to build, and
// 121 piled up in one day (#2152). Both places that build it, the Makefile
// and the release, drop the VCS stamp and the build ID.
func TestStorageServerBuildIsReproducible(t *testing.T) {
	for _, f := range []string{"Makefile", ".github/workflows/release.yml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		builds := 0
		for _, block := range regexp.MustCompile(`go build(?:[^\n]*\\\n)*[^\n]*`).FindAllString(string(raw), -1) {
			if !strings.Contains(block, "./cmd/cloudburrow-storage") {
				continue
			}
			builds++
			if !strings.Contains(block, "-buildvcs=false") {
				t.Errorf("%s builds the storage server with VCS stamping:\n%s", f, block)
			}
			if !strings.Contains(block, "-buildid=") && !strings.Contains(block, "STORAGE_LDFLAGS") {
				t.Errorf("%s builds the storage server with a build ID:\n%s", f, block)
			}
			if strings.Contains(block, "$(LDFLAGS)") || strings.Contains(block, "BUILD_DATE") {
				t.Errorf("%s builds the storage server with the build date:\n%s", f, block)
			}
		}
		if builds == 0 {
			t.Errorf("%s does not build ./cmd/cloudburrow-storage", f)
		}
	}
	mk, _ := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if !regexp.MustCompile(`(?m)^STORAGE_LDFLAGS := -s -w -buildid=$`).Match(mk) {
		t.Error("the Makefile's STORAGE_LDFLAGS is not -s -w -buildid=")
	}
}
