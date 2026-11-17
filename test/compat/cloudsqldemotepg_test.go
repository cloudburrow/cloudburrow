//go:build compat

package compat

import (
	"fmt"
	"os"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLDemotePostgres (#1614): through the official Go client,
// demote of an existing PostgreSQL instance, Database Migration Service's
// step for a migration to an existing destination: the instance becomes a
// read replica of the server a source representation instance records,
// and startExternalSync copies the selected database into it, dropping
// first the instance's own database of that name (as Database Migration
// Service documents) and keeping its other databases; a row written on the
// source afterwards reaches it, and promoteReplica ends the migration.
// covers: sql.instances.demote, sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.instances.promoteReplica, sql.projects.instances.startExternalSync, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLDemotePostgres(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	project := h.Project()
	e := sqlEngine{}
	n := time.Now().UnixNano() % 1e6
	srcName, repName, dstName := fmt.Sprintf("pgd-src-%d", n), fmt.Sprintf("pgd-rep-%d", n), fmt.Sprintf("pgd-dst-%d", n)
	tier := &sqladmin.Settings{Tier: "db-custom-1-3840"}
	src := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: srcName, DatabaseVersion: "POSTGRES_16", Settings: &sqladmin.Settings{
		Tier: "db-custom-1-3840", DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "cloudsql.logical_decoding", Value: "on"}}}})
	db := e.root(t, h, src.ConnectionName)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE ROLE migrator LOGIN REPLICATION PASSWORD 'Repl-pass1'"} {
		q := q
		eventually(t, q, func() error { _, err := db.Exec(q); return err })
	}
	shop := e.open(t, h, src.ConnectionName, "postgres", "x", "shop")
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'from the source')",
		"ALTER TABLE orders OWNER TO migrator", "GRANT CREATE ON DATABASE shop TO migrator"} {
		q := q
		eventually(t, q, func() error { _, err := shop.Exec(q); return err })
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

	// The existing destination, with a database named as the migrated one
	// and one of its own.
	dst := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: dstName, DatabaseVersion: "POSTGRES_16",
		Region: "us-central1", Settings: tier})
	ddb := e.root(t, h, dst.ConnectionName)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE DATABASE keep"} {
		q := q
		eventually(t, q, func() error { _, err := ddb.Exec(q); return err })
	}
	stale := e.open(t, h, dst.ConnectionName, "postgres", "x", "shop")
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'stale')"} {
		if _, err := stale.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = stale.Close()

	_, err = svc.Instances.DemoteMaster(project, dstName, &sqladmin.InstancesDemoteMasterRequest{
		DemoteMasterContext: &sqladmin.DemoteMasterContext{MasterInstanceName: repName}}).Do()
	wantHTTP(t, "demoteMaster of a PostgreSQL instance", err, 400)
	op, err = svc.Instances.Demote(project, dstName, &sqladmin.InstancesDemoteRequest{
		DemoteContext: &sqladmin.DemoteContext{SourceRepresentativeInstanceName: repName}}).Do()
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil || op.OperationType != "DEMOTE" {
		t.Fatalf("demote: %s %+v", op.OperationType, op.Error)
	}
	if got, err := svc.Instances.Get(project, dstName).Do(); err != nil || got.InstanceType != "READ_REPLICA_INSTANCE" ||
		got.MasterInstanceName != project+":"+repName {
		t.Fatalf("the demoted instance: %+v %v", got, err)
	}
	op, err = svc.Projects.Instances.StartExternalSync(project, dstName,
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatalf("startExternalSync: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("startExternalSync: %+v", op.Error.Errors[0])
	}
	rdb := e.open(t, h, dst.ConnectionName, "postgres", "x", "shop")
	item := func(what string, id int, want string) {
		t.Helper()
		eventually(t, what, func() error {
			var got string
			if err := rdb.QueryRow("SELECT item FROM orders WHERE id = $1", id).Scan(&got); err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("order %d is %q, want %q", id, got, want)
			}
			return nil
		})
	}
	item("the source's row in place of the instance's own", 1, "from the source")
	if _, err := shop.Exec("INSERT INTO orders VALUES (2, 'after the sync')"); err != nil {
		t.Fatal(err)
	}
	item("a row written on the source after the sync", 2, "after the sync")
	var keep int
	if err := e.root(t, h, dst.ConnectionName).QueryRow("SELECT count(*) FROM pg_database WHERE datname = 'keep'").Scan(&keep); err != nil || keep != 1 {
		t.Errorf("the instance's other database: %d %v", keep, err)
	}

	op, err = svc.Instances.PromoteReplica(project, dstName).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("promoteReplica: %+v", op.Error.Errors[0])
	}
	if got, err := svc.Instances.Get(project, dstName).Do(); err != nil || got.InstanceType == "READ_REPLICA_INSTANCE" {
		t.Errorf("the promoted instance: %+v %v", got, err)
	}
}
