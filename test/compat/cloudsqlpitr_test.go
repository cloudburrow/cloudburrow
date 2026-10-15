//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// sqlServerTime is the server's clock now, in UTC, read through the
// official connector: the time a point-in-time recovery is measured by.
func sqlServerTime(t *testing.T, h *Harness, c sqlBackupCase, conn string) time.Time {
	t.Helper()
	drv := fmt.Sprintf("cloudsql-pitr-%d", time.Now().UnixNano())
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
	ctx, cancel := context.WithTimeout(h.Context(), time.Minute)
	defer cancel()
	q := `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`
	if c.version != "POSTGRES_17" {
		q = `SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%dT%H:%i:%s.%fZ')`
	}
	var s string
	if err := db.QueryRowContext(ctx, q).Scan(&s); err != nil {
		t.Fatalf("the server's time: %v", err)
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("the server's time %q: %v", s, err)
	}
	return at
}

// waitRecoverable waits until the instance's earliest recovery time has
// passed, as getLatestRecoveryTime reports it.
func waitRecoverable(t *testing.T, svc *sqladmin.Service, project, instance string) *sqladmin.SqlInstancesGetLatestRecoveryTimeResponse {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		r, err := svc.Projects.Instances.GetLatestRecoveryTime(project, instance).Do()
		if err == nil {
			e, err1 := time.Parse(time.RFC3339Nano, r.EarliestRecoveryTime)
			l, err2 := time.Parse(time.RFC3339Nano, r.LatestRecoveryTime)
			if err1 != nil || err2 != nil {
				t.Fatalf("getLatestRecoveryTime: %+v", r)
			}
			if !l.Before(e) {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("getLatestRecoveryTime: %v %+v", err, r)
		}
		time.Sleep(time.Second)
	}
}

// TestCloudSQLPointInTimeRecovery (#1330): on a created instance with
// pointInTimeRecoveryEnabled (PostgreSQL) or binaryLogEnabled (MySQL),
// through the official Go client and connector: rows are written, the
// server's time is noted, more rows are written, and instances.clone with
// cloneContext.pointInTime makes an instance holding only the first rows;
// getLatestRecoveryTime reports the window, and a time outside it is
// refused.
// covers: sql.instances.clone, sql.projects.instances.getLatestRecoveryTime, sql.instances.insert, sql.instances.get, sql.operations.get
func TestCloudSQLPointInTimeRecovery(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var cases []sqlBackupCase
	if os.Getenv(EnvCloudSQL) != "" {
		cases = append(cases, sqlBackupCase{name: "postgres", version: "POSTGRES_17", instance: "pitr-pg-" + suffix, created: true,
			table: "pitr_rows", dsn: func(_, conn string) string {
				return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
			}, register: pgRegister})
	}
	if os.Getenv(EnvMySQL) != "" {
		cases = append(cases, sqlBackupCase{name: "mysql", version: "MYSQL_8_4", instance: "pitr-my-" + suffix, created: true,
			table: "pitr.pitr_rows", setup: "CREATE DATABASE IF NOT EXISTS pitr", dsn: func(drv, conn string) string {
				return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn)
			}, register: mysqlRegister})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bc := &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00"}
			if c.version == "POSTGRES_17" {
				bc.PointInTimeRecoveryEnabled = true
			} else {
				bc.BinaryLogEnabled = true
			}
			op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: c.instance, DatabaseVersion: c.version,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", BackupConfiguration: bc}}).Do()
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
			if got := in.Settings.BackupConfiguration; got == nil || got.PointInTimeRecoveryEnabled != bc.PointInTimeRecoveryEnabled ||
				got.BinaryLogEnabled != bc.BinaryLogEnabled {
				t.Errorf("backupConfiguration: %+v", got)
			}
			win := waitRecoverable(t, svc, project, c.instance)
			if win.Kind != "sql#getLatestRecoveryTime" {
				t.Errorf("getLatestRecoveryTime: %+v", win)
			}

			conn := project + ":us-central1:" + c.instance
			var stmts []string
			if c.setup != "" {
				stmts = append(stmts, c.setup)
			}
			stmts = append(stmts, "CREATE TABLE "+c.table+" (id INT PRIMARY KEY, name VARCHAR(20))",
				"INSERT INTO "+c.table+" VALUES (1, 'a'), (2, 'b')")
			want := []string{"1=a", "2=b"}
			if got := sqlBackupRows(t, h, c, conn, stmts...); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows written: %v", got)
			}
			at := sqlServerTime(t, h, c, conn)
			if got := sqlBackupRows(t, h, c, conn, "UPDATE "+c.table+" SET name = 'z' WHERE id = 1",
				"INSERT INTO "+c.table+" VALUES (3, 'c')"); !reflect.DeepEqual(got, []string{"1=z", "2=b", "3=c"}) {
				t.Fatalf("rows changed: %v", got)
			}

			_, err = svc.Instances.Clone(project, c.instance, &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
				DestinationInstanceName: c.instance + "-early", PointInTime: "2020-01-01T00:00:00Z"}}).Do()
			wantHTTP(t, "a clone to before the earliest recovery time", err, http.StatusBadRequest)

			clone := c.instance + "-pit"
			op, err = svc.Instances.Clone(project, c.instance, &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
				DestinationInstanceName: clone, PointInTime: at.Format(time.RFC3339Nano)}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if d, err := svc.Instances.Delete(project, clone).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
			}()
			waitSQLOp(t, svc, project, op)
			if op, err = svc.Operations.Get(project, op.Name).Do(); err != nil || op.Error != nil {
				t.Fatalf("the clone's operation: %v %+v", err, op.Error)
			}
			got, err := svc.Instances.Get(project, clone).Do()
			if err != nil || got.State != "RUNNABLE" {
				t.Fatalf("the clone: %v %+v", err, got)
			}
			if rows := sqlBackupRows(t, h, c, project+":us-central1:"+clone); !reflect.DeepEqual(rows, want) {
				t.Errorf("rows in the clone at %s: %v, want %v", at.Format(time.RFC3339Nano), rows, want)
			}
			// The source is unchanged.
			if rows := sqlBackupRows(t, h, c, conn); !reflect.DeepEqual(rows, []string{"1=z", "2=b", "3=c"}) {
				t.Errorf("rows in the source: %v", rows)
			}
		})
	}
}

// TestGcloudSQLPointInTimeClone (#1330): `gcloud sql instances create
// --enable-point-in-time-recovery`, rows written, then `gcloud sql
// instances clone --point-in-time` to between two writes: the clone holds
// only the first rows. Through the configuration `cloudburrow
// gcloud-setup` writes, with egress to Google blocked.
// covers: sql.instances.insert, sql.instances.clone, sql.instances.get, sql.instances.delete, sql.operations.get
func TestGcloudSQLPointInTimeClone(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano() % 1e6
	src, clone := fmt.Sprintf("gcloud-pitr-%d", n), fmt.Sprintf("gcloud-pitr-%d-cl", n)
	g.must("sql", "instances", "create", src, "--project", g.project, "--database-version=POSTGRES_17",
		"--tier=db-custom-1-3840", "--region=us-central1", "--enable-point-in-time-recovery", "--root-password=Root-pass1", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", src, "--project", g.project, "--quiet")
	svc := sqlAdminService(t, h)
	waitRecoverable(t, svc, g.project, src)
	c := sqlBackupCase{version: "POSTGRES_17", table: "gpitr_rows", dsn: func(_, conn string) string {
		return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
	}, register: pgRegister}
	conn := g.project + ":us-central1:" + src
	sqlBackupRows(t, h, c, conn, "CREATE TABLE gpitr_rows (id INT PRIMARY KEY, name VARCHAR(20))", "INSERT INTO gpitr_rows VALUES (1, 'a')")
	at := sqlServerTime(t, h, c, conn)
	sqlBackupRows(t, h, c, conn, "INSERT INTO gpitr_rows VALUES (2, 'b')")
	g.must("sql", "instances", "clone", src, clone, "--project", g.project, "--point-in-time", at.Format(time.RFC3339Nano), "--quiet")
	defer g.run(nil, "sql", "instances", "delete", clone, "--project", g.project, "--quiet")
	if rows := sqlBackupRows(t, h, c, g.project+":us-central1:"+clone); !reflect.DeepEqual(rows, []string{"1=a"}) {
		t.Errorf("rows in the gcloud clone at %s: %v, want [1=a]", at.Format(time.RFC3339Nano), rows)
	}
}
