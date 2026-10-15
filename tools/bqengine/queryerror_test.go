package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The emulator's patches, applied in sources.json's order to the pinned
// module, leave Repository.Query returning the error a query fails with as
// it runs as it is, without "failed to scan rows: " before it (#1230:
// ERROR('boom') must fail with "boom"). The module is read from the Go
// module cache; the test is skipped when it is not there.
func TestQueryErrorKeepsItsMessage(t *testing.T) {
	src, err := loadSources("../../third_party/bigquery-emulator")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skip("no Go module cache:", err)
	}
	for _, p := range src.Patched {
		if p.Path != src.Emulator.Path {
			continue
		}
		mod := filepath.Join(strings.TrimSpace(string(cache)), "github.com/goccy/bigquery-emulator@"+p.Version)
		if _, err := os.Stat(mod); err != nil {
			t.Skip("the pinned emulator is not in the module cache")
		}
		root := t.TempDir()
		if err := copyTree(mod, root); err != nil {
			t.Fatal(err)
		}
		for _, name := range p.Patches {
			b, err := os.ReadFile(filepath.Join("../../third_party/bigquery-emulator", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := applyPatch(root, b); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		got := read(t, filepath.Join(root, "internal/contentdata/repository.go"))
		if strings.Contains(got, `"failed to scan rows: %w"`) {
			t.Error("Repository.Query still wraps the rows' error in \"failed to scan rows: \"")
		}
		return
	}
	t.Fatal("sources.json does not patch the emulator")
}
