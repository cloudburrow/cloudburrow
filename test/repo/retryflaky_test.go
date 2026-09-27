package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// runRetryFlaky sources .github/scripts/retry-flaky.sh the way the compat job does
// (bash, pipefail) and calls retry_flaky on a log with the given contents,
// re-running `retry` in place of go test. It returns the exit status and
// what the function printed, and the log after the call. GITHUB_STEP_SUMMARY
// is cleared, so a run in CI never writes to the real job summary.
func runRetryFlaky(t *testing.T, logText, retry string) (int, string, string) {
	t.Helper()
	return runRetryFlakyWithSummary(t, logText, retry, "")
}

// runRetryFlakyWithSummary is runRetryFlaky with GITHUB_STEP_SUMMARY set to
// summary.
func runRetryFlakyWithSummary(t *testing.T, logText, retry, summary string) (int, string, string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "retry-flaky.sh"))
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "compat.log")
	if err := os.WriteFile(log, []byte(logText), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", `set -o pipefail; . "$1"; retry_flaky "$2" `+retry, "retry", script, log)
	cmd.Env = append(os.Environ(), "GITHUB_STEP_SUMMARY="+summary)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(log)
	return code, string(out), string(after)
}

// A go test that exits non-zero without naming a failed test is a
// package-level failure, such as the -timeout panic. retry_flaky used to
// find no test to retry and return 0, so a timed-out compat shard reported
// green (#675).
func TestRetryFlakyFailsAPackageLevelFailure(t *testing.T) {
	timeout := "=== RUN   TestStorageBucketLifecycle\n" +
		"panic: test timed out after 25m0s\n" +
		"\trunning tests:\n\t\tTestStorageBucketLifecycle (25m0s)\n" +
		"FAIL\tgithub.com/cloudburrow/cloudburrow/test/compat\t1500.012s\n"
	code, out, _ := runRetryFlaky(t, timeout, "true")
	if code == 0 {
		t.Fatalf("retry_flaky passed a timed-out run:\n%s", out)
	}
	if !strings.Contains(out, "package-level failure") {
		t.Errorf("retry_flaky did not say the failure was package-level:\n%s", out)
	}
}

// A failure outside the allow-list fails at once, and nothing is re-run.
func TestRetryFlakyFailsARealFailure(t *testing.T) {
	code, out, _ := runRetryFlaky(t, "--- FAIL: TestStorageBucketLifecycle (0.10s)\nFAIL\n", "false")
	if code == 0 || !strings.Contains(out, "outside the flaky allow-list") {
		t.Fatalf("retry_flaky = %d for a real failure:\n%s", code, out)
	}
}

// Only allow-listed failures are retried once, the retry is recorded in the
// log, and its own result decides the outcome.
func TestRetryFlakyRetriesAnAllowListedFailureOnce(t *testing.T) {
	flaky := "--- FAIL: TestMemorystoreAcrossRestart (3.00s)\nFAIL\n"
	code, out, after := runRetryFlaky(t, flaky, "true")
	if code != 0 {
		t.Fatalf("retry_flaky = %d after a passing retry:\n%s", code, out)
	}
	if !strings.Contains(after, "RETRIED: TestMemorystoreAcrossRestart") {
		t.Errorf("the retry was not recorded in the log:\n%s", after)
	}
	if code, out, _ := runRetryFlaky(t, flaky, "false"); code == 0 {
		t.Errorf("retry_flaky passed when the retry failed too:\n%s", out)
	}
}

// Every retry is visible on the run page: a ::warning:: annotation naming the
// test, and a job-summary line with the retry's outcome (#704).
func TestRetryFlakyAnnotatesEveryRetry(t *testing.T) {
	flaky := "--- FAIL: TestMemorystoreAcrossRestart (3.00s)\n--- FAIL: TestDatastoreAcrossRestart (2.00s)\nFAIL\n"
	for _, c := range []struct {
		retry, outcome string
		code           int
	}{
		{"true", "passed on the retry", 0},
		{"false", "failed again", 1},
	} {
		summary := filepath.Join(t.TempDir(), "summary.md")
		code, out, _ := runRetryFlakyWithSummary(t, flaky, c.retry, summary)
		if code != c.code {
			t.Errorf("retry_flaky = %d with retry %q, want %d:\n%s", code, c.retry, c.code, out)
		}
		b, err := os.ReadFile(summary)
		if err != nil {
			t.Fatalf("no job summary was written: %v", err)
		}
		for _, name := range []string{"TestMemorystoreAcrossRestart", "TestDatastoreAcrossRestart"} {
			if !strings.Contains(out, "::warning title=Flaky test retried::"+name+" ") {
				t.Errorf("no ::warning:: annotation for %s:\n%s", name, out)
			}
			if line := "- Retried once (known flaky): `" + name + "`, " + c.outcome; !strings.Contains(string(b), line) {
				t.Errorf("the job summary lacks %q:\n%s", line, b)
			}
		}
	}
}

// TestRetryAllowListMatchesTheDocs (#704): the tests CI retries once are the
// tests docs/compatibility.md lists under "Retried once", no more and no fewer.
func TestRetryAllowListMatchesTheDocs(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", ".github", "scripts", "retry-flaky.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*local allow='\^Test\(([A-Za-z0-9|]+)\)\$'$`).FindSubmatch(script)
	if m == nil {
		t.Fatal("retry-flaky.sh has no `local allow='^Test(...)$'` line")
	}
	var allowed []string
	for _, n := range strings.Split(string(m[1]), "|") {
		allowed = append(allowed, "Test"+n)
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "compatibility.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "\n#### Retried once\n")
	if !ok {
		t.Fatal(`docs/compatibility.md has no "#### Retried once" section`)
	}
	if i := strings.Index(section, "\n#"); i >= 0 {
		section = section[:i]
	}
	var documented []string
	for _, m := range regexp.MustCompile("(?m)^\\| `(Test[A-Za-z0-9]+)` \\|").FindAllStringSubmatch(section, -1) {
		documented = append(documented, m[1])
	}

	sort.Strings(allowed)
	sort.Strings(documented)
	if strings.Join(allowed, " ") != strings.Join(documented, " ") {
		t.Errorf("the retry allow-list and docs/compatibility.md differ:\n  retry-flaky.sh:   %v\n  compatibility.md: %v", allowed, documented)
	}
}

// TestNoWorkflowRunsRetryFlakyThroughEnv: retry_flaky is a shell function,
// and env(1) can only run programs, so `env VAR=... retry_flaky ...` fails
// with "No such file or directory" and a flaky test is never retried. The
// restart probes did exactly that until #705's local run noticed.
func TestNoWorkflowRunsRetryFlakyThroughEnv(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	scripts, _ := filepath.Glob(filepath.Join("..", "..", "scripts", "*.sh"))
	for _, f := range append(files, scripts...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Join continuation lines, so `env ... \` + newline + `retry_flaky` counts.
		text := strings.ReplaceAll(string(b), "\\\n", " ")
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			for i, w := range fields {
				if w == "env" {
					for _, rest := range fields[i+1:] {
						if rest == "retry_flaky" {
							t.Errorf("%s runs retry_flaky through env(1), which cannot run a shell function: %s", f, strings.TrimSpace(line))
						}
					}
				}
			}
		}
	}
}
