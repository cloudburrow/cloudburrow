package repo

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/licensecheck"
)

// The licence guard. CloudBurrow's own code is MIT-licensed (v0.1.0 to
// v0.5.0 were first published under Apache 2.0), and third-party code keeps its own
// licence, which must never be GPL, LGPL or AGPL in the repository or in
// what the CLI links (AGENTS rules; CONTRIBUTING.md).

// mitBody is the MIT License as opensource.org/license/mit publishes it,
// after its "Copyright <YEAR> <COPYRIGHT HOLDER>" line.
const mitBody = `Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the “Software”), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
`

// copyleft is a licensecheck ID of the GPL family: GPL, LGPL and AGPL in
// every version (not "CNRI-Python-GPL-Compatible", which is permissive).
var copyleft = regexp.MustCompile(`^(A|L)?GPL(-|LR|$)`)

// TestLicenceIsMIT checks LICENSE is the unmodified MIT text under one
// copyright line, that licensecheck reads it as MIT and nothing else, and
// that dependencies.json's project entry says the same.
func TestLicenceIsMIT(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	first, rest, _ := strings.Cut(string(b), "\n\n")
	if !regexp.MustCompile(`^Copyright \(c\) 20\d\d \S.*$`).MatchString(first) {
		t.Errorf("LICENSE's first line is %q, want \"Copyright (c) <year> <holder>\"", first)
	}
	if rest != mitBody {
		t.Errorf("LICENSE after its copyright line is not the MIT License text of opensource.org/license/mit")
	}
	cov := licensecheck.Scan(b)
	if cov.Percent < 95 || len(cov.Match) != 1 || cov.Match[0].ID != "MIT" {
		t.Errorf("licensecheck reads LICENSE as %+v (%.0f%%), want MIT alone", cov.Match, cov.Percent)
	}

	var deps struct {
		Project struct {
			Name, License, LicenseFrom string
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.Project.Name != "cloudburrow" || deps.Project.License != "MIT" || deps.Project.LicenseFrom != "v0.6.0" {
		t.Errorf("dependencies.json project = %+v, want cloudburrow, MIT, from v0.6.0", deps.Project)
	}
}

// isLicenceFile reports whether a file name is a licence or notice text.
func isLicenceFile(path string) bool {
	base := strings.ToUpper(filepath.Base(path))
	for _, p := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "UNLICENSE"} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return strings.Contains(filepath.ToSlash(path), "/licenses/")
}

// copyleftMatches is the GPL-family licences licensecheck finds in b.
func copyleftMatches(b []byte) []string {
	var ids []string
	for _, m := range licensecheck.Scan(b).Match {
		if copyleft.MatchString(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// TestNoCopyleftInThirdParty refuses a GPL, LGPL or AGPL licence text in
// third_party/ and in the console's vendored assets.
func TestNoCopyleftInThirdParty(t *testing.T) {
	root := repoRoot(t)
	scanned := 0
	for _, dir := range []string{"third_party", "internal/console/assets/vendor"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isLicenceFile(p) {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			scanned++
			if ids := copyleftMatches(b); len(ids) > 0 {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s: %v; GPL, LGPL and AGPL code may not be in the repository", rel, ids)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("no licence files found under third_party/")
	}
}

// TestNoCopyleftLinkedIntoTheCLI refuses a module whose licence is GPL,
// LGPL or AGPL among the packages the CLI links.
func TestNoCopyleftLinkedIntoTheCLI(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Dir}}{{end}}{{end}}", "./cmd/cloudburrow")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		mod, dir, ok := strings.Cut(line, "\t")
		if !ok || dir == "" || seen[mod] {
			continue
		}
		seen[mod] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isLicenceFile(e.Name()) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if ids := copyleftMatches(b); len(ids) > 0 {
				t.Errorf("%s (%s): %v; the CLI may not link GPL, LGPL or AGPL code", mod, e.Name(), ids)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("go list named no modules the CLI links")
	}
}

// TestCopyleftComponentsAreNotRedistributed lets dependencies.json list a
// GPL-family component (FFmpeg, MySQL) only as something a user's machine
// pulls or builds and runs as a separate program, never one CloudBurrow
// publishes.
func TestCopyleftComponentsAreNotRedistributed(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	gpl := regexp.MustCompile(`\b(A|L)?GPL\b|GPL-`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			if lic, ok := v["license"].(string); ok && gpl.MatchString(lic) {
				red, _ := v["redistribution"].(string)
				if !strings.HasPrefix(red, "not") {
					t.Errorf("%s: licence %q with redistribution %q; a GPL-family component must be not published or not redistributed", path, lic, red)
				}
			}
			for k, c := range v {
				walk(path+"/"+k, c)
			}
		case []any:
			for _, c := range v {
				walk(path+"[]", c)
			}
		}
	}
	walk("", doc)
}
