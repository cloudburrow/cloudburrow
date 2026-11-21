//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
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
// to the next one upgrades it, keeping the table and the user (#1524). A
// read replica, availabilityType REGIONAL and binaryLogEnabled are 501.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.clone, sql.instances.delete, sql.databases.insert, sql.databases.get, sql.users.insert, sql.users.list, sql.users.update, sql.connect.get, sql.connect.generateEphemeral, sql.operations.get
func TestCloudSQLMySQL57(t *testing.T) {
	testLegacyMySQL(t, "MYSQL_5_7", "5.7.44", "MYSQL_8_0", "8.0.")
}

// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.clone, sql.instances.delete, sql.databases.insert, sql.databases.get, sql.users.insert, sql.users.list, sql.users.update, sql.connect.get, sql.connect.generateEphemeral, sql.operations.get
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

	// What CloudBurrow builds on MySQL 8.0's statements is 501.
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-r", MasterInstanceName: name,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	wantHTTP(t, "a read replica of "+version, err, http.StatusNotImplemented)
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-ha", DatabaseVersion: version,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: "REGIONAL"}}).Do()
	wantHTTP(t, "a REGIONAL "+version+" instance", err, http.StatusNotImplemented)
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true}}}).Do()
	wantHTTP(t, "binaryLogEnabled on "+version, err, http.StatusNotImplemented)

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
