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

// TestCloudSQLMySQL57 (#1524): through the official Go client a MYSQL_5_7
// instance is created where the cluster's node runs linux/amd64 images
// (natively, or through Rosetta or qemu-x86_64 in binfmt_misc), as its
// official image, mysql:5.7, is linux/amd64 only; on a node that cannot,
// the insert is 501 naming why and the test skips. The instance reports
// databaseInstalledVersion MYSQL_5_7_44; a database and a user made with
// databases.insert and users.insert are there, and through the official
// connector with the MySQL driver SELECT version() is 5.7.44, as the
// user too. A read replica, availabilityType REGIONAL, binaryLogEnabled
// and a major version upgrade are 501.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.databases.insert, sql.databases.get, sql.users.insert, sql.users.list, sql.connect.get, sql.connect.generateEphemeral, sql.operations.get
func TestCloudSQLMySQL57(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("mysql57-%d", time.Now().UnixNano()%1e6)
	in := &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_5_7", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}
	op, err := svc.Instances.Insert(project, in).Do()
	if err != nil {
		wantHTTP(t, "insert MYSQL_5_7", err, http.StatusNotImplemented)
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
	if got.DatabaseVersion != "MYSQL_5_7" || got.DatabaseInstalledVersion != "MYSQL_5_7_44" || got.State != "RUNNABLE" {
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
	if !strings.HasPrefix(v, "5.7.44") {
		t.Errorf("SELECT version() = %q, want 5.7.44", v)
	}
	app := e.open(t, h, got.ConnectionName, "appuser", "App-pass1", "app")
	eventually(t, "a table as appuser", func() error {
		if _, err := app.Exec("CREATE TABLE IF NOT EXISTS t (id INT PRIMARY KEY)"); err != nil {
			return err
		}
		_, err := app.Exec("INSERT IGNORE INTO t VALUES (1)")
		return err
	})

	// What CloudBurrow builds on MySQL 8.0's statements is 501.
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-r", MasterInstanceName: name,
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	wantHTTP(t, "a read replica of MYSQL_5_7", err, http.StatusNotImplemented)
	_, err = svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-ha", DatabaseVersion: "MYSQL_5_7",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: "REGIONAL"}}).Do()
	wantHTTP(t, "a REGIONAL MYSQL_5_7 instance", err, http.StatusNotImplemented)
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true}}}).Do()
	wantHTTP(t, "binaryLogEnabled on MYSQL_5_7", err, http.StatusNotImplemented)
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{DatabaseVersion: "MYSQL_8_0"}).Do()
	wantHTTP(t, "upgrading MYSQL_5_7", err, http.StatusNotImplemented)
}
