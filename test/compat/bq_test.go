//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bq, the Cloud SDK's BigQuery CLI, against the BigQuery emulator (#696).
// Every command runs in a fresh CLOUDSDK_CONFIG and an empty BIGQUERYRC,
// with nothing inherited from the caller's CLOUDSDK_*, GOOGLE_* or GCE_*
// variables, and behind egressGuard.
//
// bq sends no credential CloudBurrow needs, but it must be given one to
// start: its gcloud wrapper refuses every command with "You do not currently
// have an active account selected" unless --oauth_access_token=<value> is on
// the command line, and then passes no --project_id of its own. The token is
// a placeholder; the emulator checks nothing.
//
// Measured with bq 2.1.38 (Google Cloud CLI 586.0.0), and pinned below:
//
//   - The exported CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY is read only with
//     --nouse_google_auth. bq takes gcloud's properties from `gcloud config
//     config-helper`, which fails with no account, and bq then carries on
//     with none, at bigquery.googleapis.com. --nouse_google_auth makes it read
//     them from `gcloud config list` instead, which needs no account, and the
//     override and CLOUDSDK_CORE_PROJECT both apply.
//   - --api http://<endpoint> with --project_id works whatever the auth flag:
//     a flag set on the command line wins over gcloud's properties.

// bqToken is the placeholder bq's wrapper requires before it runs a command.
const bqToken = "--oauth_access_token=cloudburrow-unused"

type bqSession struct {
	t     *testing.T
	bin   string
	env   []string
	flags []string
}

// newBqSession is bq with the given variables and global flags, in an
// isolated configuration, behind egressGuard.
func newBqSession(t *testing.T, vars map[string]string, flags ...string) *bqSession {
	t.Helper()
	bin := bqBinary(t)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLOUDSDK_") && !strings.HasPrefix(kv, "GOOGLE_") &&
			!strings.HasPrefix(kv, "GCE_") && !strings.HasPrefix(kv, "BIGQUERYRC=") {
			env = append(env, kv)
		}
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "bigqueryrc")
	if err := os.WriteFile(rc, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "CLOUDSDK_CONFIG="+filepath.Join(dir, "gcloud"), "BIGQUERYRC="+rc,
		"CLOUDSDK_CORE_DISABLE_PROMPTS=1", "CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true",
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true", "CLOUDSDK_CORE_CHECK_GCE_METADATA=false")
	return &bqSession{t: t, bin: bin, env: append(env, egressGuard(t)...), flags: flags}
}

// run returns bq's stdout and stderr apart: bq --format=json prints the
// result on stdout, and gcloud's config-helper complaint, when there is one,
// on stderr.
func (b *bqSession) run(extraEnv []string, args ...string) (string, string, error) {
	cmd := exec.Command(b.bin, append(append([]string{}, b.flags...), args...)...)
	cmd.Env = append(append([]string{}, b.env...), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (b *bqSession) must(args ...string) string {
	b.t.Helper()
	out, errOut, err := b.run(nil, args...)
	if err != nil {
		b.t.Fatalf("bq %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

// exerciseBq drives mk (dataset and table), insert, query and rm through
// bq, and checks each step through the official Go client too, so the
// commands are shown to have reached this emulator.
func exerciseBq(t *testing.T, h *Harness, b *bqSession, dataset string) {
	c, project := bigqueryClient(t, h)
	ds := c.Dataset(dataset)
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })

	b.must("mk", "--dataset", project+":"+dataset)
	if _, err := ds.Metadata(h.Context()); err != nil {
		t.Fatalf("the dataset bq made is not in the emulator: %v", err)
	}
	b.must("mk", "--table", dataset+".orders", "id:INTEGER,region:STRING,amount:FLOAT")
	md, err := ds.Table("orders").Metadata(h.Context())
	if err != nil {
		t.Fatalf("the table bq made is not in the emulator: %v", err)
	}
	var cols []string
	for _, f := range md.Schema {
		cols = append(cols, f.Name+":"+string(f.Type))
	}
	if strings.Join(cols, ",") != "id:INTEGER,region:STRING,amount:FLOAT" {
		t.Errorf("bq's schema read back as %v", cols)
	}

	rows := filepath.Join(t.TempDir(), "rows.json")
	if err := os.WriteFile(rows, []byte(`{"id":1,"region":"eu","amount":10}
{"id":2,"region":"eu","amount":5.5}
{"id":3,"region":"us","amount":7}
{"id":4,"region":"us","amount":1}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	b.must("insert", dataset+".orders", rows)

	out := b.must("query", "--nouse_legacy_sql", "--format=json",
		"SELECT region, SUM(amount) AS total, COUNT(*) AS n FROM `"+project+"."+dataset+
			".orders` WHERE amount > 2 GROUP BY region ORDER BY region")
	var got []map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("bq query printed no JSON rows: %v\n%s", err, out)
	}
	want := []map[string]string{{"region": "eu", "total": "15.5", "n": "2"}, {"region": "us", "total": "7", "n": "1"}}
	if len(got) != len(want) {
		t.Fatalf("bq query returned %v, want %v", got, want)
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("bq query row %d: %s = %q, want %q (%v)", i, k, got[i][k], v, got)
			}
		}
	}

	b.must("rm", "-f", "-t", dataset+".orders")
	if _, err := ds.Table("orders").Metadata(h.Context()); !isNotFound(err) {
		t.Errorf("the table bq removed still answers: %v", err)
	}
	b.must("rm", "-f", "-r", "-d", dataset)
	if _, err := ds.Metadata(h.Context()); !isNotFound(err) {
		t.Errorf("the dataset bq removed still answers: %v", err)
	}
}

// bqEnv is what `cloudburrow env` exports, with its BigQuery override
// checked against the endpoint the harness was given.
func bqEnv(t *testing.T, h *Harness) map[string]string {
	t.Helper()
	endpoint := strings.TrimSuffix(h.Endpoint(EnvBigQuery), "/")
	vars := envVarsFromCLI(t)
	if got := vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY"]; got != endpoint+"/" {
		t.Fatalf("cloudburrow env exported CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY=%q, want %q", got, endpoint+"/")
	}
	if vars["CLOUDSDK_CORE_PROJECT"] == "" {
		t.Fatalf("cloudburrow env exported no CLOUDSDK_CORE_PROJECT: %v", vars)
	}
	return vars
}

// TestBqThroughTheExportedOverride (#696): with the variables `cloudburrow
// env` exports, and no other configuration, bq --nouse_google_auth
// --oauth_access_token=<placeholder> makes a dataset and a table, inserts
// rows, runs a GoogleSQL query and removes both, against the emulator.
func TestBqThroughTheExportedOverride(t *testing.T) {
	h := New(t)
	vars := bqEnv(t, h)
	b := newBqSession(t, vars, "--nouse_google_auth", bqToken)
	exerciseBq(t, h, b, strings.ReplaceAll(h.Project(), "-", "_")+"_bqenv")
}

// TestBqThroughTheAPIFlag (#696): with no override in the environment,
// --api http://<endpoint> and --project_id, and the placeholder token, are
// enough: bq runs the same commands against the emulator. gcloud's
// config-helper still complains of no account on stderr; --nouse_google_auth
// quiets it.
func TestBqThroughTheAPIFlag(t *testing.T) {
	h := New(t)
	endpoint := strings.TrimSuffix(h.Endpoint(EnvBigQuery), "/")
	_, project := bigqueryClient(t, h)
	b := newBqSession(t, nil, "--api", endpoint, "--project_id", project, bqToken)
	for _, kv := range b.env {
		if strings.HasPrefix(kv, "CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY=") {
			t.Fatalf("the session inherited %s; this test is of the flag alone", kv)
		}
	}
	exerciseBq(t, h, b, strings.ReplaceAll(h.Project(), "-", "_")+"_bqapi")
}

// TestBqIgnoresTheOverrideWithoutNouseGoogleAuth (#696) pins why the flags
// are needed. With the exported variables alone, bq's wrapper refuses before
// any request. With the placeholder token but not --nouse_google_auth, bq
// reads no gcloud property, and its only request is to bigquery.googleapis.com,
// which the recording proxy refuses.
func TestBqIgnoresTheOverrideWithoutNouseGoogleAuth(t *testing.T) {
	h := New(t)
	vars := bqEnv(t, h)

	t.Run("no token", func(t *testing.T) {
		b := newBqSession(t, vars)
		proxy, hosts := refreshRecorder(t)
		out, errOut, err := b.run(proxy, "ls")
		if err == nil {
			t.Fatalf("bq ls ran with no credential; it is documented to refuse:\n%s", out)
		}
		if !strings.Contains(out+errOut, "You do not currently have an active account selected") {
			t.Errorf("bq failed otherwise than for want of an account:\n%s\n%s", out, errOut)
		}
		if got := hosts(); len(got) != 0 {
			t.Errorf("bq sent requests off the machine: %v", got)
		}
	})

	t.Run("token without --nouse_google_auth", func(t *testing.T) {
		b := newBqSession(t, vars, bqToken)
		proxy, hosts := refreshRecorder(t)
		out, errOut, err := b.run(proxy, "ls")
		t.Logf("bq ls:\nstdout:\n%s\nstderr:\n%s", out, errOut)
		got := hosts()
		if len(got) == 0 {
			t.Fatalf("bq sent nothing off the machine (err %v); if it now honours the override here, update docs/compatibility.md", err)
		}
		for _, host := range got {
			if host != "bigquery.googleapis.com:443" {
				t.Errorf("bq tried to reach %s", host)
			}
		}
		if err == nil {
			t.Errorf("bq ls succeeded though its request was refused")
		}
	})
}
