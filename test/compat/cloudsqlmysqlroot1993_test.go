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

	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// mysqlOpen opens a database/sql handle on conn through the official
// connector, as user.
func mysqlOpen(t *testing.T, h *Harness, conn, user, password, database string) *sql.DB {
	t.Helper()
	drv := fmt.Sprintf("cloudsql-root-%d", time.Now().UnixNano())
	cleanup, err := cloudsqlmysql.RegisterDriver(drv, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	db, err := sql.Open(drv, fmt.Sprintf("%s:%s@%s(%s)/%s", user, password, drv, conn, database))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// mysqlExecAs runs stmts through the official connector as user.
func mysqlExecAs(t *testing.T, h *Harness, conn, user, password, database string, stmts ...string) {
	t.Helper()
	db := mysqlOpen(t, h, conn, user, password, database)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s as %s: %v", s, user, err)
		}
	}
}

// mysqlAs runs stmts as mysqlExecAs does, then the query q, and returns
// the first column of its rows, joined by ";".
func mysqlAs(t *testing.T, h *Harness, conn, user, password, database string, q string, stmts ...string) string {
	t.Helper()
	mysqlExecAs(t, h, conn, user, password, database, stmts...)
	db := mysqlOpen(t, h, conn, user, password, database)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s as %s: %v", q, user, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return strings.Join(out, ";")
}

// sqlUserNames are users.list's user@host entries.
func sqlUserNames(t *testing.T, svc *sqladmin.Service, project, inst string) map[string]bool {
	t.Helper()
	list, err := svc.Users.List(project, inst).Do()
	if err != nil {
		t.Fatalf("users.list of %s: %v", inst, err)
	}
	out := map[string]bool{}
	for _, u := range list.Items {
		out[u.Name+"@"+u.Host] = true
	}
	return out
}

// deleteMySQLRoot deletes 'root'@'%', as the hashicorp/google provider
// does after it creates a MySQL instance (#1993).
func deleteMySQLRoot(t *testing.T, svc *sqladmin.Service, project, inst string) {
	t.Helper()
	op, err := svc.Users.Delete(project, inst).Name("root").Host("%").Do()
	if err != nil {
		t.Fatalf("users.delete of 'root'@'%%': %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("users.delete of 'root'@'%%': %+v", op.Error.Errors[0])
	}
}

// TestCloudSQLMySQLRootDeleted (#1993): through the official Go client
// (sqladmin/v1) and connector, a created MySQL instance's 'root'@'%' is
// deleted, as the hashicorp/google provider deletes it, and the instance
// keeps working as Cloud SQL's does, which it manages through system
// accounts of its own that gcloud cannot display: users.list lists root
// before and not after, and lists no system account; a system account is
// neither created nor deleted; and an export, an import, a backup, its
// restore, a clone and a restart all work without root. An instance made
// before #1993, which had no system account (dropped here), still answers,
// and has one again after it is stopped and started.
// covers: sql.users.delete, sql.users.list, sql.users.insert, sql.users.get, sql.instances.export, sql.instances.import, sql.backupRuns.insert, sql.instances.restoreBackup, sql.instances.clone, sql.instances.restart, sql.instances.patch
func TestCloudSQLMySQLRootDeleted(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	bh := sqlTransferBucket(t, h, "sqlroot")
	project := h.Project()
	inst := fmt.Sprintf("root-my-%d", time.Now().UnixNano()%1e6)
	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	conn := in.ConnectionName
	op, err := svc.Users.Insert(project, inst, &sqladmin.User{Name: "app", Password: "App-pass1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)

	// An instance made before #1993: no system account until it starts.
	admins := "SELECT COUNT(*) FROM mysql.user WHERE user = 'cloudsqladmin'"
	if got := mysqlAs(t, h, conn, "app", "App-pass1", "mysql", admins); got != "3" {
		t.Fatalf("system accounts of a new instance: %s, want 3", got)
	}
	mysqlExecAs(t, h, conn, "root", "Root-pass1", "mysql",
		"DROP USER 'cloudsqladmin'@'localhost', 'cloudsqladmin'@'127.0.0.1', 'cloudsqladmin'@'::1'")
	if dbs, err := svc.Databases.List(project, inst).Do(); err != nil || dbs == nil {
		t.Fatalf("databases.list without the system account: %v", err)
	}
	for _, policy := range []string{"NEVER", "ALWAYS"} {
		op, err := svc.Instances.Patch(project, inst, &sqladmin.DatabaseInstance{
			Settings: &sqladmin.Settings{ActivationPolicy: policy}}).Do()
		if err != nil {
			t.Fatalf("activationPolicy %s: %v", policy, err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("activationPolicy %s: %+v", policy, op.Error.Errors[0])
		}
	}
	if got := mysqlAs(t, h, conn, "app", "App-pass1", "mysql", admins); got != "3" {
		t.Fatalf("system accounts after a start: %s, want 3", got)
	}

	users := sqlUserNames(t, svc, project, inst)
	if !users["root@%"] || !users["app@%"] {
		t.Errorf("users.list before the delete: %v, want root@%% and app@%%", users)
	}
	for u := range users {
		if strings.HasPrefix(u, "cloudsqladmin@") || u == "root@localhost" || u == "root@127.0.0.1" || u == "root@::1" {
			t.Errorf("users.list lists the system account %s", u)
		}
	}
	deleteMySQLRoot(t, svc, project, inst)
	if users := sqlUserNames(t, svc, project, inst); users["root@%"] || !users["app@%"] {
		t.Errorf("users.list after the delete: %v, want app@%% only", users)
	}
	_, err = svc.Users.Get(project, inst, "root").Host("%").Do()
	wantHTTP(t, "users.get of the deleted root", err, http.StatusNotFound)
	_, err = svc.Users.Delete(project, inst).Name("cloudsqladmin").Host("localhost").Do()
	wantHTTP(t, "users.delete of a system account", err, http.StatusNotFound)
	_, err = svc.Users.Insert(project, inst, &sqladmin.User{Name: "cloudsqladmin", Password: "X-pass12"}).Do()
	wantHTTP(t, "users.insert of a system account's name", err, http.StatusBadRequest)

	rows := "SELECT CONCAT(id, '=', name) FROM t ORDER BY id"
	mysqlExecAs(t, h, conn, "app", "App-pass1", "mysql", "CREATE DATABASE appdb")
	if got := mysqlAs(t, h, conn, "app", "App-pass1", "appdb", rows, "CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(10))",
		"INSERT INTO t VALUES (1, 'a'), (2, 'b')"); got != "1=a;2=b" {
		t.Fatalf("rows written: %q", got)
	}

	uri := "gs://" + bh.BucketName() + "/appdb.sql"
	op, err = svc.Instances.Export(project, inst, &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: uri, FileType: "SQL", Databases: []string{"appdb"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("export without root: %s", op.Error.Errors[0].Message)
	}
	mysqlExecAs(t, h, conn, "app", "App-pass1", "appdb", "DROP TABLE t")
	op, err = svc.Instances.Import(project, inst, &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
		Uri: uri, FileType: "SQL", Database: "appdb"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("import without root: %s", op.Error.Errors[0].Message)
	}
	if got := mysqlAs(t, h, conn, "app", "App-pass1", "appdb", rows); got != "1=a;2=b" {
		t.Errorf("rows after the import: %q", got)
	}

	op, err = svc.BackupRuns.Insert(project, inst, &sqladmin.BackupRun{Description: "no root"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("backup without root: %s", op.Error.Errors[0].Message)
	}
	list, err := svc.BackupRuns.List(project, inst).Do()
	if err != nil || len(list.Items) == 0 || list.Items[0].Status != "SUCCESSFUL" {
		t.Fatalf("backupRuns.list: %v %+v", err, list)
	}
	mysqlExecAs(t, h, conn, "app", "App-pass1", "appdb", "UPDATE t SET name = 'z' WHERE id = 1")
	op, err = svc.Instances.RestoreBackup(project, inst, &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: list.Items[0].Id, InstanceId: inst, Project: project}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("restore without root: %s", op.Error.Errors[0].Message)
	}
	if got := mysqlAs(t, h, conn, "app", "App-pass1", "appdb", rows); got != "1=a;2=b" {
		t.Errorf("rows after the restore: %q", got)
	}
	if users := sqlUserNames(t, svc, project, inst); users["root@%"] || !users["app@%"] {
		t.Errorf("users.list after the restore: %v, want app@%% only", users)
	}

	clone := inst + "-cl"
	op, err = svc.Instances.Clone(project, inst, &sqladmin.InstancesCloneRequest{
		CloneContext: &sqladmin.CloneContext{DestinationInstanceName: clone}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, clone).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("clone without root: %s", op.Error.Errors[0].Message)
	}
	if got, err := svc.Instances.Get(project, clone).Do(); err != nil || got.State != "RUNNABLE" {
		t.Fatalf("the clone: %v %+v", err, got)
	}
	found := false
	if dbs, err := svc.Databases.List(project, clone).Do(); err == nil {
		for _, d := range dbs.Items {
			found = found || d.Name == "appdb"
		}
	}
	if !found {
		t.Errorf("the clone has no appdb")
	}

	op, err = svc.Instances.Restart(project, inst).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("restart without root: %s", op.Error.Errors[0].Message)
	}
	if users := sqlUserNames(t, svc, project, inst); users["root@%"] || !users["app@%"] {
		t.Errorf("users.list after the restart: %v, want app@%% only", users)
	}
	if got := mysqlAs(t, h, conn, "app", "App-pass1", "appdb", rows); got != "1=a;2=b" {
		t.Errorf("rows after the restart: %q", got)
	}
}

// TestCloudSQLMySQLRootDeletedReplica (#1993): a read replica of a MySQL
// instance whose 'root'@'%' was deleted is created, replicates, and is
// promoted, through the official Go client and connector.
// covers: sql.instances.insert, sql.instances.promoteReplica, sql.users.delete
func TestCloudSQLMySQLRootDeletedReplica(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	inst := fmt.Sprintf("rootr-my-%d", time.Now().UnixNano()%1e6)
	in := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	op, err := svc.Users.Insert(project, inst, &sqladmin.User{Name: "app", Password: "App-pass1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	deleteMySQLRoot(t, svc, project, inst)
	mysqlExecAs(t, h, in.ConnectionName, "app", "App-pass1", "mysql", "CREATE DATABASE appdb",
		"CREATE TABLE appdb.t (id INT PRIMARY KEY)", "INSERT INTO appdb.t VALUES (1)")
	rep := inst + "-r"
	createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: rep, MasterInstanceName: inst,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	mysqlExecAs(t, h, in.ConnectionName, "app", "App-pass1", "appdb", "INSERT INTO t VALUES (2)")
	eventually(t, "the replica's databases", func() error {
		dbs, err := svc.Databases.List(project, rep).Do()
		if err != nil {
			return err
		}
		for _, d := range dbs.Items {
			if d.Name == "appdb" {
				return nil
			}
		}
		return fmt.Errorf("no appdb on the replica")
	})
	if _, err := svc.Users.List(project, rep).Do(); err != nil {
		t.Errorf("users.list of the replica: %v", err)
	}
	op, err = svc.Instances.PromoteReplica(project, rep).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("promote: %s", op.Error.Errors[0].Message)
	}
	op, err = svc.Databases.Insert(project, rep, &sqladmin.Database{Name: "after"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("databases.insert on the promoted replica: %s", op.Error.Errors[0].Message)
	}
}
