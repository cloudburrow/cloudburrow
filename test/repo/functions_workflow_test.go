package repo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Functions build (#678) runs the gated Functions tests on an amd64 runner.
// It can only really run on GitHub; what is checked here is its shape: that
// it runs by hand only, where and how long it runs, what it installs, and
// that a skipped test fails it.

// TestFunctionsWorkflowIsDispatchOnly: runners are what the merge queue waits
// on, so functions.yml must never run on a pull request, in the merge queue,
// on a push or on a schedule. Its only trigger is workflow_dispatch.
func TestFunctionsWorkflowIsDispatchOnly(t *testing.T) {
	on, jobs := workflowJobs(t, "functions.yml")
	if got := strings.TrimSpace(on); got != "workflow_dispatch:" {
		t.Errorf("functions.yml's triggers are %q; want workflow_dispatch only", got)
	}
	for _, never := range []string{"pull_request", "merge_group", "schedule", "push", "workflow_run", "workflow_call"} {
		if strings.Contains(on, never) {
			t.Errorf("functions.yml runs on %s; it runs by hand only", never)
		}
	}
	if len(jobs) == 0 {
		t.Fatal("functions.yml has no jobs")
	}
	runsOn := regexp.MustCompile(`(?m)^    runs-on: (.+)$`)
	timeout := regexp.MustCompile(`(?m)^    timeout-minutes: [1-9][0-9]*$`)
	for name, j := range jobs {
		// amd64: the Google builder's only platform.
		if m := runsOn.FindStringSubmatch(j); m == nil || m[1] != "ubuntu-latest" {
			t.Errorf("job %s does not run on ubuntu-latest: %v", name, m)
		}
		if !timeout.MatchString(j) {
			t.Errorf("job %s has no timeout-minutes", name)
		}
	}
}

// TestFunctionsWorkflowRunsTheGatedTests: it installs the pinned pack,
// checksum-verified, brings up an instance, runs the Functions tests with
// the gate set, fails on a skip, and deletes the clusters.
func TestFunctionsWorkflowRunsTheGatedTests(t *testing.T) {
	_, jobs := workflowJobs(t, "functions.yml")
	all := ""
	for _, j := range jobs {
		all += j
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var deps struct {
		Components struct {
			Build struct {
				PackCli struct {
					Version string `json:"version"`
				} `json:"packCli"`
			} `json:"build"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	pack := deps.Components.Build.PackCli.Version
	if pack == "" {
		t.Fatal("dependencies.json pins no build.packCli.version")
	}

	for _, want := range []string{
		"https://github.com/buildpacks/pack/releases/download/v" + pack + "/pack-v" + pack + "-linux.tgz",
		`| sha256sum -c -`,
		"make build",
		`compat_shard run`,
		"./bin/cloudburrow up --detach",
		"./bin/cloudburrow wait",
		`scripts/compat-env.sh --strict --only`,
		"TESTS='TestFunctions'",
		`CLOUDBURROW_TEST_FUNCTIONS=1 go test -tags compat`,
		`-run "$TESTS"`,
		// The skip guard: every selected test must have passed.
		`go test -tags compat -list "$TESTS"`,
		`grep -q -- "^--- PASS: $t ("`,
		"cloudburrow diagnose",
		"kind delete cluster",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("functions.yml does not contain %q", want)
		}
	}
	if !regexp.MustCompile(`echo "[0-9a-f]{64}  \$RUNNER_TEMP/pack\.tgz" \| sha256sum -c -`).MatchString(all) {
		t.Error("functions.yml does not check pack's tarball against a pinned SHA-256")
	}

	// The selection names exactly the three gated tests, so a renamed one
	// cannot drop out unnoticed.
	files, err := filepath.Glob(filepath.Join("..", "compat", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	re := regexp.MustCompile("TestFunctions")
	var got []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range decl.FindAllStringSubmatch(string(b), -1) {
			if re.MatchString(d[1]) {
				got = append(got, d[1])
			}
		}
	}
	for _, want := range []string{"TestFunctionsFrameworkBuiltWithBuildpacks", "TestFunctionsRebuildReusesLayers", "TestFunctionsBuildBrokenModulePathFails"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("-run TestFunctions does not select %s; it selects %q", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("-run TestFunctions selects %q; want the three gated Functions tests", got)
	}
}

// Every action pinned by the SHA ci.yml pins, and nothing beyond reading the
// repository.
func TestFunctionsWorkflowPinsActionsAndPermissions(t *testing.T) {
	uses := regexp.MustCompile(`(?m)^\s+- uses: ([^@\s]+)@(\S+)`)
	ci := map[string]string{}
	for _, m := range uses.FindAllStringSubmatch(readWorkflow(t, "ci.yml"), -1) {
		ci[m[1]] = m[2]
	}
	body := readWorkflow(t, "functions.yml")
	found := uses.FindAllStringSubmatch(body, -1)
	if len(found) == 0 {
		t.Fatal("functions.yml uses no actions")
	}
	for _, m := range found {
		want, ok := ci[m[1]]
		if !ok {
			t.Errorf("functions.yml uses %s, which ci.yml does not pin", m[1])
			continue
		}
		if want != m[2] {
			t.Errorf("%s is pinned at %s; ci.yml pins %s", m[1], m[2], want)
		}
	}
	if !strings.Contains(body, "\npermissions:\n  contents: read\n\n") {
		t.Error("functions.yml does not set top-level permissions to contents: read")
	}
	if n := strings.Count(body, "permissions:"); n != 1 {
		t.Errorf("functions.yml has %d permissions blocks; want the one top-level contents: read", n)
	}
	// A dispatched run is the only kind this workflow has, so the red-run
	// report must count a dispatch on main, in both of its jobs.
	report := readWorkflow(t, "report-red-runs.yml")
	if n := strings.Count(report, "github.event.workflow_run.event == 'workflow_dispatch'"); n != 2 {
		t.Errorf("report-red-runs.yml counts a dispatched run in %d jobs; want both", n)
	}
}
