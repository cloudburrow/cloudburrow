//go:build compat

package compat

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLMySQL57 and TestCloudSQLMySQL56 (#1524): through the
// official Go client a MYSQL_5_7 (or MYSQL_5_6)
// instance is created where the cluster's node runs linux/amd64 images
// (natively, or through Rosetta or qemu-x86_64 in binfmt_misc), as its
// official image, mysql:5.7 (mysql:5.6), is linux/amd64 only; on a node
// that cannot, the insert is 501 naming why and the test skips. The
// instance reports databaseInstalledVersion MYSQL_5_7_44 (MYSQL_5_6_51);
// a database and a user made with databases.insert and users.insert are
// there, and through the official connector with the MySQL driver SELECT
// version() is 5.7.44 (5.6.51), as the user too, who connects again with
// the password users.update sets. instances.clone copies its table and
// its user, who signs in to the clone. Its upgradableDatabaseVersions is
// the next major version only, as Cloud SQL upgrades MySQL one major
// version at a time; an upgrade that skips one is 400, and instances.patch
// to the next one upgrades it, keeping the table and the user (#1524).
// Before that, the MySQL 8.0 steps run in 5.7's and 5.6's statements
// (CHANGE MASTER TO, START SLAVE, SHOW SLAVE STATUS): a read replica gets
// the primary's rows and (5.7) refuses writes (a replica of a MYSQL_5_6 replica
// is 400, Cloud SQL logging a replica's binary log on 5.7 and later only);
// with binaryLogEnabled a clone to a point in time has the rows written
// before it only; REGIONAL keeps every row across a failover; a MYSQL_5_7
// blue-green deployment's switchover gives the instance's name the green
// server with every row (MYSQL_5_6 is 400, as Cloud SQL documents 5.7, 8.0
// and 8.4); and demoteMaster, then demote and startExternalSync, make an
// instance a replica of the instance through a source representation
// instance.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.clone, sql.instances.delete, sql.instances.failover, sql.instances.promoteReplica, sql.instances.demoteMaster, sql.instances.demote, sql.projects.instances.startExternalSync, sql.projects.instances.getLatestRecoveryTime, sql.backupRuns.insert, sql.blueGreenDeployments.create, sql.blueGreenDeployments.get, sql.blueGreenDeployments.switchover, sql.blueGreenDeployments.delete, sql.databases.insert, sql.databases.get, sql.users.insert, sql.users.list, sql.users.update, sql.connect.get, sql.connect.generateEphemeral, sql.operations.get
func TestCloudSQLMySQL57(t *testing.T) {
	testLegacyMySQL(t, "MYSQL_5_7", "5.7.44", "MYSQL_8_0", "8.0.")
}

// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.clone, sql.instances.delete, sql.instances.failover, sql.instances.promoteReplica, sql.instances.demoteMaster, sql.instances.demote, sql.projects.instances.startExternalSync, sql.projects.instances.getLatestRecoveryTime, sql.backupRuns.insert, sql.blueGreenDeployments.create, sql.databases.insert, sql.databases.get, sql.users.insert, sql.users.list, sql.users.update, sql.connect.get, sql.connect.generateEphemeral, sql.operations.get
func TestCloudSQLMySQL56(t *testing.T) {
	testLegacyMySQL(t, "MYSQL_5_6", "5.6.51", "MYSQL_5_7", "5.7.44")
}

func testLegacyMySQL(t *testing.T, version, release, next, nextRelease string) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("%s-%d", strings.ReplaceAll(strings.ToLower(version), "_", ""), time.Now().UnixNano()%1e6)
	installed := version + "_" + strings.Split(release, ".")[2]
	in := &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: version, RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}
	op, err := svc.Instances.Insert(project, in).Do()
	if err != nil {
		wantHTTP(t, "insert "+version, err, http.StatusNotImplemented)
		if !strings.Contains(err.Error(), "linux/amd64 only") {
			t.Errorf("the 501 does not say why: %v", err)
		}
		t.Skipf("this cluster's node does not run linux/amd64 images: %v", err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("create %s: %+v", name, op.Error.Errors[0])
	}
	got, err := svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseVersion != version || got.DatabaseInstalledVersion != installed || got.State != "RUNNABLE" {
		t.Errorf("instance: version %s installed %s state %s", got.DatabaseVersion, got.DatabaseInstalledVersion, got.State)
	}
	dop, err := svc.Databases.Insert(project, name, &sqladmin.Database{Name: "app"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, dop)
	if d, err := svc.Databases.Get(project, name, "app").Do(); err != nil || d.Name != "app" {
		t.Errorf("databases.get app: %+v %v", d, err)
	}
	uop, err := svc.Users.Insert(project, name, &sqladmin.User{Name: "appuser", Password: "App-pass1", Host: "%"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOpDone(t, svc, project, uop)
	users, err := svc.Users.List(project, name).Do()
	found := false
	for _, u := range users.Items {
		found = found || u.Name == "appuser"
	}
	if err != nil || !found {
		t.Errorf("users.list: %v, appuser listed %v", err, found)
	}
	e := sqlEngine{mysql: true}
	var v string
	db := e.root(t, h, got.ConnectionName)
	eventually(t, "SELECT version() as root", func() error { return db.QueryRow("SELECT version()").Scan(&v) })
	if !strings.HasPrefix(v, release) {
		t.Errorf("SELECT version() = %q, want %s", v, release)
	}
	app := e.open(t, h, got.ConnectionName, "appuser", "App-pass1", "app")
	eventually(t, "a table as appuser", func() error {
		if _, err := app.Exec("CREATE TABLE IF NOT EXISTS t (id INT PRIMARY KEY)"); err != nil {
			return err
		}
		_, err := app.Exec("INSERT IGNORE INTO t VALUES (1)")
		return err
	})
	uop, err = svc.Users.Update(project, name, &sqladmin.User{Name: "appuser", Password: "App-pass2"}).Host("%").Name("appuser").Do()
	if err != nil {
		t.Fatalf("users.update: %v", err)
	}
	waitSQLOpDone(t, svc, project, uop)
	app2 := e.open(t, h, got.ConnectionName, "appuser", "App-pass2", "app")
	eventually(t, "appuser with its new password", func() error {
		var n int
		return app2.QueryRow("SELECT count(*) FROM t").Scan(&n)
	})

	// Read replicas, point-in-time recovery, REGIONAL, blue-green and
	// demotion, in 5.7's and 5.6's statements.
	testLegacyMySQLReplication(t, h, svc, project, name, version, e)

	// A clone has the table and the user.
	clone := name + "-c"
	cop, err := svc.Instances.Clone(project, name, &sqladmin.InstancesCloneRequest{
		CloneContext: &sqladmin.CloneContext{DestinationInstanceName: clone}}).Do()
	if err != nil {
		t.Fatalf("instances.clone: %v", err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, clone).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if cop = waitSQLOpDone(t, svc, project, cop); cop.Error != nil {
		t.Fatalf("clone %s: %+v", name, cop.Error.Errors[0])
	}
	cl, err := svc.Instances.Get(project, clone).Do()
	if err != nil || cl.DatabaseVersion != version || cl.State != "RUNNABLE" {
		t.Fatalf("the clone: %+v %v", cl, err)
	}
	capp := e.open(t, h, cl.ConnectionName, "appuser", "App-pass2", "app")
	eventually(t, "the clone's table as appuser", func() error {
		var n int
		if err := capp.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("%d rows, want 1", n)
		}
		return nil
	})
	if d, err := svc.Instances.Delete(project, clone).Do(); err == nil {
		waitSQLOpDone(t, svc, project, d)
	}

	// The upgrade goes to the next major version only.
	got, err = svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.UpgradableDatabaseVersions) != 1 || got.UpgradableDatabaseVersions[0].MajorVersion != next {
		t.Errorf("upgradableDatabaseVersions = %+v, want %s only", got.UpgradableDatabaseVersions, next)
	}
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: "MYSQL_8_4"}).Do()
	wantHTTP(t, "upgrading "+version+" to MYSQL_8_4", err, http.StatusBadRequest)
	uop, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: next}).Do()
	if err != nil {
		t.Fatalf("upgrading %s to %s: %v", version, next, err)
	}
	if uop = waitSQLOpDone(t, svc, project, uop); uop.Error != nil {
		t.Fatalf("upgrade to %s: %+v", next, uop.Error.Errors[0])
	}
	if got, err = svc.Instances.Get(project, name).Do(); err != nil || got.DatabaseVersion != next || got.State != "RUNNABLE" {
		t.Fatalf("after the upgrade: %+v %v", got, err)
	}
	up := e.open(t, h, got.ConnectionName, "appuser", "App-pass2", "app")
	eventually(t, "the upgraded server's table as appuser", func() error {
		var v string
		var n int
		if err := up.QueryRow("SELECT version(), (SELECT count(*) FROM t)").Scan(&v, &n); err != nil {
			return err
		}
		if !strings.HasPrefix(v, nextRelease) || n != 1 {
			return fmt.Errorf("version %s with %d rows, want %s with 1", v, n, nextRelease)
		}
		return nil
	})
}

// testLegacyMySQLReplication is TestCloudSQLMySQL57's and 56's read
// replica, point-in-time recovery, REGIONAL, blue-green and demotion of
// instance name, whose app.t has row 1 and whose appuser's password is
// App-pass2. It leaves name a standalone ZONAL instance with those.
func testLegacyMySQLReplication(t *testing.T, h *Harness, svc *sqladmin.Service, project, name, version string, e sqlEngine) {
	tier := "db-custom-1-3840"
	conn := func(n string) string { return project + ":us-central1:" + n }
	done := func(what string, op *sqladmin.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("%s: %+v", what, op.Error.Errors[0])
		}
	}
	del := func(n string) {
		if d, err := svc.Instances.Delete(project, n).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	}
	rows := func(db *sql.DB, what string, want ...int) {
		t.Helper()
		eventually(t, what, func() error {
			r, err := db.Query("SELECT id FROM app.t ORDER BY id")
			if err != nil {
				return err
			}
			defer r.Close()
			var got []int
			for r.Next() {
				var id int
				if err := r.Scan(&id); err != nil {
					return err
				}
				got = append(got, id)
			}
			if !reflect.DeepEqual(got, want) {
				return fmt.Errorf("rows %v, want %v", got, want)
			}
			return nil
		})
	}
	write := func(db *sql.DB, id int) {
		t.Helper()
		eventually(t, fmt.Sprintf("INSERT %d", id), func() error {
			_, err := db.Exec("INSERT IGNORE INTO app.t VALUES (?)", id)
			return err
		})
	}
	root := e.root(t, h, conn(name))

	// A read replica has the primary's rows, gets new ones and refuses
	// writes.
	replica := name + "-r"
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: replica, MasterInstanceName: name,
		Settings: &sqladmin.Settings{Tier: tier}}).Do()
	t.Cleanup(func() { del(replica) })
	done("a read replica of "+version, op, err)
	if r, err := svc.Instances.Get(project, replica).Do(); err != nil || r.DatabaseVersion != version || r.InstanceType != "READ_REPLICA_INSTANCE" {
		t.Fatalf("the replica: %+v %v", r, err)
	}
	rdb := e.root(t, h, conn(replica))
	rows(rdb, "the primary's row on the replica", 1)
	write(root, 2)
	rows(rdb, "a new row on the replica", 1, 2)
	// 5.7's super_read_only refuses root's write too; 5.6 has only
	// read_only, which root's SUPER passes.
	if _, err := rdb.Exec("INSERT INTO app.t VALUES (99)"); err == nil && version == "MYSQL_5_7" {
		t.Error("the replica took a write")
	}
	if version == "MYSQL_5_6" {
		_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-rr", MasterInstanceName: replica,
			Settings: &sqladmin.Settings{Tier: tier}}).Do()
		wantHTTP(t, "a replica of a MYSQL_5_6 replica", err, http.StatusBadRequest)
	}
	del(replica) // a blue-green deployment's blue instance has no replicas

	// Point-in-time recovery from the binary log.
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true, StartTime: "03:00"}}}).Do()
	done("binaryLogEnabled on "+version, op, err)
	// An on-demand backup is the first base the recovery starts from.
	op, err = svc.BackupRuns.Insert(project, name, &sqladmin.BackupRun{Description: "pitr base"}).Do()
	done("an on-demand backup of "+version, op, err)
	waitRecoverable(t, svc, project, name)
	write(root, 3)
	var at string
	eventually(t, "the server's clock", func() error {
		return root.QueryRow("SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%dT%H:%i:%s.%fZ')").Scan(&at)
	})
	time.Sleep(2 * time.Second) // 5.7's and 5.6's binary log times are in seconds
	write(root, 4)
	pit := name + "-p"
	op, err = svc.Instances.Clone(project, name, &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: pit, PointInTime: at}}).Do()
	t.Cleanup(func() { del(pit) })
	done("a clone of "+version+" to "+at, op, err)
	rows(e.root(t, h, conn(pit)), "the point-in-time clone's rows", 1, 2, 3)
	del(pit)
	rows(root, "the source's rows", 1, 2, 3, 4)

	// High availability: REGIONAL, then a failover, then ZONAL.
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{AvailabilityType: "REGIONAL"}}).Do()
	done("REGIONAL "+version, op, err)
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil || in.Settings.AvailabilityType != "REGIONAL" || in.SecondaryGceZone == "" {
		t.Fatalf("the REGIONAL instance: %+v %v", in, err)
	}
	write(root, 5)
	op, err = svc.Instances.Failover(project, name, &sqladmin.InstancesFailoverRequest{
		FailoverContext: &sqladmin.FailoverContext{SettingsVersion: in.Settings.SettingsVersion}}).Do()
	done("a failover of "+version, op, err)
	root = e.root(t, h, conn(name))
	rows(root, "the rows after the failover", 1, 2, 3, 4, 5)
	write(root, 6)
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{AvailabilityType: "ZONAL"}}).Do()
	done("ZONAL "+version, op, err)
	rows(root, "the rows on the ZONAL instance", 1, 2, 3, 4, 5, 6)

	// A blue-green deployment, on MySQL 5.7 and later.
	loc := "projects/" + project + "/locations/us-central1"
	bg := &sqladmin.BlueGreenDeployment{SourceInstance: "projects/" + project + "/instances/" + name}
	if version == "MYSQL_5_6" {
		_, err = svc.BlueGreenDeployments.Create(loc, bg).BlueGreenDeploymentId("bg-56").Do()
		wantHTTP(t, "a MYSQL_5_6 blue-green deployment", err, http.StatusBadRequest)
	} else {
		op, err = svc.BlueGreenDeployments.Create(loc, bg).BlueGreenDeploymentId("bg-57").Do()
		done("a "+version+" blue-green deployment", op, err)
		dn := loc + "/blueGreenDeployments/bg-57"
		d, err := svc.BlueGreenDeployments.Get(dn).Do()
		if err != nil {
			t.Fatal(err)
		}
		green := strings.TrimPrefix(d.SwitchoverTargetInstance, "projects/"+project+"/instances/")
		t.Cleanup(func() { del(green) })
		eventuallyTrue(t, "the deployment is SWITCHOVER_READY", func() bool {
			d, err = svc.BlueGreenDeployments.Get(dn).Do()
			return err == nil && d.State == "SWITCHOVER_READY"
		})
		write(root, 7)
		rows(e.root(t, h, conn(green)), "the blue rows on the green instance", 1, 2, 3, 4, 5, 6, 7)
		op, err = svc.BlueGreenDeployments.Switchover(dn, &sqladmin.SwitchoverBlueGreenDeploymentRequest{}).Do()
		done("the switchover", op, err)
		if bi, err := svc.Instances.Get(project, name).Do(); err != nil || bi.MasterInstanceName != "" || bi.DatabaseVersion != version {
			t.Fatalf("the blue name after the switchover: %+v %v", bi, err)
		}
		root = e.root(t, h, conn(name))
		write(root, 8)
		rows(root, "the rows through the blue name after the switchover", 1, 2, 3, 4, 5, 6, 7, 8)
		op, err = svc.BlueGreenDeployments.Delete(dn).DeleteOldSource(true).Do()
		done("delete the deployment", op, err)
	}

	// demoteMaster, then demote and startExternalSync, make another
	// instance a replica of this one through a source representation
	// instance.
	eventually(t, "the migration user", func() error {
		_, err := root.Exec("GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, SHOW VIEW, RELOAD, TRIGGER, EVENT, EXECUTE, PROCESS " +
			"ON *.* TO 'migrator'@'%' IDENTIFIED BY 'Repl-pass1'")
		if err != nil && strings.Contains(err.Error(), "1064") { // the syntax 5.7 deprecates
			if _, err = root.Exec("CREATE USER IF NOT EXISTS 'migrator'@'%' IDENTIFIED BY 'Repl-pass1'"); err == nil {
				_, err = root.Exec("GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, SHOW VIEW, RELOAD, TRIGGER, EVENT, EXECUTE, PROCESS ON *.* TO 'migrator'@'%'")
			}
		}
		return err
	})
	rep, dem := name+"-src", name+"-d"
	op, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: rep, DatabaseVersion: version,
		InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
		OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "sqlinst-" + name + ":3306", Username: "migrator", Password: "Repl-pass1"}}).Do()
	t.Cleanup(func() { del(rep) })
	done("the source representation instance", op, err)
	dd := createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: dem, DatabaseVersion: version,
		RootPassword: "Root-pass1", Region: "us-central1", Settings: &sqladmin.Settings{Tier: tier}})
	op, err = svc.Instances.Demote(project, dem, &sqladmin.InstancesDemoteRequest{
		DemoteContext: &sqladmin.DemoteContext{SourceRepresentativeInstanceName: rep}}).Do()
	done("demote of a "+version+" instance", op, err)
	op, err = svc.Projects.Instances.StartExternalSync(project, dem, &sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do()
	done("startExternalSync after demote", op, err)
	ddb := e.root(t, h, dd.ConnectionName)
	last := []int{1, 2, 3, 4, 5, 6}
	if version != "MYSQL_5_6" {
		last = append(last, 7, 8)
	}
	rows(ddb, "the source's rows after demote and startExternalSync", last...)
	write(root, 9)
	rows(ddb, "a row written after the sync", append(last, 9)...)
	op, err = svc.Instances.PromoteReplica(project, dem).Do()
	done("promoteReplica", op, err)
	// Promoted, it has every GTID it has from the source and none of its
	// own, so demoteMaster's GTID consistency check passes.
	op, err = svc.Instances.DemoteMaster(project, dem, &sqladmin.InstancesDemoteMasterRequest{DemoteMasterContext: &sqladmin.DemoteMasterContext{
		MasterInstanceName: rep, ReplicaConfiguration: &sqladmin.DemoteMasterConfiguration{MysqlReplicaConfiguration: &sqladmin.DemoteMasterMySqlReplicaConfiguration{
			Username: "migrator", Password: "Repl-pass1"}}}}).Do()
	done("demoteMaster of a "+version+" instance", op, err)
	ddb = e.root(t, h, dd.ConnectionName)
	write(root, 10)
	rows(ddb, "a row written after demoteMaster", append(last, 9, 10)...)
	del(dem)
	del(rep)
	// The later steps expect row 1 alone.
	eventually(t, "back to row 1", func() error { _, err := root.Exec("DELETE FROM app.t WHERE id > 1"); return err })
}
