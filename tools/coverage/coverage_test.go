package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRoot is a repository with the real registry and one compat file.
func fixtureRoot(t *testing.T, compat string) string {
	t.Helper()
	root := t.TempDir()
	reg, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	for dir, files := range map[string]map[string][]byte{
		"tools/coverage": {"registry.json": reg},
		"test/compat":    {"x_test.go": []byte("//go:build compat\n\npackage compat\n\nimport \"testing\"\n" + compat)},
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, b := range files {
			if err := os.WriteFile(filepath.Join(root, dir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func rowOf(t *testing.T, pages []Page, method string) Row {
	t.Helper()
	for _, p := range pages {
		for _, r := range p.Rows {
			if r.Method == method {
				return r
			}
		}
	}
	t.Fatalf("no row for %s", method)
	return Row{}
}

func TestAnAnnotatedTestVerifiesAndItsAbsenceDoesNot(t *testing.T) {
	const m = "google.cloud.tasks.v2.CloudTasks/ListTasks"
	pages, err := build(fixtureRoot(t, "\n// covers: "+m+"\nfunc TestListsTasks(t *testing.T) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, pages, m); r.Status != "Verified" || len(r.Tests) != 1 || !strings.HasSuffix(r.Tests[0], "TestListsTasks") {
		t.Errorf("annotated: %+v", r)
	}
	pages, err = build(fixtureRoot(t, "\nfunc TestListsTasks(t *testing.T) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, pages, m); r.Status != "Implemented" {
		t.Errorf("without its annotation %s is %q, want Implemented", m, r.Status)
	}
}

func TestBadAnnotationsAreRefused(t *testing.T) {
	for name, src := range map[string]string{
		"stray":                   "\n// covers: google.cloud.tasks.v2.CloudTasks/ListTasks\nvar _ = 1\n",
		"not a test":              "\n// covers: google.cloud.tasks.v2.CloudTasks/ListTasks\nfunc helper() {}\n",
		"unknown method":          "\n// covers: google.cloud.tasks.v2.CloudTasks/ListTasksEverywhere\nfunc TestX(t *testing.T) {}\n",
		"claims an unimplemented": "\n// covers: google.cloud.tasks.v2.CloudTasks/UpdateQueue\nfunc TestX(t *testing.T) {}\n",
		"refused but implemented": "\n// covers: google.cloud.tasks.v2.CloudTasks/GetQueue (unimplemented)\nfunc TestX(t *testing.T) {}\n",
	} {
		if _, err := build(fixtureRoot(t, src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every method of a service CloudBurrow implements must be classified, so a
// new RPC in an updated proto cannot appear as nothing.
func TestAnUnclassifiedOwnMethodIsRefused(t *testing.T) {
	root := fixtureRoot(t, "")
	path := filepath.Join(root, "tools", "coverage", "registry.json")
	var reg map[string]string
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &reg)
	delete(reg, "google.cloud.tasks.v2.CloudTasks/PurgeQueue")
	b, _ = json.Marshal(reg)
	_ = os.WriteFile(path, b, 0o644)
	if _, err := build(root); err == nil || !strings.Contains(err.Error(), "PurgeQueue has no registry entry") {
		t.Errorf("an unclassified method was accepted: %v", err)
	}
}
