//go:build compat

package compat

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLHighAvailability (#1408): through the official Go client a
// PostgreSQL instance is created REGIONAL, with a standby in a second zone
// (secondaryGceZone) that is not listed as an instance, and a read
// replica. Rows are written through the official connector, each its own
// commit, while the primary reports its standby synchronous. After
// instances.failover with the instance's settingsVersion, the zones swap,
// the same connection name reaches the new primary through the connector,
// every committed row is there, the new primary has a synchronous standby
// of its own (the old primary, rebuilt), and the read replica follows the
// new primary. Patching ZONAL removes the standby.
// covers: sql.instances.insert, sql.instances.get, sql.instances.list, sql.instances.failover, sql.instances.patch, sql.instances.delete, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLHighAvailability(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	name, replica := "pg-ha-"+suffix, "pg-har-"+suffix
	del := func(n string) {
		if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}
	defer del(name)
	defer del(replica)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
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
	cleanup, err := pgxv5.RegisterDriver(drv, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	open := func(conn string) *sql.DB {
		db, err := sql.Open(drv, fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open(connName)
	if _, err := db.Exec("CREATE TABLE t (n int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	syncStandby := func(db *sql.DB) {
		t.Helper()
		var names, commit, state string
		deadline := time.Now().Add(2 * time.Minute)
		for {
			err := db.QueryRow("SELECT current_setting('synchronous_standby_names'), current_setting('synchronous_commit'), " +
				"coalesce((SELECT sync_state FROM pg_stat_replication WHERE application_name = 'cloudsqlstandby' AND state = 'streaming'), '')").
				Scan(&names, &commit, &state)
			if err == nil && names == "cloudsqlstandby" && commit == "on" && state == "sync" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the primary's standby is not synchronous: names %q, synchronous_commit %q, sync_state %q (%v)", names, commit, state, err)
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

	const rows = 200
	for i := 1; i <= rows; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES ($1)", i); err != nil {
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
	var recovery bool
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err = db2.QueryRow("SELECT count(*), pg_is_in_recovery() FROM t").Scan(&n, &recovery)
		if err == nil && !recovery {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new primary through %s: %v (in recovery %v)", connName, err, recovery)
		}
		time.Sleep(time.Second)
	}
	if n != rows {
		t.Fatalf("after failover the primary has %d rows, want every committed one, %d", n, rows)
	}
	if _, err := db2.Exec("INSERT INTO t VALUES ($1)", rows+1); err != nil {
		t.Fatalf("a write on the new primary: %v", err)
	}
	syncStandby(db2)

	// The read replica follows the new primary.
	rdb := open(project + ":" + strings.Split(connName, ":")[1] + ":" + replica)
	deadline = time.Now().Add(2 * time.Minute)
	for {
		err := rdb.QueryRow("SELECT count(*) FROM t").Scan(&n)
		if err == nil && n == rows+1 {
			break
		}
		if time.Now().After(deadline) {
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
		_, err := db2.Exec("INSERT INTO t VALUES ($1)", rows+2)
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
