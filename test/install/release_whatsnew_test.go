package install

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What a scheduled release's notes say. v0.6.0's listed only landing
// titles, most of them "Fix main CI ..."; v0.7.0's listed branch names
// (`feat/2160-lb-4-e`) under Added, issue titles that state a problem
// ("AlloyDB for PostgreSQL: not served") as features, issues that were
// reopened, and every CI-fix pull request. Now the notes are the entries
// written under Unreleased in CHANGELOG.md (release-notes.sh --unreleased),
// and with none the release is refused.

const changelogV1 = `# Changelog

## [Unreleased]

### Added

- An entry from before v1.0.0 (#1).

## [0.9.0] - 2026-01-01

- older
`

// whatsNewRepo is a repository with v1.0.0, then a landing of feat/31-gpu
// (which adds CHANGELOG.md entries when entry is true), feat/32-trace and
// fix/33-race, each merged by git's own "Merge branch '...'" message, then a
// "Fix main CI" pull request that says "Closes #34". It returns the
// directory and main's head.
func whatsNewRepo(t *testing.T, entry bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	g := func(args ...string) string { return gitIn(t, dir, nil, args...) }
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		g("add", name)
	}
	g("init", "-q", "-b", "main")
	write("CHANGELOG.md", changelogV1)
	g("commit", "-q", "-m", "base")
	g("tag", "-a", "v1.0.0", "-m", "v1.0.0")
	branch := func(name, file, changelog string) {
		g("checkout", "-q", "-b", name, "main")
		write(file, file)
		if changelog != "" {
			write("CHANGELOG.md", changelog)
		}
		g("commit", "-q", "-m", "work on "+name)
		g("checkout", "-q", "main")
	}
	gpu := ""
	if entry {
		gpu = strings.Replace(changelogV1, "- An entry from before v1.0.0 (#1).\n",
			"- An entry from before v1.0.0 (#1).\n- Apple GPU for local models (#31): see\n  [the docs](docs/gpu.md).\n\n### Fixed\n\n- A race in the scheduler (#33).\n", 1)
		gpu = strings.Replace(gpu, "## [Unreleased]\n\n", "## [Unreleased]\n\n- Faster start-up (#35).\n\n", 1)
	}
	branch("feat/31-gpu", "gpu", gpu)
	branch("feat/32-trace", "trace", "")
	branch("fix/33-race", "race", "")
	branch("fix/main-ci-after-9", "ci", "")
	g("checkout", "-q", "-b", "land/9", "main")
	for _, b := range []string{"feat/31-gpu", "feat/32-trace", "fix/33-race"} {
		g("merge", "-q", "--no-ff", "--no-edit", b)
	}
	g("checkout", "-q", "main")
	g("merge", "-q", "--no-ff", "land/9", "-m", "Merge pull request #109 from o/land/9", "-m", "Landing 9")
	g("merge", "-q", "--no-ff", "fix/main-ci-after-9", "-m", "Merge pull request #110 from o/fix/main-ci-after-9", "-m", "Fix main CI after Landing 9\n\nCloses #34")
	return dir, g("rev-parse", "HEAD")
}

func releaseNotesIn(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{scriptPath(t, "release-notes.sh")}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// TestReleaseNotesUnreleasedEntries: --unreleased prints the entries added
// under Unreleased since the previous tag, under their own ### headings (an
// entry under none under "### Changes"), and not an entry the previous tag
// already had. Nothing comes from the branches, the merges or "Closes #34".
func TestReleaseNotesUnreleasedEntries(t *testing.T) {
	dir, head := whatsNewRepo(t, true)
	out, stderr, err := releaseNotesIn(t, dir, "--unreleased", "v1.0.0", head)
	if err != nil {
		t.Fatalf("release-notes.sh --unreleased: %v\n%s", err, stderr)
	}
	want := "### Changes\n\n- Faster start-up (#35).\n\n" +
		"### Added\n\n- Apple GPU for local models (#31): see\n  [the docs](docs/gpu.md).\n\n" +
		"### Fixed\n\n- A race in the scheduler (#33).\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	for _, banned := range []string{"before v1.0.0", "feat/", "fix/", "#34", "#32", "Merge", "Landing"} {
		if strings.Contains(out, banned) {
			t.Errorf("the entries hold %q:\n%s", banned, out)
		}
	}
}

// TestReleaseNotesUnreleasedEmptyFails: with no entry added, there is no
// fallback made of branch names or issue titles: it fails, prints nothing,
// and says "CHANGELOG Unreleased is empty".
func TestReleaseNotesUnreleasedEmptyFails(t *testing.T) {
	dir, head := whatsNewRepo(t, false)
	out, stderr, err := releaseNotesIn(t, dir, "--unreleased", "v1.0.0", head)
	if err == nil {
		t.Fatalf("release-notes.sh --unreleased succeeded with no entries:\n%s", out)
	}
	if out != "" {
		t.Errorf("printed notes on failure:\n%s", out)
	}
	if !strings.Contains(stderr, "CHANGELOG Unreleased is empty") {
		t.Errorf("the error does not say the Unreleased section is empty: %q", stderr)
	}
}

// generatedTag tags main's head v1.1.0 with a scheduled release's message.
func generatedTag(t *testing.T, dir, body string) {
	t.Helper()
	msg := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(msg, []byte("CloudBurrow v1.1.0\n\n<!-- cloudburrow-release-notes: generated -->\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, nil, "tag", "-a", "--cleanup=verbatim", "-F", msg, "v1.1.0")
}

// TestReleaseNotesRefuseGeneratedNotesWithoutEntries: a generated tag whose
// message holds no entry, only the intro and a folded pull request list (or
// v0.7.0's branch-name Added list inside it), is refused, so the release
// job publishes nothing rather than empty notes.
func TestReleaseNotesRefuseGeneratedNotesWithoutEntries(t *testing.T) {
	dir, _ := whatsNewRepo(t, false)
	generatedTag(t, dir, "Built from main.\n\n<details><summary>Pull requests</summary>\n\n### Added\n\n- README: a logo (#5)\n\n</details>\n")
	out, stderr, err := releaseNotesIn(t, dir, "v1.1.0")
	if err == nil || out != "" {
		t.Fatalf("notes without an entry were published: %v\n%s", err, out)
	}
	if !strings.Contains(stderr, "CHANGELOG Unreleased is empty") || !strings.Contains(stderr, "v1.1.0") {
		t.Errorf("the error does not name the tag and the empty Unreleased: %q", stderr)
	}
}

// TestReleaseNotesFromGeneratedNotesAreVerbatim: generated notes with
// entries are printed as they are, links made absolute, with nothing added.
func TestReleaseNotesFromGeneratedNotesAreVerbatim(t *testing.T) {
	dir, _ := whatsNewRepo(t, true)
	body := "Built from main.\n\n### Added\n\n- A GPU, see [docs](docs/gpu.md) (#31).\n\n<details><summary>Pull requests</summary>\n\n- README: a logo (#5)\n\n</details>\n"
	generatedTag(t, dir, body)
	cmd := exec.Command("sh", scriptPath(t, "release-notes.sh"), "v1.1.0")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RELEASE_NOTES_LINK_BASE=https://github.com/o/r/blob/v1.1.0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("release-notes.sh v1.1.0: %v", err)
	}
	if want := strings.Replace(body, "](docs/gpu.md)", "](https://github.com/o/r/blob/v1.1.0/docs/gpu.md)", 1); string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// TestReleaseNotesNeedNoIssueLookups: nothing reads issues any more, so no
// release job names a repository for gh or asks for issues: read, and the
// jobs still check out the whole history for the annotated tag.
func TestReleaseNotesNeedNoIssueLookups(t *testing.T) {
	jobs := releaseJobs(t)
	for _, name := range []string{"verify", "publish"} {
		j := job(t, jobs, name)
		if !strings.Contains(j, "sh scripts/release-notes.sh") {
			t.Fatalf("%s no longer runs release-notes.sh", name)
		}
		if !strings.Contains(j, "          fetch-depth: 0") {
			t.Errorf("release.yml's %s job does not check out the full history", name)
		}
		for _, banned := range []string{"RELEASE_NOTES_REPO", "issues: read"} {
			if strings.Contains(j, banned) {
				t.Errorf("release.yml's %s job still has %q, which only the dropped issue-title fallback used", name, banned)
			}
		}
	}
	b, err := os.ReadFile(scriptPath(t, "release-notes.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(uncommented(string(b)), "gh ") {
		t.Error("release-notes.sh still calls gh")
	}
}
