package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The macOS signing job (#605) can only really run on a tagged release with
// the maintainer's Apple secrets. What can be checked here is its place in
// the job graph, that it skips cleanly without the secrets, and that it
// cleans up after itself.

// signingSecrets are the repository secrets the maintainer adds, by the
// names docs/install.md gives.
var signingSecrets = []string{
	"MACOS_SIGNING_CERT_P12_BASE64",
	"MACOS_SIGNING_CERT_PASSWORD",
	"MACOS_SIGNING_IDENTITY",
	"APPLE_NOTARY_KEY_P8_BASE64",
	"APPLE_NOTARY_KEY_ID",
	"APPLE_NOTARY_ISSUER_ID",
}

// releaseJobs splits release.yml into its top-level jobs, by name, with
// comment lines dropped so that only what runs is checked.
func releaseJobs(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(raw), "\njobs:\n")
	if !ok {
		t.Fatal("release.yml has no jobs")
	}
	jobs := map[string]string{}
	header := regexp.MustCompile(`^  ([a-z0-9-]+):$`)
	name := ""
	for _, line := range strings.Split(body, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if name != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			jobs[name] += line + "\n"
		}
	}
	return jobs
}

func job(t *testing.T, jobs map[string]string, name string) string {
	t.Helper()
	j, ok := jobs[name]
	if !ok {
		t.Fatalf("release.yml has no %s job", name)
	}
	return j
}

// needs returns a job's needs list.
func needs(t *testing.T, j string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^    needs: (.+)$`).FindStringSubmatch(j)
	if m == nil {
		return nil
	}
	return strings.Split(strings.Trim(m[1], "[]"), ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestReleaseSignsDarwinBeforeChecksumsAndAttestation: build, then
// sign-macos, then publish, which alone computes checksums.txt, attests and
// renders the formula; so all three cover the signed archives.
func TestReleaseSignsDarwinBeforeChecksumsAndAttestation(t *testing.T) {
	jobs := releaseJobs(t)
	sign := job(t, jobs, "sign-macos")
	publish := job(t, jobs, "publish")

	if !contains(needs(t, sign), "build") {
		t.Errorf("sign-macos needs %v, want build", needs(t, sign))
	}
	if !contains(needs(t, publish), "sign-macos") {
		t.Errorf("publish needs %v, want sign-macos", needs(t, publish))
	}
	if !strings.Contains(sign, "runs-on: macos-latest") {
		t.Error("sign-macos does not run on macos-latest")
	}
	for _, want := range []string{
		"codesign --force --options runtime --timestamp",
		"xcrun notarytool submit",
		"--wait",
		"--key-id",
		"--issuer",
		"security create-keychain",
		"security delete-keychain",
		"overwrite: true",
		"name: cloudburrow-darwin-arm64",
		"name: cloudburrow-darwin-amd64",
	} {
		if !strings.Contains(sign, want) {
			t.Errorf("sign-macos does not contain %q", want)
		}
	}
	for _, s := range signingSecrets {
		if !strings.Contains(sign, "secrets."+s+" }}") {
			t.Errorf("sign-macos does not read secrets.%s", s)
		}
	}
	// The cleanup step runs whatever happened before it.
	if !regexp.MustCompile(`name: Delete the temporary keychain[^\n]*\n\s+if: always\(\)`).MatchString(sign) {
		t.Error("the keychain cleanup step is not if: always()")
	}

	// Checksums, attestation and formula are publish's alone: none may be
	// computed before signing.
	for name, j := range jobs {
		for _, marker := range []string{"checksums.txt", "attest-build-provenance", "render-formula.sh"} {
			if name == "publish" || !strings.Contains(j, marker) {
				continue
			}
			if marker == "attest-build-provenance" && name == "runtime-image-publish" {
				continue // the container image, not the archives
			}
			t.Errorf("job %s uses %s; only publish may, after signing", name, marker)
		}
	}
	for _, want := range []string{"checksums.txt", "attest-build-provenance", "render-formula.sh"} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish does not contain %q", want)
		}
	}
}

// pinnedRef matches an action or reusable workflow pinned to a full commit
// SHA, with the tag it stands for in a comment.
var pinnedRef = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\S+$`)

// isPinned reports whether a `uses:` reference is fixed to a commit. A local
// reference (./...) is this repository at the commit being run, so it is
// pinned by construction; anything else needs a full SHA.
func isPinned(ref string) bool {
	return strings.HasPrefix(ref, "./") || pinnedRef.MatchString(ref)
}

// TestReleaseActionsArePinnedBySHA: every action, including the signing
// job's, is pinned to a full commit SHA.
func TestReleaseActionsArePinnedBySHA(t *testing.T) {
	for name, j := range releaseJobs(t) {
		for _, line := range strings.Split(j, "\n") {
			_, ref, ok := strings.Cut(line, "uses: ")
			if ok && !isPinned(ref) {
				t.Errorf("job %s: %q is not pinned by commit SHA", name, strings.TrimSpace(line))
			}
		}
	}
}

// TestIsPinned: local references pass; a third-party action by tag, branch
// or short SHA does not.
func TestIsPinned(t *testing.T) {
	for ref, want := range map[string]bool{
		"./.github/workflows/action-selftest.yml": true,
		"./": true,
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1": true,
		"actions/checkout@v7":                                       false,
		"actions/checkout@v7.0.1":                                   false,
		"actions/checkout@main":                                     false,
		"actions/checkout@3d3c42e":                                  false,
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1": false, // no tag comment
		"owner/repo/.github/workflows/x.yml@v1":                     false,
		"../elsewhere":                                              false,
	} {
		if got := isPinned(ref); got != want {
			t.Errorf("isPinned(%q) = %v, want %v", ref, got, want)
		}
	}
}

// secretsCheck extracts the sign-macos step that decides whether to sign.
func secretsCheck(t *testing.T) string {
	t.Helper()
	sign := job(t, releaseJobs(t), "sign-macos")
	_, step, ok := strings.Cut(sign, "        id: secrets\n")
	if !ok {
		t.Fatal("sign-macos has no step with id: secrets")
	}
	_, script, ok := strings.Cut(step, "        run: |\n")
	if !ok {
		t.Fatal("the secrets step has no run block")
	}
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		b.WriteString(strings.TrimPrefix(line, "          ") + "\n")
	}
	return b.String()
}

// runSecretsCheck runs the step as Actions would on macOS (bash -e), with
// the given secrets set, returning its output, GITHUB_OUTPUT and error.
func runSecretsCheck(t *testing.T, set map[string]string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	out := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command("bash", "-e", "-c", secretsCheck(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + out}
	for k, v := range set {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, err := cmd.CombinedOutput()
	o, _ := os.ReadFile(out)
	return string(stdout), string(o), err
}

// TestSigningSkipsWithoutSecrets: a fork, or this repository before the
// maintainer adds the secrets, releases unsigned with a notice.
func TestSigningSkipsWithoutSecrets(t *testing.T) {
	stdout, output, err := runSecretsCheck(t, map[string]string{"MACOS_SIGNING_IDENTITY": ""})
	if err != nil {
		t.Fatalf("the check failed without secrets: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "::notice::") {
		t.Errorf("no ::notice:: when skipping:\n%s", stdout)
	}
	if output != "present=false\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want present=false", output)
	}

	// Every later step is gated on the check, except the cleanup.
	sign := job(t, releaseJobs(t), "sign-macos")
	steps := strings.Split(sign, "\n      - ")[1:]
	for _, s := range steps {
		if strings.Contains(s, "id: secrets") || strings.Contains(s, "if: always()") {
			continue
		}
		if !strings.Contains(s, "if: steps.secrets.outputs.present == 'true'") {
			t.Errorf("a sign-macos step runs without the secrets:\n%s", s)
		}
	}
}

// TestSigningWithAllSecrets: all six set means sign.
func TestSigningWithAllSecrets(t *testing.T) {
	set := map[string]string{}
	for _, s := range signingSecrets {
		set[s] = "x"
	}
	stdout, output, err := runSecretsCheck(t, set)
	if err != nil {
		t.Fatalf("the check failed with every secret: %v\n%s", err, stdout)
	}
	if output != "present=true\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want present=true", output)
	}
}

// TestSigningRefusesPartialSecrets: some but not all is a misconfiguration
// that fails the release rather than quietly shipping unsigned binaries.
func TestSigningRefusesPartialSecrets(t *testing.T) {
	stdout, output, err := runSecretsCheck(t, map[string]string{
		"MACOS_SIGNING_CERT_P12_BASE64": "x",
		"MACOS_SIGNING_IDENTITY":        "x",
	})
	if err == nil {
		t.Fatalf("a partial set of secrets was accepted:\n%s", stdout)
	}
	if !strings.Contains(stdout, "::error::") || !strings.Contains(stdout, "APPLE_NOTARY_KEY_ID") {
		t.Errorf("the error does not name what is missing:\n%s", stdout)
	}
	if output != "" {
		t.Errorf("GITHUB_OUTPUT = %q, want nothing", output)
	}
}

// The CI gate (#596): a release is built only from a commit whose ci-green
// check passed. It runs against the GitHub API, so here its place in the job
// graph is checked, and its script is run with a fake gh on PATH.

const gateJob = "require-ci-green"

// TestReleaseGateRunsFirst: every other job, build and publish included,
// waits on the gate, so an untested commit fails before anything is built,
// pushed or published.
func TestReleaseGateRunsFirst(t *testing.T) {
	jobs := releaseJobs(t)
	gate := job(t, jobs, gateJob)

	if n := needs(t, gate); len(n) != 0 {
		t.Errorf("%s needs %v; it must run first", gateJob, n)
	}
	if !strings.Contains(gate, "run: sh scripts/require-ci-green.sh \"${GITHUB_SHA}\"") {
		t.Errorf("%s does not run scripts/require-ci-green.sh on GITHUB_SHA", gateJob)
	}
	if !strings.Contains(gate, "GH_TOKEN: ${{ github.token }}") {
		t.Errorf("%s does not give gh the workflow token", gateJob)
	}
	for _, want := range []string{"checks: read", "actions: read", "contents: read"} {
		if !strings.Contains(gate, want) {
			t.Errorf("%s lacks permission %q", gateJob, want)
		}
	}
	if strings.Contains(gate, ": write") {
		t.Errorf("%s asks for write access", gateJob)
	}
	if !contains(needs(t, job(t, jobs, "publish")), gateJob) {
		t.Errorf("publish needs %v, want %s", needs(t, job(t, jobs, "publish")), gateJob)
	}

	// Reachability over needs: every job depends on the gate, directly or
	// through the jobs it needs.
	var reaches func(name string, seen map[string]bool) bool
	reaches = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		for _, n := range needs(t, job(t, jobs, name)) {
			if n == gateJob || reaches(n, seen) {
				return true
			}
		}
		return false
	}
	for name := range jobs {
		if name != gateJob && !reaches(name, map[string]bool{}) {
			t.Errorf("job %s does not wait on %s", name, gateJob)
		}
	}
	if !contains(needs(t, job(t, jobs, "verify")), gateJob) {
		t.Errorf("verify needs %v, want %s", needs(t, job(t, jobs, "verify")), gateJob)
	}
}

const gateSHA = "0123456789abcdef0123456789abcdef01234567"

// fakeGH is a gh that answers `gh api <path> --jq <expr>` with the next of
// the canned JSON responses for that endpoint (the last one repeats),
// filtered through jq as gh would. A response of "FAIL" exits 1.
const fakeGH = `#!/bin/sh
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
*/check-runs\?*) kind=checks ;;
*/actions/runs\?*) kind=runs ;;
*) echo "fake gh: unexpected path $path" >&2; exit 2 ;;
esac
n=$(($(cat "$dir/$kind.n" 2>/dev/null || echo 0) + 1))
echo "$n" > "$dir/$kind.n"
max=$(cat "$dir/$kind.max")
[ "$n" -le "$max" ] || n="$max"
if [ "$(cat "$dir/$kind.$n")" = FAIL ]; then echo "fake gh: HTTP 502" >&2; exit 1; fi
jq -r "$expr" < "$dir/$kind.$n"
`

// checkRun is one check run as the API returns it.
func checkRun(status, conclusion, app string) string {
	c := "null"
	if conclusion != "" {
		c = `"` + conclusion + `"`
	}
	return `{"name":"ci-green","status":"` + status + `","conclusion":` + c +
		`,"html_url":"https://github.com/o/r/runs/1","app":{"slug":"` + app + `"}}`
}

func checkRuns(runs ...string) string {
	return `{"total_count":` + strconv.Itoa(len(runs)) + `,"check_runs":[` + strings.Join(runs, ",") + `]}`
}

func workflowRuns(statuses ...string) string {
	var rs []string
	for _, s := range statuses {
		rs = append(rs, `{"name":"CI","path":".github/workflows/ci.yml","status":"`+s+`"}`)
	}
	// A run of another workflow for the same commit is never CI's.
	rs = append(rs, `{"name":"Release","path":".github/workflows/release.yml","status":"in_progress"}`)
	return `{"workflow_runs":[` + strings.Join(rs, ",") + `]}`
}

// runGate runs scripts/require-ci-green.sh against the fake gh, polling
// every second for up to timeout seconds. It returns the output, the number
// of check-runs requests made, and the error.
func runGate(t *testing.T, timeout int, checks, runs []string) (string, int, error) {
	t.Helper()
	for _, tool := range []string{"sh", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("gh", fakeGH, 0o755)
	if len(runs) == 0 {
		runs = []string{workflowRuns()}
	}
	for kind, rs := range map[string][]string{"checks": checks, "runs": runs} {
		write(kind+".max", strconv.Itoa(len(rs)), 0o644)
		for i, r := range rs {
			write(kind+"."+strconv.Itoa(i+1), r, 0o644)
		}
	}
	cmd := exec.Command("sh", filepath.Join("..", "..", "scripts", "require-ci-green.sh"), gateSHA)
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_REPOSITORY=cloudburrow/cloudburrow",
		"CI_GREEN_INTERVAL=1",
		"CI_GREEN_TIMEOUT=" + strconv.Itoa(timeout),
	}
	out, err := cmd.CombinedOutput()
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "repos/cloudburrow/cloudburrow/commits/"+gateSHA+"/check-runs?check_name=ci-green") {
		t.Errorf("gh was not asked for this commit's ci-green check runs:\n%s", calls)
	}
	n, _ := os.ReadFile(filepath.Join(dir, "checks.n"))
	polls, _ := strconv.Atoi(strings.TrimSpace(string(n)))
	return string(out), polls, err
}

// TestReleaseGatePassesOnSuccessfulCIGreen: a merged commit's merge-queue
// run passed; its push run on main was cancelled by the next merge. One
// pass on the commit is enough.
func TestReleaseGatePassesOnSuccessfulCIGreen(t *testing.T) {
	out, polls, err := runGate(t, 5, []string{checkRuns(
		checkRun("completed", "cancelled", "github-actions"),
		checkRun("completed", "success", "github-actions"),
	)}, nil)
	if err != nil {
		t.Fatalf("the gate refused a commit with a successful ci-green: %v\n%s", err, out)
	}
	if polls != 1 {
		t.Errorf("polled %d times, want 1", polls)
	}
}

// TestReleaseGateRefusesFailedCIGreen: every ci-green finished and none
// passed. Nothing is running, so it fails at once rather than waiting.
func TestReleaseGateRefusesFailedCIGreen(t *testing.T) {
	out, polls, err := runGate(t, 30, []string{checkRuns(
		checkRun("completed", "failure", "github-actions"),
		checkRun("completed", "cancelled", "github-actions"),
	)}, nil)
	if err == nil {
		t.Fatalf("the gate accepted a commit whose ci-green failed:\n%s", out)
	}
	if !strings.Contains(out, "::error::") || !strings.Contains(out, gateSHA) {
		t.Errorf("the error does not name the SHA:\n%s", out)
	}
	if polls != 1 {
		t.Errorf("polled %d times; a finished failure must not be waited on", polls)
	}
}

// TestReleaseGateRefusesUntestedCommit: no ci-green and no CI run at all,
// as for a manual dispatch on a commit CI never ran on. A ci-green from
// another app does not count.
func TestReleaseGateRefusesUntestedCommit(t *testing.T) {
	out, polls, err := runGate(t, 30,
		[]string{checkRuns(checkRun("completed", "success", "some-other-app"))},
		[]string{workflowRuns()})
	if err == nil {
		t.Fatalf("the gate accepted a commit CI never ran on:\n%s", out)
	}
	if !strings.Contains(out, "::error::") || !strings.Contains(out, gateSHA) ||
		!strings.Contains(out, "no ci-green check run") {
		t.Errorf("the error does not name the SHA and what is missing:\n%s", out)
	}
	if polls != 1 {
		t.Errorf("polled %d times; an untested commit must not be waited on", polls)
	}
}

// TestReleaseGateWaitsForRunningCI: a tag pushed while CI is still running
// on the commit, first before ci-green exists, then while it runs, waits
// for it to pass. A transient API error is retried.
func TestReleaseGateWaitsForRunningCI(t *testing.T) {
	out, polls, err := runGate(t, 30, []string{
		checkRuns(),
		"FAIL",
		checkRuns(checkRun("in_progress", "", "github-actions")),
		checkRuns(checkRun("completed", "success", "github-actions")),
	}, []string{workflowRuns("in_progress")})
	if err != nil {
		t.Fatalf("the gate did not wait for running CI to pass: %v\n%s", err, out)
	}
	if polls != 4 {
		t.Errorf("polled %d times, want 4", polls)
	}
	if !strings.Contains(out, "still running") {
		t.Errorf("no progress line while waiting:\n%s", out)
	}
}

// TestReleaseGateTimesOut: CI that never finishes fails the gate once the
// timeout passes, naming the SHA.
func TestReleaseGateTimesOut(t *testing.T) {
	out, polls, err := runGate(t, 2,
		[]string{checkRuns(checkRun("queued", "", "github-actions"))}, nil)
	if err == nil {
		t.Fatalf("the gate passed while CI never finished:\n%s", out)
	}
	if !strings.Contains(out, "::error::") || !strings.Contains(out, gateSHA) ||
		!strings.Contains(out, "after 2s") {
		t.Errorf("the timeout error does not name the SHA and the wait:\n%s", out)
	}
	if polls != 3 {
		t.Errorf("polled %d times, want 3 (at 0s, 1s and 2s)", polls)
	}
}
