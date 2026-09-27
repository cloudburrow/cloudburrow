package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scripts/compat-shards.sh is what each shard of ci.yml's compat job starts
// and runs, and scripts/verify-local.sh runs the same flow on a developer's
// machine (#705). Both read the lists from that one file; these tests keep
// it that way, so a local run cannot drift from CI's.

// compatShard sources compat-shards.sh and returns the variables
// compat_shard sets for shard, or COMPAT_SHARDS for shard "".
func compatShard(t *testing.T, shard string) map[string]string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "compat-shards.sh"))
	if err != nil {
		t.Fatal(err)
	}
	prog := `set -eu; . "$1"; echo "COMPAT_SHARDS=$COMPAT_SHARDS"; echo "COMPAT_BROWSER_SHARDS=$COMPAT_BROWSER_SHARDS"`
	if shard != "" {
		prog += `; compat_shard "$2"; for v in SERVICES TEST_VARS SETUP_TESTS PROBE_TESTS PROBE_VARS; do echo "$v=${!v}"; done; echo "FIXTURES=${FIXTURES[*]+${FIXTURES[*]}}"`
	}
	out, err := exec.Command(bash, "-c", prog, "bash", script, shard).CombinedOutput()
	if err != nil {
		t.Fatalf("compat_shard %s: %v\n%s", shard, err, out)
	}
	vars := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		vars[k] = v
	}
	return vars
}

// The shards the script knows are the compat job's matrix, in its order.
func TestCompatShardsMatchTheCIMatrix(t *testing.T) {
	_, ci := workflowJobs(t, "ci.yml")
	m := regexp.MustCompile(`(?m)^\s+shard: \[([^\]]*)\]`).FindStringSubmatch(ci["compat"])
	if m == nil {
		t.Fatal("ci.yml's compat job has no shard matrix")
	}
	var matrix []string
	for _, s := range strings.Split(m[1], ",") {
		matrix = append(matrix, strings.TrimSpace(s))
	}
	got := compatShard(t, "")["COMPAT_SHARDS"]
	if want := strings.Join(matrix, " "); got != want {
		t.Errorf("COMPAT_SHARDS = %q, ci.yml's compat matrix = %q", got, want)
	}
}

// The browser step runs in the shards the script names for it.
func TestCompatBrowserShardsMatchTheCIStep(t *testing.T) {
	_, ci := workflowJobs(t, "ci.yml")
	step := regexp.MustCompile(`(?s)- name: Console in a headless browser\n\s+if: ([^\n]*)`).FindStringSubmatch(ci["compat"])
	if step == nil {
		t.Fatal("ci.yml's compat job has no browser step with an if:")
	}
	var shards []string
	for _, m := range regexp.MustCompile(`matrix\.shard == '([a-z]+)'`).FindAllStringSubmatch(step[1], -1) {
		shards = append(shards, m[1])
	}
	got := compatShard(t, "")["COMPAT_BROWSER_SHARDS"]
	if want := strings.Join(shards, " "); got != want {
		t.Errorf("COMPAT_BROWSER_SHARDS = %q, the browser step runs in %q", got, want)
	}
}

// Every shard has services, only variables compat-env.sh sets, probes that
// read variables the shard exports, and tests that exist.
func TestCompatShardsAreComplete(t *testing.T) {
	list := compatEnvList(t)
	tests := compatTestNames(t)
	for _, shard := range strings.Fields(compatShard(t, "")["COMPAT_SHARDS"]) {
		v := compatShard(t, shard)
		if v["SERVICES"] == "" {
			t.Errorf("shard %s has no services", shard)
		}
		exported := map[string]bool{}
		for _, n := range strings.Split(v["TEST_VARS"], ",") {
			exported[n] = true
			if !list["CLOUDBURROW_TEST_"+n] {
				t.Errorf("shard %s exports %s, which scripts/compat-env.sh does not set", shard, n)
			}
		}
		for _, n := range strings.Split(v["PROBE_VARS"], ",") {
			if n != "" && !exported[n] {
				t.Errorf("shard %s's restart probes read %s, which the shard does not export", shard, n)
			}
		}
		if (v["PROBE_TESTS"] == "") != (v["PROBE_VARS"] == "") {
			t.Errorf("shard %s: PROBE_TESTS %q and PROBE_VARS %q must be both set or both empty", shard, v["PROBE_TESTS"], v["PROBE_VARS"])
		}
		for _, name := range strings.Fields(v["SETUP_TESTS"] + " " + v["PROBE_TESTS"]) {
			if !tests[name] {
				t.Errorf("shard %s names %s, which is not a test in test/compat", shard, name)
			}
		}
	}
}

// Neither the compat job nor verify-local.sh writes a shard list of its
// own: both source the script and call its functions.
func TestCompatShardListsLiveInOneFile(t *testing.T) {
	own := regexp.MustCompile(`(?m)^\s*(SERVICES|TEST_VARS|SETUP_TESTS|PROBE_TESTS|PROBE_VARS)\+?=`)
	_, ci := workflowJobs(t, "ci.yml")
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "verify-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"ci.yml's compat job": ci["compat"], "scripts/verify-local.sh": string(raw)} {
		for _, want := range []string{". scripts/compat-shards.sh", `compat_shard "$`, `compat_setup_exports "$`, `compat_probe_expect "$`} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not run %q", name, want)
			}
		}
		if m := own.FindString(text); m != "" {
			t.Errorf("%s sets %q itself; it belongs in scripts/compat-shards.sh", name, strings.TrimSpace(m))
		}
	}
	if !strings.Contains(ci["compat"], "RUN_SHARD_TESTS=$COMPAT_BROWSER_RUN_SHARD_TESTS") {
		t.Error("ci.yml's browser step does not take the run shard's browser tests from scripts/compat-shards.sh")
	}
	if !strings.Contains(string(raw), `"$COMPAT_BROWSER_RUN_SHARD_TESTS"`) {
		t.Error("scripts/verify-local.sh does not split the browser suite as CI does")
	}
	if !strings.Contains(ci["check"], "shellcheck -x -s bash scripts/compat-shards.sh scripts/verify-local.sh") {
		t.Error("ci.yml's check job does not shellcheck scripts/compat-shards.sh and scripts/verify-local.sh as bash")
	}
}

// verify-local.sh refuses cloud credentials and a port base on a default
// instance's ports before it starts anything.
func TestVerifyLocalRefusesBeforeStarting(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "verify-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		env  []string
		args []string
		want string
	}{
		{[]string{"GOOGLE_APPLICATION_CREDENTIALS=/nonexistent/adc.json"}, nil, "GOOGLE_APPLICATION_CREDENTIALS is set"},
		{[]string{"CLOUDSDK_AUTH_ACCESS_TOKEN=x"}, nil, "CLOUDSDK_AUTH_ACCESS_TOKEN is set"},
		{nil, []string{"--port-base", "9000"}, "overlaps a default instance"},
		{nil, []string{"--shards", "storage,nope"}, "unknown shard nope"},
	} {
		cmd := exec.Command(bash, append([]string{script}, c.args...)...)
		cmd.Env = append(os.Environ(), c.env...)
		out, err := cmd.CombinedOutput()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), c.want) {
			t.Errorf("verify-local.sh %v with %v: %v, %q; want exit 2 naming %q", c.args, c.env, err, out, c.want)
		}
	}
}

// compatTestNames are the Test functions declared in test/compat.
func compatTestNames(t *testing.T) map[string]bool {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)`)
	names := map[string]bool{}
	files, err := filepath.Glob(filepath.Join("..", "compat", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			names[m[1]] = true
		}
	}
	return names
}
