//go:build compat

package compat

import (
	"fmt"
	"os"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLExternalPrimaryReplica (#1501): through the official Go
// client, Cloud SQL's managed import from an external MySQL server, as
// Google documents it (configure-replication-from-external): the source is
// another CloudBurrow MySQL instance with GTIDs on and a replication user;
// a source representation instance (ON_PREMISES_INSTANCE) records it by
// its hostPort; a replica of that is created, verifyExternalSyncSettings
// reports what the source lacks, startExternalSync dumps the source's
// databases into the replica and starts replicating, a row written on the
// source afterwards reaches the replica, and promoteReplica ends the
// migration with a writable instance.
// covers: sql.instances.insert, sql.instances.get, sql.instances.list, sql.instances.delete, sql.instances.promoteReplica, sql.projects.instances.verifyExternalSyncSettings, sql.projects.instances.startExternalSync, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLExternalPrimaryReplica(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	srcName, repName, extName := fmt.Sprintf("ext-src-%d", n), fmt.Sprintf("ext-rep-%d", n), fmt.Sprintf("ext-rep-%d-r", n)
	src := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: srcName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	db := e.root(t, h, src.ConnectionName)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(20))",
		"INSERT INTO shop.orders VALUES (1, 'before')",
		"CREATE USER 'migrator'@'%' IDENTIFIED BY 'Repl-pass1'",
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, SHOW VIEW, RELOAD, TRIGGER, EVENT, EXECUTE, PROCESS ON *.* TO 'migrator'@'%'"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}

	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: repName, DatabaseVersion: "MYSQL_8_4",
		InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
		OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + srcName + ":3306",
			Username: "migrator", Password: "Repl-pass1"}}).Do()
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
	got, err := svc.Instances.Get(project, repName).Do()
	if err != nil || got.InstanceType != "ON_PREMISES_INSTANCE" || got.OnPremisesConfiguration == nil ||
		got.OnPremisesConfiguration.Password != "" || got.OnPremisesConfiguration.Username != "migrator" {
		t.Fatalf("the source representation instance: %+v %v", got, err)
	}

	rep := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: extName, MasterInstanceName: repName,
		RootPassword: "Root-pass1", Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	if rep.InstanceType != "READ_REPLICA_INSTANCE" || rep.DatabaseVersion != "MYSQL_8_4" {
		t.Errorf("the replica: %s %s", rep.InstanceType, rep.DatabaseVersion)
	}
	if s, err := svc.Instances.Get(project, repName).Do(); err != nil || len(s.ReplicaNames) != 1 || s.ReplicaNames[0] != extName {
		t.Errorf("the source representation instance's replicaNames: %+v %v", s, err)
	}

	// GTIDs are off on a new MySQL instance, as they are on many external
	// servers: verification says so, and startExternalSync is refused.
	v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, extName,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if !hasSyncError(v, "UNSUPPORTED_GTID_MODE") {
		t.Errorf("verification of a source without GTIDs: %+v", v.Errors)
	}
	_, err = svc.Projects.Instances.StartExternalSync(project, extName, &sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	wantHTTP(t, "startExternalSync from a source without GTIDs", err, 400)

	for _, q := range []string{"SET PERSIST enforce_gtid_consistency = ON", "SET PERSIST gtid_mode = OFF_PERMISSIVE",
		"SET PERSIST gtid_mode = ON_PERMISSIVE", "SET PERSIST gtid_mode = ON"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	v, err = svc.Projects.Instances.VerifyExternalSyncSettings(project, extName,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "ONLINE"}).Do()
	if err != nil || len(v.Errors) != 0 {
		t.Fatalf("verification of a ready source: %+v %v", v, err)
	}
	op, err = svc.Projects.Instances.StartExternalSync(project, extName,
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatalf("startExternalSync: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("startExternalSync: %+v", op.Error.Errors[0])
	}
	rdb := e.root(t, h, rep.ConnectionName)
	seen := func(what string, id int) {
		t.Helper()
		eventually(t, what, func() error {
			var c int
			if err := rdb.QueryRow("SELECT count(*) FROM shop.orders WHERE id = ?", id).Scan(&c); err != nil {
				return err
			}
			if c != 1 {
				return fmt.Errorf("order %d not on the replica", id)
			}
			return nil
		})
	}
	seen("the dumped row on the replica", 1)
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (2, 'after')"); err != nil {
		t.Fatal(err)
	}
	seen("a row written on the source after the sync", 2)
	if v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, extName,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{}).Do(); err != nil || !hasSyncError(v, "REPLICA_ALREADY_SETUP") {
		t.Errorf("verification after the sync: %+v %v", v, err)
	}
	_, err = svc.Instances.Delete(project, repName).Do()
	wantHTTP(t, "deleting a source representation instance with a replica", err, 400)

	op, err = svc.Instances.PromoteReplica(project, extName).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("promoteReplica: %+v", op.Error.Errors[0])
	}
	if p, err := svc.Instances.Get(project, extName).Do(); err != nil || p.MasterInstanceName != "" || p.InstanceType != "CLOUD_SQL_INSTANCE" {
		t.Errorf("the promoted instance: %+v %v", p, err)
	}
	eventually(t, "a write on the promoted instance", func() error {
		_, err := rdb.Exec("INSERT INTO shop.orders VALUES (3, 'migrated')")
		return err
	})
}

func hasSyncError(v *sqladmin.SqlInstancesVerifyExternalSyncSettingsResponse, typ string) bool {
	for _, e := range v.Errors {
		if e.Type == typ {
			return true
		}
	}
	return false
}
