//go:build compat

package compat

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLReadReplicas (#1330): through the official Go client, a
// primary is created on each engine, and a read replica of it with
// masterInstanceName and instanceType READ_REPLICA_INSTANCE. Through the
// official connector a row written on the primary is read on the replica,
// a write on the replica is refused by its server, the primary lists the
// replica in replicaNames and refuses to be deleted while it has it;
// stopReplica and startReplica pause and resume replication, and
// promoteReplica makes the replica a primary that takes writes.
// covers: sql.instances.insert, sql.instances.get, sql.instances.promoteReplica, sql.instances.stopReplica, sql.instances.startReplica, sql.instances.delete, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLReadReplicas(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	type engine struct {
		version  string
		dsn      func(drv, conn string) string
		register func(string, ...cloudsqlconn.Option) (func() error, error)
		create   string
	}
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var engines []engine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, engine{version: "POSTGRES_17", create: "CREATE TABLE t (v text)",
			dsn: func(_, conn string) string {
				return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
			},
			register: func(n string, o ...cloudsqlconn.Option) (func() error, error) { return pgxv5.RegisterDriver(n, o...) }})
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, engine{version: "MYSQL_8_4", create: "CREATE TABLE app.t (v varchar(20))",
			dsn: func(drv, conn string) string { return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn) },
			register: func(n string, o ...cloudsqlconn.Option) (func() error, error) {
				return cloudsqlmysql.RegisterDriver(n, o...)
			}})
	}
	for _, e := range engines {
		t.Run(e.version, func(t *testing.T) {
			h := h.For(t)
			short := strings.ToLower(strings.SplitN(e.version, "_", 2)[0])
			primary, replica := short+"-p-"+suffix, short+"-r-"+suffix
			table := "t"
			if strings.HasPrefix(e.version, "MYSQL") {
				table = "app.t"
			}
			del := func(n string) {
				if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
			}
			defer del(primary)
			defer del(replica)
			op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: primary, DatabaseVersion: e.version,
				RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			p, err := svc.Instances.Get(project, primary).Do()
			if err != nil {
				t.Fatal(err)
			}
			conn := func(connName string) *sql.DB {
				t.Helper()
				drv := "cloudsql-rep-" + suffix + fmt.Sprint(time.Now().UnixNano())
				cleanup, err := e.register(drv, connectorOptions(h)...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cleanup() })
				db, err := sql.Open(drv, e.dsn(drv, connName))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			pdb := conn(p.ConnectionName)
			if strings.HasPrefix(e.version, "MYSQL") {
				if _, err := pdb.Exec("CREATE DATABASE app"); err != nil {
					t.Fatal(err)
				}
			}
			for _, q := range []string{e.create, "INSERT INTO " + table + " VALUES ('before')"} {
				if _, err := pdb.Exec(q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}

			op, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: replica, MasterInstanceName: primary,
				InstanceType: "READ_REPLICA_INSTANCE", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			r, err := svc.Instances.Get(project, replica).Do()
			if err != nil {
				t.Fatal(err)
			}
			if r.InstanceType != "READ_REPLICA_INSTANCE" || r.MasterInstanceName != project+":"+primary ||
				r.DatabaseVersion != e.version || r.State != "RUNNABLE" {
				t.Errorf("replica: %+v", r)
			}
			if p, err = svc.Instances.Get(project, primary).Do(); err != nil || len(p.ReplicaNames) != 1 || p.ReplicaNames[0] != replica {
				t.Errorf("primary's replicaNames: %v (%v)", p.ReplicaNames, err)
			}
			if _, err := svc.Instances.Delete(project, primary).Do(); err == nil || !strings.Contains(err.Error(), "replica") {
				t.Errorf("deleting a primary with a replica: %v", err)
			}

			rdb := conn(r.ConnectionName)
			read := func(want string) {
				t.Helper()
				deadline := time.Now().Add(time.Minute)
				for {
					var n int
					err := rdb.QueryRow("SELECT count(*) FROM " + table + " WHERE v = '" + want + "'").Scan(&n)
					if err == nil && n == 1 {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("the replica does not have %q: %d %v", want, n, err)
					}
					time.Sleep(500 * time.Millisecond)
				}
			}
			read("before")
			if _, err := pdb.Exec("INSERT INTO " + table + " VALUES ('after')"); err != nil {
				t.Fatal(err)
			}
			read("after")
			if _, err := rdb.Exec("INSERT INTO " + table + " VALUES ('on-replica')"); err == nil ||
				!strings.Contains(strings.ToLower(err.Error()), "read") {
				t.Errorf("a write on the replica: %v", err)
			}

			op, err = svc.Instances.StopReplica(project, replica).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			if r, _ = svc.Instances.Get(project, replica).Do(); r == nil || r.Settings.DatabaseReplicationEnabled {
				t.Errorf("after stopReplica: %+v", r)
			}
			op, err = svc.Instances.StartReplica(project, replica).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)

			op, err = svc.Instances.PromoteReplica(project, replica).Do()
			if err != nil {
				t.Fatal(err)
			}
			waitSQLOp(t, svc, project, op)
			if r, err = svc.Instances.Get(project, replica).Do(); err != nil || r.InstanceType != "CLOUD_SQL_INSTANCE" || r.MasterInstanceName != "" {
				t.Errorf("promoted: %+v %v", r, err)
			}
			deadline := time.Now().Add(time.Minute)
			for {
				_, err := rdb.Exec("INSERT INTO " + table + " VALUES ('promoted')")
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("a write on the promoted replica: %v", err)
				}
				time.Sleep(500 * time.Millisecond)
			}
			read("promoted")
			if p, _ = svc.Instances.Get(project, primary).Do(); p != nil && len(p.ReplicaNames) != 0 {
				t.Errorf("after promotion the primary still lists %v", p.ReplicaNames)
			}
		})
	}
}

// TestCloudSQLReplicaOfUpsServer (#1330): the servers `up` runs can be a
// replica's primary: a replica of `postgres` or `mysql` becomes RUNNABLE,
// is listed in the primary's replicaNames, and (PostgreSQL, whose
// superuser the test knows) serves the primary's data read-only through the
// connector; deleting it leaves the primary with none.
// covers: sql.instances.insert, sql.instances.get, sql.instances.delete
func TestCloudSQLReplicaOfUpsServer(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	var ups []string
	if os.Getenv(EnvCloudSQL) != "" {
		ups = append(ups, "postgres")
	}
	if os.Getenv(EnvMySQL) != "" {
		ups = append(ups, "mysql")
	}
	for _, up := range ups {
		t.Run(up, func(t *testing.T) {
			h := h.For(t)
			name := up + "-upr-" + suffix
			op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, MasterInstanceName: up}).Do()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
					waitSQLOp(t, svc, project, d)
				}
				if p, err := svc.Instances.Get(project, up).Do(); err != nil || len(p.ReplicaNames) != 0 {
					t.Errorf("after the replica's delete %s lists %v (%v)", up, p.ReplicaNames, err)
				}
			}()
			waitSQLOp(t, svc, project, op)
			r, err := svc.Instances.Get(project, name).Do()
			if err != nil || r.State != "RUNNABLE" || r.InstanceType != "READ_REPLICA_INSTANCE" || r.MasterInstanceName != project+":"+up {
				t.Fatalf("replica: %+v %v", r, err)
			}
			if p, err := svc.Instances.Get(project, up).Do(); err != nil || len(p.ReplicaNames) != 1 || p.ReplicaNames[0] != name {
				t.Errorf("%s's replicaNames: %v (%v)", up, p.ReplicaNames, err)
			}
			if up != "postgres" {
				return
			}
			drv := "cloudsql-upr-" + suffix
			cleanup, err := pgxv5.RegisterDriver(drv, connectorOptions(h)...)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			db, err := sql.Open(drv, fmt.Sprintf("host=%s user=cloudburrow dbname=cloudburrow sslmode=disable", r.ConnectionName))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var inRecovery bool
			if err := db.QueryRow("SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil || !inRecovery {
				t.Errorf("the replica of %s is not a standby: %v %v", up, inRecovery, err)
			}
		})
	}
}

// TestGcloudSQLReadReplica (#1330): `gcloud sql instances create
// --master-instance-name` makes a read replica, which describe shows as
// such and the primary lists; `gcloud sql instances promote-replica`
// promotes it.
// covers: sql.instances.insert, sql.instances.get, sql.instances.promoteReplica, sql.instances.delete
func TestGcloudSQLReadReplica(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano() % 1e6
	primary, replica := fmt.Sprintf("gcloud-pgp-%d", n), fmt.Sprintf("gcloud-pgr-%d", n)
	g.must("sql", "instances", "create", primary, "--project", g.project, "--database-version=POSTGRES_17",
		"--tier=db-custom-1-3840", "--region=us-central1", "--no-backup", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", primary, "--project", g.project, "--quiet")
	g.must("sql", "instances", "create", replica, "--project", g.project, "--master-instance-name="+primary,
		"--tier=db-custom-1-3840", "--region=us-central1", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", replica, "--project", g.project, "--quiet")
	out := g.must("sql", "instances", "describe", replica, "--project", g.project, "--format=value(state,instanceType,masterInstanceName)")
	if !strings.Contains(out, "RUNNABLE") || !strings.Contains(out, "READ_REPLICA_INSTANCE") || !strings.Contains(out, primary) {
		t.Errorf("describe of the replica: %s", out)
	}
	if out := g.must("sql", "instances", "describe", primary, "--project", g.project, "--format=value(replicaNames)"); !strings.Contains(out, replica) {
		t.Errorf("the primary's replicaNames: %s", out)
	}
	g.must("sql", "instances", "promote-replica", replica, "--project", g.project, "--quiet")
	if out := g.must("sql", "instances", "describe", replica, "--project", g.project, "--format=value(instanceType)"); !strings.Contains(out, "CLOUD_SQL_INSTANCE") {
		t.Errorf("after promote-replica: %s", out)
	}
}
