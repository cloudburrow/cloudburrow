//go:build compat

package compat

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLReadReplicasBeforePostgres12 (#1524), with google.golang.org/
// api/sqladmin/v1 and the official connector (pgx): a PostgreSQL 9.6, 10
// and 11 instance has a read replica and a replica of that (cascading),
// which stream its writes and refuse their own; stopReplica pauses and
// startReplica resumes replay; promoteReplica makes the
// middle replica a primary (pg_ctl promote, as these versions have no
// pg_promote()), whose write reaches its own replica on the new timeline.
// covers: sql.instances.insert, sql.instances.promoteReplica, sql.instances.stopReplica, sql.instances.startReplica
func TestCloudSQLReadReplicasBeforePostgres12(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	for _, version := range []string{"POSTGRES_9_6", "POSTGRES_10", "POSTGRES_11"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			e := sqlEngine{}
			short := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(version, "POSTGRES_"), "_", ""))
			p := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: "pg" + short + "-rp-" + suffix, DatabaseVersion: version,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
			pdb := e.root(t, h, p.ConnectionName)
			for _, q := range []string{"CREATE TABLE t (v varchar(20))", "INSERT INTO t VALUES ('one')"} {
				q := q
				eventually(t, q, func() error { _, err := pdb.Exec(q); return err })
			}
			r1 := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: "pg" + short + "-rr1-" + suffix,
				MasterInstanceName: p.Name, InstanceType: "READ_REPLICA_INSTANCE"})
			if r1.DatabaseVersion != version || r1.MasterInstanceName != project+":"+p.Name {
				t.Errorf("replica: version %s master %s", r1.DatabaseVersion, r1.MasterInstanceName)
			}
			r2 := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: "pg" + short + "-rr2-" + suffix,
				MasterInstanceName: r1.Name, InstanceType: "READ_REPLICA_INSTANCE"})
			r1db, r2db := e.root(t, h, r1.ConnectionName), e.root(t, h, r2.ConnectionName)
			has := func(db *sql.DB, v string) func() error {
				return func() error {
					var n int
					if err := db.QueryRow("SELECT count(*) FROM t WHERE v = '" + v + "'").Scan(&n); err != nil {
						return err
					}
					if n != 1 {
						return fmt.Errorf("%d rows of %q", n, v)
					}
					return nil
				}
			}
			eventually(t, "the replica has the primary's row", has(r1db, "one"))
			if _, err := pdb.Exec("INSERT INTO t VALUES ('two')"); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the cascading replica receives a write on the primary", has(r2db, "two"))
			var rec bool
			if err := r1db.QueryRow("SELECT pg_is_in_recovery()").Scan(&rec); err != nil || !rec {
				t.Errorf("the replica: pg_is_in_recovery() = %v, %v", rec, err)
			}
			if _, err := r1db.Exec("INSERT INTO t VALUES ('x')"); err == nil {
				t.Error("a write on the replica succeeded")
			}

			// stopReplica pauses replay (pg_xlog_replay_pause on 9.6,
			// pg_wal_replay_pause after), startReplica resumes it.
			sop, err := svc.Instances.StopReplica(project, r1.Name).Do()
			if err != nil {
				t.Fatalf("stopReplica: %v", err)
			}
			waitSQLOpDone(t, svc, project, sop)
			var paused bool
			if err := r1db.QueryRow("SELECT pg_is_" + map[bool]string{true: "xlog", false: "wal"}[version == "POSTGRES_9_6"] + "_replay_paused()").Scan(&paused); err != nil || !paused {
				t.Errorf("after stopReplica: replay paused = %v, %v", paused, err)
			}
			if _, err := pdb.Exec("INSERT INTO t VALUES ('paused')"); err != nil {
				t.Fatal(err)
			}
			sop, err = svc.Instances.StartReplica(project, r1.Name).Do()
			if err != nil {
				t.Fatalf("startReplica: %v", err)
			}
			waitSQLOpDone(t, svc, project, sop)
			eventually(t, "after startReplica the replica has the write made while it was stopped", has(r1db, "paused"))

			op, err := svc.Instances.PromoteReplica(project, r1.Name).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOpDone(t, svc, project, op)
			if got, err := svc.Instances.Get(project, r1.Name).Do(); err != nil || got.MasterInstanceName != "" || got.InstanceType != "CLOUD_SQL_INSTANCE" {
				t.Errorf("the promoted replica: %+v %v", got, err)
			}
			eventually(t, "a write on the promoted replica", func() error {
				_, err := r1db.Exec("INSERT INTO t VALUES ('three')")
				return err
			})
			eventually(t, "its replica receives it on the new timeline", has(r2db, "three"))
		})
	}
}
