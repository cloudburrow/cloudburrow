//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/backupdr/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestTerraformSQLDataSources (#1304): on the Cloud SQL instance `up` runs
// (postgres, or mysql where only it is served), a google_sql_database
// applies beside the data sources google_sql_databases,
// google_sql_database_instances, google_sql_tiers, google_sql_ca_certs and
// google_sql_backup_run (most_recent, of a backup the official client
// takes first), each compared with what Google's generated sqladmin client
// reads; the plan is clean; a second database applies and
// google_sql_databases lists it; the plan is clean again; destroy deletes
// both databases.
// covers: sql.databases.list, sql.instances.list, sql.tiers.list, sql.instances.listServerCas, sql.backupRuns.list
func TestTerraformSQLDataSources(t *testing.T) {
	h := New(t)
	instance := sqlDataSourceInstance(t)
	svc := sqlAdminService(t, h) // skips without CLOUDBURROW_TEST_SQLADMIN
	m := newTFModule(t)
	p := m.project
	op, err := svc.BackupRuns.Insert(p, instance, &sqladmin.BackupRun{Description: "tf-data"}).Do()
	if err != nil {
		t.Fatalf("backupRuns.insert: %v", err)
	}
	waitSQLOp(t, svc, p, op)

	module := func(second bool) string {
		extra := ""
		if second {
			extra = fmt.Sprintf(`
resource "google_sql_database" "b" {
  project  = %q
  instance = %q
  name     = "tf_ds_two"
}
`, p, instance)
		}
		after := "google_sql_database.a"
		if second {
			after += ", google_sql_database.b"
		}
		return fmt.Sprintf(`
resource "google_sql_database" "a" {
  project  = %[1]q
  instance = %[2]q
  name     = "tf_ds_one"
}
%[3]s
data "google_sql_databases" "d" {
  project    = %[1]q
  instance   = %[2]q
  depends_on = [%[4]s]
}
data "google_sql_database_instances" "i" {
  project = %[1]q
}
data "google_sql_tiers" "t" {
  project = %[1]q
}
data "google_sql_ca_certs" "c" {
  project  = %[1]q
  instance = %[2]q
}
data "google_sql_backup_run" "b" {
  project     = %[1]q
  instance    = %[2]q
  most_recent = true
}
output "outputs" {
  value = {
    databases = sort([for d in data.google_sql_databases.d.databases : d.name])
    instances = sort([for i in data.google_sql_database_instances.i.instances : "${i.name}|${i.database_version}|${i.region}"])
    tiers     = sort([for t in data.google_sql_tiers.t.tiers : t.tier])
    certs     = sort([for c in data.google_sql_ca_certs.c.certs : c.sha1_fingerprint])
    active    = data.google_sql_ca_certs.c.active_version
    backup    = "${data.google_sql_backup_run.b.status}|${data.google_sql_backup_run.b.start_time}|${data.google_sql_backup_run.b.location}"
  }
}
`, p, instance, extra, after)
	}
	check := func(when string, dbs int) {
		t.Helper()
		var got struct {
			Databases, Instances, Tiers, Certs []string
			Active, Backup                     string
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		var want struct {
			Databases, Instances, Tiers, Certs []string
			Active, Backup                     string
		}
		dl, err := svc.Databases.List(p, instance).Do()
		if err != nil {
			t.Fatalf("%s: databases.list: %v", when, err)
		}
		for _, d := range dl.Items {
			want.Databases = append(want.Databases, d.Name)
		}
		il, err := svc.Instances.List(p).Do()
		if err != nil {
			t.Fatalf("%s: instances.list: %v", when, err)
		}
		for _, i := range il.Items {
			want.Instances = append(want.Instances, i.Name+"|"+i.DatabaseVersion+"|"+i.Region)
		}
		tl, err := svc.Tiers.List(p).Do()
		if err != nil {
			t.Fatalf("%s: tiers.list: %v", when, err)
		}
		for _, tier := range tl.Items {
			want.Tiers = append(want.Tiers, tier.Tier)
		}
		cas, err := svc.Instances.ListServerCas(p, instance).Do()
		if err != nil {
			t.Fatalf("%s: instances.listServerCas: %v", when, err)
		}
		for _, c := range cas.Certs {
			want.Certs = append(want.Certs, c.Sha1Fingerprint)
		}
		want.Active = cas.ActiveVersion
		br, err := svc.BackupRuns.List(p, instance).MaxResults(1).Do()
		if err != nil || len(br.Items) == 0 {
			t.Fatalf("%s: backupRuns.list: %v %+v", when, err, br)
		}
		want.Backup = br.Items[0].Status + "|" + br.Items[0].StartTime + "|" + br.Items[0].Location
		for _, l := range [][]string{want.Databases, want.Instances, want.Tiers, want.Certs} {
			sort.Strings(l)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: the data sources read\n%+v\nthe official client\n%+v", when, got, want)
		}
		n := 0
		for _, d := range got.Databases {
			if strings.HasPrefix(d, "tf_ds_") {
				n++
			}
		}
		if n != dbs || len(got.Instances) == 0 || len(got.Tiers) == 0 || len(got.Certs) == 0 || !strings.HasPrefix(got.Backup, "SUCCESSFUL|") {
			t.Errorf("%s: %d of this test's databases (want %d), instances %v, tiers %v, certs %v, backup %q", when, n, dbs, got.Instances, got.Tiers, got.Certs, got.Backup)
		}
	}

	m.write(module(false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 1)
	m.planClean()

	m.write(module(true))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "1 added, 0 changed, 0 destroyed") {
		t.Errorf("the second database was not added alone:\n%s", lastLines(out, 20))
	}
	check("after the change", 2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, db := range []string{"tf_ds_one", "tf_ds_two"} {
		if _, err := svc.Databases.Get(p, instance, db).Do(); httpCode(err) != 404 {
			t.Errorf("database %s after destroy: %v; want 404", db, err)
		}
	}
}

// sqlDataSourceInstance is the Cloud SQL instance `up` runs: postgres where
// Cloud SQL for PostgreSQL is served, else mysql; it skips without either.
func sqlDataSourceInstance(t *testing.T) string {
	t.Helper()
	switch {
	case os.Getenv(EnvCloudSQL) != "":
		return "postgres"
	case os.Getenv(EnvMySQL) != "":
		return "mysql"
	}
	t.Skip("neither Cloud SQL server is exported")
	return ""
}

// TestTerraformBackupDRDataSources (#1304): TestTerraformBackupDR's vault,
// plan and association of the Cloud SQL instance apply, with a backup the
// official client triggers; the data sources
// google_backup_dr_backup_plan_associations, google_backup_dr_data_sources,
// google_backup_dr_data_source, google_backup_dr_data_source_references,
// google_backup_dr_data_source_reference and google_backup_dr_backup are
// compared with what Google's generated backupdr client reads; the plan is
// clean; a second association-free change (the vault's description)
// applies in place; the plan is clean again; destroy deletes the vault.
// google_backup_dr_management_server is not here: management servers are
// refused 501 (TestBackupDRRefusals).
// covers: backupdr.projects.locations.backupPlanAssociations.list, backupdr.projects.locations.backupVaults.dataSources.list, backupdr.projects.locations.backupVaults.dataSources.get, backupdr.projects.locations.dataSourceReferences.list, backupdr.projects.locations.dataSourceReferences.get, backupdr.projects.locations.backupVaults.dataSources.backups.list
func TestTerraformBackupDRDataSources(t *testing.T) {
	h := New(t)
	instance := backupDRInstance(t)
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	vault := fmt.Sprintf("tfdsvault-%d", n)
	loc := "projects/" + m.project + "/locations/us-central1"
	assoc := fmt.Sprintf("%s/backupPlanAssociations/tfdsassoc-%d", loc, n)
	resources := func(desc string) string {
		return fmt.Sprintf(`
resource "google_backup_dr_backup_vault" "v" {
  project                                    = %[1]q
  location                                   = "us-central1"
  backup_vault_id                            = %[2]q
  description                                = %[5]q
  backup_minimum_enforced_retention_duration = "100000s"
  ignore_inactive_datasources                = true
  force_delete                               = true
}
resource "google_backup_dr_backup_plan" "p" {
  project        = %[1]q
  location       = "us-central1"
  backup_plan_id = "tfdsplan-%[3]d"
  resource_type  = "sqladmin.googleapis.com/Instance"
  backup_vault   = google_backup_dr_backup_vault.v.id
  log_retention_days = 7
  backup_rules {
    rule_id               = "daily"
    backup_retention_days = 7
    standard_schedule {
      recurrence_type = "DAILY"
      time_zone       = "UTC"
      backup_window {
        start_hour_of_day = 1
        end_hour_of_day   = 5
      }
    }
  }
}
resource "google_backup_dr_backup_plan_association" "a" {
  project                    = %[1]q
  location                   = "us-central1"
  backup_plan_association_id = "tfdsassoc-%[3]d"
  resource                   = "projects/%[1]s/instances/%[4]s"
  resource_type              = "sqladmin.googleapis.com/Instance"
  backup_plan                = google_backup_dr_backup_plan.p.name
}
`, m.project, vault, n, instance, desc)
	}
	// The data sources, added once the association has a data source and a
	// backup, as the provider reads them at plan time.
	dataSources := func(ds, ref string) string {
		return fmt.Sprintf(`
data "google_backup_dr_backup_plan_associations" "a" {
  project    = %[1]q
  location   = "us-central1"
  depends_on = [google_backup_dr_backup_plan_association.a]
}
data "google_backup_dr_data_sources" "d" {
  project         = %[1]q
  location        = "us-central1"
  backup_vault_id = google_backup_dr_backup_vault.v.backup_vault_id
  depends_on      = [google_backup_dr_backup_plan_association.a]
}
data "google_backup_dr_data_source" "d" {
  project         = %[1]q
  location        = "us-central1"
  backup_vault_id = google_backup_dr_backup_vault.v.backup_vault_id
  data_source_id  = %[2]q
}
data "google_backup_dr_data_source_references" "r" {
  project    = %[1]q
  location   = "us-central1"
  depends_on = [google_backup_dr_backup_plan_association.a]
}
data "google_backup_dr_data_source_reference" "r" {
  project                  = %[1]q
  location                 = "us-central1"
  data_source_reference_id = %[3]q
}
data "google_backup_dr_backup" "b" {
  project         = %[1]q
  location        = "us-central1"
  backup_vault_id = google_backup_dr_backup_vault.v.backup_vault_id
  data_source_id  = %[2]q
}
output "outputs" {
  value = {
    associations = sort([for a in data.google_backup_dr_backup_plan_associations.a.associations : "${a.name}|${a.data_source}|${a.resource}"])
    data_sources = sort([for d in data.google_backup_dr_data_sources.d.data_sources : "${d.name}|${d.backup_count}"])
    data_source  = "${data.google_backup_dr_data_source.d.name}|${data.google_backup_dr_data_source.d.backup_count}|${data.google_backup_dr_data_source.d.config_state}"
    references   = sort([for r in data.google_backup_dr_data_source_references.r.data_source_references : "${r.name}|${r.data_source}|${r.gcp_resource_name}"])
    reference    = "${data.google_backup_dr_data_source_reference.r.name}|${data.google_backup_dr_data_source_reference.r.data_source}|${data.google_backup_dr_data_source_reference.r.resource_type}"
    backups      = sort([for b in data.google_backup_dr_backup.b.backups : b.name])
  }
}
`, m.project, ds, ref)
	}

	m.write(resources("first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assoc).Do()
	if err != nil || a.DataSource == "" {
		t.Fatalf("the association: %v %+v", err, a)
	}
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assoc, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())
	refs, err := bdr.Projects.Locations.DataSourceReferences.List(loc).Do()
	if err != nil {
		t.Fatalf("dataSourceReferences.list: %v", err)
	}
	var refID string
	for _, r := range refs.DataSourceReferences {
		if r.DataSource == a.DataSource {
			refID = path.Base(r.Name)
		}
	}
	if refID == "" {
		t.Fatalf("no data source reference names %s: %+v", a.DataSource, refs.DataSourceReferences)
	}
	dsID := path.Base(a.DataSource)

	check := func(when string) {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		want := map[string]any{}
		al, err := bdr.Projects.Locations.BackupPlanAssociations.List(loc).Do()
		if err != nil {
			t.Fatalf("%s: backupPlanAssociations.list: %v", when, err)
		}
		var as []string
		for _, x := range al.BackupPlanAssociations {
			as = append(as, x.Name+"|"+x.DataSource+"|"+x.Resource)
		}
		dl, err := bdr.Projects.Locations.BackupVaults.DataSources.List(loc + "/backupVaults/" + vault).Do()
		if err != nil {
			t.Fatalf("%s: dataSources.list: %v", when, err)
		}
		var ds []string
		for _, x := range dl.DataSources {
			ds = append(ds, fmt.Sprintf("%s|%d", x.Name, x.BackupCount))
		}
		d, err := bdr.Projects.Locations.BackupVaults.DataSources.Get(a.DataSource).Do()
		if err != nil {
			t.Fatalf("%s: dataSources.get: %v", when, err)
		}
		rl, err := bdr.Projects.Locations.DataSourceReferences.List(loc).Do()
		if err != nil {
			t.Fatalf("%s: dataSourceReferences.list: %v", when, err)
		}
		var rs []string
		for _, x := range rl.DataSourceReferences {
			rs = append(rs, x.Name+"|"+x.DataSource+"|"+x.DataSourceGcpResourceInfo.GcpResourcename)
		}
		r, err := bdr.Projects.Locations.DataSourceReferences.Get(loc + "/dataSourceReferences/" + refID).Do()
		if err != nil {
			t.Fatalf("%s: dataSourceReferences.get: %v", when, err)
		}
		bl, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(a.DataSource).Do()
		if err != nil {
			t.Fatalf("%s: backups.list: %v", when, err)
		}
		var bs []string
		for _, x := range bl.Backups {
			bs = append(bs, x.Name)
		}
		for _, l := range [][]string{as, ds, rs, bs} {
			sort.Strings(l)
		}
		toAny := func(l []string) []any {
			out := []any{}
			for _, s := range l {
				out = append(out, s)
			}
			return out
		}
		want["associations"], want["data_sources"], want["references"], want["backups"] = toAny(as), toAny(ds), toAny(rs), toAny(bs)
		want["data_source"] = fmt.Sprintf("%s|%d|%s", d.Name, d.BackupCount, d.ConfigState)
		want["reference"] = r.Name + "|" + r.DataSource + "|" + r.DataSourceGcpResourceInfo.Type
		if fmt.Sprint(got) != fmt.Sprint(want) || len(bs) == 0 {
			t.Errorf("%s: the data sources read\n%v\nthe official client\n%v", when, got, want)
		}
	}

	m.write(resources("first") + dataSources(dsID, refID))
	m.apply()
	check("after apply")
	m.planClean()

	m.write(resources("second") + dataSources(dsID, refID))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the vault was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := bdr.Projects.Locations.BackupVaults.Get(loc + "/backupVaults/" + vault).Do(); err == nil {
		t.Errorf("the vault survived destroy")
	}
}

// TestTerraformSQLLatestRecoveryTime (#1304): a
// google_sql_database_instance with point-in-time recovery applies (the
// server `up` runs keeps no transaction log, and refuses the call, as
// Google does an instance without it), and the data source
// google_sql_database_instance_latest_recovery_time reads a time within the
// window Google's generated sqladmin client's
// projects.instances.getLatestRecoveryTime answers around it. No output
// names the time, which moves on with every read, so the plan is clean; a
// new label applies in place; the plan is clean again; destroy deletes the
// instance.
// covers: sql.projects.instances.getLatestRecoveryTime
func TestTerraformSQLLatestRecoveryTime(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-lrt-%d", time.Now().UnixNano()%1e6)
	module := func(env string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "i" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  deletion_protection = false
  settings {
    tier        = "db-custom-1-3840"
    user_labels = { env = %[3]q }
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }
  }
}
data "google_sql_database_instance_latest_recovery_time" "l" {
  project    = %[1]q
  instance   = google_sql_database_instance.i.name
  depends_on = [google_sql_database_instance.i]
}
`, m.project, name, env)
	}
	check := func(when string) {
		t.Helper()
		before, err := svc.Projects.Instances.GetLatestRecoveryTime(m.project, name).Do()
		if err != nil {
			t.Fatalf("%s: getLatestRecoveryTime: %v", when, err)
		}
		m.must("refresh", "-input=false", "-no-color")
		var state struct {
			Values struct {
				RootModule struct {
					Resources []struct {
						Address string
						Values  map[string]any
					}
				} `json:"root_module"`
			}
		}
		if err := json.Unmarshal([]byte(m.must("show", "-json")), &state); err != nil {
			t.Fatalf("%s: show -json: %v", when, err)
		}
		after, err := svc.Projects.Instances.GetLatestRecoveryTime(m.project, name).Do()
		if err != nil {
			t.Fatalf("%s: getLatestRecoveryTime: %v", when, err)
		}
		var got string
		for _, r := range state.Values.RootModule.Resources {
			if r.Address == "data.google_sql_database_instance_latest_recovery_time.l" {
				got, _ = r.Values["latest_recovery_time"].(string)
			}
		}
		g, err1 := time.Parse(time.RFC3339Nano, got)
		lo, err2 := time.Parse(time.RFC3339Nano, before.LatestRecoveryTime)
		hi, err3 := time.Parse(time.RFC3339Nano, after.LatestRecoveryTime)
		if err1 != nil || err2 != nil || err3 != nil || g.Before(lo) || g.After(hi) {
			t.Errorf("%s: the data source reads %q; the official client %q before it and %q after (%v %v %v)",
				when, got, before.LatestRecoveryTime, after.LatestRecoveryTime, err1, err2, err3)
		}
	}

	m.write(module("dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply")
	m.planClean()

	m.write(module("prod"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the label was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svc.Instances.Get(m.project, name).Do(); httpCode(err) != 404 {
		t.Errorf("the instance after destroy: %v; want 404", err)
	}
}
