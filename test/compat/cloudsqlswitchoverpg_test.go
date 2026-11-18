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

// TestCloudSQLSwitchoverPostgres (#1524): through the official Go client,
// as Cloud SQL documents advanced disaster recovery for PostgreSQL
// (docs.cloud.google.com/sql/docs/postgres/use-advanced-disaster-recovery),
// an Enterprise Plus PostgreSQL 17 primary designates its cross-region read
// replica its DR replica (replicationCluster.failoverDrReplicaName; the
// replica reports drReplica), and instances.switchover, called on the
// replica, reverses their roles with no data lost: the replica is promoted
// and is the primary (no masterInstanceName, the old primary in
// replicaNames and as its DR replica), and the old primary, rebuilt from a
// base backup of it, is its streaming read replica (masterInstanceName the
// new primary, drReplica). Rows written before the switchover are read on
// both through the official connector; rows written and changed on the new
// primary after it reach the old one, which refuses writes. A second
// switchover switches them back, and a row written on the original primary
// reaches its replica again. pg_is_in_recovery() says which server is the
// standby at each step. A switchover of a primary with another read
// replica, which has a cascading replica of its own, leaves both replicas
// of their masters, rebuilt, streaming the new primary's writes.
// covers: sql.instances.switchover, sql.instances.patch, sql.instances.insert, sql.instances.get, sql.instances.delete, sql.instances.promoteReplica, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLSwitchoverPostgres(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	project := h.Project()
	e := sqlEngine{}
	n := time.Now().UnixNano() % 1e6
	pName, rName := fmt.Sprintf("swpg-p-%d", n), fmt.Sprintf("swpg-r-%d", n)
	plus := func() *sqladmin.Settings {
		return &sqladmin.Settings{Tier: "db-perf-optimized-N-2", Edition: "ENTERPRISE_PLUS"}
	}
	p := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: pName, DatabaseVersion: "POSTGRES_17",
		RootPassword: "Root-pass1", Region: "us-central1", Settings: plus()})
	pdb := e.root(t, h, p.ConnectionName)
	for _, q := range []string{"CREATE TABLE orders (id int PRIMARY KEY, item text)", "INSERT INTO orders VALUES (1, 'before the replica')"} {
		q := q
		eventually(t, q, func() error { _, err := pdb.Exec(q); return err })
	}
	r := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: rName, DatabaseVersion: "POSTGRES_17",
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
	if _, err := pdb.Exec("INSERT INTO orders VALUES (2, 'before the switchover')"); err != nil {
		t.Fatal(err)
	}
	seen := func(db *sql.DB, what string, id int, item string) {
		t.Helper()
		eventually(t, what, func() error {
			var got string
			if err := db.QueryRow("SELECT item FROM orders WHERE id = $1", id).Scan(&got); err != nil {
				return err
			}
			if got != item {
				return fmt.Errorf("order %d is %q, want %q", id, got, item)
			}
			return nil
		})
	}
	seen(rdb, "the primary's rows on its replica", 2, "before the switchover")
	// standby is whether a server is a hot standby, in recovery.
	standby := func(db *sql.DB, what string, want bool) {
		t.Helper()
		eventually(t, what, func() error {
			var in bool
			if err := db.QueryRow("SELECT pg_is_in_recovery()").Scan(&in); err != nil {
				return err
			}
			if in != want {
				return fmt.Errorf("pg_is_in_recovery() is %v, want %v", in, want)
			}
			return nil
		})
	}
	standby(rdb, "the replica is a standby", true)

	_, err := svc.Instances.Switchover(project, rName).Do()
	wantCode(t, "switchover of a replica not designated", err, 400, "not the DR replica")
	designate := func(primary, dr string) (*sqladmin.Operation, error) {
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
	standby(rdb, "the new primary is out of recovery", false)
	standby(pdb, "the old primary, rebuilt, is a standby", true)
	seen(rdb, "a row of before the switchover on the new primary", 2, "before the switchover")
	seen(pdb, "the same row on the rebuilt old primary", 2, "before the switchover")
	eventually(t, "a write on the new primary", func() error {
		_, err := rdb.Exec("INSERT INTO orders VALUES (3, 'on the new primary')")
		return err
	})
	if _, err := rdb.Exec("UPDATE orders SET item = 'changed on the new primary' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	seen(pdb, "a row written on the new primary, on the old one", 3, "on the new primary")
	seen(pdb, "a row changed on the new primary, on the old one", 1, "changed on the new primary")
	if _, err := pdb.Exec("INSERT INTO orders VALUES (100, 'on the old primary')"); err == nil {
		t.Error("the old primary, now a replica, took a write")
	}

	// Switched back, the original primary takes writes again.
	switchover(pName)
	roles(pName, rName)
	standby(pdb, "the original primary is out of recovery", false)
	standby(rdb, "the replica, rebuilt, is a standby again", true)
	eventually(t, "a write on the original primary", func() error {
		_, err := pdb.Exec("INSERT INTO orders VALUES (4, 'switched back')")
		return err
	})
	seen(rdb, "a row written after switching back, on the replica", 4, "switched back")
	seen(rdb, "the row of the first switchover, on the replica", 3, "on the new primary")
	if _, err := rdb.Exec("INSERT INTO orders VALUES (101, 'on the replica')"); err == nil {
		t.Error("the replica took a write after switching back")
	}
	// The primary's other read replicas stay its replicas, cascading
	// replicas of the new primary through it, and a cascading replica of
	// one of them stays theirs: each is rebuilt from its master and
	// streams the new primary's writes.
	oName, cName := fmt.Sprintf("swpg-o-%d", n), fmt.Sprintf("swpg-c-%d", n)
	o := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: oName, DatabaseVersion: "POSTGRES_17",
		MasterInstanceName: pName, InstanceType: "READ_REPLICA_INSTANCE", Region: "us-west1", Settings: plus()})
	cr := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: cName, DatabaseVersion: "POSTGRES_17",
		MasterInstanceName: oName, InstanceType: "READ_REPLICA_INSTANCE", Region: "us-west1", Settings: plus()})
	odb, cdb := e.root(t, h, o.ConnectionName), e.root(t, h, cr.ConnectionName)
	seen(cdb, "the primary's rows on the cascading replica", 4, "switched back")
	switchover(rName)
	pi, err := svc.Instances.Get(project, pName).Do()
	if err != nil {
		t.Fatal(err)
	}
	if pi.MasterInstanceName != project+":"+rName || strings.Join(pi.ReplicaNames, ",") != oName || pi.State != "RUNNABLE" ||
		pi.ReplicationCluster == nil || !pi.ReplicationCluster.DrReplica {
		t.Errorf("the old primary %s: master %q replicas %v state %s cluster %+v", pName, pi.MasterInstanceName, pi.ReplicaNames,
			pi.State, pi.ReplicationCluster)
	}
	for _, c := range []struct{ name, master string }{{oName, pName}, {cName, oName}} {
		in, err := svc.Instances.Get(project, c.name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if in.MasterInstanceName != project+":"+c.master || in.State != "RUNNABLE" || in.InstanceType != "READ_REPLICA_INSTANCE" {
			t.Errorf("the replica %s: master %q state %s type %s, want master %s RUNNABLE", c.name, in.MasterInstanceName, in.State,
				in.InstanceType, c.master)
		}
	}
	standby(odb, "the old primary's other replica is a standby", true)
	standby(cdb, "its cascading replica is a standby", true)
	eventually(t, "a write on the new primary", func() error {
		_, err := rdb.Exec("INSERT INTO orders VALUES (5, 'after a switchover with other replicas')")
		return err
	})
	seen(pdb, "the new primary's write on the old primary", 5, "after a switchover with other replicas")
	seen(odb, "the new primary's write on the old primary's other replica", 5, "after a switchover with other replicas")
	seen(cdb, "the new primary's write on the cascading replica", 5, "after a switchover with other replicas")
	seen(cdb, "a row of before the switchover on the cascading replica", 4, "switched back")
	if _, err := odb.Exec("INSERT INTO orders VALUES (102, 'on a cascading replica')"); err == nil {
		t.Error("the old primary's other replica took a write")
	}
}
