package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runRetryFlaky sources .github/scripts/retry-flaky.sh the way the compat job does
// (bash, pipefail) and calls retry_flaky on a log with the given contents,
// re-running `retry` in place of go test. It returns the exit status and
// what the function printed, and the log after the call.
func runRetryFlaky(t *testing.T, logText, retry string) (int, string, string) {
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
