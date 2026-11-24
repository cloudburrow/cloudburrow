package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The What's new section scripts/release-notes.sh puts in a scheduled
// release's generated notes: v0.6.0's notes listed only landing titles, most
// of them "Fix main CI ...", and said nothing about what was released.

const changelogV1 = `# Changelog

## [Unreleased]

### Added

- An entry from before v1.0.0 (#1).

## [0.9.0] - 2026-01-01

- older
`

// whatsNewRepo is a repository with v1.0.0, then a landing of feat/31-gpu
// (which adds a CHANGELOG.md entry when entry is true), feat/32-trace and
// fix/33-race, then a "Fix main CI" pull request, tagged v1.1.0 with
// generated notes whose landing list holds both.
func whatsNewRepo(t *testing.T, entry bool) string {
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
			"- An entry from before v1.0.0 (#1).\n- Apple GPU for local models (#31): see\n  [the docs](docs/gpu.md).\n", 1)
	}
	branch("feat/31-gpu", "gpu", gpu)
	branch("feat/32-trace", "trace", "")
	branch("fix/33-race", "race", "")
	branch("fix/main-ci-after-9", "ci", "")
	g("checkout", "-q", "-b", "land/9", "main")
	for _, b := range []string{"feat/31-gpu", "feat/32-trace", "fix/33-race"} {
		g("merge", "-q", "--no-ff", b, "-m", "Landing 9: merge "+b)
	}
	g("checkout", "-q", "main")
	g("merge", "-q", "--no-ff", "land/9", "-m", "Merge pull request #109 from o/land/9", "-m", "Landing 9")
	g("merge", "-q", "--no-ff", "fix/main-ci-after-9", "-m", "Merge pull request #110 from o/fix/main-ci-after-9", "-m", "Fix main CI after Landing 9\n\nCloses #34")
	msg := filepath.Join(t.TempDir(), "msg")
	body := "CloudBurrow v1.1.0\n\n<!-- cloudburrow-release-notes: generated -->\nReleased automatically.\n\n" +
		"### Landings\n\n- Fix main CI after Landing 9 (#110)\n- Landing 9 (#109)\n"
	if err := os.WriteFile(msg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g("tag", "-a", "--cleanup=verbatim", "-F", msg, "v1.1.0")
	return dir
}

func whatsNew(t *testing.T, dir string, env ...string) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "release-notes.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "v1.1.0")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"RELEASE_NOTES_REPO=", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, env...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("release-notes.sh v1.1.0: %v", err)
	}
	return string(out)
}

// TestReleaseNotesWhatsNewFromTheChangelog: the entries added under
// Unreleased since the previous tag lead the notes, under their headings and
// with their links made absolute, an older entry is not repeated, landed
// issues no entry names follow, and the landings, "Fix main CI" among them,
// are only in the collapsed All landings block.
func TestReleaseNotesWhatsNewFromTheChangelog(t *testing.T) {
	out := whatsNew(t, whatsNewRepo(t, true), "RELEASE_NOTES_LINK_BASE=https://github.com/o/r/blob/v1.1.0")
	want := "Released automatically.\n\n" +
		"### What's new\n\n" +
		"The [CHANGELOG.md](https://github.com/o/r/blob/v1.1.0/CHANGELOG.md) entries added under **Unreleased** since v1.0.0:\n\n" +
		"#### Added\n\n" +
		"- Apple GPU for local models (#31): see\n  [the docs](https://github.com/o/r/blob/v1.1.0/docs/gpu.md).\n\n" +
		"Also closed by the landings since v1.0.0, with no CHANGELOG.md entry:\n\n" +
		"#### Other\n\n" +
		"- `feat/32-trace` (#32)\n- `fix/33-race` (#33)\n- Issue #34 (#34)\n\n" +
		"<details><summary>All landings</summary>\n\n" +
		"- Fix main CI after Landing 9 (#110)\n- Landing 9 (#109)\n\n" +
		"</details>\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if strings.Contains(out, "before v1.0.0") {
		t.Error("an entry released before v1.0.0 is listed again")
	}
	if strings.Contains(out, "main-ci") {
		t.Error("the fix/main-ci branch is listed as a change")
	}
}

// fakeGHIssues answers `gh issue view N ... --jq ...` the way the --jq
// expression in release-notes.sh would: "<workflow-red?>\t<title>".
const fakeGHIssues = `#!/bin/sh
echo "$*" >> "$(dirname "$0")/calls"
[ "$1 $2" = "issue view" ] || exit 1
case "$3" in
31) printf 'false\tApple GPU (Metal) acceleration for local AI\n' ;;
32) printf 'false\tCloud Trace: serve the Trace API\n' ;;
33) printf 'false\tConsole parity: Pub/Sub pages match the Google Cloud console\n' ;;
34) printf 'true\tCLI integration is failing\n' ;;
*) exit 1 ;;
esac
`

// TestReleaseNotesWhatsNewFromIssues: with no CHANGELOG.md entry added, the
// landed issues are listed by the area their title names, Other last, read
// from the repository with gh; an issue labelled workflow-red is left out.
func TestReleaseNotesWhatsNewFromIssues(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGHIssues), 0o755); err != nil {
		t.Fatal(err)
	}
	out := whatsNew(t, whatsNewRepo(t, false),
		"RELEASE_NOTES_REPO=o/r", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	want := "Released automatically.\n\n" +
		"### What's new\n\n" +
		"No CHANGELOG.md entry was added since v1.0.0; these are the issues its landings closed:\n\n" +
		"#### Cloud Trace\n\n- serve the Trace API (#32)\n\n" +
		"#### Console\n\n- Pub/Sub pages match the Google Cloud console (#33)\n\n" +
		"#### Other\n\n- Apple GPU (Metal) acceleration for local AI (#31)\n\n" +
		"<details><summary>All landings</summary>\n\n" +
		"- Fix main CI after Landing 9 (#110)\n- Landing 9 (#109)\n\n" +
		"</details>\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	calls, _ := os.ReadFile(filepath.Join(bin, "calls"))
	if !strings.Contains(string(calls), "issue view 31 -R o/r ") {
		t.Errorf("gh was not asked for the issues of o/r:\n%s", calls)
	}
}

// TestReleaseNotesJobsCanReadThePreviousTag: each release.yml job that runs
// release-notes.sh checks out the whole history, so the previous tag and its
// CHANGELOG.md are there, and names the repository and the issues
// permission its gh lookups need. The scheduled release grants that
// permission to the release it calls.
func TestReleaseNotesJobsCanReadThePreviousTag(t *testing.T) {
	jobs := releaseJobs(t)
	for _, name := range []string{"verify", "publish"} {
		j := job(t, jobs, name)
		if !strings.Contains(j, "sh scripts/release-notes.sh") {
			t.Fatalf("%s no longer runs release-notes.sh", name)
		}
		for _, want := range []string{"          fetch-depth: 0", "RELEASE_NOTES_REPO: ${{ github.repository }}", "      issues: read\n"} {
			if !strings.Contains(j, want) {
				t.Errorf("release.yml's %s job lacks %q", name, want)
			}
		}
	}
	rel := job(t, workflowJobs(t, filepath.Join("..", "..", ".github", "workflows", "scheduled-release.yml")), "release")
	if !strings.Contains(rel, "      issues: read\n") {
		t.Error("scheduled-release.yml's release job does not grant issues: read to release.yml")
	}
}
