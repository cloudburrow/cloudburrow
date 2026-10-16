//go:build compat

package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// EnvSQLAdmin is the host address of the Cloud SQL Admin API (#1301).
const EnvSQLAdmin = "CLOUDBURROW_TEST_SQLADMIN"

func sqlAdminService(t *testing.T, h *Harness) *sqladmin.Service {
	t.Helper()
	svc, err := sqladmin.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvSQLAdmin)+"/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func wantHTTP(t *testing.T, what string, err error, code int) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != code {
		t.Errorf("%s: %v; want HTTP %d", what, err, code)
	}
}

// TestSQLAdminInstancesDescribeTheRunningServers (#1301): through the
// official Go client (google.golang.org/api/sqladmin/v1), instances.list
// and instances.get describe the PostgreSQL and MySQL servers the instance
// runs: RUNNABLE, the Cloud SQL databaseVersion, the server's own installed
// version, a project:region:instance connection name, and a PRIMARY
// address the server is reached at with an ordinary driver.
// covers: sql.instances.list, sql.instances.get
func TestSQLAdminInstancesDescribeTheRunningServers(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	project := h.Project()
	list, err := svc.Instances.List(project).Do()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	if os.Getenv(EnvCloudSQL) != "" {
		want["postgres"] = "POSTGRES_17"
	}
	if os.Getenv(EnvMySQL) != "" {
		want["mysql"] = "MYSQL_8_4"
	}
	if len(list.Items) != len(want) {
		t.Fatalf("instances.list: %d items, want %v", len(list.Items), want)
	}
	for _, in := range list.Items {
		if want[in.Name] == "" {
			t.Errorf("instances.list: unexpected instance %q", in.Name)
			continue
		}
		got, err := svc.Instances.Get(project, in.Name).Do()
		if err != nil {
			t.Fatalf("instances.get %s: %v", in.Name, err)
		}
		if got.State != "RUNNABLE" || got.DatabaseVersion != want[in.Name] ||
			got.ConnectionName != project+":us-central1:"+in.Name || got.Project != project ||
			len(got.IpAddresses) != 1 || got.IpAddresses[0].Type != "PRIMARY" || got.IpAddresses[0].IpAddress != "127.0.0.1" {
			t.Errorf("instances.get %s: %+v", in.Name, got)
		}
		prefix := strings.SplitN(want[in.Name], "_", 2)[0] + "_"
		if !strings.HasPrefix(got.DatabaseInstalledVersion, want[in.Name]+"_") || !strings.HasPrefix(got.DatabaseInstalledVersion, prefix) {
			t.Errorf("instances.get %s: databaseInstalledVersion %q", in.Name, got.DatabaseInstalledVersion)
		}
	}
	_, err = svc.Instances.Get(project, "no-such-instance").Do()
	wantHTTP(t, "instances.get of an unknown instance", err, http.StatusNotFound)
}

// TestSQLAdminRefusesWhatItCannotDoFaithfully (#1301, #1330):
// pointInTimeRestore (a Backup and DR datasource; a clone to a point in
// time of the server `up` runs, which keeps no log, is a 400),
// legacy failover replicas, IAM database authentication (an access token in
// generateEphemeralCert) answer 501 (tiers and flags are served since
// #1380, TestSQLAdminTiersAndFlags);
// the instances `up` runs refuse a patch, restart or delete with the
// reason (instance writes act on created instances, TestCloudSQLInstanceWrites).
// covers: sql.instances.pointInTimeRestore (unimplemented)
func TestSQLAdminRefusesWhatItCannotDoFaithfully(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	p := h.Project()
	inst := "postgres"
	if os.Getenv(EnvCloudSQL) == "" {
		inst = "mysql"
	}
	_, err := svc.Instances.Patch(p, inst, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{Tier: "db-f1-micro"}}).Do()
	wantHTTP(t, "instances.patch of the instance up runs", err, http.StatusBadRequest)
	_, err = svc.Instances.Delete(p, inst).Do()
	wantHTTP(t, "instances.delete of the instance up runs", err, http.StatusBadRequest)
	_, err = svc.Instances.Restart(p, inst).Do()
	wantHTTP(t, "instances.restart of the instance up runs", err, http.StatusBadRequest)
	_, err = svc.Instances.Clone(p, inst, &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{DestinationInstanceName: "c",
		PointInTime: "2026-01-01T00:00:00Z"}}).Do()
	// The servers `up` runs keep no transaction log (TestCloudSQLPointInTimeRecovery).
	wantHTTP(t, "instances.clone to a point in time", err, http.StatusBadRequest)
	_, err = svc.Instances.PointInTimeRestore("projects/"+p, &sqladmin.PointInTimeRestoreContext{
		Datasource: "projects/" + p + "/instances/" + inst, TargetInstance: "pitr"}).Do()
	wantHTTP(t, "instances.pointInTimeRestore", err, 501)
	_, err = svc.Instances.Failover(p, inst, &sqladmin.InstancesFailoverRequest{}).Do()
	// The instances `up` runs are ZONAL, and refuse a failover as they
	// refuse a patch (#1408).
	wantHTTP(t, "instances.failover of the instance up runs", err, http.StatusBadRequest)
	_, err = svc.Instances.Insert(p, &sqladmin.DatabaseInstance{Name: "replica", DatabaseVersion: "POSTGRES_17",
		MasterInstanceName: inst, ReplicaConfiguration: &sqladmin.ReplicaConfiguration{FailoverTarget: true},
		Settings: &sqladmin.Settings{Tier: "db-f1-micro"}}).Do()
	wantHTTP(t, "instances.insert of a failover replica", err, 501)
	_, err = svc.Connect.GenerateEphemeralCert(p, inst, &sqladmin.GenerateEphemeralCertRequest{PublicKey: "x", AccessToken: "iam"}).Do()
	wantHTTP(t, "connect.generateEphemeralCert with an access token (IAM database authentication)", err, 501)
}

// sqlAdminEngine is one server under test: the instance name and how to
// ask the server itself what it holds.
type sqlAdminEngine struct {
	instance string
	// databases and users are read from the server's own catalogue.
	databases func(t *testing.T) map[string]string // name -> charset
	users     func(t *testing.T) map[string]bool
	// login opens the database as user with password, or reports why not.
	login func(user, password, database string) error
}

func pgEngine(t *testing.T, h *Harness) sqlAdminEngine {
	addr := h.Endpoint(EnvCloudSQL)
	query := func(t *testing.T, q string) [][2]string {
		c, err := pgx.Connect(h.Context(), fmt.Sprintf("postgres://cloudburrow@%s/cloudburrow?sslmode=disable", addr))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(context.Background())
		rows, err := c.Query(h.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][2]string
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, [2]string{a, b})
		}
		return out
	}
	return sqlAdminEngine{
		instance: "postgres",
		databases: func(t *testing.T) map[string]string {
			m := map[string]string{}
			for _, r := range query(t, "SELECT datname, pg_encoding_to_char(encoding) FROM pg_database") {
				m[r[0]] = r[1]
			}
			return m
		},
		users: func(t *testing.T) map[string]bool {
			m := map[string]bool{}
			for _, r := range query(t, "SELECT rolname, '' FROM pg_roles WHERE rolcanlogin") {
				m[r[0]] = true
			}
			return m
		},
		login: func(user, _, database string) error {
			// The server trusts every local connection, as docs/cloudsql.md
			// says, so the password is stored but not checked.
			c, err := pgx.Connect(h.Context(), fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable", user, addr, database))
			if err != nil {
				return err
			}
			defer c.Close(context.Background())
			// Since PostgreSQL 15 only the database's owner creates in
			// public, as on Cloud SQL, so the login is proved by a query.
			var who string
			if err := c.QueryRow(h.Context(), "SELECT current_user").Scan(&who); err != nil {
				return err
			}
			if who != user {
				return fmt.Errorf("connected as %s", who)
			}
			return nil
		},
	}
}

func myEngine(t *testing.T, h *Harness) sqlAdminEngine {
	addr := h.Endpoint(EnvMySQL)
	open := func(user, password, database string) (*sql.DB, error) {
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = user, password, "tcp", addr, database
		return sql.Open("mysql", cfg.FormatDSN())
	}
	query := func(t *testing.T, q string) [][2]string {
		db, err := open("cloudburrow", os.Getenv(EnvMySQLPassword), "")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.QueryContext(h.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][2]string
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, [2]string{a, b})
		}
		return out
	}
	return sqlAdminEngine{
		instance: "mysql",
		databases: func(t *testing.T) map[string]string {
			m := map[string]string{}
			// The application's user sees the databases it was granted,
			// which is every database the API creates.
			for _, r := range query(t, "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME FROM information_schema.SCHEMATA") {
				m[r[0]] = r[1]
			}
			return m
		},
		users: nil, // mysql.user is root's to read; checked by logging in
		login: func(user, password, database string) error {
			db, err := open(user, password, database)
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.ExecContext(h.Context(), "CREATE TABLE t (id int)")
			if err == nil {
				_, err = db.ExecContext(h.Context(), "DROP TABLE t")
			}
			return err
		},
	}
}

// TestSQLAdminDatabasesAndUsersAreTheServers (#1301): through the official
// Go client, on each running server, databases.insert creates a database the
// server's own catalogue lists and a driver opens; list, get, patch (a MySQL
// charset change, read back from information_schema) and delete agree with
// the server; users.insert creates a login that opens that database
// (MySQL checks the password, and after users.update only the new one
// works), users.list and users.get find it, and users.delete removes it.
// Each mutation answers a DONE operation that operations.get and
// operations.list return. Inserting a database twice is 409.
// covers: sql.databases.insert, sql.databases.list, sql.databases.get, sql.databases.patch, sql.databases.update, sql.databases.delete
// covers: sql.users.insert, sql.users.list, sql.users.get, sql.users.update, sql.users.delete
// covers: sql.operations.get, sql.operations.list
func TestSQLAdminDatabasesAndUsersAreTheServers(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	p := h.Project()
	var engines []sqlAdminEngine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, pgEngine(t, h))
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, myEngine(t, h))
	}
	if len(engines) == 0 {
		t.Skip("neither Cloud SQL server is exported")
	}
	for _, e := range engines {
		t.Run(e.instance, func(t *testing.T) {
			const db, user = "sqladmin_orders", "sqladmin_app"
			done := func(what string, op *sqladmin.Operation, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if op.Status != "DONE" || op.TargetId != e.instance || op.TargetProject != p {
					t.Fatalf("%s: operation %+v", what, op)
				}
				got, err := svc.Operations.Get(p, op.Name).Do()
				if err != nil || got.Status != "DONE" || got.OperationType != op.OperationType {
					t.Fatalf("%s: operations.get: %v %+v", what, err, got)
				}
			}
			op, err := svc.Databases.Insert(p, e.instance, &sqladmin.Database{Name: db}).Do()
			done("databases.insert", op, err)
			if _, ok := e.databases(t)[db]; !ok {
				t.Fatalf("after databases.insert the server does not list %s", db)
			}
			_, err = svc.Databases.Insert(p, e.instance, &sqladmin.Database{Name: db}).Do()
			wantHTTP(t, "databases.insert twice", err, http.StatusConflict)

			dl, err := svc.Databases.List(p, e.instance).Do()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range dl.Items {
				found = found || d.Name == db
				if d.Name == "information_schema" || d.Name == "template0" {
					t.Errorf("databases.list lists a system database %s", d.Name)
				}
			}
			if !found {
				t.Errorf("databases.list: no %s in %d items", db, len(dl.Items))
			}
			got, err := svc.Databases.Get(p, e.instance, db).Do()
			if err != nil || got.Name != db || got.Charset == "" || got.Instance != e.instance {
				t.Errorf("databases.get: %v %+v", err, got)
			}

			if e.instance == "mysql" {
				op, err = svc.Databases.Patch(p, e.instance, db, &sqladmin.Database{Charset: "latin1", Collation: "latin1_swedish_ci"}).Do()
				done("databases.patch", op, err)
				if cs := e.databases(t)[db]; cs != "latin1" {
					t.Errorf("after databases.patch the server's charset is %q, want latin1", cs)
				}
				op, err = svc.Databases.Update(p, e.instance, db, &sqladmin.Database{Name: db, Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"}).Do()
				done("databases.update", op, err)
				if cs := e.databases(t)[db]; cs != "utf8mb4" {
					t.Errorf("after databases.update the server's charset is %q, want utf8mb4", cs)
				}
			} else {
				// PostgreSQL fixes a database's encoding at creation.
				_, err = svc.Databases.Patch(p, e.instance, db, &sqladmin.Database{Charset: "LATIN1"}).Do()
				wantHTTP(t, "databases.patch of a PostgreSQL encoding", err, 501)
				op, err = svc.Databases.Update(p, e.instance, db, &sqladmin.Database{Name: db}).Do()
				done("databases.update with nothing to change", op, err)
			}

			op, err = svc.Users.Insert(p, e.instance, &sqladmin.User{Name: user, Password: "first-Pass1"}).Do()
			done("users.insert", op, err)
			if e.users != nil && !e.users(t)[user] {
				t.Errorf("after users.insert the server has no login %s", user)
			}
			if err := e.login(user, "first-Pass1", db); err != nil {
				t.Errorf("the new user cannot use %s: %v", db, err)
			}
			ul, err := svc.Users.List(p, e.instance).Do()
			if err != nil {
				t.Fatal(err)
			}
			var host string
			found = false
			for _, u := range ul.Items {
				if u.Name == user {
					found, host = true, u.Host
				}
			}
			if !found {
				t.Errorf("users.list: no %s", user)
			}
			if e.instance == "mysql" && host != "%" {
				t.Errorf("users.list: %s's host is %q, want %%", user, host)
			}
			gu, err := svc.Users.Get(p, e.instance, user).Host(host).Do()
			if err != nil || gu.Name != user {
				t.Errorf("users.get: %v %+v", err, gu)
			}
			op, err = svc.Users.Update(p, e.instance, &sqladmin.User{Password: "second-Pass2"}).Name(user).Host(host).Do()
			done("users.update", op, err)
			if e.instance == "mysql" {
				if err := e.login(user, "first-Pass1", db); err == nil {
					t.Error("after users.update the old password still logs in")
				}
				if err := e.login(user, "second-Pass2", db); err != nil {
					t.Errorf("after users.update the new password does not log in: %v", err)
				}
			}
			op, err = svc.Users.Delete(p, e.instance).Name(user).Host(host).Do()
			done("users.delete", op, err)
			if e.users != nil && e.users(t)[user] {
				t.Errorf("after users.delete the server still has %s", user)
			}
			if err := e.login(user, "second-Pass2", db); err == nil && e.instance == "mysql" {
				t.Error("a deleted MySQL user still logs in")
			}
			_, err = svc.Users.Get(p, e.instance, user).Host(host).Do()
			wantHTTP(t, "users.get after delete", err, http.StatusNotFound)

			op, err = svc.Databases.Delete(p, e.instance, db).Do()
			done("databases.delete", op, err)
			if _, ok := e.databases(t)[db]; ok {
				t.Errorf("after databases.delete the server still lists %s", db)
			}
			_, err = svc.Databases.Get(p, e.instance, db).Do()
			wantHTTP(t, "databases.get after delete", err, http.StatusNotFound)
			_, err = svc.Databases.Delete(p, e.instance, "cloudburrow").Do()
			wantHTTP(t, "databases.delete of the server's own database", err, http.StatusBadRequest)

			ops, err := svc.Operations.List(p).Instance(e.instance).Do()
			if err != nil {
				t.Fatal(err)
			}
			if len(ops.Items) < 5 || ops.Items[0].OperationType != "DELETE_DATABASE" {
				t.Errorf("operations.list: %d items, the first %+v", len(ops.Items), ops.Items[0])
			}
		})
	}
}

// TestGcloudSQL (#1301): through gcloud-setup's configuration, the real
// gcloud lists and describes the instances, creates a database and a user
// that the official Go client then finds, lists both, and deletes both,
// over v1beta4 on the sqladmin port.
// covers: sql.instances.list, sql.instances.get, sql.databases.insert, sql.databases.list, sql.databases.delete, sql.users.insert, sql.users.list, sql.users.delete, sql.operations.get
func TestGcloudSQL(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	inst := "postgres"
	if os.Getenv(EnvCloudSQL) == "" {
		inst = "mysql"
	}
	if got := g.must("sql", "instances", "list", p, "--format=value(name,state)"); !strings.Contains(got, inst+"\tRUNNABLE") {
		t.Errorf("sql instances list = %q", got)
	}
	if got := g.must("sql", "instances", "describe", inst, p, "--format=value(connectionName)"); strings.TrimSpace(got) != h.Project()+":us-central1:"+inst {
		t.Errorf("sql instances describe = %q", got)
	}
	g.must("sql", "databases", "create", "gcloud_db", "--instance="+inst, p)
	if _, err := svc.Databases.Get(h.Project(), inst, "gcloud_db").Do(); err != nil {
		t.Errorf("databases.get after gcloud create: %v", err)
	}
	if got := g.must("sql", "databases", "list", "--instance="+inst, p, "--format=value(name)"); !strings.Contains(got, "gcloud_db") {
		t.Errorf("sql databases list = %q", got)
	}
	g.must("sql", "users", "create", "gcloud_user", "--instance="+inst, "--password=gcloud-Pass1", p)
	if got := g.must("sql", "users", "list", "--instance="+inst, p, "--format=value(name)"); !strings.Contains(got, "gcloud_user") {
		t.Errorf("sql users list = %q", got)
	}
	del := []string{"sql", "users", "delete", "gcloud_user", "--instance=" + inst, p, "--quiet"}
	if inst == "mysql" {
		del = append(del, "--host=%")
	}
	g.must(del...)
	g.must("sql", "databases", "delete", "gcloud_db", "--instance="+inst, p, "--quiet")
	_, err := svc.Databases.Get(h.Project(), inst, "gcloud_db").Do()
	wantHTTP(t, "databases.get after gcloud delete", err, http.StatusNotFound)
}

// TestConsoleCloudSQLUsersMatchTheAdminAPI (#1301): on each Cloud SQL page,
// a database's Add user creates a user the Cloud SQL Admin API's users.get
// finds (and, on MySQL, that logs in with its password), and Delete user
// removes one the API created, so the console and the API manage the same
// users in the same server.
func TestConsoleCloudSQLUsersMatchTheAdminAPI(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	addr := consoleAddr(t, h)
	project := h.Project()
	type page struct{ provider, instance, host string }
	var pages []page
	if os.Getenv(EnvCloudSQL) != "" {
		pages = append(pages, page{"cloudsql", "postgres", ""})
	}
	if os.Getenv(EnvMySQL) != "" {
		pages = append(pages, page{"cloudsql-mysql", "mysql", "%"})
	}
	for _, pg := range pages {
		t.Run(pg.instance, func(t *testing.T) {
			act := func(action string, values map[string]string) {
				t.Helper()
				body, _ := json.Marshal(map[string]any{"Path": []string{"cloudburrow"}, "Action": action, "Values": values})
				if code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/"+pg.provider+"?project="+project, string(body)); code != http.StatusOK {
					t.Fatalf("console %s = %d: %s", action, code, out)
				}
			}
			act("add-user", map[string]string{"name": "console_user", "password": "console-Pass1"})
			if _, err := svc.Users.Get(project, pg.instance, "console_user").Host(pg.host).Do(); err != nil {
				t.Errorf("users.get of the console's user: %v", err)
			}
			if pg.instance == "mysql" {
				if err := myEngine(t, h).login("console_user", "console-Pass1", "cloudburrow"); err != nil {
					t.Errorf("the console's MySQL user cannot log in: %v", err)
				}
			}
			if _, err := svc.Users.Insert(project, pg.instance, &sqladmin.User{Name: "api_user", Password: "api-Pass1"}).Do(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"console_user", "api_user"} {
				act("delete-user", map[string]string{"name": name, "host": pg.host})
				_, err := svc.Users.Get(project, pg.instance, name).Host(pg.host).Do()
				wantHTTP(t, "users.get after the console's Delete user of "+name, err, http.StatusNotFound)
			}
		})
	}
}
