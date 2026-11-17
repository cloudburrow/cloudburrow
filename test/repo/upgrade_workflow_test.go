package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Upgrade test workflow (#1714) runs test/upgrade nightly: 11 minutes of
// a runner, two instances and the older release's published binary. It
// can only really run on GitHub. What can be checked here is its shape: when
// it runs, where the older release comes from, that a skipped test cannot
// pass it, and that nothing in it can run unbounded, unpinned or with more
// than read access.

const upgradeWorkflow = "upgrade.yml"

func upgradeJobs(t *testing.T) (string, string) {
	t.Helper()
	on, jobs := workflowJobs(t, upgradeWorkflow)
	all := ""
	for _, j := range jobs {
		all += j
	}
	return on, all
}

func TestUpgradeWorkflowRunsNightlyAndByHandOnly(t *testing.T) {
	on, jobs := workflowJobs(t, upgradeWorkflow)
	for _, want := range []string{"schedule:", "cron:", "workflow_dispatch:"} {
		if !strings.Contains(on, want) {
			t.Errorf("%s's triggers have no %s:\n%s", upgradeWorkflow, want, on)
		}
	}
	// PR CI runs fast jobs only, and a runner per landing on main is not
	// worth it: nightly and by hand.
	for _, never := range []string{"pull_request", "merge_group", "push"} {
		if strings.Contains(on, never) {
			t.Errorf("%s runs on %s; it is nightly and by hand only", upgradeWorkflow, never)
		}
	}
	if len(jobs) == 0 {
		t.Fatalf("%s has no jobs", upgradeWorkflow)
	}
	runsOn := regexp.MustCompile(`(?m)^    runs-on: ubuntu-latest$`)
	timeout := regexp.MustCompile(`(?m)^    timeout-minutes: ([1-9][0-9]*)$`)
	goTimeout := regexp.MustCompile(`go test [^\n]*-timeout ([0-9]+)m`)
	for name, j := range jobs {
		if !runsOn.MatchString(j) {
			t.Errorf("job %s does not run on ubuntu-latest", name)
		}
		m := timeout.FindStringSubmatch(j)
		if m == nil {
			t.Errorf("job %s has no timeout-minutes", name)
			continue
		}
		// go test's own deadline must fire first, so a hung test ends with
		// its goroutine dump and the test's cleanup, not a cancelled job.
		job, _ := strconv.Atoi(m[1])
		for _, g := range goTimeout.FindAllStringSubmatch(j, -1) {
			if n, _ := strconv.Atoi(g[1]); n >= job {
				t.Errorf("job %s: go test -timeout %sm is not below timeout-minutes %s", name, g[1], m[1])
			}
		}
	}
	if !strings.Contains(readWorkflow(t, upgradeWorkflow), "\nconcurrency:\n  group: upgrade-${{ github.ref }}\n  cancel-in-progress: true\n") {
		t.Errorf("%s has no per-ref concurrency group", upgradeWorkflow)
	}
}

// The older release is the one test/upgrade names, downloaded from this
// repository's own GitHub release with the run's token and checked against
// the release's checksums, never fetched from anywhere else.
func TestUpgradeWorkflowDownloadsTheReleaseTheTestNames(t *testing.T) {
	_, all := upgradeJobs(t)
	for _, want := range []string{
		"GH_TOKEN: ${{ github.token }}",
		`gh release download "$OLDER" --repo "$GITHUB_REPOSITORY" --pattern "$ARCHIVE" --pattern checksums.txt`,
		`ARCHIVE="cloudburrow_${OLDER}_linux_amd64.tar.gz"`,
		"sha256sum -c -",
		`CLOUDBURROW_UPGRADE_OLD_BIN=`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("%s does not contain %q", upgradeWorkflow, want)
		}
	}
	if strings.Contains(all, "releases/download") {
		t.Errorf("%s downloads a release by URL; use gh release download", upgradeWorkflow)
	}
	// The checksum is checked before the archive is unpacked.
	if c, x := strings.Index(all, "sha256sum -c -"), strings.Index(all, `tar -xzf "$ARCHIVE"`); x < 0 || c > x {
		t.Errorf("%s does not check the archive's checksum before unpacking it", upgradeWorkflow)
	}

	// The step's own line reads the tag from the test, so moving
	// OlderRelease forward moves the workflow with it. Run it.
	line := regexp.MustCompile(`(?m)^\s*(OLDER=\$\(sed .+\))$`).FindStringSubmatch(all)
	if line == nil {
		t.Fatalf("%s does not read OLDER from test/upgrade", upgradeWorkflow)
	}
	src, err := os.ReadFile(filepath.Join("..", "upgrade", "upgrade_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`(?m)^const OlderRelease = "(v[^"]+)"$`).FindStringSubmatch(string(src))
	if want == nil {
		t.Fatal("test/upgrade/upgrade_test.go declares no OlderRelease")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	cmd := exec.Command("bash", "-c", line[1]+`; printf %s "$OLDER"`)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", line[1], err)
	}
	if string(out) != want[1] {
		t.Errorf("the workflow reads OLDER=%q from test/upgrade; the test names %s", out, want[1])
	}
}

// It runs the one end-to-end test, and a skip (the test skips without its
// binary) does not pass it.
func TestUpgradeWorkflowRunsTheTestAndRequiresItToPass(t *testing.T) {
	_, all := upgradeJobs(t)
	for _, want := range []string{
		"make build",
		`CLOUDBURROW_UPGRADE_BIN="$PWD/bin/cloudburrow"`,
		"go test -tags upgrade -count=1",
		"-run '^TestUpgradeFromOlderRelease$' ./test/upgrade/",
		"grep -q -- '^--- PASS: TestUpgradeFromOlderRelease (' upgrade.log",
		"CLOUDBURROW_UPGRADE_LOGS:",
		// Diagnostics on failure, and no cluster left behind.
		"name: upgrade-diagnostics",
		"kind export logs",
		"kind delete cluster",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("%s does not contain %q", upgradeWorkflow, want)
		}
	}
	src, err := os.ReadFile(filepath.Join("..", "upgrade", "upgrade_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func TestUpgradeFromOlderRelease(t *testing.T)",
		`os.Getenv("CLOUDBURROW_UPGRADE_BIN")`,
		`os.Getenv("CLOUDBURROW_UPGRADE_OLD_BIN")`,
		`os.Getenv("CLOUDBURROW_UPGRADE_LOGS")`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("test/upgrade/upgrade_test.go no longer has %s, which %s relies on", want, upgradeWorkflow)
		}
	}
}

// Every action is pinned by SHA, by the SHA ci.yml pins, and the workflow
// asks for nothing beyond reading the repository.
func TestUpgradeWorkflowIsPinnedAndReadOnly(t *testing.T) {
	uses := regexp.MustCompile(`(?m)^\s+- uses: ([^@\s]+)@(\S+)`)
	ci := map[string]string{}
	for _, m := range uses.FindAllStringSubmatch(readWorkflow(t, "ci.yml"), -1) {
		ci[m[1]] = m[2]
	}
	body := readWorkflow(t, upgradeWorkflow)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	found := uses.FindAllStringSubmatch(body, -1)
	if len(found) == 0 {
		t.Fatalf("%s uses no actions", upgradeWorkflow)
	}
	for _, m := range found {
		if !sha.MatchString(m[2]) {
			t.Errorf("%s is not pinned by SHA: %s", m[1], m[2])
		}
		if want, ok := ci[m[1]]; ok && want != m[2] {
			t.Errorf("%s is pinned at %s; ci.yml pins %s", m[1], m[2], want)
		}
	}
	if !strings.Contains(body, "\npermissions:\n  contents: read\n\n") {
		t.Errorf("%s does not set top-level permissions to contents: read", upgradeWorkflow)
	}
	if n := strings.Count(body, "permissions:"); n != 1 {
		t.Errorf("%s has %d permissions blocks; want the one top-level contents: read", upgradeWorkflow, n)
	}
	if strings.Contains(body, "secrets.") {
		t.Errorf("%s reads a secret; it needs only the run's token", upgradeWorkflow)
	}
}
