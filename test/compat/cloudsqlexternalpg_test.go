//go:build compat

package compat

import (
	"fmt"
	"os"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLExternalPostgresPrimary (#1524): through the official Go
// client, a replica of an external PostgreSQL server, as Google documents
// it (configure-replication-from-external): the source is another
// CloudBurrow PostgreSQL instance; a source representation instance
// records it by its hostPort, with a replication user; a replica of that
// is created, verifyExternalSyncSettings reports what the source lacks
// (wal_level, the user's REPLICATION attribute, CREATE on the database and
// table ownership, which CloudBurrow's publication needs), the
// source is given cloudsql.logical_decoding, startExternalSync copies its
// database into the replica and goes on replicating it, a row written on
// the source afterwards reaches the replica, and promoteReplica ends the
// migration with a writable instance. A second replica takes an OFFLINE
// sync: the rows as they were, and nothing after.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.instances.promoteReplica, sql.projects.instances.verifyExternalSyncSettings, sql.projects.instances.startExternalSync, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLExternalPostgresPrimary(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	project := h.Project()
	e := sqlEngine{}
	n := time.Now().UnixNano() % 1e6
	srcName, repName := fmt.Sprintf("pgx-src-%d", n), fmt.Sprintf("pgx-rep-%d", n)
	onlineName, offlineName := repName+"-on", repName+"-off"
	src := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: srcName, DatabaseVersion: "POSTGRES_16",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	db := e.root(t, h, src.ConnectionName)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE ROLE migrator LOGIN PASSWORD 'Repl-pass1'"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	shop := e.open(t, h, src.ConnectionName, "postgres", "x", "shop")
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'before')",
		"GRANT SELECT ON orders TO migrator"} {
		if _, err := shop.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: repName, DatabaseVersion: "POSTGRES_16",
		InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
		OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + srcName + ":5432",
			Username: "migrator", Password: "Repl-pass1", SelectedObjects: []*sqladmin.SelectedObjects{{Database: "shop"}}}}).Do()
	if err != nil {
		t.Fatalf("insert the source representation instance: %v", err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, repName).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("create the source representation instance: %+v", op.Error.Errors[0])
	}
	if got, err := svc.Instances.Get(project, repName).Do(); err != nil || got.InstanceType != "ON_PREMISES_INSTANCE" ||
		got.OnPremisesConfiguration.Password != "" || got.DatabaseVersion != "POSTGRES_16" {
		t.Fatalf("the source representation instance: %+v %v", got, err)
	}

	rep := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: onlineName, MasterInstanceName: repName,
		DatabaseVersion: "POSTGRES_17", Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	if rep.InstanceType != "READ_REPLICA_INSTANCE" || rep.DatabaseVersion != "POSTGRES_17" {
		t.Errorf("the replica: %s %s", rep.InstanceType, rep.DatabaseVersion)
	}

	// A new instance has wal_level replica, and the user neither has
	// REPLICATION nor owns the table.
	v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, onlineName,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !hasSyncError(v, "INVALID_WAL_LEVEL") || !hasSyncError(v, "INSUFFICIENT_PRIVILEGE") {
		t.Errorf("verification of an unprepared source: %+v", v.Errors)
	}
	_, err = svc.Projects.Instances.StartExternalSync(project, onlineName, &sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	wantHTTP(t, "startExternalSync from an unprepared source", err, 400)

	// Google's steps for a Cloud SQL source: the flags, a restart, and a
	// user with REPLICATION.
	op, err = svc.Instances.Patch(project, srcName, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "cloudsql.logical_decoding", Value: "on"}, {Name: "cloudsql.enable_pglogical", Value: "on"}}}}).Do()
	if err != nil {
		t.Fatalf("patch the source's flags: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("patch the source's flags: %+v", op.Error.Errors[0])
	}
	for _, q := range []string{"ALTER ROLE migrator REPLICATION"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	eventually(t, "the table owned by the migration user", func() error {
		_, err := shop.Exec("ALTER TABLE orders OWNER TO migrator")
		return err
	})
	if _, err := shop.Exec("GRANT CREATE ON DATABASE shop TO migrator"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "verification of a ready source", func() error {
		v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, onlineName,
			&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "ONLINE"}).Do()
		if err != nil {
			return err
		}
		if len(v.Errors) != 0 {
			return fmt.Errorf("%s: %s", v.Errors[0].Type, v.Errors[0].Detail)
		}
		return nil
	})
	op, err = svc.Projects.Instances.StartExternalSync(project, onlineName,
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatalf("startExternalSync: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("startExternalSync: %+v", op.Error.Errors[0])
	}
	rdb := e.open(t, h, rep.ConnectionName, "postgres", "x", "shop")
	count := func(what string, id, want int) {
		t.Helper()
		eventually(t, what, func() error {
			var c int
			if err := rdb.QueryRow("SELECT count(*) FROM orders WHERE id = $1", id).Scan(&c); err != nil {
				return err
			}
			if c != want {
				return fmt.Errorf("order %d: %d rows on the replica, want %d", id, c, want)
			}
			return nil
		})
	}
	count("the copied row on the replica", 1, 1)
	if _, err := shop.Exec("INSERT INTO orders VALUES (2, 'after')"); err != nil {
		t.Fatal(err)
	}
	count("a row written on the source after the sync", 2, 1)
	if v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, onlineName,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{}).Do(); err != nil || !hasSyncError(v, "REPLICA_ALREADY_SETUP") {
		t.Errorf("verification after the sync: %+v %v", v, err)
	}

	// An OFFLINE sync copies the rows once.
	off := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: offlineName, MasterInstanceName: repName,
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	op, err = svc.Projects.Instances.StartExternalSync(project, offlineName,
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "OFFLINE"}).Do()
	if err != nil {
		t.Fatalf("startExternalSync OFFLINE: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("startExternalSync OFFLINE: %+v", op.Error.Errors[0])
	}
	odb := e.open(t, h, off.ConnectionName, "postgres", "x", "shop")
	var c int
	if err := odb.QueryRow("SELECT count(*) FROM orders").Scan(&c); err != nil || c != 2 {
		t.Errorf("the offline copy: %d rows, %v; want 2", c, err)
	}
	if _, err := shop.Exec("INSERT INTO orders VALUES (3, 'later')"); err != nil {
		t.Fatal(err)
	}
	count("a later row on the online replica", 3, 1)
	if err := odb.QueryRow("SELECT count(*) FROM orders WHERE id = 3").Scan(&c); err != nil || c != 0 {
		t.Errorf("a later row on the offline copy: %d, %v; want none", c, err)
	}

	_, err = svc.Instances.Delete(project, repName).Do()
	wantHTTP(t, "deleting a source representation instance with replicas", err, 400)
	for _, name := range []string{onlineName, offlineName} {
		op, err = svc.Instances.PromoteReplica(project, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("promoteReplica %s: %+v", name, op.Error.Errors[0])
		}
		if p, err := svc.Instances.Get(project, name).Do(); err != nil || p.MasterInstanceName != "" || p.InstanceType != "CLOUD_SQL_INSTANCE" {
			t.Errorf("the promoted instance %s: %+v %v", name, p, err)
		}
	}
	if _, err := rdb.Exec("INSERT INTO orders VALUES (4, 'migrated')"); err != nil {
		t.Errorf("a write on the promoted instance: %v", err)
	}
	if _, err := shop.Exec("INSERT INTO orders VALUES (5, 'source')"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if err := rdb.QueryRow("SELECT count(*) FROM orders WHERE id = 5").Scan(&c); err != nil || c != 0 {
		t.Errorf("a source row after the promotion: %d, %v; want none", c, err)
	}
	var slots int
	if err := db.QueryRow("SELECT count(*) FROM pg_replication_slots").Scan(&slots); err != nil || slots != 0 {
		t.Errorf("the source's replication slots after the promotion: %d, %v; want none", slots, err)
	}
}
