//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
)

// TestBackupDRVaultAndPlanThroughThePage (#1433), in the sql shard, whose
// instance serves the Backup and DR Service. The page had a provider but no
// route until it was given one. Reached from the drawer's Backup and DR row:
// Create backup vault makes a vault backupVaults.get reads ACTIVE with its
// minimum enforced retention, and Create backup plan on its page makes a
// plan backupPlans.get reads with its rule and vault.
func TestBackupDRVaultAndPlanThroughThePage(t *testing.T) {
	needService(t, "backupdr")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := servedProject(t)
	loc := "projects/" + project + "/locations/us-central1"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	vault, plan := "browser-vault-"+suffix, "browser-plan-"+suffix
	t.Cleanup(func() {
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(loc + "/backupPlans/" + plan).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(loc + "/backupVaults/" + vault).Force(true).Do()
	})

	p := open(t)
	p.navigateByDrawer("Storage", "/backupdr", "Backup and DR")
	p.navigate("/backupdr?project=" + project)
	p.clickText("#view button", "Create backup vault")
	p.submitForm(map[string]string{"id": vault, "retention": "172800s", "description": "made in the browser"}, "id")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, vault))
	v, err := bdr.Projects.Locations.BackupVaults.Get(loc + "/backupVaults/" + vault).Do()
	if err != nil || v.State != "ACTIVE" || v.BackupMinimumEnforcedRetentionDuration != "172800s" || v.Description != "made in the browser" {
		t.Fatalf("backupVaults.get after Create backup vault = %+v, %v; want it ACTIVE with 172800s and its description", v, err)
	}

	p.navigate("/backupdr/" + vault + "?project=" + project)
	p.pageAction("Create backup plan", map[string]string{"id": plan, "rule": "nightly", "recurrence": "DAILY", "retention": "3"}, "")
	got, err := bdr.Projects.Locations.BackupPlans.Get(loc + "/backupPlans/" + plan).Do()
	if err != nil {
		t.Fatalf("backupPlans.get after Create backup plan: %v", err)
	}
	if len(got.BackupRules) != 1 || got.BackupRules[0].RuleId != "nightly" || got.BackupRules[0].BackupRetentionDays != 3 ||
		got.BackupVault != loc+"/backupVaults/"+vault {
		t.Errorf("the plan made in the browser is %+v; want rule nightly, 3 days, in %s", got, vault)
	}
}

// TestBackupDRDeletesThroughThePage, in the sql shard: Delete backup plan
// on a vault's page, confirmed by the vault's name, deletes a plan
// backupPlans.get then reports gone, and the page stays the vault's. Delete
// association and Delete backup, which need a Cloud SQL instance and a
// backup of it, are exercised by TestConsoleBackupDRDeletes.
func TestBackupDRDeletesThroughThePage(t *testing.T) {
	needService(t, "backupdr")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := servedProject(t)
	loc := "projects/" + project + "/locations/us-central1"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	vault, plan := "browser-delvault-"+suffix, "browser-delplan-"+suffix
	t.Cleanup(func() {
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(loc + "/backupPlans/" + plan).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(loc + "/backupVaults/" + vault).Force(true).Do()
	})
	if _, err := bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).
		BackupVaultId(vault).Do(); err != nil {
		t.Fatalf("backupVaults.create: %v", err)
	}
	if _, err := bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: loc + "/backupVaults/" + vault,
		ResourceType: "sqladmin.googleapis.com/Instance",
		BackupRules: []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7,
			StandardSchedule: &backupdr.StandardSchedule{RecurrenceType: "DAILY", TimeZone: "UTC",
				BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 0, EndHourOfDay: 24}}}}}).BackupPlanId(plan).Do(); err != nil {
		t.Fatalf("backupPlans.create: %v", err)
	}

	p := open(t)
	p.navigate("/backupdr/" + vault + "?project=" + project)
	p.pageAction("Delete backup plan", map[string]string{"plan": plan}, vault)
	if _, err := bdr.Projects.Locations.BackupPlans.Get(loc + "/backupPlans/" + plan).Do(); err == nil {
		t.Errorf("backupPlans.get after Delete backup plan found %s", plan)
	}
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/backupdr/"+vault))
}
