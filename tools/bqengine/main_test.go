package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const original = "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"

// A description before the diff is ignored; hunks change, insert and
// create files, with git's header lines between them.
func TestApplyPatch(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), original)
	patch := `A description of the change, which is not part of the diff.

diff --git a/a.txt b/a.txt
index 1111111..2222222 100644
--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
@@ -5,2 +5,4 @@
 five
+five and a half
+
 six
--- /dev/null
+++ b/dir/new.txt
@@ -0,0 +1,2 @@
+hello
+world
`
	if err := applyPatch(root, []byte(patch)); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, filepath.Join(root, "a.txt")), "one\nTWO\nthree\nfour\nfive\nfive and a half\n\nsix\nseven\n"; got != want {
		t.Errorf("a.txt =\n%q\nwant\n%q", got, want)
	}
	if got := read(t, filepath.Join(root, "dir/new.txt")); got != "hello\nworld\n" {
		t.Errorf("new.txt = %q", got)
	}
}

// Strict: a hunk whose context is not exactly at its line fails, even when
// the same lines exist elsewhere, and so does creating a file that exists.
func TestApplyPatchIsStrict(t *testing.T) {
	for name, patch := range map[string]string{
		"moved context": "--- a/a.txt\n+++ b/a.txt\n@@ -2,2 +2,2 @@\n one\n-two\n+TWO\n",
		"changed line":  "--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-TWO\n+2\n",
		"past the end":  "--- a/a.txt\n+++ b/a.txt\n@@ -7,2 +7,2 @@\n seven\n-eight\n+8\n",
		"file exists":   "--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1 @@\n+x\n",
		"no such file":  "--- a/b.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-x\n+y\n",
		"escape":        "--- /dev/null\n+++ b/../x.txt\n@@ -0,0 +1 @@\n+x\n",
		"delete":        "--- a/a.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-one\n",
		"rename":        "--- a/a.txt\n+++ b/c.txt\n@@ -1 +1 @@\n-one\n+1\n",
		"truncated":     "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n",
		"no newline":    "--- a/a.txt\n+++ b/a.txt\n@@ -7 +7 @@\n-seven\n+7\n\\ No newline at end of file\n",
		"not a diff":    "just words\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, filepath.Join(root, "a.txt"), original)
			if err := applyPatch(root, []byte(patch)); err == nil {
				t.Errorf("applied:\n%s", patch)
			}
			if read(t, filepath.Join(root, "a.txt")) != original {
				t.Errorf("a.txt changed by a failed patch")
			}
		})
	}
}

// The repository's patches parse, name only files under their module, and
// each carries a header naming CloudBurrow's issue and the licence.
func TestRepositoryPatches(t *testing.T) {
	src, err := loadSources("../../third_party/bigquery-emulator")
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Patched) == 0 {
		t.Fatal("sources.json patches nothing")
	}
	for _, p := range src.Patched {
		for _, name := range p.Patches {
			b, err := os.ReadFile(filepath.Join("../../third_party/bigquery-emulator", name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parsePatch(b); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			header := string(b[:strings.Index(string(b), "\n--- ")])
			for _, want := range []string{"cloudburrow/cloudburrow#", "MIT License", p.Path + " " + p.Version} {
				if !strings.Contains(header, want) {
					t.Errorf("%s: the header does not name %q", name, want)
				}
			}
		}
	}
}

// sources.json refuses what would build something other than it says.
func TestLoadSourcesRefuses(t *testing.T) {
	good := Sources{
		Emulator: Module{Path: "example.com/emu", Version: "v1.0.0", Sum: "h1:emu="},
		Patched: []Patched{{Module: Module{Path: "example.com/dep", Version: "v0.1.0", Sum: "h1:dep="},
			Dir: "build/dep", Patches: []string{"patches/dep/0001.patch"}}},
		Version: "v1.0.0-cloudburrow.1",
	}
	for name, tt := range map[string]struct {
		edit  func(*Sources)
		gosum string
	}{
		"ok":            {func(*Sources) {}, "example.com/emu v1.0.0 h1:emu=\n"},
		"not in go.sum": {func(*Sources) {}, "example.com/emu v1.0.0 h1:other=\n"},
		"no patches":    {func(s *Sources) { s.Patched[0].Patches = nil }, "example.com/emu v1.0.0 h1:emu=\n"},
		"no sum":        {func(s *Sources) { s.Patched[0].Sum = "" }, "example.com/emu v1.0.0 h1:emu=\n"},
		"dir outside":   {func(s *Sources) { s.Patched[0].Dir = "../dep" }, "example.com/emu v1.0.0 h1:emu=\n"},
	} {
		t.Run(name, func(t *testing.T) {
			s := good
			s.Patched = []Patched{good.Patched[0]}
			tt.edit(&s)
			dir := t.TempDir()
			b, _ := json.Marshal(s)
			write(t, filepath.Join(dir, "sources.json"), string(b))
			write(t, filepath.Join(dir, "go.sum"), tt.gosum)
			_, err := loadSources(dir)
			if (name == "ok") != (err == nil) {
				t.Errorf("loadSources = %v", err)
			}
		})
	}
}

// dependencies.json's bigqueryEmulator entry records the same pins as
// sources.json, so the inventory says what is built.
func TestDependenciesRecordTheSources(t *testing.T) {
	src, err := loadSources("../../third_party/bigquery-emulator")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	var e struct {
		Version, Module, Sum, Commit, BuiltVersion string
		Patched                                    []struct{ Module, Version, Sum, Commit string }
	}
	if err := json.Unmarshal(d.Components["emulatorBackends"]["bigqueryEmulator"], &e); err != nil {
		t.Fatal(err)
	}
	if "v"+e.Version != src.Emulator.Version || e.Module != src.Emulator.Path || e.Sum != src.Emulator.Sum || e.Commit != src.Emulator.Commit || e.BuiltVersion != src.Version {
		t.Errorf("dependencies.json bigqueryEmulator = %+v; sources.json has %+v, version %s", e, src.Emulator, src.Version)
	}
	if len(e.Patched) != len(src.Patched) {
		t.Fatalf("dependencies.json records %d patched modules, sources.json %d", len(e.Patched), len(src.Patched))
	}
	for i, p := range src.Patched {
		got := e.Patched[i]
		if got.Module != p.Path || got.Version != p.Version || got.Sum != p.Sum || got.Commit != p.Commit {
			t.Errorf("dependencies.json patched[%d] = %+v; sources.json has %+v", i, got, p.Module)
		}
	}
}

// -prebuilt accepts binaries built from these sources by any go command,
// and refuses a missing one, one built from other sources, and one whose
// stamp names another architecture (#1087).
func TestCheckPrebuilt(t *testing.T) {
	if got := toolchain("go version go1.27.1 darwin/arm64\n"); got != "go1.27.1/darwin/arm64" {
		t.Errorf("toolchain = %q", got)
	}
	setup := func(t *testing.T, stamps map[string]string) string {
		out := t.TempDir()
		for name, stamp := range stamps {
			write(t, filepath.Join(out, name), "gz")
			write(t, filepath.Join(out, name+".inputs"), stamp+"\n")
		}
		return out
	}
	good := map[string]string{
		"bigquery-emulator-linux-amd64.gz":  "src go1.27.1/linux/amd64 amd64",
		"bigquery-emulator-linux-arm64.gz":  "src go1.26.3/linux/amd64 arm64",
		"bigquery-emulator-licenses.txt.gz": "src go1.27.1/linux/amd64",
	}
	if err := checkPrebuilt(setup(t, good), "src", []string{"amd64", "arm64"}); err != nil {
		t.Errorf("built from these sources: %v", err)
	}
	for name, edit := range map[string]func(map[string]string){
		"other sources": func(m map[string]string) { m["bigquery-emulator-linux-arm64.gz"] = "old go1.27.1/linux/amd64 arm64" },
		"other arch":    func(m map[string]string) { m["bigquery-emulator-linux-arm64.gz"] = "src go1.27.1/linux/amd64 amd64" },
		"no licences":   func(m map[string]string) { delete(m, "bigquery-emulator-licenses.txt.gz") },
		"old stamp":     func(m map[string]string) { m["bigquery-emulator-linux-amd64.gz"] = "0123abcd amd64" },
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range good {
				m[k] = v
			}
			edit(m)
			if err := checkPrebuilt(setup(t, m), "src", []string{"amd64", "arm64"}); err == nil {
				t.Error("accepted")
			}
		})
	}
	if err := checkPrebuilt(t.TempDir(), "src", []string{"amd64"}); err == nil {
		t.Error("accepted an empty directory")
	}
}
