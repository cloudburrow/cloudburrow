//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGcloudSQLHighAvailability (#1408): `gcloud sql instances create
// --availability-type=regional` makes a PostgreSQL instance with a standby
// (describe shows REGIONAL and a secondaryGceZone), and `gcloud sql
// instances failover` fails it over, swapping gceZone and
// secondaryGceZone, through the configuration `cloudburrow gcloud-setup`
// writes, with egress to Google blocked.
// covers: sql.instances.insert, sql.instances.get, sql.instances.failover, sql.instances.delete, sql.operations.get
func TestGcloudSQLHighAvailability(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	g := newGcloudSession(t, h)
	name := fmt.Sprintf("gcloud-ha-%d", time.Now().UnixNano()%1e6)
	g.must("sql", "instances", "create", name, "--project", g.project, "--database-version=POSTGRES_17",
		"--tier=db-custom-1-3840", "--region=us-central1", "--availability-type=regional", "--quiet")
	defer g.run(nil, "sql", "instances", "delete", name, "--project", g.project, "--quiet")
	desc := func() (avail, zone, secondary string) {
		t.Helper()
		out := g.must("sql", "instances", "describe", name, "--project", g.project,
			"--format=value(settings.availabilityType,gceZone,secondaryGceZone)")
		f := strings.Fields(out)
		if len(f) != 3 {
			t.Fatalf("describe: %q, want availabilityType, gceZone and secondaryGceZone", out)
		}
		return f[0], f[1], f[2]
	}
	avail, zone, secondary := desc()
	if avail != "REGIONAL" || zone == secondary {
		t.Fatalf("describe: %s %s %s", avail, zone, secondary)
	}
	g.must("sql", "instances", "failover", name, "--project", g.project, "--quiet")
	avail, zone2, secondary2 := desc()
	if avail != "REGIONAL" || zone2 != secondary || secondary2 != zone {
		t.Errorf("after failover: %s %s %s, want the zones of %s %s swapped", avail, zone2, secondary2, zone, secondary)
	}
	g.must("sql", "instances", "delete", name, "--project", g.project, "--quiet")
}

// TestTerraformSQLHighAvailability (#1408): a google_sql_database_instance
// with availability_type = "REGIONAL" (and the backups and point-in-time
// recovery Cloud SQL requires of a PostgreSQL HA instance) applies through
// `cloudburrow terraform` with the official hashicorp/google provider, its
// plan is then empty; changing it to ZONAL and back to REGIONAL updates it
// in place (its createTime stays), each with a clean plan after; destroy
// removes it.
// covers: sql.instances.insert, sql.instances.get, sql.instances.patch, sql.instances.delete, sql.operations.get
func TestTerraformSQLHighAvailability(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-ha-%d", time.Now().UnixNano()%1e6)
	module := func(avail string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "main" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  deletion_protection = false
  settings {
    tier              = "db-custom-1-3840"
    availability_type = %[3]q
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }
  }
}
`, m.project, name, avail)
	}
	m.write(module("REGIONAL"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := svc.Instances.Get(m.project, name).Do()
	if err != nil || in.Settings.AvailabilityType != "REGIONAL" || in.SecondaryGceZone == "" {
		t.Fatalf("after apply: %+v %v", in, err)
	}
	m.planClean()
	for _, avail := range []string{"ZONAL", "REGIONAL"} {
		m.write(module(avail))
		m.apply()
		got, err := svc.Instances.Get(m.project, name).Do()
		if err != nil || got.Settings.AvailabilityType != avail || (got.SecondaryGceZone != "") != (avail == "REGIONAL") ||
			got.CreateTime != in.CreateTime || got.State != "RUNNABLE" {
			t.Errorf("after the change to %s: %+v %v", avail, got, err)
		}
		m.planClean()
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = svc.Instances.Get(m.project, name).Do()
	wantHTTP(t, "instances.get after destroy", err, 404)
}
