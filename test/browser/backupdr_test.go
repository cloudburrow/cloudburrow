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
