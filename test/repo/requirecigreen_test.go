package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/require-ci-green.sh --main-push decides which commit the scheduled
// release ships. On 2026-11-23 it refused main's head fc4d70d1, whose CI run
// 45202563361 had passed in full on its third attempt: listing that run's
// jobs answered HTTP 502 (GitHub's 10s limit; the endpoint takes 5-11s for a
// run with several attempts), and with CI_GREEN_TIMEOUT=0 one failure was
// final. These tests run the script against a fake gh.

// fakeGH answers the three listings --main-push makes. Each key (runs, jobs,
// suite) prints the file of that name; a "<key>.fail" file holding N makes
// the first N calls fail with HTTP 502. Calls are logged, one URL a line.
const fakeGH = `#!/bin/sh
for a; do case "$a" in repos/*) url="$a" ;; esac; done
case "$url" in
*/actions/workflows/ci.yml/runs*) key=runs ;;
*/actions/runs/*/jobs*) key=jobs ;;
*/check-suites/*/check-runs*) key=suite ;;
*) echo "fake gh: unexpected $url" >&2; exit 2 ;;
esac
echo "$key $url" >>"$FAKE_GH/calls"
n=$(grep -c "^$key " "$FAKE_GH/calls")
if [ -f "$FAKE_GH/$key.fail" ] && [ "$n" -le "$(cat "$FAKE_GH/$key.fail")" ]; then
	echo '{ "message": "Server Error" }'
	echo 'gh: Server Error (HTTP 502)' >&2
	exit 1
fi
cat "$FAKE_GH/$key"
`

// A run that passed in full, as "<conclusion><TAB><name>" lines.
const fullPassJobs = "success\tdetect changes\n" +
	"success\tofficial SDK compatibility (storage)\n" +
	"success\tevery compat test ran in a shard\n" +
	"success\tintegration (cluster)\n" +
	"skipped\treport main failure\n" +
	"success\tci-green\n"

// runRequireCIGreen runs require-ci-green.sh --main-push with CI_GREEN_TIMEOUT=0,
// as scheduled-release.sh does, against a fake gh fed files. It returns the
// exit status, the output and the fake's call log.
func runRequireCIGreen(t *testing.T, files map[string]string) (int, string, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "require-ci-green.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bin, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(data, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(sh, script, "--main-push", "fc4d70d1f77df1ffd739fdc8535b335b0c670384")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GH="+data,
		"GITHUB_REPOSITORY=cloudburrow/cloudburrow",
		"CI_GREEN_TIMEOUT=0",
		"CI_GREEN_PAUSE=0",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(data, "calls"))
	return code, string(out), string(calls)
}

const passedRun = "45202563361 completed success https://github.com/cloudburrow/cloudburrow/actions/runs/45202563361 122320165880\n"

// When the jobs endpoint answers 502, the run's check suite's latest check
// runs stand in for its jobs, and gh's error is in the warning.
func TestRequireCIGreenFallsBackToTheCheckSuite(t *testing.T) {
	code, out, calls := runRequireCIGreen(t, map[string]string{
		"runs":      passedRun,
		"jobs.fail": "99",
		"suite":     fullPassJobs,
	})
	if code != 0 || !strings.Contains(out, "main CI passed in full") {
		t.Fatalf("require-ci-green.sh = %d, want a pass from the check suite:\n%s", code, out)
	}
	if !strings.Contains(out, "HTTP 502") {
		t.Errorf("the warning does not carry gh's error:\n%s", out)
	}
	if !strings.Contains(calls, "check-suites/122320165880/check-runs?filter=latest") {
		t.Errorf("the check suite's latest check runs were not listed:\n%s", calls)
	}
}

// Without a check suite, a listing that fails twice is tried again, even
// with CI_GREEN_TIMEOUT=0.
func TestRequireCIGreenRetriesTheJobsListing(t *testing.T) {
	code, out, calls := runRequireCIGreen(t, map[string]string{
		"runs":      strings.Replace(passedRun, " 122320165880", " none", 1),
		"jobs.fail": "2",
		"jobs":      fullPassJobs,
	})
	if code != 0 || !strings.Contains(out, "main CI passed in full") {
		t.Fatalf("require-ci-green.sh = %d, want a pass on the third try:\n%s", code, out)
	}
	if n := strings.Count(calls, "jobs "); n != 3 {
		t.Errorf("jobs listed %d times, want 3:\n%s", n, calls)
	}
}

// A run that cannot be listed at all is refused after three tries, not
// passed, and says why.
func TestRequireCIGreenRefusesAnUnlistableRun(t *testing.T) {
	code, out, calls := runRequireCIGreen(t, map[string]string{
		"runs":       passedRun,
		"jobs.fail":  "99",
		"suite.fail": "99",
	})
	if code == 0 {
		t.Fatalf("require-ci-green.sh passed a run whose jobs it could not list:\n%s", out)
	}
	if !strings.Contains(out, "try 3 of 3") || !strings.Contains(out, "HTTP 502") {
		t.Errorf("the output does not show three tries with gh's error:\n%s", out)
	}
	if n := strings.Count(calls, "jobs "); n != 3 {
		t.Errorf("jobs listed %d times, want 3:\n%s", n, calls)
	}
}

// The check suite's check runs count only as a full pass, like the jobs: a
// failed latest attempt is still refused.
func TestRequireCIGreenRefusesAFailedCheckSuite(t *testing.T) {
	code, out, _ := runRequireCIGreen(t, map[string]string{
		"runs":      passedRun,
		"jobs.fail": "99",
		"suite":     strings.Replace(fullPassJobs, "success\tintegration (cluster)", "failure\tintegration (cluster)", 1),
	})
	if code == 0 || !strings.Contains(out, "integration (cluster): failure") {
		t.Fatalf("require-ci-green.sh = %d, want a refusal naming the failed job:\n%s", code, out)
	}
}
