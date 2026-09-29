package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The linux/arm64 nightly (#689) can only really run on GitHub's arm64
// runners. What can be checked here is its shape: where it runs, when, that
// it brings an instance up and runs an SDK subset, and that nothing in it
// can run unbounded or unpinned.

// workflowJobs splits a workflow file into its trigger block and its
// top-level jobs, by name, with comment lines dropped so that only what runs
// is checked.
func workflowJobs(t *testing.T, file string) (string, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", file))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	text := strings.Join(kept, "\n")
	head, body, ok := strings.Cut(text, "\njobs:\n")
	if !ok {
		t.Fatalf("%s has no jobs", file)
	}
	_, on, ok := strings.Cut(head, "\non:\n")
	if !ok {
		t.Fatalf("%s has no on: block", file)
	}
	on, _, _ = strings.Cut(on, "\n\n")
	jobs := map[string]string{}
	header := regexp.MustCompile(`^  ([a-z0-9-]+):$`)
	name := ""
	for _, line := range strings.Split(body, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			name = m[1]
			continue
		}
		if name != "" {
			jobs[name] += line + "\n"
		}
	}
	return on, jobs
}

func TestArm64WorkflowRunsNightlyOnArm64(t *testing.T) {
	on, jobs := workflowJobs(t, "arm64.yml")
	for _, want := range []string{"schedule:", "cron:", "workflow_dispatch:"} {
		if !strings.Contains(on, want) {
			t.Errorf("arm64.yml's triggers have no %s:\n%s", want, on)
		}
	}
	// Runners are what the merge queue waits on (#689).
	for _, never := range []string{"pull_request", "merge_group"} {
		if strings.Contains(on, never) {
			t.Errorf("arm64.yml runs on %s; it is nightly and by hand only", never)
		}
	}
	if len(jobs) == 0 {
		t.Fatal("arm64.yml has no jobs")
	}
	runsOn := regexp.MustCompile(`(?m)^    runs-on: (.+)$`)
	timeout := regexp.MustCompile(`(?m)^    timeout-minutes: [1-9][0-9]*$`)
	for name, j := range jobs {
		// bigquery-emulator only builds the embedded emulator binaries, once
		// per run and from ci.yml's cache (#1130); it cross-compiles, so it
		// runs on the runner ci.yml's build uses. Every other job tests arm64.
		wantRunner := "ubuntu-24.04-arm"
		if name == "bigquery-emulator" {
			wantRunner = "ubuntu-latest"
		}
		if m := runsOn.FindStringSubmatch(j); m == nil || m[1] != wantRunner {
			t.Errorf("job %s does not run on %s: %v", name, wantRunner, m)
		}
		if !timeout.MatchString(j) {
			t.Errorf("job %s has no timeout-minutes", name)
		}
	}
}

func TestArm64WorkflowRunsCheckUpAndACompatSubset(t *testing.T) {
	_, jobs := workflowJobs(t, "arm64.yml")
	all := ""
	for _, j := range jobs {
		all += j
	}
	for _, want := range []string{
		"run: make check",
		"make build",
		"./bin/cloudburrow up --detach",
		"./bin/cloudburrow wait",
		"go test -tags=compat",
		`-run "$TESTS"`,
		// The skip guard: every selected test must have passed.
		`go test -tags=compat -list "$TESTS"`,
		`grep -q -- "^--- PASS: $t ("`,
		"kind delete cluster",
		"kind export logs",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("arm64.yml does not contain %q", want)
		}
	}
	// The subset names official-SDK tests that exist in test/compat.
	m := regexp.MustCompile(`TESTS='([^']+)'`).FindStringSubmatch(all)
	if m == nil {
		t.Fatal("arm64.yml sets no TESTS regex")
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("TESTS is not a valid regex: %v", err)
	}
	// A renamed test would drop out of the subset silently: the skip guard
	// lists what matches, so it would not notice. Count what matches here.
	files, err := filepath.Glob(filepath.Join("..", "compat", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	var storage, pubsub int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range decl.FindAllStringSubmatch(string(b), -1) {
			if !re.MatchString(d[1]) {
				continue
			}
			switch {
			case strings.HasPrefix(d[1], "TestStorage"):
				storage++
			case strings.HasPrefix(d[1], "TestPubSub"):
				pubsub++
			}
		}
	}
	// The eight Storage and four Pub/Sub tests the workflow names.
	if storage != 8 || pubsub != 4 {
		t.Errorf("TESTS matches %d Storage and %d Pub/Sub tests in test/compat; the workflow names 8 and 4", storage, pubsub)
	}
}

// Every action is pinned by SHA, and by the same SHA ci.yml pins, so a
// dependency update cannot leave the nightly on an older action.
func TestArm64WorkflowPinsActionsLikeCI(t *testing.T) {
	uses := regexp.MustCompile(`(?m)^\s+- uses: ([^@\s]+)@(\S+)`)
	read := func(file string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ci := map[string]string{}
	for _, m := range uses.FindAllStringSubmatch(read("ci.yml"), -1) {
		ci[m[1]] = m[2]
	}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	found := uses.FindAllStringSubmatch(read("arm64.yml"), -1)
	if len(found) == 0 {
		t.Fatal("arm64.yml uses no actions")
	}
	for _, m := range found {
		if !sha.MatchString(m[2]) {
			t.Errorf("%s is not pinned by SHA: %s", m[1], m[2])
		}
		if want, ok := ci[m[1]]; ok && want != m[2] {
			t.Errorf("%s is pinned at %s; ci.yml pins %s", m[1], m[2], want)
		}
	}
}

// The workflow asks for nothing beyond reading the repository.
func TestArm64WorkflowHasMinimalPermissions(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "arm64.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\npermissions:\n  contents: read\n\n") {
		t.Error("arm64.yml does not set top-level permissions to contents: read")
	}
	if n := strings.Count(string(b), "permissions:"); n != 1 {
		t.Errorf("arm64.yml has %d permissions blocks; want the one top-level contents: read", n)
	}
}
