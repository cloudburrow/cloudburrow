package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestVendoredAVIFMatchesProvenance (#1729): every file of internal/avif is
// the one third_party/avif/PROVENANCE.md and dependencies.json record, by
// SHA-256, and the package holds no file they do not record.
func TestVendoredAVIFMatchesProvenance(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "third_party", "avif", "PROVENANCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := regexp.MustCompile("(?m)^\\| `([^`]+)` \\| `([0-9a-f]{64})` \\| `([0-9a-f]{64})` \\|").FindAllStringSubmatch(string(raw), -1)
	if len(rows) < 8 {
		t.Fatalf("PROVENANCE.md records %d files, want the 8 vendored", len(rows))
	}
	recorded := map[string]bool{}
	for _, r := range rows {
		name, want := r[1], r[3]
		recorded[name] = true
		b, err := os.ReadFile(filepath.Join(root, "internal", "avif", filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("internal/avif/%s: SHA-256 %s, PROVENANCE.md records %s", name, got, want)
		}
	}
	var deps struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	raw, err = os.ReadFile(filepath.Join(root, "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	for _, group := range deps.Components {
		if c, ok := group["goAVIFVendored"]; ok {
			var v struct {
				Files map[string]string `json:"files"`
			}
			if err := json.Unmarshal(c, &v); err != nil {
				t.Fatal(err)
			}
			files = v.Files
		}
	}
	if len(files) != len(rows) {
		t.Errorf("dependencies.json's goAVIFVendored records %d files, PROVENANCE.md %d", len(files), len(rows))
	}
	for _, r := range rows {
		if got := files["internal/avif/"+r[1]]; got != "sha256:"+r[3] {
			t.Errorf("dependencies.json records internal/avif/%s as %q, PROVENANCE.md sha256:%s", r[1], got, r[3])
		}
	}
	err = filepath.WalkDir(filepath.Join(root, "internal", "avif"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(filepath.Join(root, "internal", "avif"), path)
		if err != nil {
			return err
		}
		if !recorded[filepath.ToSlash(rel)] {
			t.Errorf("internal/avif/%s is not recorded in third_party/avif/PROVENANCE.md", filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
