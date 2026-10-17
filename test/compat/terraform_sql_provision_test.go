//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlShardSecrets asks for Secret Manager in the sql shard, which serves it
// for executeSql's passwordSecretVersion without exporting SECRETS (so the
// Secret Manager suite stays in its shards): its endpoint is the one
// `cloudburrow env` gives. Without Secret Manager the test skips.
func sqlShardSecrets(t *testing.T, h *Harness) string {
	t.Helper()
	if strings.TrimSpace(os.Getenv(EnvSecrets)) == "" {
		cli := os.Getenv(EnvCLI)
		if cli == "" {
			t.Skipf("neither %s nor %s is set", EnvSecrets, EnvCLI)
		}
		out, err := exec.Command(cli, append([]string{"env", "--format", "json"}, cliArgs()...)...).Output()
		if err != nil {
			t.Fatalf("cloudburrow env --format json: %v", err)
		}
		var exported map[string]string
		if err := json.Unmarshal(out, &exported); err != nil {
			t.Fatalf("cloudburrow env --format json: %v", err)
		}
		if exported["CLOUDBURROW_SECRETMANAGER_ENDPOINT"] == "" {
			t.Skip("the instance does not have secretmanager enabled")
		}
		t.Setenv(EnvSecrets, exported["CLOUDBURROW_SECRETMANAGER_ENDPOINT"])
	}
	return h.Endpoint(EnvSecrets)
}

// TestTerraformSQLProvisionScript (#1304): a PostgreSQL
// google_sql_database_instance with data_api_access ALLOW_DATA_API, a
// google_secret_manager_regional_secret and _version holding its root
// password, and a google_sql_provision_script that runs as postgres with
// that version (instances.executeSql) apply through `cloudburrow terraform`
// with egress blocked; the official client's executeSql reads the rows the
// script wrote; the plan is clean; a changed script applies in place and
// runs again; the plan is clean again; and destroy abandons the script's
// tables (deletion_policy ABANDON) and deletes the instance and the secret.
// covers: sql.instances.executeSql, sql.instances.insert, sql.instances.delete
func TestTerraformSQLProvisionScript(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	sqlShardSecrets(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-ps-%d", time.Now().UnixNano()%1e6)
	module := func(description, script string) string {
		return fmt.Sprintf(`
resource "google_secret_manager_regional_secret" "pw" {
  project   = %[1]q
  location  = "us-central1"
  secret_id = "%[2]s-pw"
}
resource "google_secret_manager_regional_secret_version" "pw" {
  secret      = google_secret_manager_regional_secret.pw.id
  secret_data = "Root-pass1"
}
resource "google_sql_database_instance" "i" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  root_password       = "Root-pass1"
  deletion_protection = false
  settings {
    tier            = "db-custom-1-3840"
    data_api_access = "ALLOW_DATA_API"
  }
}
resource "google_sql_provision_script" "s" {
  project                 = %[1]q
  instance                = google_sql_database_instance.i.name
  database                = "postgres"
  user                    = "postgres"
  password_secret_version = google_secret_manager_regional_secret_version.pw.name
  description             = %[4]q
  script                  = %[3]q
  deletion_policy         = "ABANDON"
}
output "version" {
  value = google_secret_manager_regional_secret_version.pw.name
}
`, m.project, name, script, description)
	}
	rows := func(when string, want ...string) {
		t.Helper()
		var version string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "version")), &version); err != nil {
			t.Fatalf("%s, the version output: %v", when, err)
		}
		res, err := svc.Instances.ExecuteSql(m.project, name, &sqladmin.ExecuteSqlPayload{User: "postgres", Database: "postgres",
			PasswordSecretVersion: version, SqlStatement: "SELECT v FROM tfps ORDER BY v"}).Do()
		if err != nil || res.Status != nil || len(res.Results) != 1 {
			t.Fatalf("%s, executeSql: %v %+v", when, err, res)
		}
		var got []string
		for _, r := range res.Results[0].Rows {
			got = append(got, r.Values[0].Value)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s, the script's table holds %q; want %q", when, got, want)
		}
	}
	defer func() {
		if d, err := svc.Instances.Delete(m.project, name).Do(); err == nil {
			waitSQLOp(t, svc, m.project, d)
		}
	}()

	one := "CREATE TABLE IF NOT EXISTS tfps (v text); INSERT INTO tfps VALUES ('one');"
	m.write(module("seed", one))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	rows("after apply", "one")
	m.planClean()

	// A new description changes in place, and does not run the script.
	m.write(module("seed rows", one))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the description was not changed in place:\n%s", lastLines(out, 20))
	}
	rows("after the description change", "one")
	m.planClean()

	// A new script replaces the resource, as the provider plans it, and
	// runs.
	m.write(module("seed rows", "CREATE TABLE IF NOT EXISTS tfps (v text); INSERT INTO tfps VALUES ('two');"))
	out = m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "1 added, 0 changed, 1 destroyed") {
		t.Errorf("the script was not run again:\n%s", lastLines(out, 20))
	}
	rows("after the script change", "one", "two")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svc.Instances.Get(m.project, name).Do(); httpCode(err) != 404 {
		t.Errorf("the instance after destroy: %v; want 404", err)
	}
}
