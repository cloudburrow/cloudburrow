package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const unvMethod = "google.cloud.tasks.v2.CloudTasks/GetQueue"

// An annotation is listed in its service's Unverified table, and the
// Verified row it belongs to says a code is unverified (#384).
func TestAnUnverifiedCodeIsListedAndMarkedOnItsRow(t *testing.T) {
	root := fixtureRoot(t, "\n// covers: "+unvMethod+"\n// unverified: "+unvMethod+" FAILED_PRECONDITION: a paused queue\nfunc TestGetsQueue(t *testing.T) {}\n")
	pages, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	r := rowOf(t, pages, unvMethod)
	if r.Status != "Verified" || len(r.Unverified) != 1 || r.Unverified[0].Code != "FAILED_PRECONDITION" || r.Unverified[0].Case != "a paused queue" {
		t.Fatalf("row = %+v", r)
	}
	files := render(pages)
	page := string(files["tasks.md"])
	if !strings.Contains(page, "1 error code(s) UNVERIFIED") {
		t.Errorf("the Verified row does not say its code is unverified:\n%s", page)
	}
	if !strings.Contains(page, "## Unverified error codes") ||
		!strings.Contains(page, "| `"+unvMethod+"` | `FAILED_PRECONDITION` | a paused queue | [`TestGetsQueue`](../../test/compat/x_test.go#L") {
		t.Errorf("the Unverified table is missing the annotation:\n%s", page)
	}
	if !strings.Contains(string(files["README.md"]), "| Unverified codes |") {
		t.Error("the summary has no Unverified codes column")
	}
}

// A page with no annotations gets no Unverified section.
func TestAPageWithoutAnnotationsHasNoUnverifiedSection(t *testing.T) {
	pages, err := build(fixtureRoot(t, "\nfunc TestNothing(t *testing.T) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range render(pages) {
		if name != "README.md" && strings.Contains(string(b), "Unverified error codes") {
			t.Errorf("%s has an Unverified section with no annotations", name)
		}
	}
}

// An in-process test, where a fake clock reaches cases a compat test cannot,
// may carry the annotation too.
func TestAnInProcessTestMayCarryAnUnverifiedCode(t *testing.T) {
	root := fixtureRoot(t, "\n// covers: "+unvMethod+"\nfunc TestGetsQueue(t *testing.T) {}\n")
	dir := filepath.Join(root, "internal", "service", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package tasks\n\nimport \"testing\"\n\n// unverified: " + unvMethod + " NOT_FOUND: a deleted queue\nfunc TestDeleted(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "clock_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, pages, unvMethod); len(r.Unverified) != 1 || !strings.HasPrefix(r.Unverified[0].Test, "internal/service/tasks/clock_test.go:") {
		t.Errorf("row = %+v", r)
	}
}

// -check fails, naming file:line, for each malformed annotation.
func TestBadUnverifiedAnnotationsAreRefused(t *testing.T) {
	for name, c := range map[string]struct{ src, want string }{
		"unknown method":  {"\n// unverified: google.cloud.tasks.v2.CloudTasks/GetQueues NOT_FOUND: x\nfunc TestX(t *testing.T) {}\n", "not a method of any listed service"},
		"misspelt code":   {"\n// unverified: " + unvMethod + " INVALID_ARGUMNET: x\nfunc TestX(t *testing.T) {}\n", `"INVALID_ARGUMNET" is not a gRPC code name`},
		"stray":           {"\n// unverified: " + unvMethod + " NOT_FOUND: x\nvar _ = 1\n", "outside a test function's doc comment"},
		"not a test":      {"\n// unverified: " + unvMethod + " NOT_FOUND: x\nfunc helper() {}\n", "which is not a test"},
		"no case":         {"\n// unverified: " + unvMethod + " NOT_FOUND\nfunc TestX(t *testing.T) {}\n", "must be"},
		"camel-case code": {"\n// unverified: " + unvMethod + " NotFound: x\nfunc TestX(t *testing.T) {}\n", "must be"},
	} {
		_, err := build(fixtureRoot(t, c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "x_test.go:") {
			t.Errorf("%s: %v, want an error naming x_test.go:<line> and %q", name, err, c.want)
		}
	}
}
