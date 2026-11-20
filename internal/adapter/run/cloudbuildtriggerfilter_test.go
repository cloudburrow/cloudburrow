package run

import (
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
)

func TestFileGlobMatch(t *testing.T) {
	for _, c := range []struct {
		glob, name string
		want       bool
	}{
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/x/y/a.md", true},
		{"docs/**", "src/docs/a.md", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "a/b/c.go", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/x/a.go", false},
		{"src/**/test/*.go", "src/test/a.go", true},
		{"src/**/test/*.go", "src/a/b/test/a.go", true},
		{"main.go", "main.go", true},
		{"main.?o", "main.go", true},
		{"[a-c].txt", "b.txt", true},
	} {
		if got := fileGlobMatch(c.glob, c.name); got != c.want {
			t.Errorf("fileGlobMatch(%q, %q) = %t; want %t", c.glob, c.name, got, c.want)
		}
	}
}

func TestTriggerFilesAccept(t *testing.T) {
	tr := func(inc, ign []string) *cbpb.BuildTrigger {
		return &cbpb.BuildTrigger{IncludedFiles: inc, IgnoredFiles: ign}
	}
	for _, c := range []struct {
		name  string
		t     *cbpb.BuildTrigger
		files []string
		want  bool
	}{
		{"no filters", tr(nil, nil), []string{"a"}, true},
		{"no filters, no files", tr(nil, nil), nil, true},
		{"only ignored files", tr(nil, []string{"docs/**"}), []string{"docs/a.md"}, false},
		{"one file outside ignored", tr(nil, []string{"docs/**"}), []string{"docs/a.md", "main.go"}, true},
		{"included matches", tr([]string{"src/**"}, nil), []string{"src/a.go"}, true},
		{"included does not match", tr([]string{"src/**"}, nil), []string{"README.md"}, false},
		{"ignored wins over included", tr([]string{"src/**"}, []string{"src/gen/**"}), []string{"src/gen/a.go"}, false},
		{"ignored and an included file", tr([]string{"src/**"}, []string{"src/gen/**"}), []string{"src/gen/a.go", "src/b.go"}, true},
		{"included with no files", tr([]string{"src/**"}, nil), nil, false},
	} {
		if got := triggerFilesAccept(c.t, c.files); got != c.want {
			t.Errorf("%s: triggerFilesAccept = %t; want %t", c.name, got, c.want)
		}
	}
	if err := checkFileGlobs("includedFiles", []string{"src/[a.go"}); err == nil {
		t.Error("a malformed glob was accepted")
	}
}

func TestTriggerFilter(t *testing.T) {
	tr := &cbpb.BuildTrigger{Name: "t", Substitutions: map[string]string{"_ACTION": "", "_IMAGE_TAG": ""},
		Filter: `_IMAGE_TAG != "" && _ACTION == "INSERT"`}
	if ok, err := triggerFilterAccepts(tr, map[string]string{"_ACTION": "INSERT", "_IMAGE_TAG": "v1"}); err != nil || !ok {
		t.Errorf("an INSERT with a tag: %t, %v; want accepted", ok, err)
	}
	if ok, err := triggerFilterAccepts(tr, map[string]string{"_ACTION": "DELETE", "_IMAGE_TAG": "v1"}); err != nil || ok {
		t.Errorf("a DELETE: %t, %v; want refused", ok, err)
	}
	tr.Filter = `BRANCH_NAME.matches("^release-") || TAG_NAME != ""`
	if ok, _ := triggerFilterAccepts(tr, map[string]string{"BRANCH_NAME": "release-1"}); !ok {
		t.Error("a release branch was refused")
	}
	if ok, _ := triggerFilterAccepts(tr, map[string]string{"BRANCH_NAME": "main"}); ok {
		t.Error("main was accepted")
	}
	for _, bad := range []string{`!(_ACTION == "INSERT")`, `_UNKNOWN == "x"`, `_ACTION`, `_ACTION ==`} {
		tr.Filter = bad
		if _, err := compileTriggerFilter(tr); err == nil {
			t.Errorf("filter %q was accepted", bad)
		}
	}
	tr.Filter = `_ACTION != "DELETE"`
	if _, err := compileTriggerFilter(tr); err != nil {
		t.Errorf("filter %q: %v", tr.Filter, err)
	}
}
