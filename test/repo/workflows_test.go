package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// workflowsDir is .github/workflows, from this package's directory.
var workflowsDir = filepath.Join("..", "..", ".github", "workflows")

func readWorkflow(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workflowsDir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// listUnder returns the "- item" entries directly below the line that is
// exactly key (indentation included), up to the first line that is neither
// an entry nor a comment. Quotes are dropped.
func listUnder(t *testing.T, file, body, key string) []string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line != key {
			continue
		}
		var items []string
		for _, l := range lines[i+1:] {
			s := strings.TrimSpace(l)
			if strings.HasPrefix(s, "#") {
				continue
			}
			item, ok := strings.CutPrefix(s, "- ")
			if !ok {
				break
			}
			items = append(items, strings.Trim(item, `"'`))
		}
		return items
	}
	t.Fatalf("%s has no line %q", file, key)
	return nil
}

// workflowNames maps each workflow's top-level name: to its file.
func workflowNames(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(workflowsDir, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if name, ok := strings.CutPrefix(line, "name: "); ok {
				names[strings.Trim(name, `"'`)] = filepath.Base(f)
				break
			}
		}
	}
	return names
}

// TestReportRedRunsWatchesTheUnreportedWorkflows (#702): the reporting
// workflow watches exactly the workflows that have no reporting of their
// own, and each name it watches is a real workflow's name:, since
// workflow_run matches by name and a renamed workflow would drop out of
// reporting silently.
func TestReportRedRunsWatchesTheUnreportedWorkflows(t *testing.T) {
	const file = "report-red-runs.yml"
	body := readWorkflow(t, file)
	watched := listUnder(t, file, body, "    workflows:")
	want := map[string]string{
		"Action self-test":    "action-selftest.yml",
		"KMS oracle":          "oracle.yml",
		"Storage oracle":      "storage-oracle.yml",
		"Dependencies":        "dependencies.yml",
		"CLI integration":     "cli-integration.yml",
		"Offline":             "offline.yml",
		"linux/arm64 nightly": "arm64.yml",
	}
	pending := map[string]bool{}
	if len(watched) != len(want) {
		t.Errorf("%s watches %q, want exactly %d workflows", file, watched, len(want))
	}
	names := workflowNames(t)
	for _, w := range watched {
		wf, ok := want[w]
		if !ok {
			t.Errorf("%s watches %q, which is not one of the watched workflows", file, w)
			continue
		}
		if _, err := os.Stat(filepath.Join(workflowsDir, wf)); pending[wf] && os.IsNotExist(err) {
			continue
		}
		if got := names[w]; got != wf {
			t.Errorf("no workflow is named %q in %s (found in %q)", w, wf, got)
		}
	}
	for w := range want {
		if !slices.Contains(watched, w) {
			t.Errorf("%s does not watch %q", file, w)
		}
	}
	if !strings.Contains(body, "    types: [completed]") {
		t.Errorf("%s does not trigger on completed runs", file)
	}
}

// TestReportRedRunsJobsAreBoundedAndLeastPrivilege: no default permissions,
// every job has a timeout, and no job can write anything but issues.
func TestReportRedRunsJobsAreBoundedAndLeastPrivilege(t *testing.T) {
	const file = "report-red-runs.yml"
	body := readWorkflow(t, file)
	if !strings.Contains(body, "\npermissions: {}\n") {
		t.Errorf("%s does not drop the default permissions", file)
	}
	_, jobs, ok := strings.Cut(body, "\njobs:\n")
	if !ok {
		t.Fatalf("%s has no jobs", file)
	}
	header := regexp.MustCompile(`(?m)^  ([a-z0-9-]+):$`)
	names := header.FindAllStringSubmatch(jobs, -1)
	parts := header.Split(jobs, -1)[1:]
	if len(names) == 0 {
		t.Fatalf("%s has no jobs", file)
	}
	for i, m := range names {
		j := parts[i]
		if !strings.Contains(j, "\n    timeout-minutes: ") {
			t.Errorf("job %s has no timeout-minutes", m[1])
		}
		for _, line := range strings.Split(j, "\n") {
			s := strings.TrimSpace(line)
			if strings.HasSuffix(s, ": write") && s != "issues: write" {
				t.Errorf("job %s asks for %q", m[1], s)
			}
			if strings.HasPrefix(s, "uses: ") || strings.HasPrefix(s, "- uses: ") {
				t.Errorf("job %s uses an action (%q); it needs only gh", m[1], s)
			}
		}
	}
}

// TestActionSelftestRunsOnCLIChanges (#702): the action self-test builds the
// CLI from source, so a pull request that changes the CLI runs it; and the
// changes job recognises every action path, or a pull request to the action
// would skip its failing-test and release jobs.
func TestActionSelftestRunsOnCLIChanges(t *testing.T) {
	const file = "action-selftest.yml"
	body := readWorkflow(t, file)
	_, on, ok := strings.Cut(body, "\non:\n")
	if !ok {
		t.Fatalf("%s has no on:", file)
	}
	on, _, _ = strings.Cut(on, "\n  push:")
	paths := listUnder(t, file, on, "    paths:")
	cli := []string{"cmd/**", "internal/**", "Makefile", "go.mod", "go.sum"}
	for _, p := range cli {
		if !slices.Contains(paths, p) {
			t.Errorf("%s pull_request paths %q lack %s", file, paths, p)
		}
	}

	m := regexp.MustCompile(`grep -qE '([^']+)'`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s: no action-path pattern in the changes job", file)
	}
	action := regexp.MustCompile(m[1])
	for _, p := range paths {
		sample := strings.ReplaceAll(p, "**", "x/y.go")
		if slices.Contains(cli, p) {
			if action.MatchString(sample) {
				t.Errorf("%s: CLI path %s counts as an action change", file, sample)
			}
			continue
		}
		if !action.MatchString(sample) {
			t.Errorf("%s: action path %s is not recognised by the changes job", file, sample)
		}
	}
}

// TestActionSelftestCountsOnlyJobsThatRan pins the fix for the first
// CLI-only pull request (#765): the jobs API also lists a job its `if:`
// skipped, so verify-cleanup counted three action jobs where one ran and
// failed. The count must leave skipped jobs out.
func TestActionSelftestCountsOnlyJobsThatRan(t *testing.T) {
	body := readWorkflow(t, "action-selftest.yml")
	if !strings.Contains(body, `select(.conclusion != "skipped")`) {
		t.Error(`verify-cleanup counts the action jobs without leaving out skipped ones; add select(.conclusion != "skipped")`)
	}
}
