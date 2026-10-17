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

// TestCloudSQLExternalMySQLDumpFileAndOffline (#1524): through the
// official Go client, the two other ways Google documents to seed a
// replica of an external MySQL server. With dumpFilePath, the source
// representation instance names a dump in Cloud Storage written with the
// source's GTIDs as purged (mysqldump --set-gtid-purged=ON); the replica
// loads it when it is created and then replicates from the source on its
// own, so a row written after the dump reaches it with no
// startExternalSync. With syncMode OFFLINE, startExternalSync copies the
// source's databases once, and nothing after.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete, sql.instances.promoteReplica, sql.projects.instances.verifyExternalSyncSettings, sql.projects.instances.startExternalSync, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLExternalMySQLDumpFileAndOffline(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	bh := sqlTransferBucket(t, h, "extdump")
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	srcName := fmt.Sprintf("dmp-src-%d", n)
	src := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: srcName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	db := e.root(t, h, src.ConnectionName)
	for _, q := range []string{"SET PERSIST enforce_gtid_consistency = ON", "SET PERSIST gtid_mode = OFF_PERMISSIVE",
		"SET PERSIST gtid_mode = ON_PERMISSIVE", "SET PERSIST gtid_mode = ON",
		"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(20))",
		"INSERT INTO shop.orders VALUES (1, 'dumped')",
		"CREATE USER 'migrator'@'%' IDENTIFIED BY 'Repl-pass1'",
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, SHOW VIEW, RELOAD, TRIGGER, EVENT, EXECUTE, PROCESS ON *.* TO 'migrator'@'%'"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	// The dump mysqldump --set-gtid-purged=ON writes: the source's
	// executed GTIDs, then its data.
	var gtids string
	if err := db.QueryRow("SELECT @@GLOBAL.gtid_executed").Scan(&gtids); err != nil {
		t.Fatal(err)
	}
	sqlWriteObject(t, h, bh, "shop.sql", "SET @@SESSION.SQL_LOG_BIN = 0;\nSET @@GLOBAL.GTID_PURGED = '+"+strings.ReplaceAll(gtids, "\n", "")+"';\n"+
		"CREATE DATABASE shop;\nCREATE TABLE shop.orders (id int primary key, item varchar(20));\nINSERT INTO shop.orders VALUES (1, 'dumped');\n")
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (2, 'after the dump')"); err != nil {
		t.Fatal(err)
	}

	source := func(name, dump string) {
		t.Helper()
		op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_8_4",
			InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + srcName + ":3306",
				Username: "migrator", Password: "Repl-pass1", DumpFilePath: dump}}).Do()
		if err != nil {
			t.Fatalf("insert the source representation instance %s: %v", name, err)
		}
		t.Cleanup(func() {
			if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
				waitSQLOpDone(t, svc, project, d)
			}
		})
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("create the source representation instance %s: %+v", name, op.Error.Errors[0])
		}
	}
	count := func(d *sql.DB, what string, id, want int) {
		t.Helper()
		eventually(t, what, func() error {
			var c int
			if err := d.QueryRow("SELECT count(*) FROM shop.orders WHERE id = ?", id).Scan(&c); err != nil {
				return err
			}
			if c != want {
				return fmt.Errorf("order %d: %d rows, want %d", id, c, want)
			}
			return nil
		})
	}

	dumpSrc, dumpRep := fmt.Sprintf("dmp-rep-%d", n), fmt.Sprintf("dmp-rep-%d-r", n)
	source(dumpSrc, "gs://"+bh.BucketName()+"/shop.sql")
	if got, err := svc.Instances.Get(project, dumpSrc).Do(); err != nil || got.OnPremisesConfiguration.DumpFilePath != "gs://"+bh.BucketName()+"/shop.sql" {
		t.Errorf("the source representation instance's dumpFilePath: %+v %v", got, err)
	}
	rep := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: dumpRep, MasterInstanceName: dumpSrc,
		RootPassword: "Root-pass1", Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	rdb := e.root(t, h, rep.ConnectionName)
	count(rdb, "the dump's row on the replica", 1, 1)
	count(rdb, "a row written after the dump, replicated", 2, 1)
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (3, 'later')"); err != nil {
		t.Fatal(err)
	}
	count(rdb, "a later row, replicated", 3, 1)
	if v, err := svc.Projects.Instances.VerifyExternalSyncSettings(project, dumpRep,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{}).Do(); err != nil || !hasSyncError(v, "REPLICA_ALREADY_SETUP") {
		t.Errorf("verification of a seeded replica: %+v %v", v, err)
	}

	offSrc, offRep := fmt.Sprintf("off-rep-%d", n), fmt.Sprintf("off-rep-%d-r", n)
	source(offSrc, "")
	off := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: offRep, MasterInstanceName: offSrc,
		RootPassword: "Root-pass1", Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	op, err := svc.Projects.Instances.StartExternalSync(project, offRep,
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "OFFLINE"}).Do()
	if err != nil {
		t.Fatalf("startExternalSync OFFLINE: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("startExternalSync OFFLINE: %+v", op.Error.Errors[0])
	}
	odb := e.root(t, h, off.ConnectionName)
	var c int
	if err := odb.QueryRow("SELECT count(*) FROM shop.orders").Scan(&c); err != nil || c != 3 {
		t.Errorf("the offline copy: %d rows, %v; want 3", c, err)
	}
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (4, 'after the copy')"); err != nil {
		t.Fatal(err)
	}
	count(rdb, "a row after the offline copy, on the replicating replica", 4, 1)
	if err := odb.QueryRow("SELECT count(*) FROM shop.orders WHERE id = 4").Scan(&c); err != nil || c != 0 {
		t.Errorf("a row after the offline copy: %d, %v; want none", c, err)
	}

	for _, name := range []string{dumpRep, offRep} {
		op, err := svc.Instances.PromoteReplica(project, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("promoteReplica %s: %+v", name, op.Error.Errors[0])
		}
	}
	eventually(t, "a write on the promoted, seeded instance", func() error {
		_, err := rdb.Exec("INSERT INTO shop.orders VALUES (5, 'migrated')")
		return err
	})
}
