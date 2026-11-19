//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
)

// TestBackupDRVaultAndPlanThroughThePage (#1433, #1844), in the sql shard,
// whose instance serves the Backup and DR Service. Reached from the drawer's
// Backup and DR row, which opens the Overview with Google's menu in Google's
// order. Create a backup vault, Google's page, makes a vault
// backupVaults.get reads ACTIVE with its minimum enforced retention (days)
// and description, and Create Backup plan, Google's page with its Backup
// rules, makes a plan backupPlans.get reads with its rule, window and vault.
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
	p.navigateByDrawer("Storage", "/backupdr-overview", "Backup and DR")
	var menu string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).join("|")`, &menu)
	if want := "Overview|Vaulted backups|Backup plans|Backup plan associations|Backup vaults|Jobs|Reports"; !strings.Contains(menu, want) {
		t.Errorf("Backup and DR's menu reads %q; want %q", menu, want)
	}
	p.navigate("/backupdr?project=" + project)
	p.clickText("#view button", "Create backup vault")
	p.waitFor(`location.pathname === "/backupdr/create" && document.querySelector("#view h1").textContent.includes("Create a backup vault")`)
	p.submitForm(map[string]string{"id": vault, "retentionDays": "2", "description": "made in the browser"}, "id")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, vault))
	v, err := bdr.Projects.Locations.BackupVaults.Get(loc + "/backupVaults/" + vault).Do()
	if err != nil || v.State != "ACTIVE" || v.BackupMinimumEnforcedRetentionDuration != "172800s" || v.Description != "made in the browser" ||
		v.AccessRestriction != "WITHIN_ORGANIZATION" {
		t.Fatalf("backupVaults.get after Create a backup vault = %+v, %v; want it ACTIVE with 172800s, its description and Google's default access", v, err)
	}

	p.navigate("/backupdr-plans/create?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-vault option")].some((o) => o.value === %q)`, loc+"/backupVaults/"+vault))
	p.submitForm(map[string]string{"id": plan, "vault": loc + "/backupVaults/" + vault,
		"rules": `[{"ruleId":"nightly","recurrence":"DAILY","hourly":"","start":"1","end":"5","retention":"3"}]`}, "id")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr-plans" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, plan))
	got, err := bdr.Projects.Locations.BackupPlans.Get(loc + "/backupPlans/" + plan).Do()
	if err != nil {
		t.Fatalf("backupPlans.get after Create Backup plan: %v", err)
	}
	if len(got.BackupRules) != 1 || got.BackupRules[0].RuleId != "nightly" || got.BackupRules[0].BackupRetentionDays != 3 ||
		got.BackupRules[0].StandardSchedule.BackupWindow.StartHourOfDay != 1 || got.BackupRules[0].StandardSchedule.BackupWindow.EndHourOfDay != 5 ||
		got.BackupVault != loc+"/backupVaults/"+vault {
		t.Errorf("the plan made in the browser is %+v; want rule nightly, 01:00 to 05:00, 3 days, in %s", got, vault)
	}
}

// TestBackupDRDeletesThroughThePage, in the sql shard: Delete on a backup
// plan's page, confirmed by the plan's name, deletes a plan backupPlans.get
// then reports gone. Remove backup plan and a backup's Delete are exercised
// by TestBackupDRScheduleAndRestoreThroughThePages.
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

	// The plan's own page (#1844): Delete, confirmed by its name, leaves
	// for Backup plans.
	p := open(t)
	p.navigate("/backupdr-plans/" + plan + "?project=" + project)
	p.clickAction("Delete", plan)
	if _, err := bdr.Projects.Locations.BackupPlans.Get(loc + "/backupPlans/" + plan).Do(); err == nil {
		t.Errorf("backupPlans.get after Delete found %s", plan)
	}
	p.waitFor(`location.pathname === "/backupdr-plans"`)
}
