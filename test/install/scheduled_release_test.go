package install

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The scheduled release (#1898): scheduled-release.yml decides with
// scripts/scheduled-release.sh, pushes an annotated tag whose message holds
// generated notes, and calls release.yml with it. A release needs the
// commit's own push run of ci.yml on main to have passed in full
// (scripts/require-ci-green.sh --main-push), never only a pull request's
// ci-green.

// releaseOnly is the condition of every release.yml job that publishes: a
// pushed tag, or the tag the scheduled release passes.
const releaseOnly = "startsWith(github.ref, 'refs/tags/v') || startsWith(inputs.release_tag, 'v')"

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// uncommented drops comment lines, so only what runs is checked.
func uncommented(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// TestReleaseIsCallableWithATag: release.yml still runs on a pushed tag, and
// can be called with one. Called, GITHUB_REF and GITHUB_SHA are the caller's
// (main's head when the schedule fired), so no job may read them: the tag is
// RELEASE_TAG and every checkout checks out RELEASE_REF.
func TestReleaseIsCallableWithATag(t *testing.T) {
	raw := readWorkflow(t, "release.yml")
	code := uncommented(raw)
	for _, want := range []string{
		"  push:\n    tags: [\"v*\"]\n",
		"  workflow_call:\n    inputs:\n      release_tag:\n",
		"  RELEASE_TAG: ${{ inputs.release_tag || (startsWith(github.ref, 'refs/tags/v') && github.ref_name) || '' }}\n",
		"  RELEASE_REF: ${{ inputs.release_tag && format('refs/tags/{0}', inputs.release_tag) || '' }}\n",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	for _, banned := range []string{"GITHUB_SHA", "GITHUB_REF_NAME", "GITHUB_REF_TYPE", "github.sha"} {
		if strings.Contains(code, banned) {
			t.Errorf("release.yml reads %s, which is the caller's when the scheduled release calls it", banned)
		}
	}
	checkout := regexp.MustCompile(`(?m)^      - uses: actions/checkout@[0-9a-f]{40} # .*\n        with:\n          ref: \$\{\{ env\.RELEASE_REF \}\}\n`)
	if all, ok := strings.Count(code, "actions/checkout@"), len(checkout.FindAllString(code, -1)); all == 0 || all != ok {
		t.Errorf("%d of release.yml's %d checkouts check out ${{ env.RELEASE_REF }}", ok, all)
	}
	jobs := releaseJobs(t)
	for _, name := range []string{"runtime-image", "publish", "smoke", "action-selftest", "promote", "tap"} {
		if !strings.Contains(job(t, jobs, name), "    if: "+releaseOnly+"\n") {
			t.Errorf("%s does not run exactly for a pushed or a passed tag (%s)", name, releaseOnly)
		}
	}
	if !strings.Contains(job(t, jobs, "action-selftest"), "      ref: ${{ inputs.release_tag && format('refs/tags/{0}', inputs.release_tag) || '' }}\n") {
		t.Error("action-selftest is not given the tag's ref, so a called release would test the action at main's head")
	}
}

// TestScheduledReleaseWorkflow: the schedule, a manual dispatch that is a
// dry run unless told otherwise, one job that writes (the tag), and the real
// release through release.yml.
func TestScheduledReleaseWorkflow(t *testing.T) {
	code := uncommented(readWorkflow(t, "scheduled-release.yml"))
	for _, want := range []string{
		"  schedule:\n    - cron: ",
		"      dry_run:\n", "        type: boolean\n        required: false\n        default: true\n",
		"  group: scheduled-release\n  cancel-in-progress: false\n",
		"run: sh scripts/scheduled-release.sh ",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("scheduled-release.yml lacks %q", want)
		}
	}
	jobs := workflowJobs(t, filepath.Join("..", "..", ".github", "workflows", "scheduled-release.yml"))
	for name, j := range jobs {
		if strings.Contains(j, "contents: write") && name != "tag" && name != "release" {
			t.Errorf("job %s asks for contents: write; only tag and the called release write", name)
		}
	}
	plan := job(t, jobs, "plan")
	if strings.Contains(plan, ": write") {
		t.Error("plan asks for write access")
	}
	tag := job(t, jobs, "tag")
	if strings.Contains(tag, "actions/checkout") {
		t.Error("tag checks out the source tree; it must run nothing from it")
	}
	if !strings.Contains(tag, "!(github.event_name == 'workflow_dispatch' && inputs.dry_run)") {
		t.Error("tag does not skip a dry run")
	}
	rel := job(t, jobs, "release")
	for _, want := range []string{
		"    needs: [plan, tag]\n",
		"    uses: ./.github/workflows/release.yml\n",
		"      release_tag: ${{ needs.plan.outputs.tag }}\n",
		"    secrets: inherit\n",
		"      id-token: write\n", "      attestations: write\n", "      packages: write\n",
	} {
		if !strings.Contains(rel, want) {
			t.Errorf("release job lacks %q", want)
		}
	}
}

// fakeGHMain answers the API calls of --main-push and scheduled-release.sh
// from files in its directory: main.json for main's successful push runs,
// push-<sha>.json for one commit's push runs, jobs-<id>.json for a run's
// jobs, and empty.json for anything it has no file for.
const fakeGHMain = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/calls"
path=""; expr=""
while [ $# -gt 0 ]; do
	case "$1" in
	--jq) expr="$2"; shift 2 ;;
	--paginate|api) shift ;;
	*) path="$1"; shift ;;
	esac
done
case "$path" in
*/actions/workflows/ci.yml/runs\?head_sha=*) sha="${path#*head_sha=}"; f="$dir/push-${sha%%&*}.json" ;;
*/actions/workflows/ci.yml/runs\?*) f="$dir/main.json" ;;
*/actions/runs/*/jobs\?*) id="${path#*/actions/runs/}"; f="$dir/jobs-${id%%/*}.json" ;;
*) echo "fake gh: unexpected path $path" >&2; exit 2 ;;
esac
[ -f "$f" ] || f="$dir/empty.json"
jq -r "$expr" < "$f"
`

// ciJobs is a main push run's jobs, all passed unless overridden by name.
func ciJobs(override map[string]string) string {
	names := []string{
		"detect changes", "build the BigQuery emulator", "check (ubuntu-latest, go 1.27)",
		"integration (cluster)", "official SDK compatibility (storage)",
		"official SDK compatibility (sql)", "every compat test ran in a shard",
		"report main recovered", "report main failure", "ci-green",
	}
	type j struct {
		Name       string  `json:"name"`
		Conclusion *string `json:"conclusion"`
	}
	var js []j
	for _, n := range names {
		c := "success"
		if n == "report main failure" {
			c = "skipped"
		}
		if o, ok := override[n]; ok {
			c = o
		}
		cc := c
		js = append(js, j{n, &cc})
	}
	b, _ := json.Marshal(map[string]any{"jobs": js})
	return string(b)
}

// pushRun is one ci.yml run of a push to main.
func pushRun(id int, sha, status, conclusion string) map[string]any {
	r := map[string]any{
		"id": id, "head_sha": sha, "status": status, "event": "push", "head_branch": "main",
		"path": ".github/workflows/ci.yml", "html_url": fmt.Sprintf("https://github.com/o/r/actions/runs/%d", id),
	}
	if conclusion != "" {
		r["conclusion"] = conclusion
	}
	return r
}

func runsJSON(runs ...map[string]any) string {
	if runs == nil {
		runs = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"workflow_runs": runs})
	return string(b)
}

// fakeAPI writes the fake gh and its answers into a new directory.
func fakeAPI(t *testing.T, files map[string]string) string {
	t.Helper()
	for _, tool := range []string{"sh", "jq", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	dir := t.TempDir()
	files["gh"] = fakeGHMain
	files["empty.json"] = `{"workflow_runs":[],"jobs":[]}`
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "gh" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runMainGate(t *testing.T, files map[string]string) (string, error) {
	t.Helper()
	dir := fakeAPI(t, files)
	cmd := exec.Command("sh", filepath.Join("..", "..", "scripts", "require-ci-green.sh"), "--main-push", gateSHA)
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_REPOSITORY=o/r", "CI_GREEN_INTERVAL=1", "CI_GREEN_TIMEOUT=2",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestMainPushGatePassesOnAFullMainRun(t *testing.T) {
	out, err := runMainGate(t, map[string]string{
		"push-" + gateSHA + ".json": runsJSON(pushRun(7, gateSHA, "completed", "cancelled"), pushRun(8, gateSHA, "completed", "success")),
		"jobs-8.json":               ciJobs(nil),
	})
	if err != nil || !strings.Contains(out, "actions/runs/8") {
		t.Fatalf("the gate refused a commit whose main push run passed in full: %v\n%s", err, out)
	}
}

// A pull request's ci-green does not count, and a push run cancelled by the
// next push, or one whose compat shards were skipped, proves nothing.
func TestMainPushGateRefuses(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no push run (only a PR's ci-green)": {},
		"push run cancelled": {
			"push-" + gateSHA + ".json": runsJSON(pushRun(7, gateSHA, "completed", "cancelled")),
		},
		"compat shards skipped": {
			"push-" + gateSHA + ".json": runsJSON(pushRun(8, gateSHA, "completed", "success")),
			"jobs-8.json": ciJobs(map[string]string{
				"official SDK compatibility (storage)": "skipped", "official SDK compatibility (sql)": "skipped",
				"every compat test ran in a shard": "skipped",
			}),
		},
		"a job failed in a run marked success": {
			"push-" + gateSHA + ".json": runsJSON(pushRun(8, gateSHA, "completed", "success")),
			"jobs-8.json":               ciJobs(map[string]string{"check (ubuntu-latest, go 1.27)": "failure"}),
		},
		"still running at the timeout": {
			"push-" + gateSHA + ".json": runsJSON(pushRun(8, gateSHA, "in_progress", "")),
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runMainGate(t, files)
			if err == nil {
				t.Fatalf("the gate passed:\n%s", out)
			}
			if !strings.Contains(out, "::error::") || !strings.Contains(out, gateSHA) {
				t.Errorf("the error does not name the SHA:\n%s", out)
			}
		})
	}
}

// A small repository shaped like main: v0.3.0, then landing 1 (a feat/ and
// a fix/ branch), then landing 2 (a fix/ branch only).
type schedRepo struct {
	dir                  string
	t0                   int64
	base, landing1, head string
}

func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newSchedRepo(t *testing.T) schedRepo {
	t.Helper()
	r := schedRepo{dir: t.TempDir(), t0: 1795000000}
	g := func(args ...string) string { return gitIn(t, r.dir, nil, args...) }
	commit := func(file string) {
		if err := os.WriteFile(filepath.Join(r.dir, file), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		g("add", file)
		g("commit", "-q", "-m", "add "+file)
	}
	g("init", "-q", "-b", "main")
	commit("base")
	r.base = g("rev-parse", "HEAD")
	gitIn(t, r.dir, []string{"GIT_COMMITTER_DATE=@" + strconv.FormatInt(r.t0, 10) + " +0000"}, "tag", "-a", "v0.3.0", "-m", "v0.3.0")
	landing := func(n int, branches ...string) string {
		for _, b := range branches {
			g("checkout", "-q", "-b", b, "main")
			commit(strings.ReplaceAll(b, "/", "-"))
		}
		g("checkout", "-q", "-b", fmt.Sprintf("land/%d", n), "main")
		for i, b := range branches {
			g("merge", "-q", "--no-ff", b, "-m", fmt.Sprintf("Merge %s (#%d%d: change %s) into Landing %d", b, n, i, b, n))
		}
		g("checkout", "-q", "main")
		g("merge", "-q", "--no-ff", fmt.Sprintf("land/%d", n),
			"-m", fmt.Sprintf("Merge pull request #%d from o/land/%d", 100+n, n), "-m", fmt.Sprintf("Landing %d: things", n))
		return g("rev-parse", "HEAD")
	}
	r.landing1 = landing(1, "feat/11-thing", "fix/12-bug")
	r.head = landing(2, "fix/21-other")
	return r
}

// plan runs scripts/scheduled-release.sh in the repository, hours after
// v0.3.0, against the fake API, and returns its outputs and the notes.
func (r schedRepo) plan(t *testing.T, hours int, files map[string]string, env ...string) (map[string]string, string) {
	t.Helper()
	api := fakeAPI(t, files)
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "scheduled-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(t.TempDir(), "notes.md")
	output := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command("sh", script, notes)
	cmd.Dir = r.dir
	cmd.Env = append([]string{
		"PATH=" + api + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GITHUB_REPOSITORY=o/r", "MAIN_REF=main", "GITHUB_OUTPUT=" + output,
		"NOW=" + strconv.FormatInt(r.t0+int64(hours)*3600, 10),
	}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scheduled-release.sh: %v\n%s", err, out)
	}
	got := map[string]string{}
	b, _ := os.ReadFile(output)
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	n, _ := os.ReadFile(notes)
	return got, string(n)
}

// green makes these commits' push runs pass in full, newest first.
func green(shas ...string) map[string]string {
	files := map[string]string{}
	var main []map[string]any
	for i, sha := range shas {
		id := 100 + i
		main = append(main, pushRun(id, sha, "completed", "success"))
		files["push-"+sha+".json"] = runsJSON(pushRun(id, sha, "completed", "success"))
		files[fmt.Sprintf("jobs-%d.json", id)] = ciJobs(nil)
	}
	files["main.json"] = runsJSON(main...)
	return files
}

func TestScheduledReleaseReleasesMainsHeadAfter24Hours(t *testing.T) {
	r := newSchedRepo(t)
	got, notes := r.plan(t, 30, green(r.head, r.landing1))
	if got["release"] != "true" || got["sha"] != r.head || got["tag"] != "v0.4.0" || got["bump"] != "minor" {
		t.Fatalf("want v0.4.0 (a feat/ branch landed) at main's head, got %v", got)
	}
	if first, _, _ := strings.Cut(notes, "\n"); first != "<!-- cloudburrow-release-notes: generated -->" {
		t.Errorf("the notes do not start with the marker release-notes.sh looks for:\n%s", notes)
	}
	for _, want := range []string{
		"### Added\n\n- #10: change feat/11-thing (`feat/11-thing`, Landing 1)\n",
		"### Fixed\n\n- #20: change fix/21-other (`fix/21-other`, Landing 2)\n- #11: change fix/12-bug (`fix/12-bug`, Landing 1)\n",
		"### Landings\n\n- Landing 2: things (#102)\n- Landing 1: things (#101)\n",
		"https://github.com/o/r/compare/v0.3.0...v0.4.0",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes lack %q:\n%s", want, notes)
		}
	}
}

func TestScheduledReleaseTiming(t *testing.T) {
	r := newSchedRepo(t)
	// Main's head has no full run (a PR's ci-green only); landing 1 has one.
	onlyLanding1 := green(r.landing1)
	for _, c := range []struct {
		name    string
		hours   int
		files   map[string]string
		env     []string
		release bool
		sha     string
	}{
		{"under 24 hours", 10, green(r.head), nil, false, ""},
		{"under 24 hours, forced", 10, green(r.head), []string{"FORCE=true"}, true, r.head},
		{"24 to 48 hours, head not green yet", 30, onlyLanding1, nil, false, ""},
		{"48 hours, newest green commit", 50, onlyLanding1, nil, true, r.landing1},
		{"nothing green", 50, map[string]string{"main.json": runsJSON()}, nil, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := r.plan(t, c.hours, c.files, c.env...)
			if (got["release"] == "true") != c.release || (c.release && got["sha"] != c.sha) {
				t.Errorf("got %v, want release=%v sha=%s", got, c.release, c.sha)
			}
		})
	}
}

func TestScheduledReleaseVersions(t *testing.T) {
	r := newSchedRepo(t)
	if got, _ := r.plan(t, 50, green(r.head), "VERSION=v1.0.0"); got["tag"] != "v1.0.0" {
		t.Errorf("VERSION=v1.0.0: got %v", got)
	}
	// Only a fix/ branch since v0.4.0: a patch.
	gitIn(t, r.dir, []string{"GIT_COMMITTER_DATE=@" + strconv.FormatInt(r.t0, 10) + " +0000"}, "tag", "-a", "v0.4.0", "-m", "v0.4.0", r.landing1)
	if got, _ := r.plan(t, 50, green(r.head)); got["tag"] != "v0.4.1" || got["bump"] != "patch" {
		t.Errorf("fix only since v0.4.0: got %v, want v0.4.1", got)
	}
	// Already released.
	gitIn(t, r.dir, nil, "tag", "-a", "v0.4.1", "-m", "v0.4.1", r.head)
	if got, _ := r.plan(t, 50, green(r.head)); got["release"] != "false" {
		t.Errorf("an already tagged commit was released again: %v", got)
	}
}

// release-notes.sh takes a scheduled release's notes from its annotated tag
// when CHANGELOG.md has no section, verbatim (headings included) after the
// What's new section it adds, and only when the message carries the marker.
func TestReleaseNotesFromAGeneratedTag(t *testing.T) {
	r := newSchedRepo(t)
	if err := os.WriteFile(filepath.Join(r.dir, "CHANGELOG.md"), []byte("# Changelog\n\n## [0.3.0] - 2026-11-18\n\n- old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := filepath.Join(t.TempDir(), "msg")
	body := "CloudBurrow v0.4.0\n\n<!-- cloudburrow-release-notes: generated -->\nReleased automatically.\n\n### Added\n\n- a thing, see [docs](docs/x.md)\n"
	if err := os.WriteFile(msg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The API stores the message verbatim; git tag needs --cleanup=verbatim
	// to keep the "#" of a heading.
	gitIn(t, r.dir, nil, "tag", "-a", "--cleanup=verbatim", "-F", msg, "v0.4.0", r.head)
	gitIn(t, r.dir, nil, "tag", "-a", "-m", "v0.4.1", "v0.4.1", r.head)
	script, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "release-notes.sh"))
	run := func(tag string) (string, error) {
		cmd := exec.Command("sh", script, tag)
		cmd.Dir = r.dir
		cmd.Env = append(os.Environ(), "RELEASE_NOTES_LINK_BASE=https://github.com/o/r/blob/"+tag)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("v0.4.0")
	if err != nil {
		t.Fatalf("release-notes.sh v0.4.0: %v\n%s", err, out)
	}
	// No CHANGELOG.md is committed, so What's new is the landed branches'
	// issues, named by branch with no RELEASE_NOTES_REPO to read titles from.
	if want := "Released automatically.\n\n### What's new\n\nNo CHANGELOG.md entry was added since v0.3.0; these are the issues its landings closed:\n\n#### Other\n\n" +
		"- `feat/11-thing` (#11)\n- `fix/12-bug` (#12)\n- `fix/21-other` (#21)\n\n" +
		"### Added\n\n- a thing, see [docs](https://github.com/o/r/blob/v0.4.0/docs/x.md)\n"; out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if out, err := run("v0.4.1"); err == nil {
		t.Errorf("a tag without the marker gave notes:\n%s", out)
	}
	if out, err := run("v0.3.0"); err != nil || out != "- old\n" {
		t.Errorf("the CHANGELOG.md section no longer wins: %v\n%s", err, out)
	}
}
