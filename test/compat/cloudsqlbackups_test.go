//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlBackupCase is one instance a backup test writes rows into.
type sqlBackupCase struct {
	name, version, instance string
	created                 bool
	table                   string // where the rows go
	setup                   string // run before the table is created
	dsn                     func(drv, conn string) string
	register                func(string, ...cloudsqlconn.Option) (func() error, error)
}

func pgRegister(n string, o ...cloudsqlconn.Option) (func() error, error) {
	return pgxv5.RegisterDriver(n, o...)
}
func mysqlRegister(n string, o ...cloudsqlconn.Option) (func() error, error) {
	return cloudsqlmysql.RegisterDriver(n, o...)
}

// sqlBackupRows opens a fresh connection through the official connector,
// runs stmts, and returns the rows of c.table: a restore ends the sessions
// it finds, so every check dials again.
func sqlBackupRows(t *testing.T, h *Harness, c sqlBackupCase, conn string, stmts ...string) []string {
	t.Helper()
	drv := fmt.Sprintf("cloudsql-bk-%d", time.Now().UnixNano())
	cleanup, err := c.register(drv, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	db, err := sql.Open(drv, c.dsn(drv, conn))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(h.Context(), 2*time.Minute)
	defer cancel()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT CONCAT(id, '=', name) FROM "+c.table+" ORDER BY id")
	if err != nil {
		t.Fatalf("read %s: %v", c.table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestCloudSQLBackups (#1330): through the official Go client (sqladmin/v1)
// and the official connector, rows are written, backupRuns.insert takes a
// backup, the rows are changed, instances.restoreBackup restores the
// backup, and the original rows read back; on a created instance,
// instances.clone makes a new instance holding the same rows. Both on the
// servers `up` runs and on created instances, for each engine enabled.
// covers: sql.backupRuns.insert, sql.backupRuns.list, sql.backupRuns.get, sql.backupRuns.delete, sql.instances.restoreBackup, sql.instances.clone, sql.operations.get
func TestCloudSQLBackups(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var cases []sqlBackupCase
	if os.Getenv(EnvCloudSQL) != "" {
		cases = append(cases,
			sqlBackupCase{name: "up-postgres", version: "POSTGRES_17", instance: "postgres", table: "bk_rows",
				dsn: func(_, conn string) string {
					return fmt.Sprintf("host=%s user=cloudburrow dbname=cloudburrow sslmode=disable", conn)
				}, register: pgRegister},
			sqlBackupCase{name: "created-postgres", version: "POSTGRES_17", instance: "bk-pg-" + suffix, created: true, table: "bk_rows",
				dsn: func(_, conn string) string {
					return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
				}, register: pgRegister})
	}
	if os.Getenv(EnvMySQL) != "" {
		cases = append(cases,
			sqlBackupCase{name: "up-mysql", version: "MYSQL_8_4", instance: "mysql", table: "bk_rows",
				dsn: func(drv, conn string) string {
					return fmt.Sprintf("cloudburrow:%s@%s(%s)/cloudburrow", os.Getenv(EnvMySQLPassword), drv, conn)
				}, register: mysqlRegister},
			sqlBackupCase{name: "created-mysql", version: "MYSQL_8_4", instance: "bk-my-" + suffix, created: true, table: "bk.bk_rows",
				setup: "CREATE DATABASE IF NOT EXISTS bk",
				dsn: func(drv, conn string) string {
					return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn)
				}, register: mysqlRegister})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.created {
				op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: c.instance, DatabaseVersion: c.version,
					RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
						BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00"}}}).Do()
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if d, err := svc.Instances.Delete(project, c.instance).Do(); err == nil {
						waitSQLOp(t, svc, project, d)
					}
				}()
				waitSQLOp(t, svc, project, op)
				in, err := svc.Instances.Get(project, c.instance).Do()
				if err != nil {
					t.Fatal(err)
				}
				if bc := in.Settings.BackupConfiguration; bc == nil || !bc.Enabled || bc.StartTime != "03:00" {
					t.Errorf("backupConfiguration: %+v", bc)
				}
			}
			conn := project + ":us-central1:" + c.instance
			var stmts []string
			if c.setup != "" {
				stmts = append(stmts, c.setup)
			}
			stmts = append(stmts, "DROP TABLE IF EXISTS "+c.table, "CREATE TABLE "+c.table+" (id INT PRIMARY KEY, name VARCHAR(20))",
				"INSERT INTO "+c.table+" VALUES (1, 'a'), (2, 'b')")
			want := []string{"1=a", "2=b"}
			if got := sqlBackupRows(t, h, c, conn, stmts...); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows written: %v", got)
			}
			defer sqlBackupRows(t, h, c, conn, "DELETE FROM "+c.table)

			op, err := svc.BackupRuns.Insert(project, c.instance, &sqladmin.BackupRun{Description: "compat"}).Do()
			if err != nil {
				t.Fatal(err)
			}
			if op.OperationType != "BACKUP_VOLUME" {
				t.Errorf("backup's operation: %+v", op)
			}
			waitSQLOp(t, svc, project, op)
			list, err := svc.BackupRuns.List(project, c.instance).Do()
			if err != nil || len(list.Items) == 0 {
				t.Fatalf("backupRuns.list: %v %+v", err, list)
			}
			run := list.Items[0]
			if run.Status != "SUCCESSFUL" || run.Type != "ON_DEMAND" || run.Description != "compat" || run.Instance != c.instance {
				t.Errorf("backup run: %+v", run)
			}
			if got, err := svc.BackupRuns.Get(project, c.instance, run.Id).Do(); err != nil || got.Id != run.Id {
				t.Errorf("backupRuns.get: %v %+v", err, got)
			}
			defer func() {
				if d, err := svc.BackupRuns.Delete(project, c.instance, run.Id).Do(); err != nil {
					t.Errorf("backupRuns.delete: %v", err)
				} else {
					waitSQLOp(t, svc, project, d)
					_, err := svc.BackupRuns.Get(project, c.instance, run.Id).Do()
					wantHTTP(t, "backupRuns.get after delete", err, http.StatusNotFound)
				}
			}()

			if got := sqlBackupRows(t, h, c, conn, "UPDATE "+c.table+" SET name = 'z' WHERE id = 1",
				"INSERT INTO "+c.table+" VALUES (3, 'c')"); !reflect.DeepEqual(got, []string{"1=z", "2=b", "3=c"}) {
				t.Fatalf("rows changed: %v", got)
			}
			op, err = svc.Instances.RestoreBackup(project, c.instance, &sqladmin.InstancesRestoreBackupRequest{
				RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: run.Id, InstanceId: c.instance, Project: project}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			if got := sqlBackupRows(t, h, c, conn); !reflect.DeepEqual(got, want) {
				t.Errorf("rows after the restore: %v, want %v", got, want)
			}

			if !c.created {
				return
			}
			clone := c.instance + "-cl"
			op, err = svc.Instances.Clone(project, c.instance, &sqladmin.InstancesCloneRequest{
				CloneContext: &sqladmin.CloneContext{DestinationInstanceName: clone}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if d, err := svc.Instances.Delete(project, clone).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
			}()
			waitSQLOp(t, svc, project, op)
			in, err := svc.Instances.Get(project, clone).Do()
			if err != nil || in.State != "RUNNABLE" || in.DatabaseVersion != c.version {
				t.Fatalf("the clone: %v %+v", err, in)
			}
			if got := sqlBackupRows(t, h, c, project+":us-central1:"+clone); !reflect.DeepEqual(got, want) {
				t.Errorf("rows in the clone: %v, want %v", got, want)
			}
			_, err = svc.Instances.Clone(project, c.instance, &sqladmin.InstancesCloneRequest{
				CloneContext: &sqladmin.CloneContext{DestinationInstanceName: clone + "2", PointInTime: time.Now().UTC().Format(time.RFC3339)}}).Do()
			// Point-in-time recovery is off on this instance (TestCloudSQLPointInTimeRecovery).
			wantHTTP(t, "a point-in-time clone", err, http.StatusBadRequest)
		})
	}
}

// TestGcloudSQLBackups (#1330): `gcloud sql backups create`, list and
// restore on the PostgreSQL server `up` runs, through the configuration
// `cloudburrow gcloud-setup` writes, with egress to Google blocked: the
// rows changed after the backup are gone after the restore.
// covers: sql.backupRuns.insert, sql.backupRuns.list, sql.backupRuns.get, sql.backupRuns.delete, sql.instances.restoreBackup, sql.instances.get, sql.operations.get
func TestGcloudSQLBackups(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	c := pgConnect(t, h, "cloudburrow")
	exec := func(s string) {
		t.Helper()
		if _, err := c.Exec(h.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	exec("DROP TABLE IF EXISTS gbk_rows")
	exec("CREATE TABLE gbk_rows (id INT PRIMARY KEY)")
	exec("INSERT INTO gbk_rows VALUES (1), (2)")
	defer func() {
		c2 := pgConnect(t, h, "cloudburrow")
		_, _ = c2.Exec(h.Context(), "DROP TABLE IF EXISTS gbk_rows")
	}()
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	g.must("sql", "backups", "create", "--instance=postgres", "--description=gcloud", p)
	out := g.must("sql", "backups", "list", "--instance=postgres", p, "--format=value(id,status,description)")
	var id string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[1] == "SUCCESSFUL" && f[2] == "gcloud" {
			id = f[0]
			break
		}
	}
	if id == "" {
		t.Fatalf("sql backups list: %q", out)
	}
	exec("INSERT INTO gbk_rows VALUES (3)")
	g.must("sql", "backups", "restore", id, "--restore-instance=postgres", p, "--quiet")
	c3 := pgConnect(t, h, "cloudburrow")
	var n int
	if err := c3.QueryRow(h.Context(), "SELECT count(*) FROM gbk_rows").Scan(&n); err != nil || n != 2 {
		t.Errorf("rows after gcloud's restore: %d, %v, want 2", n, err)
	}
	g.must("sql", "backups", "delete", id, "--instance=postgres", p, "--quiet")
}
