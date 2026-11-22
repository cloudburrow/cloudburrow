package repo

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestONNXPackages (#2131): `make onnx-test`, which CI's onnx job runs, tests
// every package the onnx tag changes with -tags onnx: each internal/ package
// with a file under an onnx build constraint, and each internal/ package
// importing one. speechconfig's TestUnserved held only without the tag, and
// nothing ran it with the tag.
func TestONNXPackages(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ONNX_PKGS := ((?:.*\\\n)*.*)$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("Makefile has no ONNX_PKGS")
	}
	listed := strings.Fields(strings.ReplaceAll(m[1], "\\\n", " "))
	covered := func(dir string) bool {
		for _, p := range listed {
			p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
			if tree, ok := strings.CutSuffix(p, "/..."); ok {
				if dir == tree || strings.HasPrefix(dir, tree+"/") {
					return true
				}
			} else if dir == p {
				return true
			}
		}
		return false
	}

	const module = "github.com/cloudburrow/cloudburrow/"
	constraint := regexp.MustCompile(`(?m)^//go:build .*\bonnx\b`)
	tagged := map[string]bool{}
	imports := map[string]map[string]bool{} // dir -> imported dirs, non-test files
	root := filepath.Join("..", "..")
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if constraint.Match(src) {
			tagged[rel] = true
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if dep, ok := strings.CutPrefix(p, module); ok {
				if imports[rel] == nil {
					imports[rel] = map[string]bool{}
				}
				imports[rel][dep] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) == 0 {
		t.Fatal("no internal/ package has an onnx build constraint")
	}
	want := map[string]bool{}
	for dir := range tagged {
		want[dir] = true
	}
	for dir, deps := range imports {
		for dep := range deps {
			if tagged[dep] {
				want[dir] = true
			}
		}
	}
	var missing []string
	for dir := range want {
		if !covered(dir) {
			missing = append(missing, dir)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("Makefile's ONNX_PKGS = %v misses %v, which the onnx tag changes", listed, missing)
	}
}
