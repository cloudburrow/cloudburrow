//go:build compat

package compat

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLDemote (#1524): through the official Go client, the reverse
// of promoteReplica. demoteMaster makes a standalone MySQL instance a read
// replica of the server a source representation instance records, keeping
// its data and replicating by GTID auto-positioning: rows written on the
// source reach it, it refuses writes, and it is refused again once it has
// transactions of its own (verifyGtidConsistency) until that check is
// turned off. demote, Database Migration Service's step for a migration
// to an existing instance, leaves an instance a replica that
// startExternalSync then fills from the source.
// covers: sql.instances.demoteMaster, sql.instances.demote, sql.instances.insert, sql.instances.get, sql.instances.delete, sql.instances.promoteReplica, sql.projects.instances.startExternalSync, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLDemote(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	srcName, repName := fmt.Sprintf("dem-src-%d", n), fmt.Sprintf("dem-rep-%d", n)
	tier := &sqladmin.Settings{Tier: "db-custom-1-3840"}
	src := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: srcName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Settings: tier})
	db := e.root(t, h, src.ConnectionName)
	for _, q := range []string{"SET PERSIST enforce_gtid_consistency = ON", "SET PERSIST gtid_mode = OFF_PERMISSIVE",
		"SET PERSIST gtid_mode = ON_PERMISSIVE", "SET PERSIST gtid_mode = ON",
		"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(20))",
		"INSERT INTO shop.orders VALUES (1, 'before')",
		"CREATE USER 'migrator'@'%' IDENTIFIED BY 'Repl-pass1'",
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, SHOW VIEW, RELOAD, TRIGGER, EVENT, EXECUTE, PROCESS ON *.* TO 'migrator'@'%'"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: repName, DatabaseVersion: "MYSQL_8_4",
		InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
		OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + srcName + ":3306", Username: "migrator", Password: "Repl-pass1"}}).Do()
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
	bName, cName := fmt.Sprintf("dem-b-%d", n), fmt.Sprintf("dem-c-%d", n)
	b := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: bName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Region: "us-central1", Settings: tier})
	c := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: cName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Region: "us-central1", Settings: tier})

	demoteMaster := func(ctx *sqladmin.DemoteMasterContext) (*sqladmin.Operation, error) {
		ctx.MasterInstanceName = repName
		ctx.ReplicaConfiguration = &sqladmin.DemoteMasterConfiguration{MysqlReplicaConfiguration: &sqladmin.DemoteMasterMySqlReplicaConfiguration{
			Username: "migrator", Password: "Repl-pass1"}}
		return svc.Instances.DemoteMaster(project, bName, &sqladmin.InstancesDemoteMasterRequest{DemoteMasterContext: ctx}).Do()
	}
	_, err = demoteMaster(&sqladmin.DemoteMasterContext{SkipReplicationSetup: true})
	wantCode(t, "demoteMaster with skipReplicationSetup", err, 501, "mysql.*")
	op, err = demoteMaster(&sqladmin.DemoteMasterContext{})
	if err != nil {
		t.Fatalf("demoteMaster: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil || op.OperationType != "DEMOTE_MASTER" {
		t.Fatalf("demoteMaster: %s %+v", op.OperationType, op.Error)
	}
	if got, err := svc.Instances.Get(project, bName).Do(); err != nil || got.InstanceType != "READ_REPLICA_INSTANCE" ||
		got.MasterInstanceName != project+":"+repName {
		t.Fatalf("the demoted instance: %+v %v", got, err)
	}
	seen := func(rdb *sql.DB, what string, id int) {
		t.Helper()
		eventually(t, what, func() error {
			var n int
			if err := rdb.QueryRow("SELECT count(*) FROM shop.orders WHERE id = ?", id).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("order %d is not there", id)
			}
			return nil
		})
	}
	bdb := e.root(t, h, b.ConnectionName)
	seen(bdb, "the source's earlier row on the demoted instance", 1)
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (2, 'after demote')"); err != nil {
		t.Fatal(err)
	}
	seen(bdb, "a row written on the source after demoteMaster", 2)
	if _, err := bdb.Exec("INSERT INTO shop.orders VALUES (100, 'on the replica')"); err == nil {
		t.Error("the demoted instance took a write")
	}
	_, err = demoteMaster(&sqladmin.DemoteMasterContext{})
	wantHTTP(t, "demoteMaster of a replica", err, 400)

	// Promoted and written to, it has a transaction the source has not.
	op, err = svc.Instances.PromoteReplica(project, bName).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("promoteReplica: %+v", op.Error.Errors[0])
	}
	eventually(t, "a write on the promoted instance", func() error {
		_, err := bdb.Exec("INSERT INTO shop.orders VALUES (3, 'promoted')")
		return err
	})
	_, err = demoteMaster(&sqladmin.DemoteMasterContext{})
	wantCode(t, "demoteMaster of an instance with GTIDs the source lacks", err, 400, "not consistent")
	op, err = demoteMaster(&sqladmin.DemoteMasterContext{VerifyGtidConsistency: false, ForceSendFields: []string{"VerifyGtidConsistency"}})
	if err != nil {
		t.Fatalf("demoteMaster without verifyGtidConsistency: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("demoteMaster without verifyGtidConsistency: %+v", op.Error.Errors[0])
	}
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (4, 'demoted again')"); err != nil {
		t.Fatal(err)
	}
	seen(bdb, "a row written on the source after the second demoteMaster", 4)

	// demote, then startExternalSync, fills an existing instance.
	op, err = svc.Instances.Demote(project, cName, &sqladmin.InstancesDemoteRequest{
		DemoteContext: &sqladmin.DemoteContext{SourceRepresentativeInstanceName: repName}}).Do()
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("demote: %+v", op.Error.Errors[0])
	}
	if got, err := svc.Instances.Get(project, cName).Do(); err != nil || got.InstanceType != "READ_REPLICA_INSTANCE" {
		t.Fatalf("the instance demote made a replica: %+v %v", got, err)
	}
	op, err = svc.Projects.Instances.StartExternalSync(project, cName, &sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatalf("startExternalSync after demote: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("startExternalSync after demote: %+v", op.Error.Errors[0])
	}
	cdb := e.root(t, h, c.ConnectionName)
	seen(cdb, "the dumped rows on the instance demote made a replica", 4)
	if _, err := db.Exec("INSERT INTO shop.orders VALUES (5, 'after the sync')"); err != nil {
		t.Fatal(err)
	}
	seen(cdb, "a row written on the source after the sync", 5)
	seen(bdb, "the same row on the demoteMaster replica", 5)
}
