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

// TestCloudSQLSwitchover (#1614): through the official Go client, an
// Enterprise Plus MySQL primary designates its cross-region read replica
// its DR replica (replicationCluster.failoverDrReplicaName; the replica
// reports drReplica), and instances.switchover, called on the replica,
// reverses their roles: the replica is the primary (CLOUD_SQL_INSTANCE,
// no masterInstanceName, the old primary in replicaNames and as its DR
// replica), and the old primary its read replica (READ_REPLICA_INSTANCE,
// masterInstanceName the new primary, drReplica). Rows written before the
// switchover are read on both through the official connector; a row
// written on the new primary after it reaches the old one, which refuses
// writes. A second switchover switches them back, and a row written on the
// original primary reaches its replica again. Switchover of a replica not
// designated, a designation on a replica, and a PostgreSQL primary's DR
// replica and switchover (501) are refused.
// covers: sql.instances.switchover, sql.instances.patch, sql.instances.insert, sql.instances.get, sql.instances.delete, sql.instances.promoteReplica, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLSwitchover(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	pName, rName := fmt.Sprintf("sw-p-%d", n), fmt.Sprintf("sw-r-%d", n)
	plus := func() *sqladmin.Settings {
		return &sqladmin.Settings{Tier: "db-perf-optimized-N-2", Edition: "ENTERPRISE_PLUS"}
	}
	p := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: pName, DatabaseVersion: "MYSQL_8_4",
		RootPassword: "Root-pass1", Region: "us-central1", Settings: plus()})
	pdb := e.root(t, h, p.ConnectionName)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(40))",
		"INSERT INTO shop.orders VALUES (1, 'before the replica')"} {
		q := q
		eventually(t, q, func() error { _, err := pdb.Exec(q); return err })
	}
	r := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: rName, DatabaseVersion: "MYSQL_8_4",
		MasterInstanceName: pName, InstanceType: "READ_REPLICA_INSTANCE", Region: "us-east1", Settings: plus()})
	t.Cleanup(func() {
		// Whichever is the replica is promoted, so both can be deleted.
		for _, nm := range []string{pName, rName} {
			if in, err := svc.Instances.Get(project, nm).Do(); err == nil && in.MasterInstanceName != "" {
				if op, err := svc.Instances.PromoteReplica(project, nm).Do(); err == nil {
					waitSQLOpDone(t, svc, project, op)
				}
			}
		}
	})
	rdb := e.root(t, h, r.ConnectionName)
	if _, err := pdb.Exec("INSERT INTO shop.orders VALUES (2, 'before the switchover')"); err != nil {
		t.Fatal(err)
	}
	seen := func(db *sql.DB, what string, id int, item string) {
		t.Helper()
		eventually(t, what, func() error {
			var got string
			if err := db.QueryRow("SELECT item FROM shop.orders WHERE id = ?", id).Scan(&got); err != nil {
				return err
			}
			if got != item {
				return fmt.Errorf("order %d is %q, want %q", id, got, item)
			}
			return nil
		})
	}
	seen(rdb, "the primary's rows on its replica", 2, "before the switchover")

	_, err := svc.Instances.Switchover(project, rName).Do()
	wantCode(t, "switchover of a replica not designated", err, 400, "not the DR replica")
	designate := func(primary, dr string) (*sqladmin.Operation, error) {
		// An empty name removes the designation, sent as "".
		return svc.Instances.Patch(project, primary, &sqladmin.DatabaseInstance{
			ReplicationCluster: &sqladmin.ReplicationCluster{FailoverDrReplicaName: dr, ForceSendFields: []string{"FailoverDrReplicaName"}}}).Do()
	}
	_, err = designate(rName, pName)
	wantCode(t, "a DR replica designated on a replica", err, 400, "read replica")
	op, err := designate(pName, project+":"+rName)
	if err != nil {
		t.Fatalf("designate the DR replica: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("designate the DR replica: %+v", op.Error.Errors[0])
	}
	roles := func(primary, replica string) {
		t.Helper()
		pi, err := svc.Instances.Get(project, primary).Do()
		if err != nil {
			t.Fatal(err)
		}
		ri, err := svc.Instances.Get(project, replica).Do()
		if err != nil {
			t.Fatal(err)
		}
		if pi.InstanceType != "CLOUD_SQL_INSTANCE" || pi.MasterInstanceName != "" || pi.ReplicationCluster == nil ||
			pi.ReplicationCluster.FailoverDrReplicaName != project+":"+replica || pi.ReplicationCluster.DrReplica ||
			strings.Join(pi.ReplicaNames, ",") != replica || pi.State != "RUNNABLE" {
			t.Errorf("the primary %s: type %s master %q cluster %+v replicas %v state %s", primary, pi.InstanceType,
				pi.MasterInstanceName, pi.ReplicationCluster, pi.ReplicaNames, pi.State)
		}
		if ri.InstanceType != "READ_REPLICA_INSTANCE" || ri.MasterInstanceName != project+":"+primary || ri.ReplicationCluster == nil ||
			!ri.ReplicationCluster.DrReplica || ri.ReplicationCluster.FailoverDrReplicaName != "" || len(ri.ReplicaNames) != 0 ||
			ri.State != "RUNNABLE" {
			t.Errorf("the DR replica %s: type %s master %q cluster %+v replicas %v state %s", replica, ri.InstanceType,
				ri.MasterInstanceName, ri.ReplicationCluster, ri.ReplicaNames, ri.State)
		}
	}
	roles(pName, rName)

	switchover := func(dr string) {
		t.Helper()
		op, err := svc.Instances.Switchover(project, dr).DbTimeout("600s").Do()
		if err != nil {
			t.Fatalf("switchover to %s: %v", dr, err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil || op.OperationType != "SWITCHOVER" {
			t.Fatalf("switchover to %s: %s %+v", dr, op.OperationType, op.Error)
		}
	}
	switchover(rName)
	roles(rName, pName)
	seen(rdb, "a row of before the switchover on the new primary", 2, "before the switchover")
	seen(pdb, "the same row on the old primary", 2, "before the switchover")
	eventually(t, "a write on the new primary", func() error {
		_, err := rdb.Exec("INSERT INTO shop.orders VALUES (3, 'on the new primary')")
		return err
	})
	if _, err := rdb.Exec("UPDATE shop.orders SET item = 'changed on the new primary' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	seen(pdb, "a row written on the new primary, on the old one", 3, "on the new primary")
	seen(pdb, "a row changed on the new primary, on the old one", 1, "changed on the new primary")
	if _, err := pdb.Exec("INSERT INTO shop.orders VALUES (100, 'on the old primary')"); err == nil {
		t.Error("the old primary, now a replica, took a write")
	}

	// Switched back, the original primary takes writes again.
	switchover(pName)
	roles(pName, rName)
	eventually(t, "a write on the original primary", func() error {
		_, err := pdb.Exec("INSERT INTO shop.orders VALUES (4, 'switched back')")
		return err
	})
	seen(rdb, "a row written after switching back, on the replica", 4, "switched back")
	seen(rdb, "the row of the first switchover, on the replica", 3, "on the new primary")
	if _, err := rdb.Exec("INSERT INTO shop.orders VALUES (101, 'on the replica')"); err == nil {
		t.Error("the replica took a write after switching back")
	}
	// The designation removed, switchover is refused.
	if op, err = designate(pName, ""); err == nil {
		op = waitSQLOpDone(t, svc, project, op)
	}
	if err != nil || op.Error != nil {
		t.Fatalf("remove the DR replica: %v", err)
	}
	if ri, err := svc.Instances.Get(project, rName).Do(); err != nil || ri.ReplicationCluster != nil {
		t.Errorf("the replica after the designation is removed: %+v %v", ri, err)
	}
	_, err = svc.Instances.Switchover(project, rName).Do()
	wantCode(t, "switchover after the designation is removed", err, 400, "not the DR replica")

	if os.Getenv(EnvCloudSQL) != "" {
		pg := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: fmt.Sprintf("sw-pg-%d", n), DatabaseVersion: "POSTGRES_17",
			Region: "us-central1", Settings: plus()})
		_, err = designate(pg.Name, "x")
		wantCode(t, "a PostgreSQL primary's DR replica", err, 501, "PostgreSQL")
		_, err = svc.Instances.Switchover(project, pg.Name).Do()
		wantCode(t, "switchover of a PostgreSQL instance", err, 501, "PostgreSQL")
	}
}

// TestGcloudSQLSwitchover (#1614): `gcloud sql instances patch
// --failover-dr-replica-name` designates a cross-region MySQL replica the
// DR replica, `gcloud sql instances switchover` (with --db-timeout) makes
// it the primary and the old primary its replica, as describe shows, and
// `--clear-failover-dr-replica-name` removes the designation, through the
// configuration `cloudburrow gcloud-setup` writes, with egress to Google
// blocked.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.switchover, sql.instances.delete, sql.operations.get
func TestGcloudSQLSwitchover(t *testing.T) {
	h := New(t)
	_ = h.Endpoint(EnvSQLAdmin)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano() % 1e6
	p, r := fmt.Sprintf("gsw-p-%d", n), fmt.Sprintf("gsw-r-%d", n)
	g.must("sql", "instances", "create", p, "--project", g.project, "--database-version=MYSQL_8_4", "--edition=enterprise-plus",
		"--tier=db-perf-optimized-N-2", "--region=us-central1", "--root-password=Root-pass1", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", p, "--project", g.project, "--quiet")
	g.must("sql", "instances", "create", r, "--project", g.project, "--master-instance-name="+p, "--edition=enterprise-plus",
		"--tier=db-perf-optimized-N-2", "--region=us-east1", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", r, "--project", g.project, "--quiet")
	desc := func(name string) string {
		t.Helper()
		return strings.Join(strings.Fields(g.must("sql", "instances", "describe", name, "--project", g.project,
			"--format=value(instanceType,masterInstanceName,replicationCluster.failoverDrReplicaName,replicationCluster.drReplica)")), " ")
	}
	g.must("sql", "instances", "patch", p, "--project", g.project, "--failover-dr-replica-name="+r, "--quiet")
	if got, want := desc(p), "CLOUD_SQL_INSTANCE "+g.project+":"+r; got != want {
		t.Errorf("the primary after designating: %q, want %q", got, want)
	}
	if got, want := desc(r), "READ_REPLICA_INSTANCE "+g.project+":"+p+" True"; got != want {
		t.Errorf("the DR replica: %q, want %q", got, want)
	}
	g.must("sql", "instances", "switchover", r, "--project", g.project, "--db-timeout=10m", "--quiet")
	if got, want := desc(r), "CLOUD_SQL_INSTANCE "+g.project+":"+p; got != want {
		t.Errorf("the new primary: %q, want %q", got, want)
	}
	if got, want := desc(p), "READ_REPLICA_INSTANCE "+g.project+":"+r+" True"; got != want {
		t.Errorf("the old primary: %q, want %q", got, want)
	}
	g.must("sql", "instances", "patch", r, "--project", g.project, "--clear-failover-dr-replica-name", "--quiet")
	if got, want := desc(r), "CLOUD_SQL_INSTANCE"; got != want {
		t.Errorf("the primary after clearing its DR replica: %q, want %q", got, want)
	}
	if out, err := g.run(nil, "sql", "instances", "switchover", p, "--project", g.project, "--quiet"); err == nil {
		t.Errorf("switchover without a DR replica succeeded: %s", out)
	}
	g.must("sql", "instances", "promote-replica", p, "--project", g.project, "--quiet")
}
