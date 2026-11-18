package repo

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Cloud SQL ephemeral nightly (#1752) runs ci.yml's sql compat shard on
// an instance in --mode ephemeral, so the per-server volume of #1740 stays
// tested without slowing PR CI. It can only really run on GitHub; what is
// checked here is its shape: nightly and by hand only, the shard from
// scripts/compat-shards.sh in ephemeral mode, a skip of the tests #1740
// fixed does not pass it, and nothing in it is unbounded, unpinned or more
// than read-only.

const nightlySQLWorkflow = "nightly-sql-ephemeral.yml"

func TestNightlySQLEphemeralRunsNightlyAndByHandOnly(t *testing.T) {
	on, jobs := workflowJobs(t, nightlySQLWorkflow)
	for _, want := range []string{"schedule:", "cron:", "workflow_dispatch:"} {
		if !strings.Contains(on, want) {
			t.Errorf("%s's triggers have no %s:\n%s", nightlySQLWorkflow, want, on)
		}
	}
	// PR CI must not change, and a runner per landing is not worth it.
	for _, never := range []string{"pull_request", "merge_group", "push"} {
		if strings.Contains(on, never) {
			t.Errorf("%s runs on %s; it is nightly and by hand only", nightlySQLWorkflow, never)
		}
	}
	if len(jobs) == 0 {
		t.Fatalf("%s has no jobs", nightlySQLWorkflow)
	}
	timeout := regexp.MustCompile(`(?m)^    timeout-minutes: ([1-9][0-9]*)$`)
	goTimeout := regexp.MustCompile(`go test [^\n]*-timeout ([0-9]+)m`)
	for name, j := range jobs {
		m := timeout.FindStringSubmatch(j)
		if m == nil {
			t.Errorf("job %s has no timeout-minutes", name)
			continue
		}
		job, _ := strconv.Atoi(m[1])
		for _, g := range goTimeout.FindAllStringSubmatch(j, -1) {
			if n, _ := strconv.Atoi(g[1]); n >= job {
				t.Errorf("job %s: go test -timeout %sm is not below timeout-minutes %s", name, g[1], m[1])
			}
		}
	}
}

// It runs the sql shard as scripts/compat-shards.sh defines it, on an
// ephemeral instance, and the tests #1740 fixed must pass, not skip.
func TestNightlySQLEphemeralRunsTheSQLShardEphemeral(t *testing.T) {
	_, jobs := workflowJobs(t, nightlySQLWorkflow)
	all := ""
	for _, j := range jobs {
		all += j
	}
	for _, want := range []string{
		"SHARD: sql",
		". scripts/compat-shards.sh",
		`compat_shard "$SHARD"`,
		"--mode ephemeral",
		"grep -qE '^mode: +ephemeral$'",
		"scripts/compat-env.sh --strict",
		"go test -tags=compat -timeout 60m -v ./test/compat/...",
		"TestCloudSQLInstanceWrites",
		"TestCloudSQLDemoteSkipReplicationSetup",
		"TestCloudSQLDemotePostgres",
		"TestCloudSQLExternalPostgresPrimary",
		`grep -q -- "^--- PASS: $t (" compat.log`,
		"kind delete cluster",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("%s does not contain %q", nightlySQLWorkflow, want)
		}
	}
	if regexp.MustCompile(`(?m)^\s*(SERVICES|TEST_VARS)\+?=`).MatchString(all) {
		t.Errorf("%s sets the shard's services or variables itself; they belong in scripts/compat-shards.sh", nightlySQLWorkflow)
	}
}

// Every action is pinned by SHA, by the SHA ci.yml pins, and the workflow
// asks for nothing beyond reading the repository.
func TestNightlySQLEphemeralIsPinnedAndReadOnly(t *testing.T) {
	uses := regexp.MustCompile(`(?m)^\s+- uses: ([^@\s]+)@(\S+)`)
	ci := map[string]string{}
	for _, m := range uses.FindAllStringSubmatch(readWorkflow(t, "ci.yml"), -1) {
		ci[m[1]] = m[2]
	}
	body := readWorkflow(t, nightlySQLWorkflow)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, m := range uses.FindAllStringSubmatch(body, -1) {
		if !sha.MatchString(m[2]) {
			t.Errorf("%s is not pinned by SHA: %s", m[1], m[2])
		}
		if want, ok := ci[m[1]]; ok && want != m[2] {
			t.Errorf("%s is pinned at %s; ci.yml pins %s", m[1], m[2], want)
		}
	}
	if !strings.Contains(body, "\npermissions:\n  contents: read\n\n") {
		t.Errorf("%s does not set top-level permissions to contents: read", nightlySQLWorkflow)
	}
	if n := strings.Count(body, "permissions:"); n != 1 {
		t.Errorf("%s has %d permissions blocks; want the one top-level contents: read", nightlySQLWorkflow, n)
	}
	if strings.Contains(body, "secrets.") {
		t.Errorf("%s reads a secret; it needs only the run's token", nightlySQLWorkflow)
	}
}
