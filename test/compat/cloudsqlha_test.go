//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLHighAvailability (#1408): through the official Go client a
// PostgreSQL and a MySQL instance are each created REGIONAL, with a standby in a second zone
// (secondaryGceZone) that is not listed as an instance, and a read
// replica. Rows are written through the official connector, each its own
// commit, while the primary reports its standby synchronous (PostgreSQL's
// synchronous_standby_names with sync_state sync; MySQL's lossless
// semi-synchronous replication, AFTER_SYNC, with the standby counted). After
// instances.failover with the instance's settingsVersion, the zones swap,
// the same connection name reaches the new primary through the connector,
// every committed row is there, the new primary has a synchronous standby
// of its own (the old primary, rebuilt), and the read replica follows the
// new primary. Patching ZONAL removes the standby.
// covers: sql.instances.insert, sql.instances.get, sql.instances.list, sql.instances.failover, sql.instances.patch, sql.instances.delete, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLHighAvailability(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	var engines []haEngine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, haPostgres)
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, haMySQL)
	}
	for _, e := range engines {
		t.Run(e.version, func(t *testing.T) {
			h := h.For(t)
			testHighAvailability(t, h, e)
		})
	}
}

// haEngine is what TestCloudSQLHighAvailability does differently per engine.
type haEngine struct {
	version, short string
	register       func(string, ...cloudsqlconn.Option) (func() error, error)
	dsn            func(drv, conn string) string
	setup          []string
	table, insert  string
	// synchronous is the query whose single row is "yes" when the primary
	// waits for a connected standby on every commit.
	synchronous string
	// writable is the query that is true on a primary.
	writable string
}

var haPostgres = haEngine{version: "POSTGRES_17", short: "pg",
	register: func(n string, o ...cloudsqlconn.Option) (func() error, error) { return pgxv5.RegisterDriver(n, o...) },
	dsn: func(_, conn string) string {
		return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
	},
	setup: []string{"CREATE TABLE t (n int PRIMARY KEY)"}, table: "t", insert: "INSERT INTO t VALUES ($1)",
	synchronous: "SELECT CASE WHEN current_setting('synchronous_standby_names') = 'cloudsqlstandby' AND " +
		"current_setting('synchronous_commit') = 'on' AND EXISTS (SELECT 1 FROM pg_stat_replication WHERE " +
		"application_name = 'cloudsqlstandby' AND state = 'streaming' AND sync_state = 'sync') THEN 'yes' ELSE 'no' END",
	writable: "SELECT NOT pg_is_in_recovery()"}

var haMySQL = haEngine{version: "MYSQL_8_4", short: "my",
	register: func(n string, o ...cloudsqlconn.Option) (func() error, error) {
		return cloudsqlmysql.RegisterDriver(n, o...)
	},
	dsn:   func(drv, conn string) string { return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn) },
	setup: []string{"CREATE DATABASE app", "CREATE TABLE app.t (n int PRIMARY KEY)"}, table: "app.t", insert: "INSERT INTO app.t VALUES (?)",
	synchronous: "SELECT IF(@@GLOBAL.rpl_semi_sync_source_enabled = 1 AND @@GLOBAL.rpl_semi_sync_source_wait_point = 'AFTER_SYNC' AND " +
		"@@GLOBAL.rpl_semi_sync_source_wait_no_replica = 1 AND @@GLOBAL.rpl_semi_sync_source_timeout = 4294967295 AND " +
		"(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Rpl_semi_sync_source_status') = 'ON' AND " +
		"(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Rpl_semi_sync_source_clients') >= 1, 'yes', 'no')",
	writable: "SELECT @@GLOBAL.read_only = 0"}

func testHighAvailability(t *testing.T, h *Harness, e haEngine) {
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	name, replica := e.short+"-ha-"+suffix, e.short+"-har-"+suffix
	del := func(n string) {
		if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}
	defer del(name)
	defer del(replica)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: e.version, RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: "REGIONAL"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.Settings.AvailabilityType != "REGIONAL" || in.SecondaryGceZone == "" || in.SecondaryGceZone == in.GceZone {
		t.Fatalf("REGIONAL instance: state %s, %s, zones %s and %s", in.State, in.Settings.AvailabilityType, in.GceZone, in.SecondaryGceZone)
	}
	zone, connName := in.GceZone, in.ConnectionName

	drv := "cloudsql-ha-" + suffix
	cleanup, err := e.register(drv, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	open := func(conn string) *sql.DB {
		db, err := sql.Open(drv, e.dsn(drv, conn))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open(connName)
	for _, q := range e.setup {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	syncStandby := func(db *sql.DB) {
		t.Helper()
		var yes string
		deadline := time.Now().Add(2 * time.Minute)
		for {
			err := db.QueryRow(e.synchronous).Scan(&yes)
			if err == nil && yes == "yes" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the primary's standby is not synchronous: %q %v", yes, err)
			}
			time.Sleep(time.Second)
		}
	}
	syncStandby(db)

	// A read replica of the HA primary, which is not its standby.
	op, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: replica, MasterInstanceName: name,
		InstanceType: "READ_REPLICA_INSTANCE", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	l, err := svc.Instances.List(project).Do()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range l.Items {
		if strings.Contains(i.Name, name) && i.Name != name || strings.HasPrefix(i.Name, "sqlha-") {
			t.Errorf("the standby is listed as an instance: %s", i.Name)
		}
	}

	if e.short == "pg" {
		commitWaitsForTheStandby(t, db, "sqlha-"+name)
	}

	const rows = 200
	for i := 1; i <= rows; i++ {
		if _, err := db.Exec(e.insert, i); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	in, _ = svc.Instances.Get(project, name).Do()
	if _, err := svc.Instances.Failover(project, name, &sqladmin.InstancesFailoverRequest{
		FailoverContext: &sqladmin.FailoverContext{SettingsVersion: in.Settings.SettingsVersion + 7}}).Do(); err == nil {
		t.Error("a failover with a stale settingsVersion was accepted")
	}
	op, err = svc.Instances.Failover(project, name, &sqladmin.InstancesFailoverRequest{
		FailoverContext: &sqladmin.FailoverContext{SettingsVersion: in.Settings.SettingsVersion}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := waitSQLOpDone(t, svc, project, op); got.Error != nil || got.OperationType != "FAILOVER" {
		t.Fatalf("failover: %+v %+v", got, got.Error)
	}
	in, err = svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.ConnectionName != connName || in.GceZone == zone || in.SecondaryGceZone != zone {
		t.Errorf("after failover: state %s, connection %s, zones %s and %s (was %s)", in.State, in.ConnectionName, in.GceZone, in.SecondaryGceZone, zone)
	}

	// The same connection name, through the same connector, reaches the
	// new primary, which has every committed row and takes writes.
	db2 := open(connName)
	var n int
	var writable bool
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err = db2.QueryRow(e.writable).Scan(&writable)
		if err == nil && writable {
			err = db2.QueryRow("SELECT count(*) FROM " + e.table).Scan(&n)
		}
		if err == nil && writable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new primary through %s: %v (writable %v)", connName, err, writable)
		}
		time.Sleep(time.Second)
	}
	if n != rows {
		t.Fatalf("after failover the primary has %d rows, want every committed one, %d", n, rows)
	}
	if _, err := db2.Exec(e.insert, rows+1); err != nil {
		t.Fatalf("a write on the new primary: %v", err)
	}
	syncStandby(db2)

	// The read replica follows the new primary.
	rdb := open(project + ":" + strings.Split(connName, ":")[1] + ":" + replica)
	deadline = time.Now().Add(2 * time.Minute)
	for {
		err := rdb.QueryRow("SELECT count(*) FROM " + e.table).Scan(&n)
		if err == nil && n == rows+1 {
			break
		}
		if time.Now().After(deadline) {
			if kc := os.Getenv(envKubeconfig); kc != "" {
				out, _ := exec.Command("kubectl", "--kubeconfig", kc, "-n", "cloudburrow", "logs", "deployment/sqlinst-"+replica, "--tail=30").CombinedOutput()
				t.Logf("the replica's server:\n%s", out)
			}
			t.Fatalf("the read replica after failover has %d rows, want %d (%v)", n, rows+1, err)
		}
		time.Sleep(time.Second)
	}

	// ZONAL: the standby goes, and the instance serves on.
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{AvailabilityType: "ZONAL"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	in, _ = svc.Instances.Get(project, name).Do()
	if in.Settings.AvailabilityType != "ZONAL" || in.SecondaryGceZone != "" {
		t.Errorf("ZONAL: %s, secondary zone %q", in.Settings.AvailabilityType, in.SecondaryGceZone)
	}
	deadline = time.Now().Add(time.Minute)
	for {
		_, err := db2.Exec(e.insert, rows+2)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a write on the ZONAL instance: %v", err)
		}
		time.Sleep(time.Second)
	}
	if _, err := svc.Instances.Failover(project, name, &sqladmin.InstancesFailoverRequest{}).Do(); err == nil {
		t.Error("a ZONAL instance failed over")
	}
}

// commitWaitsForTheStandby checks a commit on the primary waits while its
// standby, the Deployment standby, is stopped, and returns once it is back
// (#1408). It needs the cluster (CLOUDBURROW_TEST_KUBECONFIG).
func commitWaitsForTheStandby(t *testing.T, db *sql.DB, standby string) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Logf("%s is not set: the commit is not checked to wait for a stopped standby", envKubeconfig)
		return
	}
	kubectl := func(args ...string) {
		t.Helper()
		out, err := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", "cloudburrow"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
	}
	if _, err := db.Exec("CREATE TABLE paused (n int)"); err != nil {
		t.Fatal(err)
	}
	kubectl("scale", "deployment/"+standby, "--replicas=0")
	kubectl("wait", "--for=delete", "pod", "-l", "app="+standby, "--timeout=120s")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := db.ExecContext(ctx, "INSERT INTO paused VALUES (1)")
	cancel()
	if err == nil {
		t.Error("a commit returned while the standby was stopped")
	}
	kubectl("scale", "deployment/"+standby, "--replicas=1")
	kubectl("rollout", "status", "deployment/"+standby, "--timeout=180s")
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, "INSERT INTO paused VALUES (2)"); err != nil {
		t.Fatalf("a commit once the standby is back: %v", err)
	}
}
