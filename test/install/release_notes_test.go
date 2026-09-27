package install

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// notes runs scripts/release-notes.sh, returning stdout and stderr apart:
// on failure the release workflow must get no notes at all, only an error.
func notes(t *testing.T, env []string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{scriptPath(t, "release-notes.sh")}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

func changelog(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const fixture = `# Changelog

## [Unreleased]

### Added

- not released yet

## [1.2.0-rc.1] - 2026-01-02

- the release candidate

## [1.2.0] - 2026-01-03

### Fixed

- the real thing, see [the docs](docs/x.md), [a site](https://example.com/a) and [here](#top)

## [1.1.0] - 2026-01-01

- older

## [1.0.0] - 2025-12-01

[Unreleased]: https://example.com/compare/v1.2.0...HEAD
[1.2.0]: https://example.com/v1.2.0
`

// TestReleaseNotesAreTheTagsOwnSection (#603): exactly the tagged section,
// without its heading, a neighbour's entries or the link references, and
// 1.2.0 is not confused with 1.2.0-rc.1.
func TestReleaseNotesAreTheTagsOwnSection(t *testing.T) {
	p := changelog(t, fixture)
	got, stderr, err := notes(t, nil, "v1.2.0", p)
	if err != nil {
		t.Fatalf("release-notes.sh v1.2.0: %v\n%s", err, stderr)
	}
	want := "### Fixed\n\n- the real thing, see [the docs](docs/x.md), [a site](https://example.com/a) and [here](#top)\n"
	if got != want {
		t.Errorf("notes for v1.2.0:\n got %q\nwant %q", got, want)
	}

	got, _, err = notes(t, nil, "v1.2.0-rc.1", p)
	if err != nil || got != "- the release candidate\n" {
		t.Errorf("notes for v1.2.0-rc.1: %q (%v)", got, err)
	}
	got, _, err = notes(t, nil, "v1.1.0", p)
	if err != nil || got != "- older\n" {
		t.Errorf("notes for v1.1.0: %q (%v)", got, err)
	}
}

// TestReleaseNotesMakeRepositoryLinksAbsolute: a release page resolves a
// relative link against itself, so paths are pinned to the tag's tree.
func TestReleaseNotesMakeRepositoryLinksAbsolute(t *testing.T) {
	p := changelog(t, fixture)
	got, stderr, err := notes(t, []string{"RELEASE_NOTES_LINK_BASE=https://github.com/o/r/blob/v1.2.0/"}, "v1.2.0", p)
	if err != nil {
		t.Fatalf("release-notes.sh: %v\n%s", err, stderr)
	}
	for _, s := range []string{
		"[the docs](https://github.com/o/r/blob/v1.2.0/docs/x.md)",
		"[a site](https://example.com/a)",
		"[here](#top)",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("notes lack %q:\n%s", s, got)
		}
	}
}

// TestReleaseNotesRefuseATagWithoutASection: a tag with no section, or an
// empty one, must fail the release rather than publish notes that say
// nothing, and must print no notes to be used by mistake.
func TestReleaseNotesRefuseATagWithoutASection(t *testing.T) {
	p := changelog(t, fixture)
	for _, tag := range []string{"v9.9.9", "v1.2", "v1.0.0"} {
		got, stderr, err := notes(t, nil, tag, p)
		if err == nil {
			t.Errorf("%s: notes were produced for a missing or empty section: %q", tag, got)
		}
		if got != "" {
			t.Errorf("%s: printed notes on failure: %q", tag, got)
		}
		if !strings.Contains(stderr, tag) {
			t.Errorf("%s: the error does not name the tag: %q", tag, stderr)
		}
	}
	if _, _, err := notes(t, nil, "main", p); err == nil {
		t.Error("a branch name was accepted as a release tag")
	}
}

// TestTheChangelogHasAnUnreleasedSectionAndV010: the repository's own
// CHANGELOG.md, which the release workflow reads.
func TestTheChangelogHasAnUnreleasedSectionAndV010(t *testing.T) {
	p, err := filepath.Abs(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\n## [Unreleased]\n") {
		t.Error("CHANGELOG.md has no ## [Unreleased] section")
	}
	got, stderr, err := notes(t, nil, "v0.1.0", p)
	if err != nil {
		t.Fatalf("no v0.1.0 notes: %v\n%s", err, stderr)
	}
	if strings.Contains(got, "\n## ") || strings.Contains(got, "]: https://") {
		t.Errorf("the v0.1.0 notes run past their section:\n%s", got)
	}
}
