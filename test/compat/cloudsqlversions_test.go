//go:build compat

package compat

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlDriverSeq numbers the connector drivers sqlEngine.open registers.
var sqlDriverSeq atomic.Int64

// sqlEngine is how a test reaches one engine's instances through the
// official Go connector.
type sqlEngine struct {
	mysql bool
}

func (e sqlEngine) open(t *testing.T, h *Harness, connName, user, password, db string) *sql.DB {
	t.Helper()
	// A process-wide counter, not the clock: parallel subtests read the same
	// nanosecond, and database/sql panics on a driver name registered twice.
	drv := fmt.Sprintf("cloudsql-v-%d", sqlDriverSeq.Add(1))
	var register func(string, ...cloudsqlconn.Option) (func() error, error)
	var dsn string
	if e.mysql {
		register = func(n string, o ...cloudsqlconn.Option) (func() error, error) {
			return cloudsqlmysql.RegisterDriver(n, o...)
		}
		dsn = fmt.Sprintf("%s:%s@%s(%s)/%s", user, password, drv, connName, db)
	} else {
		register = func(n string, o ...cloudsqlconn.Option) (func() error, error) { return pgxv5.RegisterDriver(n, o...) }
		dsn = fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", connName, user, password, db)
	}
	cleanup, err := register(drv, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	d, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func (e sqlEngine) root(t *testing.T, h *Harness, connName string) *sql.DB {
	if e.mysql {
		return e.open(t, h, connName, "root", "Root-pass1", "mysql")
	}
	return e.open(t, h, connName, "postgres", "x", "postgres")
}

func createSQLInstance(t *testing.T, svc *sqladmin.Service, project string, in *sqladmin.DatabaseInstance) *sqladmin.DatabaseInstance {
	t.Helper()
	op, err := svc.Instances.Insert(project, in).Do()
	if err != nil {
		t.Fatalf("insert %s (%s): %v", in.Name, in.DatabaseVersion, err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, in.Name).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("create %s: %+v", in.Name, op.Error.Errors[0])
	}
	got, err := svc.Instances.Get(project, in.Name).Do()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// eventually runs f until it succeeds, for a minute.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestCloudSQLDatabaseVersions (#1381): through the official Go client an
// instance of each other database version Cloud SQL offers is created;
// it reports the version, the databaseInstalledVersion its server reports
// and its upgradableDatabaseVersions, flags.list answers that version's
// flags, and through the official connector, with pgx or the MySQL driver,
// SELECT version() is that major version's server. PostgreSQL 9.6 to 12
// are served (#1462), and 9.6 to 11 refuse high availability and read
// replicas with why; MYSQL_5_7, MYSQL_5_6 and SQL Server (whose images
// are linux/amd64 only) and a version Cloud SQL no longer offers are 501.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.flags.list, sql.databases.insert, sql.databases.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLDatabaseVersions(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	type version struct {
		name, installed, server string
		upgradable              int
	}
	var versions []version
	if os.Getenv(EnvCloudSQL) != "" {
		versions = append(versions,
			version{"POSTGRES_9_6", "POSTGRES_9_6_24", "PostgreSQL 9.6.24", 9},
			version{"POSTGRES_10", "POSTGRES_10_23", "PostgreSQL 10.23", 8},
			version{"POSTGRES_11", "POSTGRES_11_22", "PostgreSQL 11.22", 7},
			version{"POSTGRES_12", "POSTGRES_12_22", "PostgreSQL 12.22", 6},
			version{"POSTGRES_13", "POSTGRES_13_23", "PostgreSQL 13.23", 5},
			version{"POSTGRES_14", "POSTGRES_14_24", "PostgreSQL 14.24", 4},
			version{"POSTGRES_15", "POSTGRES_15_19", "PostgreSQL 15.19", 3},
			version{"POSTGRES_16", "POSTGRES_16_15", "PostgreSQL 16.15", 2},
			version{"POSTGRES_18", "POSTGRES_18_6", "PostgreSQL 18.6", 0})
	}
	if os.Getenv(EnvMySQL) != "" {
		versions = append(versions, version{"MYSQL_8_0", "MYSQL_8_0_46", "8.0.46", 1})
	}
	for _, bad := range []string{"MYSQL_5_7", "MYSQL_5_6", "SQLSERVER_2022_STANDARD", "POSTGRES_8_4"} {
		_, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: "bad-" + suffix, DatabaseVersion: bad,
			Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
		wantHTTP(t, "insert "+bad, err, 501)
	}
	for _, v := range versions {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			e := sqlEngine{mysql: strings.HasPrefix(v.name, "MYSQL_")}
			name := strings.ToLower(strings.ReplaceAll(v.name, "_", "-")) + "-" + suffix
			in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: v.name,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			if in.DatabaseVersion != v.name || in.DatabaseInstalledVersion != v.installed || in.State != "RUNNABLE" ||
				len(in.UpgradableDatabaseVersions) != v.upgradable {
				t.Errorf("instance: version %s installed %s state %s upgradable %d", in.DatabaseVersion,
					in.DatabaseInstalledVersion, in.State, len(in.UpgradableDatabaseVersions))
			}
			old := map[string]bool{"POSTGRES_9_6": true, "POSTGRES_10": true, "POSTGRES_11": true, "POSTGRES_12": true}[v.name]
			flags, err := svc.Flags.List().DatabaseVersion(v.name).Do()
			if old {
				// The flag table here is Cloud SQL's for PostgreSQL 13 and later.
				wantHTTP(t, "flags.list "+v.name, err, 501)
				flags = &sqladmin.FlagsListResponse{}
			} else if err != nil || len(flags.Items) < 100 {
				t.Errorf("flags.list %s: %v", v.name, err)
			}
			for _, f := range flags.Items {
				if len(f.AppliesTo) != 1 || f.AppliesTo[0] != v.name {
					t.Errorf("flag %s applies to %v", f.Name, f.AppliesTo)
					break
				}
			}
			db := e.root(t, h, in.ConnectionName)
			var got string
			eventually(t, "SELECT version()", func() error { return db.QueryRow("SELECT version()").Scan(&got) })
			if !strings.Contains(got, v.server) {
				t.Errorf("SELECT version() = %q, want %q", got, v.server)
			}
			if v.name == "POSTGRES_9_6" || v.name == "POSTGRES_11" {
				// Before PostgreSQL 12 (no pg_promote, no recovery.signal):
				// high availability and read replicas are 501 with why.
				_, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{
					Settings: &sqladmin.Settings{AvailabilityType: "REGIONAL"}}).Do()
				wantHTTP(t, "REGIONAL on "+v.name, err, 501)
				_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-r", MasterInstanceName: name,
					Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
				wantHTTP(t, "a read replica of "+v.name, err, 501)
				// Its databases and users are served.
				op, err := svc.Databases.Insert(project, name, &sqladmin.Database{Name: "app"}).Do()
				if err != nil {
					t.Fatal(err)
				}
				waitSQLOpDone(t, svc, project, op)
				if d, err := svc.Databases.Get(project, name, "app").Do(); err != nil || d.Charset != "UTF8" {
					t.Errorf("databases.get app: %+v %v", d, err)
				}
			}
		})
	}
}

// TestCloudSQLMajorVersionUpgrade (#1381): instances.patch with a newer
// databaseVersion, as gcloud sql instances patch --database-version does,
// runs a MAJOR_VERSION_UPGRADE operation; afterwards the server is the new
// version, and a table's rows and a user created through users.insert,
// with its password, are still there. A downgrade and an engine change are
// 400.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.users.insert, sql.operations.get
func TestCloudSQLMajorVersionUpgrade(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	type path struct{ from, to, server string }
	var paths []path
	if os.Getenv(EnvCloudSQL) != "" {
		paths = append(paths, path{"POSTGRES_16", "POSTGRES_17", "PostgreSQL 17."})
	}
	if os.Getenv(EnvMySQL) != "" {
		paths = append(paths, path{"MYSQL_8_0", "MYSQL_8_4", "8.4."})
	}
	for _, p := range paths {
		t.Run(p.from, func(t *testing.T) {
			t.Parallel()
			e := sqlEngine{mysql: strings.HasPrefix(p.from, "MYSQL_")}
			name := "up-" + strings.ToLower(strings.ReplaceAll(p.from, "_", "-")) + "-" + suffix
			in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: p.from,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			op, err := svc.Users.Insert(project, name, &sqladmin.User{Name: "app", Password: "App-pass1"}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOpDone(t, svc, project, op)
			db := e.root(t, h, in.ConnectionName)
			table := "t"
			stmts := []string{"CREATE TABLE t (v varchar(20))", "INSERT INTO t VALUES ('kept')"}
			if e.mysql {
				table = "app.t"
				stmts = []string{"CREATE DATABASE app", "CREATE TABLE app.t (v varchar(20))", "INSERT INTO app.t VALUES ('kept')"}
			}
			for _, q := range stmts {
				q := q
				eventually(t, q, func() error { _, err := db.Exec(q); return err })
			}
			for _, bad := range []string{"POSTGRES_13", "MYSQL_8_0", "POSTGRES_16"} {
				if bad == p.from {
					continue
				}
				if !e.mysql && bad == "POSTGRES_16" {
					bad = "MYSQL_8_4"
				}
				_, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: bad}).Do()
				wantHTTP(t, "patch to "+bad, err, 400)
			}
			op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: p.to}).Do()
			if err != nil {
				t.Fatal(err)
			}
			if op.OperationType != "MAJOR_VERSION_UPGRADE" {
				t.Errorf("operation type %s", op.OperationType)
			}
			if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
				t.Fatalf("upgrade: %+v", op.Error.Errors[0])
			}
			got, err := svc.Instances.Get(project, name).Do()
			if err != nil || got.DatabaseVersion != p.to || !strings.HasPrefix(got.DatabaseInstalledVersion, p.to+"_") || got.State != "RUNNABLE" {
				t.Fatalf("after the upgrade: %+v %v", got, err)
			}
			db = e.root(t, h, in.ConnectionName)
			var v string
			eventually(t, "SELECT version()", func() error { return db.QueryRow("SELECT version()").Scan(&v) })
			if !strings.Contains(v, p.server) {
				t.Errorf("SELECT version() = %q, want %s", v, p.server)
			}
			var n int
			if err := db.QueryRow("SELECT count(*) FROM " + table + " WHERE v = 'kept'").Scan(&n); err != nil || n != 1 {
				t.Errorf("the row after the upgrade: %d %v", n, err)
			}
			appDB := "postgres"
			if e.mysql {
				appDB = "app"
			}
			if err := e.open(t, h, in.ConnectionName, "app", "App-pass1", appDB).Ping(); err != nil {
				t.Errorf("the user app after the upgrade: %v", err)
			}
		})
	}
}

// TestCloudSQLCascadingAndCrossRegionReplicas (#1381): a read replica in
// another region than its primary's reports that region and a connection
// name in it, and the connector reaches it by that name; a replica of
// that replica (a cascading replica) receives the rows written on the top
// primary. Promoting the middle replica, as Cloud SQL documents for a
// cascading replica, makes it a primary whose own replica goes on
// receiving its writes. A cascading replica beyond Cloud SQL's four levels
// is 400.
// covers: sql.instances.insert, sql.instances.get, sql.instances.promoteReplica, sql.instances.delete, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLCascadingAndCrossRegionReplicas(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var versions []string
	if os.Getenv(EnvCloudSQL) != "" {
		versions = append(versions, "POSTGRES_17")
	}
	if os.Getenv(EnvMySQL) != "" {
		versions = append(versions, "MYSQL_8_4")
	}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			e := sqlEngine{mysql: strings.HasPrefix(version, "MYSQL_")}
			short := strings.ToLower(strings.SplitN(version, "_", 2)[0])
			// Cleanups run last-in first-out: the leaf replica goes first.
			p := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: short + "-cp-" + suffix, DatabaseVersion: version,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			pdb := e.root(t, h, p.ConnectionName)
			table := "t"
			stmts := []string{"CREATE TABLE t (v varchar(20))", "INSERT INTO t VALUES ('one')"}
			if e.mysql {
				table = "app.t"
				stmts = []string{"CREATE DATABASE app", "CREATE TABLE app.t (v varchar(20))", "INSERT INTO app.t VALUES ('one')"}
			}
			for _, q := range stmts {
				q := q
				eventually(t, q, func() error { _, err := pdb.Exec(q); return err })
			}
			r1 := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: short + "-cr1-" + suffix,
				MasterInstanceName: p.Name, InstanceType: "READ_REPLICA_INSTANCE", Region: "us-east1"})
			if r1.Region != "us-east1" || r1.GceZone != "us-east1-a" || r1.ConnectionName != project+":us-east1:"+r1.Name {
				t.Errorf("cross-region replica: region %s zone %s connection %s", r1.Region, r1.GceZone, r1.ConnectionName)
			}
			r2 := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: short + "-cr2-" + suffix,
				MasterInstanceName: r1.Name, InstanceType: "READ_REPLICA_INSTANCE"})
			if r2.MasterInstanceName != project+":"+r1.Name || r2.InstanceType != "READ_REPLICA_INSTANCE" || r2.Region != "us-east1" {
				t.Errorf("cascading replica: master %s type %s region %s", r2.MasterInstanceName, r2.InstanceType, r2.Region)
			}
			if got, err := svc.Instances.Get(project, r1.Name).Do(); err != nil || len(got.ReplicaNames) != 1 || got.ReplicaNames[0] != r2.Name {
				t.Errorf("the middle replica's replicaNames: %v %v", got.ReplicaNames, err)
			}
			r1db, r2db := e.root(t, h, r1.ConnectionName), e.root(t, h, r2.ConnectionName)
			has := func(db *sql.DB, v string) func() error {
				return func() error {
					var n int
					if err := db.QueryRow("SELECT count(*) FROM " + table + " WHERE v = '" + v + "'").Scan(&n); err != nil {
						return err
					}
					if n != 1 {
						return fmt.Errorf("%d rows of %q", n, v)
					}
					return nil
				}
			}
			eventually(t, "the cascading replica has the primary's first row", has(r2db, "one"))
			if _, err := pdb.Exec("INSERT INTO " + table + " VALUES ('two')"); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the cascading replica receives a write on the top primary", has(r2db, "two"))
			eventually(t, "the cross-region replica has it", has(r1db, "two"))
			if _, err := r2db.Exec("INSERT INTO " + table + " VALUES ('x')"); err == nil {
				t.Error("a write on the cascading replica succeeded")
			}

			op, err := svc.Instances.PromoteReplica(project, r1.Name).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOpDone(t, svc, project, op)
			if got, err := svc.Instances.Get(project, r1.Name).Do(); err != nil || got.MasterInstanceName != "" || len(got.ReplicaNames) != 1 {
				t.Errorf("the promoted middle replica: master %q replicas %v %v", got.MasterInstanceName, got.ReplicaNames, err)
			}
			eventually(t, "a write on the promoted replica", func() error {
				_, err := r1db.Exec("INSERT INTO " + table + " VALUES ('three')")
				return err
			})
			eventually(t, "its replica receives it", has(r2db, "three"))

			if e.mysql {
				return
			}
			// Levels 2 to 4 under the promoted r1, whose replica r2 is at
			// level 1; a fifth level is refused.
			parent := r2.Name
			for i := 3; i <= 6; i++ {
				in := &sqladmin.DatabaseInstance{Name: fmt.Sprintf("%s-cr%d-%s", short, i, suffix), MasterInstanceName: parent,
					InstanceType: "READ_REPLICA_INSTANCE"}
				if i == 6 {
					_, err := svc.Instances.Insert(project, in).Do()
					wantHTTP(t, "a fifth level of replicas", err, 400)
					break
				}
				parent = createSQLInstance(t, svc, project, in).Name
			}
		})
	}
}

// TestGcloudSQLDatabaseVersions (#1381): `gcloud sql instances create
// --database-version=POSTGRES_15`, `patch --database-version=POSTGRES_16`
// (a major version upgrade), and a cross-region replica with
// `--master-instance-name` and `--region`, through the configuration
// `cloudburrow gcloud-setup` writes, with egress to Google blocked.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.operations.get
func TestGcloudSQLDatabaseVersions(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	g := newGcloudSession(t, h)
	name := fmt.Sprintf("gcloud-v-%d", time.Now().UnixNano()%1e6)
	g.must("sql", "instances", "create", name, "--project", g.project, "--database-version=POSTGRES_15",
		"--tier=db-custom-1-3840", "--region=us-central1", "--no-backup", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", name, "--project", g.project, "--quiet")
	out := g.must("sql", "instances", "describe", name, "--project", g.project, "--format=value(databaseVersion,databaseInstalledVersion)")
	if !strings.Contains(out, "POSTGRES_15") || !strings.Contains(out, "POSTGRES_15_19") {
		t.Errorf("describe: %s", out)
	}
	g.must("sql", "instances", "patch", name, "--project", g.project, "--database-version=POSTGRES_16", "--quiet")
	out = g.must("sql", "instances", "describe", name, "--project", g.project, "--format=value(databaseVersion,databaseInstalledVersion,state)")
	if !strings.Contains(out, "POSTGRES_16_15") || !strings.Contains(out, "RUNNABLE") {
		t.Errorf("describe after the upgrade: %s", out)
	}
	replica := name + "-r"
	g.must("sql", "instances", "create", replica, "--project", g.project, "--master-instance-name="+name,
		"--region=europe-west1", "--tier=db-custom-1-3840", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", replica, "--project", g.project, "--quiet")
	out = g.must("sql", "instances", "describe", replica, "--project", g.project, "--format=value(region,connectionName,instanceType)")
	if !strings.Contains(out, "europe-west1") || !strings.Contains(out, g.project+":europe-west1:"+replica) || !strings.Contains(out, "READ_REPLICA_INSTANCE") {
		t.Errorf("describe the cross-region replica: %s", out)
	}
	g.must("sql", "instances", "delete", replica, "--project", g.project, "--quiet")
}

// TestTerraformSQLDatabaseVersion (#1381): a google_sql_database_instance
// with database_version POSTGRES_14 applies through `cloudburrow
// terraform` with the official hashicorp/google provider, its plan is
// then empty, changing database_version to POSTGRES_15 upgrades it in
// place (the instance is not replaced: its createTime stays), and destroy
// removes it.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.operations.get
func TestTerraformSQLDatabaseVersion(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-v-%d", time.Now().UnixNano()%1e6)
	module := func(version string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "main" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = %[3]q
  deletion_protection = false
  settings {
    tier = "db-custom-1-3840"
  }
}
`, m.project, name, version)
	}
	m.write(module("POSTGRES_14"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := svc.Instances.Get(m.project, name).Do()
	if err != nil || in.DatabaseInstalledVersion != "POSTGRES_14_24" {
		t.Fatalf("after apply: %+v %v", in, err)
	}
	m.planClean()
	m.write(module("POSTGRES_15"))
	m.apply()
	up, err := svc.Instances.Get(m.project, name).Do()
	if err != nil || up.DatabaseVersion != "POSTGRES_15" || up.DatabaseInstalledVersion != "POSTGRES_15_19" || up.CreateTime != in.CreateTime {
		t.Errorf("after the version change: %+v %v", up, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = svc.Instances.Get(m.project, name).Do()
	wantHTTP(t, "instances.get after destroy", err, 404)
}
