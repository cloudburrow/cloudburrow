//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// waitSQLOp polls operations.get until op is DONE and fails on its error.
func waitSQLOp(t *testing.T, svc *sqladmin.Service, project string, op *sqladmin.Operation) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for op.Status != "DONE" {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s (%s) did not finish", op.Name, op.OperationType)
		}
		time.Sleep(time.Second)
		var err error
		if op, err = svc.Operations.Get(project, op.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		t.Fatalf("operation %s (%s): %s", op.Name, op.OperationType, op.Error.Errors[0].Message)
	}
}

// TestCloudSQLInstanceWrites (#1330): through the official Go client
// (sqladmin/v1), an instance is created on each engine, served by a server
// of its own: the operation finishes once the server serves, and the
// official connector (cloudsqlconn) then dials it by connection name and
// runs SELECT 1. A patch changes labels, the tier as recorded and a database
// flag, which the server itself reports afterwards; deletion protection
// refuses a delete until a patch lifts it; a restart restarts the server;
// delete removes it, and the instances `up` runs refuse to be deleted.
// covers: sql.instances.insert, sql.instances.get, sql.instances.list, sql.instances.patch, sql.instances.update, sql.instances.restart, sql.instances.delete, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLInstanceWrites(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	type engine struct {
		version, flag, value, show string
		dsn                        func(drv, conn string) string
		register                   func(string, ...cloudsqlconn.Option) (func() error, error)
	}
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var engines []engine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, engine{version: "POSTGRES_17", flag: "max_connections", value: "42",
			dsn: func(_, conn string) string {
				return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
			}, show: "SHOW max_connections",
			register: func(n string, o ...cloudsqlconn.Option) (func() error, error) { return pgxv5.RegisterDriver(n, o...) }})
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, engine{version: "MYSQL_8_4", flag: "max_connections", value: "42",
			dsn: func(drv, conn string) string { return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn) }, show: "SELECT @@max_connections",
			register: func(n string, o ...cloudsqlconn.Option) (func() error, error) {
				return cloudsqlmysql.RegisterDriver(n, o...)
			}})
	}
	for _, e := range engines {
		t.Run(e.version, func(t *testing.T) {
			name := strings.ToLower(strings.ReplaceAll(e.version, "_", "-")) + "-" + suffix
			op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{
				Name: name, DatabaseVersion: e.version, Region: "us-central1", RootPassword: "Root-pass1",
				Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", UserLabels: map[string]string{"env": "test"},
					DeletionProtectionEnabled: true},
			}).Do()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				p, _ := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
					DeletionProtectionEnabled: false, ForceSendFields: []string{"DeletionProtectionEnabled"}}}).Do()
				if p != nil {
					waitSQLOp(t, svc, project, p)
				}
				if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
			}()
			if op.OperationType != "CREATE" || op.TargetId != name {
				t.Errorf("insert's operation: %+v", op)
			}
			waitSQLOp(t, svc, project, op)
			in, err := svc.Instances.Get(project, name).Do()
			if err != nil {
				t.Fatal(err)
			}
			if in.State != "RUNNABLE" || in.ConnectionName != project+":us-central1:"+name || in.DatabaseVersion != e.version ||
				in.Settings.Tier != "db-custom-1-3840" || in.Settings.UserLabels["env"] != "test" || !in.Settings.DeletionProtectionEnabled ||
				len(in.IpAddresses) != 1 || in.DatabaseInstalledVersion == "" {
				t.Errorf("instance: %+v settings %+v", in, in.Settings)
			}
			list, err := svc.Instances.List(project).Do()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, i := range list.Items {
				found = found || i.Name == name
			}
			if !found {
				t.Errorf("instances.list does not list %s", name)
			}

			query := func(q string) string {
				t.Helper()
				drv := "cloudsql-" + strings.ToLower(e.version) + "-" + suffix + fmt.Sprint(time.Now().UnixNano())
				cleanup, err := e.register(drv, connectorOptions(h)...)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				db, err := sql.Open(drv, e.dsn(drv, in.ConnectionName))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				ctx, cancel := context.WithTimeout(h.Context(), time.Minute)
				defer cancel()
				var out string
				if err := db.QueryRowContext(ctx, q).Scan(&out); err != nil {
					t.Fatalf("%s through the connector: %v", q, err)
				}
				return out
			}
			if got := query("SELECT 1"); got != "1" {
				t.Errorf("SELECT 1: %s", got)
			}

			// Deletion protection.
			_, err = svc.Instances.Delete(project, name).Do()
			wantHTTP(t, "delete of a protected instance", err, http.StatusBadRequest)

			// patch: labels, tier, a flag the server takes, protection off.
			op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
				Tier: "db-custom-2-7680", UserLabels: map[string]string{"env": "patched"},
				DatabaseFlags:             []*sqladmin.DatabaseFlags{{Name: e.flag, Value: e.value}},
				DeletionProtectionEnabled: false, ForceSendFields: []string{"DeletionProtectionEnabled"}}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			in, err = svc.Instances.Get(project, name).Do()
			if err != nil {
				t.Fatal(err)
			}
			if in.Settings.Tier != "db-custom-2-7680" || in.Settings.UserLabels["env"] != "patched" || in.Settings.DeletionProtectionEnabled ||
				len(in.Settings.DatabaseFlags) != 1 || in.State != "RUNNABLE" {
				t.Errorf("after patch: %+v", in.Settings)
			}
			if got := query(e.show); got != e.value {
				t.Errorf("%s after the patch: %s, want %s", e.show, got, e.value)
			}

			// update replaces the settings: the labels go.
			op, err = svc.Instances.Update(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
				Tier: "db-custom-2-7680", DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: e.flag, Value: e.value}}}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			if in, _ = svc.Instances.Get(project, name).Do(); in == nil || len(in.Settings.UserLabels) != 0 {
				t.Errorf("after update the labels remain: %+v", in)
			}

			op, err = svc.Instances.Restart(project, name).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			if got := query("SELECT 1"); got != "1" {
				t.Errorf("SELECT 1 after restart: %s", got)
			}

			op, err = svc.Instances.Delete(project, name).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			_, err = svc.Instances.Get(project, name).Do()
			wantHTTP(t, "get after delete", err, http.StatusNotFound)
		})
	}

	// The instances `up` runs stay.
	up := "postgres"
	if os.Getenv(EnvCloudSQL) == "" {
		up = "mysql"
	}
	_, err := svc.Instances.Delete(project, up).Do()
	wantHTTP(t, "delete of the instance up runs", err, http.StatusBadRequest)
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: "old-" + suffix, DatabaseVersion: "POSTGRES_9_6",
		Settings: &sqladmin.Settings{Tier: "db-f1-micro"}}).Do()
	wantHTTP(t, "a version no image provides", err, http.StatusNotImplemented)
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: "rep-" + suffix, DatabaseVersion: "POSTGRES_17",
		MasterInstanceName: up, Settings: &sqladmin.Settings{Tier: "db-f1-micro"}}).Do()
	wantHTTP(t, "a replica", err, http.StatusNotImplemented)
}

// TestGcloudSQLInstances (#1330): `gcloud sql instances create`, describe,
// list and delete, through the configuration `cloudburrow gcloud-setup`
// writes, with egress to Google blocked.
// covers: sql.instances.insert, sql.instances.get, sql.instances.list, sql.instances.delete, sql.operations.get
func TestGcloudSQLInstances(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	g := newGcloudSession(t, h)
	name := fmt.Sprintf("gcloud-pg-%d", time.Now().UnixNano()%1e6)
	g.must("sql", "instances", "create", name, "--project", g.project, "--database-version=POSTGRES_17",
		"--tier=db-custom-1-3840", "--region=us-central1", "--no-backup", "--quiet")
	out := g.must("sql", "instances", "describe", name, "--project", g.project, "--format=value(state,databaseVersion,connectionName)")
	if !strings.Contains(out, "RUNNABLE") || !strings.Contains(out, "POSTGRES_17") || !strings.Contains(out, g.project+":us-central1:"+name) {
		t.Errorf("describe: %s", out)
	}
	if out := g.must("sql", "instances", "list", "--project", g.project, "--format=value(name)"); !strings.Contains(out, name) {
		t.Errorf("list: %s", out)
	}
	g.must("sql", "instances", "delete", name, "--project", g.project, "--quiet")
	if out, err := g.run(nil, "sql", "instances", "describe", name, "--project", g.project); err == nil {
		t.Errorf("describe after delete succeeded: %s", out)
	}
}

// TestTerraformSQLDatabaseInstance (#1330): a google_sql_database_instance
// and a google_sql_database in it apply with the official hashicorp/google
// provider through `cloudburrow terraform`, the second plan is empty, a
// label change applies in place, and destroy removes both.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.databases.insert, sql.databases.get, sql.databases.delete, sql.operations.get
func TestTerraformSQLDatabaseInstance(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-pg-%d", time.Now().UnixNano()%1e6)
	module := func(label string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "main" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  deletion_protection = false
  settings {
    tier        = "db-custom-1-3840"
    user_labels = { env = %[3]q }
    database_flags {
      name  = "max_connections"
      value = "60"
    }
  }
}
resource "google_sql_database" "orders" {
  project  = %[1]q
  instance = google_sql_database_instance.main.name
  name     = "orders"
}
`, m.project, name, label)
	}
	m.write(module("a"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := svc.Instances.Get(m.project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.Settings.UserLabels["env"] != "a" {
		t.Errorf("after apply: %s %+v", in.State, in.Settings)
	}
	if _, err := svc.Databases.Get(m.project, name, "orders").Do(); err != nil {
		t.Errorf("databases.get: %v", err)
	}
	m.planClean()
	m.write(module("b"))
	m.apply()
	if in, err = svc.Instances.Get(m.project, name).Do(); err != nil || in.Settings.UserLabels["env"] != "b" {
		t.Errorf("after the label change: %v %+v", err, in)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = svc.Instances.Get(m.project, name).Do()
	wantHTTP(t, "instances.get after destroy", err, http.StatusNotFound)
}
