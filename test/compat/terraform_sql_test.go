//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"testing"
)

// TestTerraformSQLDatabaseAndUser (#1301): a google_sql_database and a
// google_sql_user on each instance `up` runs (the instance is not managed:
// it is named by its name) apply with the official hashicorp/google
// provider through `cloudburrow terraform`, which sets sql_custom_endpoint
// to the Cloud SQL Admin API's v1beta4 path. Each is read back through the
// official Go client and the server's own catalogue, the second plan is
// empty, a password change applies in place, and after destroy the
// database and the user are gone.
// covers: sql.databases.insert, sql.databases.get, sql.databases.delete, sql.users.insert, sql.users.list, sql.users.update, sql.users.delete, sql.operations.get
func TestTerraformSQLDatabaseAndUser(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	var engines []sqlAdminEngine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, pgEngine(t, h))
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, myEngine(t, h))
	}
	if len(engines) == 0 {
		t.Skip("neither Cloud SQL server is exported")
	}
	module := func(password string) string {
		var b string
		for _, e := range engines {
			host := ""
			if e.instance == "mysql" {
				host = `  host     = "%"` + "\n"
			}
			b += fmt.Sprintf(`
resource "google_sql_database" "%[1]s" {
  project  = %[2]q
  instance = %[1]q
  name     = "tf_orders"
}
resource "google_sql_user" "%[1]s" {
  project  = %[2]q
  instance = %[1]q
  name     = "tf_app"
  password = %[3]q
%[4]s}
`, e.instance, m.project, password, host)
		}
		return b
	}
	check := func(when, password string) {
		t.Helper()
		for _, e := range engines {
			if _, ok := e.databases(t)["tf_orders"]; !ok {
				t.Errorf("%s: %s does not list tf_orders", when, e.instance)
			}
			if _, err := svc.Databases.Get(m.project, e.instance, "tf_orders").Do(); err != nil {
				t.Errorf("%s: databases.get on %s: %v", when, e.instance, err)
			}
			if err := e.login("tf_app", password, "tf_orders"); err != nil {
				t.Errorf("%s: tf_app cannot use %s's tf_orders: %v", when, e.instance, err)
			}
		}
	}
	m.write(module("first-Pass1"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "first-Pass1")
	m.planClean()

	m.write(module("second-Pass2"))
	m.apply()
	check("after the password change", "second-Pass2")
	for _, e := range engines {
		if e.instance == "mysql" {
			if err := e.login("tf_app", "first-Pass1", "tf_orders"); err == nil {
				t.Error("after the password change the old MySQL password still logs in")
			}
		}
	}

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, e := range engines {
		_, err := svc.Databases.Get(m.project, e.instance, "tf_orders").Do()
		wantHTTP(t, "databases.get after destroy on "+e.instance, err, http.StatusNotFound)
		host := ""
		if e.instance == "mysql" {
			host = "%"
		}
		_, err = svc.Users.Get(m.project, e.instance, "tf_app").Host(host).Do()
		wantHTTP(t, "users.get after destroy on "+e.instance, err, http.StatusNotFound)
	}
}
