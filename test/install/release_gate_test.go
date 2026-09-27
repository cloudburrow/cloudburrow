package install

import (
	"regexp"
	"strings"
	"testing"
)

// A release is published as a prerelease, smoke-installed, and only then
// marked latest and pushed to the Homebrew tap (#680). None of that can run
// outside a tagged release, so what is checked here is the job graph and the
// flags that make it so: drop any of them and a release that failed its smoke
// test reaches install.sh, the action's `version: latest` or `brew upgrade`.

// ghCommand returns the gh invocation in j that starts with prefix (such as
// "gh release create"), its shell line continuations joined, or "" if there
// is none.
func ghCommand(j, prefix string) string {
	joined := regexp.MustCompile(`\\\n\s*`).ReplaceAllString(j, "")
	for _, line := range strings.Split(joined, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "- "), "run:"))
		if strings.HasPrefix(line, prefix+" ") {
			return line
		}
	}
	return ""
}

func hasFlag(cmd, flag string) bool {
	for _, f := range strings.Fields(cmd) {
		if f == flag {
			return true
		}
	}
	return false
}

func TestReleaseIsPublishedAsAPrerelease(t *testing.T) {
	create := ghCommand(job(t, releaseJobs(t), "publish"), "gh release create")
	if create == "" {
		t.Fatal("publish has no gh release create")
	}
	if !hasFlag(create, "--prerelease") {
		t.Errorf("publish creates the release without --prerelease, so it is latest before smoke has run:\n%s", create)
	}
	// A draft's assets cannot be downloaded anonymously, so install.sh, the
	// formula and the smoke test that uses them would all fail.
	if hasFlag(create, "--draft") {
		t.Errorf("publish creates a draft; smoke cannot install a draft's assets:\n%s", create)
	}
	if hasFlag(create, "--latest") {
		t.Errorf("publish marks the release latest before smoke has run:\n%s", create)
	}
}

func TestReleaseIsMarkedLatestOnlyAfterSmoke(t *testing.T) {
	jobs := releaseJobs(t)
	var promoters []string
	for name, j := range jobs {
		edit := ghCommand(j, "gh release edit")
		if hasFlag(edit, "--latest") || hasFlag(edit, "--prerelease=false") {
			promoters = append(promoters, name)
		}
	}
	if len(promoters) != 1 {
		t.Fatalf("jobs that mark the release latest: %v, want exactly one", promoters)
	}
	name := promoters[0]
	j := jobs[name]
	edit := ghCommand(j, "gh release edit")
	for _, f := range []string{"--prerelease=false", "--latest"} {
		if !hasFlag(edit, f) {
			t.Errorf("%s: gh release edit without %s:\n%s", name, f, edit)
		}
	}
	if n := needs(t, j); !contains(n, "smoke") {
		t.Errorf("%s needs %v; it must need smoke, so a release is latest only once smoke has passed on every OS", name, n)
	}
	if !strings.Contains(j, "    if: startsWith(github.ref, 'refs/tags/v')\n") {
		t.Errorf("%s does not run only for a tag", name)
	}
	if !regexp.MustCompile(`(?m)^      contents: write$`).MatchString(j) {
		t.Errorf("%s has no contents: write, which gh release edit needs", name)
	}
	// Smoke must not wait on the promotion it gates.
	if contains(needs(t, job(t, jobs, "smoke")), name) {
		t.Errorf("smoke needs %s, the job it gates", name)
	}
}

func TestTheTapIsUpdatedOnlyAfterSmoke(t *testing.T) {
	n := needs(t, job(t, releaseJobs(t), "tap"))
	if !contains(n, "smoke") {
		t.Errorf("tap needs %v; it must need smoke, or brew upgrade delivers a release that failed it", n)
	}
}

func TestGhCommand(t *testing.T) {
	j := "    steps:\n      - run: |\n          gh release create \"v1\" \\\n            --verify-tag \\\n            --prerelease \\\n            dist/*\n      - run: gh release edit v1 --latest\n"
	if got, want := ghCommand(j, "gh release create"), `gh release create "v1" --verify-tag --prerelease dist/*`; got != want {
		t.Errorf("create: got %q, want %q", got, want)
	}
	if got, want := ghCommand(j, "gh release edit"), "gh release edit v1 --latest"; got != want {
		t.Errorf("edit: got %q, want %q", got, want)
	}
	if got := ghCommand(j, "gh release delete"); got != "" {
		t.Errorf("delete: got %q, want none", got)
	}
}
