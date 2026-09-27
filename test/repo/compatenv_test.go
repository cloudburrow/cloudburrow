package repo

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// scripts/compat-env.sh is the one supported way to set the compat harness's
// environment from a running instance, for a developer and for CI (#711).
// The suites read 48 CLOUDBURROW_TEST_* variables while the README named 13,
// so a contributor's run skipped most of what they changed and looked green.
// These tests keep the script, the suites, the README and the workflows in
// step.

// compatEnvExceptions are the CLOUDBURROW_TEST_* variables in test/compat
// that the script deliberately does not set, because no running instance
// has a value for them: each is a test-mode switch or a fixture a job makes
// for itself. Each must still be listed in test/compat/README.md.
var compatEnvExceptions = map[string]string{
	// Restart probes: which half of a probe a run is, and where the probe
	// keeps what it wrote. ci.yml sets them around a stop and an up.
	"CLOUDBURROW_TEST_KMS_PROBE":                "restart probe fixture file",
	"CLOUDBURROW_TEST_SECRETS_PROBE":            "restart probe fixture file",
	"CLOUDBURROW_TEST_TASKS_PROBE":              "restart probe fixture file",
	"CLOUDBURROW_TEST_SCHEDULER_PROBE":          "restart probe fixture file",
	"CLOUDBURROW_TEST_STORAGE_RESTART_PROBE":    "restart probe mode and fixture file",
	"CLOUDBURROW_TEST_STORAGE_VERSIONING_PROBE": "builtin storage-server restart probe mode and file",
	"CLOUDBURROW_TEST_CLOUDSQL_SETUP":           "restart probe setup switch",
	"CLOUDBURROW_TEST_DATASTORE_EXPECT":         "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_KMS_EXPECT":               "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_MEMORYSTORE_EXPECT":       "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_MYSQL_EXPECT":             "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_CLOUDSQL_EXPECT":          "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_SECRETS_EXPECT":           "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_TASKS_EXPECT":             "restart probe expectation (present or absent)",
	"CLOUDBURROW_TEST_SCHEDULER_EXPECT":         "restart probe expectation (present or absent)",
	// An opt-in switch for a slow test no CI shard runs (#678).
	"CLOUDBURROW_TEST_FUNCTIONS": "opt-in switch",
	// The signed URL tests' key and account, for the builtin storage-server
	// started with --signing-cert; an instance has neither.
	"CLOUDBURROW_TEST_SIGNING_KEY":   "fixture for the builtin storage-server",
	"CLOUDBURROW_TEST_SIGNING_EMAIL": "fixture for the builtin storage-server",
	// The workloads' namespace, which defaults to the one every instance
	// uses; set only for an instance configured with another.
	"CLOUDBURROW_TEST_NAMESPACE": "defaults to the instance default",
}

var compatVarRE = regexp.MustCompile(`CLOUDBURROW_TEST_[A-Z0-9_]*[A-Z0-9]`)

// compatVars returns every CLOUDBURROW_TEST_* variable named in test/compat,
// in any file, whatever its build tags, but the README that documents them.
func compatVars(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join("..", "compat")
	readme := filepath.Join(root, "README.md")
	vars := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == readme {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, v := range compatVarRE.FindAllString(string(b), -1) {
			vars[v] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) == 0 {
		t.Fatal("found no CLOUDBURROW_TEST_* variable in test/compat")
	}
	return vars
}

func compatEnvScript(t *testing.T) (bash, script string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	script, err = filepath.Abs(filepath.Join("..", "..", "scripts", "compat-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return bash, script
}

// compatEnvList is what the script says it sets.
func compatEnvList(t *testing.T) map[string]bool {
	t.Helper()
	bash, script := compatEnvScript(t)
	out, err := exec.Command(bash, script, "--list").Output()
	if err != nil {
		t.Fatalf("compat-env.sh --list: %v", err)
	}
	list := map[string]bool{}
	for _, v := range strings.Fields(string(out)) {
		list[v] = true
	}
	return list
}

func sortedKeys(m map[string]bool) []string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

// A variable a suite reads is either set by the script or a listed
// exception, and the README names it: otherwise a suite added with a new
// variable skips on every developer's instance, and nobody notices.
func TestCompatEnvCoversEveryHarnessVariable(t *testing.T) {
	vars := compatVars(t)
	list := compatEnvList(t)
	for _, v := range sortedKeys(vars) {
		if !list[v] && compatEnvExceptions[v] == "" {
			t.Errorf("%s is read in test/compat but scripts/compat-env.sh does not set it; add it there, "+
				"or, if no instance has a value for it, to compatEnvExceptions with the reason", v)
		}
	}
	for _, v := range sortedKeys(list) {
		if !vars[v] {
			t.Errorf("scripts/compat-env.sh sets %s, which nothing in test/compat reads", v)
		}
		if compatEnvExceptions[v] != "" {
			t.Errorf("%s is both set by scripts/compat-env.sh and listed as an exception", v)
		}
	}
	for v := range compatEnvExceptions {
		if !vars[v] {
			t.Errorf("compatEnvExceptions lists %s, which nothing in test/compat reads", v)
		}
	}

	readme, err := os.ReadFile(filepath.Join("..", "compat", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, v := range compatVarRE.FindAllString(string(readme), -1) {
		documented[v] = true
	}
	for _, v := range sortedKeys(vars) {
		if !documented[v] {
			t.Errorf("test/compat/README.md does not list %s", v)
		}
	}
	for _, stale := range []string{"OS-assigned, so two", "--state-dir ./state &"} {
		if strings.Contains(string(readme), stale) {
			t.Errorf("test/compat/README.md still says %q", stale)
		}
	}
	if !strings.Contains(string(readme), "scripts/compat-env.sh") {
		t.Error("test/compat/README.md does not point to scripts/compat-env.sh")
	}
}

// compatEnvFixture writes a recorded instance: its instance directory, with
// the files `up` leaves there, and `env` and `status` output naming it.
func compatEnvFixture(t *testing.T, edit func(env map[string]string)) (dir, envFile, statusFile string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "state", "ct")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("credentials.json", "{}\n")
	write("kubeconfig", "apiVersion: v1\n")
	write("admin-token", "fixture-admin-token\n")
	write("up.log", "starting\nlocal AI:  http://127.0.0.1:9020\n  model:   m\n")
	write("up.json", `{"pid": 1, "control": "127.0.0.1:50001", "log": "`+filepath.Join(dir, "up.log")+`",
  "endpoints": {"control": "127.0.0.1:50001", "metadata": "127.0.0.1:9005", "console": "127.0.0.1:9090",
    "tasks": "127.0.0.1:9003", "secretmanager": "127.0.0.1:9006", "run": "127.0.0.1:50002"}}`)
	env := map[string]string{
		"GOOGLE_APPLICATION_CREDENTIALS":        filepath.Join(dir, "credentials.json"),
		"GOOGLE_CLOUD_PROJECT":                  "ct-local",
		"STORAGE_EMULATOR_HOST":                 "http://127.0.0.1:50003",
		"PUBSUB_EMULATOR_HOST":                  "127.0.0.1:50004",
		"CLOUDBURROW_SCHEDULER_ENDPOINT":        "127.0.0.1:9008",
		"CLOUDBURROW_KMS_ENDPOINT":              "127.0.0.1:9018",
		"CLOUDBURROW_LOGGING_ENDPOINT":          "127.0.0.1:9009",
		"CLOUDBURROW_RESOURCEMANAGER_ENDPOINT":  "127.0.0.1:9007",
		"SPANNER_EMULATOR_HOST":                 "127.0.0.1:9013",
		"DATASTORE_EMULATOR_HOST":               "127.0.0.1:9011",
		"FIRESTORE_EMULATOR_HOST":               "127.0.0.1:9010",
		"BIGTABLE_EMULATOR_HOST":                "127.0.0.1:9012",
		"REDIS_HOST":                            "127.0.0.1",
		"REDIS_PORT":                            "9016",
		"MYSQL_HOST":                            "127.0.0.1",
		"MYSQL_PORT":                            "9017",
		"MYSQL_PASSWORD":                        "fixture-'pw'",
		"PGHOST":                                "127.0.0.1",
		"PGPORT":                                "9019",
		"CLOUDBURROW_BIGQUERY_ENDPOINT":         "http://127.0.0.1:9014",
		"CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT": "127.0.0.1:9015",
	}
	if edit != nil {
		edit(env)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	envFile = filepath.Join(t.TempDir(), "env.json")
	statusFile = filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(envFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusFile, []byte(`{"schema_version": 3, "state": "ready", "cluster": {"name": "cloudburrow-ct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, envFile, statusFile
}

// runCompatEnv runs the script with the recorded sources, --plain, and
// returns its exit status, the variables it printed, and its stderr.
func runCompatEnv(t *testing.T, envFile, statusFile string, extra ...string) (int, map[string]string, string) {
	t.Helper()
	bash, script := compatEnvScript(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not on PATH")
	}
	args := append([]string{script, "--plain", "--env-json", envFile, "--status-json", statusFile,
		"--cli", "/opt/cb/bin/cloudburrow", "--gcloud", "/opt/gcloud/bin/gcloud", "--tofu", "/opt/tofu"}, extra...)
	cmd := exec.Command(bash, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return code, got, stderr.String()
}

// Every variable the script can set comes out of a recorded instance, from
// the source ci.yml's shards used to read it from inline.
func TestCompatEnvFromARecordedInstance(t *testing.T) {
	dir, envFile, statusFile := compatEnvFixture(t, nil)
	flags := []string{"--", "--name", "ct", "--state-dir", filepath.Dir(dir)}
	code, got, stderr := runCompatEnv(t, envFile, statusFile, append([]string{"--strict"}, flags...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := map[string]string{
		"CREDENTIALS":      filepath.Join(dir, "credentials.json"),
		"KUBECONFIG":       filepath.Join(dir, "kubeconfig"),
		"CLUSTER":          "cloudburrow-ct",
		"CONTROL":          "127.0.0.1:50001",
		"METADATA":         "127.0.0.1:9005",
		"CONSOLE":          "127.0.0.1:9090",
		"ADMIN_TOKEN":      "fixture-admin-token",
		"CLI":              "/opt/cb/bin/cloudburrow",
		"CLI_ARGS":         "--name ct --state-dir " + filepath.Dir(dir),
		"STORAGE":          "http://127.0.0.1:50003",
		"PUBSUB":           "127.0.0.1:50004",
		"TASKS":            "127.0.0.1:9003",
		"SECRETS":          "127.0.0.1:9006",
		"SCHEDULER":        "127.0.0.1:9008",
		"RUN":              "127.0.0.1:50002",
		"KMS":              "127.0.0.1:9018",
		"LOGGING":          "127.0.0.1:9009",
		"RESOURCEMANAGER":  "127.0.0.1:9007",
		"RUN_STORAGE":      "http://127.0.0.1:50003",
		"RUN_PUBSUB":       "127.0.0.1:50004",
		"RUN_KMS":          "127.0.0.1:9018",
		"RUN_SCHEDULER":    "127.0.0.1:9008",
		"RUN_LOGGING":      "127.0.0.1:9009",
		"SPANNER":          "127.0.0.1:9013",
		"DATASTORE":        "127.0.0.1:9011",
		"FIRESTORE":        "127.0.0.1:9010",
		"BIGTABLE":         "127.0.0.1:9012",
		"MEMORYSTORE":      "127.0.0.1:9016",
		"MYSQL":            "127.0.0.1:9017",
		"MYSQL_PASSWORD":   "fixture-'pw'",
		"CLOUDSQL":         "127.0.0.1:9019",
		"BIGQUERY":         "http://127.0.0.1:9014",
		"BIGQUERY_STORAGE": "127.0.0.1:9015",
		"BIGQUERY_PROJECT": "ct-local",
		"LOCALAI":          "127.0.0.1:9020",
		"GCLOUD":           "/opt/gcloud/bin/gcloud",
		"TOFU":             "/opt/tofu",
	}
	list := compatEnvList(t)
	if len(want) != len(list) {
		t.Errorf("the script lists %d variables; this test checks %d", len(list), len(want))
	}
	for k, v := range want {
		name := "CLOUDBURROW_TEST_" + k
		if !list[name] {
			t.Errorf("%s is not in --list", name)
		}
		if got[name] != v {
			t.Errorf("%s = %q, want %q", name, got[name], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("printed %d variables, want %d: %v", len(got), len(want), got)
	}

	// --only selects, with or without the prefix, in the order given.
	code, got, stderr = runCompatEnv(t, envFile, statusFile, "--strict", "--only", "STORAGE,CLOUDBURROW_TEST_PUBSUB")
	if code != 0 || len(got) != 2 || got["CLOUDBURROW_TEST_STORAGE"] == "" || got["CLOUDBURROW_TEST_PUBSUB"] == "" {
		t.Errorf("--only STORAGE,CLOUDBURROW_TEST_PUBSUB: exit %d, %v, %s", code, got, stderr)
	}
	// A name the script does not know is a usage error, not a silent skip.
	if code, _, stderr = runCompatEnv(t, envFile, statusFile, "--only", "STORAGE,NOPE"); code != 2 || !strings.Contains(stderr, "CLOUDBURROW_TEST_NOPE") {
		t.Errorf("--only with an unknown name: exit %d, %s", code, stderr)
	}
}

// The export form survives eval, quotes and all.
func TestCompatEnvExportsEvaluate(t *testing.T) {
	_, envFile, statusFile := compatEnvFixture(t, nil)
	bash, script := compatEnvScript(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not on PATH")
	}
	out, err := exec.Command(bash, "-c", `set -e; e=$("$1" --strict --only MYSQL_PASSWORD,STORAGE --env-json "$2" --status-json "$3"); eval "$e"; `+
		`printf '%s|%s' "$CLOUDBURROW_TEST_MYSQL_PASSWORD" "$(printenv CLOUDBURROW_TEST_STORAGE)"`,
		"x", script, envFile, statusFile).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != "fixture-'pw'|http://127.0.0.1:50003" {
		t.Errorf("after eval: %q", out)
	}
}

// --strict is the per-shard check ci.yml used to do inline: a missing value
// or a port of 0 fails, naming the variable. Without it, the variable is
// left out and named on stderr, so a developer's tests for it skip.
func TestCompatEnvStrictFailsOnAMissingValue(t *testing.T) {
	_, envFile, statusFile := compatEnvFixture(t, func(env map[string]string) { delete(env, "PUBSUB_EMULATOR_HOST") })
	code, _, stderr := runCompatEnv(t, envFile, statusFile, "--strict", "--only", "STORAGE,PUBSUB")
	if code == 0 || !strings.Contains(stderr, "CLOUDBURROW_TEST_PUBSUB is empty") {
		t.Errorf("a missing PUBSUB_EMULATOR_HOST: exit %d, stderr %q", code, stderr)
	}
	code, got, stderr := runCompatEnv(t, envFile, statusFile, "--only", "STORAGE,PUBSUB")
	if code != 0 || got["CLOUDBURROW_TEST_STORAGE"] == "" || len(got) != 1 || !strings.Contains(stderr, "CLOUDBURROW_TEST_PUBSUB") {
		t.Errorf("without --strict: exit %d, %v, stderr %q", code, got, stderr)
	}

	_, envFile, statusFile = compatEnvFixture(t, func(env map[string]string) { env["CLOUDBURROW_KMS_ENDPOINT"] = "127.0.0.1:0" })
	code, _, stderr = runCompatEnv(t, envFile, statusFile, "--strict", "--only", "KMS")
	if code == 0 || !strings.Contains(stderr, "unbound port: CLOUDBURROW_TEST_KMS=127.0.0.1:0") {
		t.Errorf("a port of 0: exit %d, stderr %q", code, stderr)
	}

	dir, envFile, statusFile := compatEnvFixture(t, nil)
	if err := os.Remove(filepath.Join(dir, "admin-token")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCompatEnv(t, envFile, statusFile, "--strict", "--only", "ADMIN_TOKEN")
	if code == 0 || !strings.Contains(stderr, "CLOUDBURROW_TEST_ADMIN_TOKEN is empty") {
		t.Errorf("no admin token: exit %d, stderr %q", code, stderr)
	}
}

// The workflows set the script's variables through the script, never by an
// inline jq of their own, which is how ci.yml and the README drifted apart.
func TestWorkflowsSetCompatEnvThroughTheScript(t *testing.T) {
	list := compatEnvList(t)
	assign := regexp.MustCompile(`(CLOUDBURROW_TEST_[A-Z0-9_]*[A-Z0-9])=\$\(`)
	for _, file := range []string{"ci.yml", "arm64.yml"} {
		_, jobs := workflowJobs(t, file)
		for name, job := range jobs {
			for _, m := range assign.FindAllStringSubmatch(job, -1) {
				if list[m[1]] {
					t.Errorf("%s job %s sets %s itself; take it from scripts/compat-env.sh", file, name, m[1])
				}
			}
		}
	}
	_, ci := workflowJobs(t, "ci.yml")
	for _, want := range []string{
		`COMPAT_ENV=$(scripts/compat-env.sh --strict --only "$TEST_VARS"`,
		`scripts/compat-env.sh --strict --plain --only "$PROBE_VARS"`,
		`scripts/compat-env.sh --strict --plain --only STORAGE,PUBSUB,RUN`,
		`scripts/compat-env.sh --strict --only CONSOLE,CONTROL,ADMIN_TOKEN`,
	} {
		if !strings.Contains(ci["compat"], want) {
			t.Errorf("ci.yml's compat job does not run %q", want)
		}
	}
	if !strings.Contains(ci["check"], "shellcheck -s bash .github/scripts/*.sh scripts/compat-env.sh") {
		t.Error("ci.yml's check job does not shellcheck scripts/compat-env.sh as bash")
	}
	_, arm := workflowJobs(t, "arm64.yml")
	if !strings.Contains(arm["cluster"], "scripts/compat-env.sh --strict --only STORAGE,PUBSUB") {
		t.Error("arm64.yml does not take its endpoints from scripts/compat-env.sh")
	}
}
