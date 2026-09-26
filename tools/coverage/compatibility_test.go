package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCoverageStorageMatchesCompatibility (#520): every Cloud Storage
// method docs/compatibility.md names in its JSON API tables has the status
// the generated report gives it, and every method the server serves is named
// in one of those rows, so neither document can drift from the other.
func TestCoverageStorageMatchesCompatibility(t *testing.T) {
	pages, err := build("../..")
	if err != nil {
		t.Fatal(err)
	}
	report := map[string]string{}
	for _, p := range pages {
		if p.Key == "storage" {
			for _, r := range p.Rows {
				report[r.Method] = r.Status
			}
		}
	}
	if len(report) == 0 {
		t.Fatal("the report has no storage page")
	}

	b, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "## Cloud Storage — JSON API v1")
	if start < 0 {
		t.Fatal("docs/compatibility.md has no Cloud Storage JSON API section")
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	methodToken := regexp.MustCompile("`([a-z][A-Za-z]*(?:\\.[a-zA-Z]+)+)`")
	boldStatus := regexp.MustCompile(`\*\*([A-Za-z ,]+?)\*\*`)
	named := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		op, status := cells[1], cells[2]
		m := boldStatus.FindStringSubmatch(status)
		if m == nil {
			continue
		}
		want := strings.Fields(strings.TrimRight(m[1], ","))[0]
		for _, tok := range methodToken.FindAllStringSubmatch(op, -1) {
			id := "storage." + tok[1]
			got, ok := report[id]
			if !ok {
				continue // a field or a parameter, not a method
			}
			named[id] = true
			if got != want {
				t.Errorf("docs/compatibility.md calls %s %s; the coverage report says %s", id, want, got)
			}
		}
	}
	for id, st := range report {
		if (st == "Verified" || st == "Implemented") && !named[id] {
			t.Errorf("%s is %s in the coverage report but no Cloud Storage row in docs/compatibility.md names it", id, st)
		}
	}
}
