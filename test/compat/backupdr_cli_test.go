//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// backupDRInstance is a served Cloud SQL instance every project has, for
// the CLI tests to enroll.
func backupDRInstance(t *testing.T) string {
	t.Helper()
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	switch {
	case os.Getenv(EnvCloudSQL) != "":
		return "postgres"
	case os.Getenv(EnvMySQL) != "":
		return "mysql"
	}
	t.Skip("needs --services cloudsql or cloudsql-mysql")
	return ""
}

// TestGcloudBackupDR (#1433): `gcloud backup-dr` creates, describes and
// lists a backup vault and a backup plan with a rule, associates a Cloud SQL
// instance, lists the data source it makes, triggers a backup, lists it, and
// deletes the association, the plan and the vault, through the configuration
// `cloudburrow gcloud-setup` writes with egress to Google blocked.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.list, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.get, backupdr.projects.locations.backupPlans.delete, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.get, backupdr.projects.locations.backupPlanAssociations.delete, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.list, backupdr.projects.locations.backupVaults.dataSources.backups.list, backupdr.projects.locations.operations.get
func TestGcloudBackupDR(t *testing.T) {
	h := New(t)
	instance := backupDRInstance(t)
	g := newGcloudSession(t, h)
	n := time.Now().UnixNano() % 1e6
	vault, plan, assoc := fmt.Sprintf("gvault-%d", n), fmt.Sprintf("gplan-%d", n), fmt.Sprintf("gassoc-%d", n)
	locFlags := []string{"--location", "us-central1", "--project", g.project, "--quiet"}
	run := func(args ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"backup-dr"}, args...), locFlags...)...)
		if err != nil {
			t.Fatalf("gcloud backup-dr %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	run("backup-vaults", "create", vault, "--backup-min-enforced-retention", "1d")
	defer g.run(nil, append([]string{"backup-dr", "backup-vaults", "delete", vault, "--ignore-inactive-datasources",
		"--ignore-backup-plan-references", "--no-async"}, locFlags...)...)
	if out := run("backup-vaults", "describe", vault, "--format", "value(state)"); !strings.Contains(out, "ACTIVE") {
		t.Errorf("backup-vaults describe: %s", out)
	}
	if out := run("backup-vaults", "list", "--format", "value(name)"); !strings.Contains(out, vault) {
		t.Errorf("backup-vaults list: %s", out)
	}
	run("backup-plans", "create", plan, "--backup-vault", "projects/"+g.project+"/locations/us-central1/backupVaults/"+vault,
		"--resource-type", "sqladmin.googleapis.com/Instance", "--log-retention-days", "7",
		"--backup-rule", "rule-id=daily,retention-days=7,recurrence=DAILY,backup-window-start=1,backup-window-end=5")
	defer g.run(nil, append([]string{"backup-dr", "backup-plans", "delete", plan}, locFlags...)...)
	if out := run("backup-plans", "describe", plan, "--format", "value(backupRules[0].ruleId)"); !strings.Contains(out, "daily") {
		t.Errorf("backup-plans describe: %s", out)
	}
	run("backup-plan-associations", "create", assoc, "--backup-plan", "projects/"+g.project+"/locations/us-central1/backupPlans/"+plan,
		"--resource", "projects/"+g.project+"/instances/"+instance, "--resource-type", "sqladmin.googleapis.com/Instance")
	defer g.run(nil, append([]string{"backup-dr", "backup-plan-associations", "delete", assoc}, locFlags...)...)
	ds := strings.TrimSpace(run("backup-plan-associations", "describe", assoc, "--format", "value(dataSource)"))
	if !strings.Contains(ds, "/backupVaults/"+vault+"/dataSources/") {
		t.Fatalf("backup-plan-associations describe: dataSource %q", ds)
	}
	if out := run("data-sources", "list", "--backup-vault", vault, "--format", "value(name)"); !strings.Contains(out, ds) {
		t.Errorf("data-sources list: %s", out)
	}
	run("backup-plan-associations", "trigger-backup", assoc, "--backup-rule-id", "daily")
	dsID := ds[strings.LastIndex(ds, "/")+1:]
	if out := run("backups", "list", "--backup-vault", vault, "--data-source", dsID, "--format", "value(state)"); !strings.Contains(out, "ACTIVE") {
		t.Errorf("backups list after trigger-backup: %s", out)
	}
}

// TestTerraformBackupDR (#1433): a google_backup_dr_backup_vault, a
// google_backup_dr_backup_plan with a rule and a
// google_backup_dr_backup_plan_association of a Cloud SQL instance apply
// with the official hashicorp/google provider through `cloudburrow
// terraform` (backup_dr_custom_endpoint), are read back through the
// official client, plan clean and destroy.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.get, backupdr.projects.locations.backupPlans.delete, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.get, backupdr.projects.locations.backupPlanAssociations.delete, backupdr.projects.locations.operations.get
func TestTerraformBackupDR(t *testing.T) {
	h := New(t)
	instance := backupDRInstance(t)
	bdr := backupDRService(t, h)
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	vault := fmt.Sprintf("tfvault-%d", n)
	m.write(fmt.Sprintf(`
resource "google_backup_dr_backup_vault" "v" {
  project                                    = %[1]q
  location                                   = "us-central1"
  backup_vault_id                            = %[2]q
  backup_minimum_enforced_retention_duration = "100000s"
  ignore_inactive_datasources                = true
}
resource "google_backup_dr_backup_plan" "p" {
  project        = %[1]q
  location       = "us-central1"
  backup_plan_id = "tfplan-%[3]d"
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
  backup_plan_association_id = "tfassoc-%[3]d"
  resource                   = "projects/%[1]s/instances/%[4]s"
  resource_type              = "sqladmin.googleapis.com/Instance"
  backup_plan                = google_backup_dr_backup_plan.p.name
}
`, m.project, vault, n, instance))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	loc := "projects/" + m.project + "/locations/us-central1"
	a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(fmt.Sprintf("%s/backupPlanAssociations/tfassoc-%d", loc, n)).Do()
	if err != nil || !strings.HasPrefix(a.DataSource, loc+"/backupVaults/"+vault+"/dataSources/") {
		t.Fatalf("the association: %v %+v", err, a)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := bdr.Projects.Locations.BackupVaults.Get(loc + "/backupVaults/" + vault).Do(); err == nil {
		t.Errorf("the vault survived destroy")
	}
}
