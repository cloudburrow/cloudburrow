//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestGcloudSQLExternalPostgresOffline (#1524): the gcloud steps of an
// external PostgreSQL primary, through the configuration `cloudburrow
// gcloud-setup` writes, with egress to Google blocked: `gcloud sql
// instances create --database-flags=cloudsql.logical_decoding=on` makes the
// source (another CloudBurrow instance), `create --master-instance-name`
// a replica of its source representation instance, and `promote-replica`
// ends the migration. gcloud has no command for verifyExternalSyncSettings
// or startExternalSync (Google documents them as REST calls), so the
// official Go client makes them, with syncMode OFFLINE: the replica gets a
// copy of the source's database.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete, sql.instances.promoteReplica, sql.projects.instances.verifyExternalSyncSettings, sql.projects.instances.startExternalSync, sql.operations.get
func TestGcloudSQLExternalPostgresOffline(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	e := sqlEngine{}
	n := time.Now().UnixNano() % 1e6
	srcName, repName, offName := fmt.Sprintf("gx-src-%d", n), fmt.Sprintf("gx-rep-%d", n), fmt.Sprintf("gx-rep-%d-off", n)
	g.must("sql", "instances", "create", srcName, "--project", g.project, "--database-version=POSTGRES_17",
		"--tier=db-custom-1-3840", "--region=us-central1", "--database-flags=cloudsql.logical_decoding=on", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", srcName, "--project", g.project, "--quiet")
	src, err := svc.Instances.Get(g.project, srcName).Do()
	if err != nil {
		t.Fatal(err)
	}
	db := e.root(t, h, src.ConnectionName)
	eventually(t, "the source's database", func() error { _, err := db.Exec("CREATE DATABASE shop"); return err })
	shop := e.open(t, h, src.ConnectionName, "postgres", "x", "shop")
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'copied')"} {
		if _, err := shop.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	op, err := svc.Instances.Insert(g.project, &sqladmin.DatabaseInstance{Name: repName, DatabaseVersion: "POSTGRES_17",
		InstanceType: "ON_PREMISES_INSTANCE", OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{
			HostPort: "sqlinst-" + srcName + ":5432", Username: "postgres"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, g.project, op)
	defer g.run(nil, "sql", "instances", "delete", repName, "--project", g.project, "--quiet")
	g.must("sql", "instances", "create", offName, "--project", g.project, "--master-instance-name="+repName,
		"--tier=db-custom-1-3840", "--region=us-central1", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", offName, "--project", g.project, "--quiet")
	off, err := svc.Instances.Get(g.project, offName).Do()
	if err != nil || off.InstanceType != "READ_REPLICA_INSTANCE" || !strings.HasSuffix(off.MasterInstanceName, ":"+repName) {
		t.Fatalf("the replica gcloud created: %+v %v", off, err)
	}
	v, err := svc.Projects.Instances.VerifyExternalSyncSettings(g.project, offName,
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "OFFLINE"}).Do()
	if err != nil || len(v.Errors) != 0 {
		t.Fatalf("verification of a ready source: %+v %v", v, err)
	}
	op, err = svc.Projects.Instances.StartExternalSync(g.project, offName, &sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "OFFLINE"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, g.project, op); op.Error != nil {
		t.Fatalf("startExternalSync OFFLINE: %+v", op.Error.Errors[0])
	}
	odb := e.open(t, h, off.ConnectionName, "postgres", "x", "shop")
	eventually(t, "the offline copy", func() error {
		var c int
		if err := odb.QueryRow("SELECT count(*) FROM orders").Scan(&c); err != nil {
			return err
		}
		if c != 1 {
			return fmt.Errorf("%d rows, want 1", c)
		}
		return nil
	})
	g.must("sql", "instances", "promote-replica", offName, "--project", g.project, "--quiet")
}
