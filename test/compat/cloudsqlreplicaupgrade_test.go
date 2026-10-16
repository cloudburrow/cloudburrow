//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLMySQLReplicaUpgradedOnItsOwn (#1462): through the official
// Go client, a MySQL read replica is upgraded on its own, before its
// primary, as Cloud SQL documents ("upgrade the read replicas before you
// upgrade the primary instance"): instances.patch of the replica with
// MYSQL_8_4 is a MAJOR_VERSION_UPGRADE, after which the replica's server
// is 8.4 and goes on receiving the 8.0 primary's writes, as MySQL
// replicates to a replica of the next version. The primary is then
// upgraded without includeReplicasForMajorVersionUpgrade, and its replica
// still receives its writes.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.instances.delete, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLMySQLReplicaUpgradedOnItsOwn(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	e := sqlEngine{mysql: true}
	name := fmt.Sprintf("my-ru-%d", time.Now().UnixNano()%1e6)
	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_8_0",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	db := e.root(t, h, in.ConnectionName)
	for _, q := range []string{"CREATE DATABASE app", "CREATE TABLE app.t (v varchar(20))", "INSERT INTO app.t VALUES ('first')"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	rep := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name + "-r", MasterInstanceName: name,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	seen := func(what, v string) {
		t.Helper()
		rdb := e.root(t, h, rep.ConnectionName)
		eventually(t, what, func() error {
			var n int
			if err := rdb.QueryRow("SELECT count(*) FROM app.t WHERE v = ?", v).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("%d rows %q on the replica", n, v)
			}
			return nil
		})
	}
	seen("the first row on the replica", "first")
	if r, err := svc.Instances.Get(project, rep.Name).Do(); err != nil || len(r.UpgradableDatabaseVersions) != 1 ||
		r.UpgradableDatabaseVersions[0].MajorVersion != "MYSQL_8_4" {
		t.Errorf("the replica's upgradableDatabaseVersions: %+v %v", r, err)
	}

	op, err := svc.Instances.Patch(project, rep.Name, &sqladmin.DatabaseInstance{DatabaseVersion: "MYSQL_8_4"}).Do()
	if err != nil {
		t.Fatalf("upgrade the replica: %v", err)
	}
	if op.OperationType != "MAJOR_VERSION_UPGRADE" {
		t.Errorf("operation type %s", op.OperationType)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("upgrade the replica: %+v", op.Error.Errors[0])
	}
	got, err := svc.Instances.Get(project, rep.Name).Do()
	if err != nil || got.DatabaseVersion != "MYSQL_8_4" || got.State != "RUNNABLE" || !strings.HasSuffix(got.MasterInstanceName, ":"+name) {
		t.Fatalf("the replica after its upgrade: %+v %v", got, err)
	}
	if p, err := svc.Instances.Get(project, name).Do(); err != nil || p.DatabaseVersion != "MYSQL_8_0" {
		t.Errorf("the primary after its replica's upgrade: %+v %v", p, err)
	}
	var v string
	eventually(t, "SELECT version() on the replica", func() error { return e.root(t, h, rep.ConnectionName).QueryRow("SELECT version()").Scan(&v) })
	if !strings.HasPrefix(v, "8.4.") {
		t.Errorf("the replica's server is %s, want 8.4", v)
	}
	seen("the first row on the upgraded replica", "first")
	if _, err := db.Exec("INSERT INTO app.t VALUES ('second')"); err != nil {
		t.Fatal(err)
	}
	seen("a row written on the 8.0 primary after the replica's upgrade", "second")

	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: "MYSQL_8_4"}).Do()
	if err != nil {
		t.Fatalf("upgrade the primary: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("upgrade the primary: %+v", op.Error.Errors[0])
	}
	db = e.root(t, h, in.ConnectionName)
	eventually(t, "a write on the upgraded primary", func() error { _, err := db.Exec("INSERT INTO app.t VALUES ('third')"); return err })
	seen("a row written on the upgraded primary", "third")
}

// TestCloudSQLPostgresOldVersionsAndRefusals (#1462): a POSTGRES_10
// instance is upgraded to POSTGRES_12 keeping its rows; a read replica of
// it is created, and upgrading that replica on its own is 501 with why
// (physical replication does not cross major versions). Changing a
// PostgreSQL database's charset is 400, as Cloud SQL documents it is not
// set through the API; a BAK import is 501 (SQL Server's); a legacy
// failover replica is 400, as Cloud SQL refuses one since 2025.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.instances.import, sql.instances.delete, sql.databases.insert, sql.databases.patch, sql.operations.get
func TestCloudSQLPostgresOldVersionsAndRefusals(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	e := sqlEngine{}
	name := fmt.Sprintf("pg-old-%d", time.Now().UnixNano()%1e6)
	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_10",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	if in.DatabaseInstalledVersion != "POSTGRES_10_23" {
		t.Errorf("installed version %s", in.DatabaseInstalledVersion)
	}
	db := e.root(t, h, in.ConnectionName)
	for _, q := range []string{"CREATE TABLE t (v varchar(20))", "INSERT INTO t VALUES ('kept')"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	op, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: "POSTGRES_12"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("upgrade: %+v", op.Error.Errors[0])
	}
	got, err := svc.Instances.Get(project, name).Do()
	if err != nil || got.DatabaseInstalledVersion != "POSTGRES_12_22" || got.State != "RUNNABLE" {
		t.Fatalf("after the upgrade: %+v %v", got, err)
	}
	db = e.root(t, h, in.ConnectionName)
	var n int
	eventually(t, "the row after the upgrade", func() error { return db.QueryRow("SELECT count(*) FROM t WHERE v = 'kept'").Scan(&n) })
	if n != 1 {
		t.Errorf("%d rows kept", n)
	}

	rep := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name + "-r", MasterInstanceName: name,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	eventually(t, "the row on the POSTGRES_12 replica", func() error {
		return e.root(t, h, rep.ConnectionName).QueryRow("SELECT count(*) FROM t WHERE v = 'kept'").Scan(&n)
	})
	_, err = svc.Instances.Patch(project, rep.Name, &sqladmin.DatabaseInstance{DatabaseVersion: "POSTGRES_13"}).Do()
	wantHTTP(t, "upgrading a PostgreSQL replica on its own", err, 501)

	op, err = svc.Databases.Insert(project, name, &sqladmin.Database{Name: "app"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, op)
	_, err = svc.Databases.Patch(project, name, "app", &sqladmin.Database{Charset: "LATIN1"}).Do()
	wantHTTP(t, "changing a PostgreSQL database's charset", err, 400)

	_, err = svc.Instances.Import(project, name, &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
		FileType: "BAK", Uri: "gs://b/x.bak", Database: "app"}}).Do()
	wantHTTP(t, "a BAK import", err, 501)
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-f", MasterInstanceName: name,
		ReplicaConfiguration: &sqladmin.ReplicaConfiguration{FailoverTarget: true}, Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	wantHTTP(t, "a legacy failover replica", err, 400)
}

// TestGcloudSQLOldPostgresVersion (#1462): `gcloud sql instances create
// --database-version=POSTGRES_12` creates an instance whose
// databaseInstalledVersion is POSTGRES_12_22; `gcloud sql databases patch
// --charset` of its database is refused (400), and `--database-version=MYSQL_5_7`
// is 501 naming why, through the configuration `cloudburrow gcloud-setup`
// writes, with egress to Google blocked.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete, sql.databases.insert, sql.databases.get, sql.databases.patch, sql.operations.get
func TestGcloudSQLOldPostgresVersion(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	name := fmt.Sprintf("gcloud-pg12-%d", time.Now().UnixNano()%1e6)
	g.must("sql", "instances", "create", name, "--project", g.project, "--database-version=POSTGRES_12",
		"--tier=db-custom-1-3840", "--region=us-central1", "--no-backup", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", name, "--project", g.project, "--quiet")
	out := g.must("sql", "instances", "describe", name, "--project", g.project, "--format=value(databaseVersion,databaseInstalledVersion)")
	if !strings.Contains(out, "POSTGRES_12_22") {
		t.Errorf("describe: %s", out)
	}
	g.must("sql", "databases", "create", "app", "--instance", name, "--project", g.project, "--quiet")
	if out, err := g.run(nil, "sql", "databases", "patch", "app", "--instance", name, "--project", g.project, "--charset=LATIN1", "--quiet"); err == nil ||
		!strings.Contains(out, "can't be changed") {
		t.Errorf("databases patch --charset: %v %s", err, out)
	}
	if out, err := g.run(nil, "sql", "instances", "create", name+"-x", "--project", g.project, "--database-version=MYSQL_5_7",
		"--tier=db-custom-1-3840", "--region=us-central1", "--quiet"); err == nil || !strings.Contains(out, "linux/amd64 only") {
		t.Errorf("create MYSQL_5_7: %v %s", err, out)
	}
}

// TestTerraformSQLOldPostgresVersion (#1462): a google_sql_database_instance
// with database_version POSTGRES_11 applies through `cloudburrow
// terraform` with the official hashicorp/google provider, its plan is
// then empty, and destroy removes it.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete, sql.operations.get
func TestTerraformSQLOldPostgresVersion(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-pg11-%d", time.Now().UnixNano()%1e6)
	m.write(fmt.Sprintf(`
resource "google_sql_database_instance" "main" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_11"
  deletion_protection = false
  settings {
    tier = "db-custom-1-3840"
  }
}
`, m.project, name))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := svc.Instances.Get(m.project, name).Do()
	if err != nil || in.DatabaseInstalledVersion != "POSTGRES_11_22" {
		t.Fatalf("after apply: %+v %v", in, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = svc.Instances.Get(m.project, name).Do()
	wantHTTP(t, "instances.get after destroy", err, 404)
}
