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
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// confirmWord is the word the open confirmation asks to be typed back.
func (p *tab) confirmWord() string {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null`)
	var word string
	p.eval(`document.querySelector(".modal label[for=confirm-input] strong").textContent`, &word)
	return word
}

// TestBackupDRScheduleAndRestoreThroughThePages (#1844), in the sql shard,
// with a Cloud SQL instance, a vault and a plan made through the API. On
// Google's pages:
//   - Vaulted backups' Schedule backup (Resource Type, Region, Resources,
//     Backup plan; no association ID) makes the association
//     backupPlanAssociations.list reads for the instance;
//   - the resource's page's Create on-demand backup takes a backup
//     backups.list reads ACTIVE, and Jobs lists its job Succeeded;
//   - the backup row's Restore, Overwrite the source instance, confirmed,
//     is a RESTORE_VOLUME operation of the instance that finishes;
//   - Remove backup plan, confirmed, deletes the association, leaving the
//     backup in the vault.
func TestBackupDRScheduleAndRestoreThroughThePages(t *testing.T) {
	needService(t, "backupdr")
	needService(t, "cloudsql-instances")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	loc := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	inst, vault, plan := fmt.Sprintf("browser-bdrs-%d", n), fmt.Sprintf("browser-svault-%d", n), fmt.Sprintf("browser-splan-%d", n)
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	if _, err := bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "0s"}).
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
	assocOf := func() *backupdr.BackupPlanAssociation {
		t.Helper()
		l, err := bdr.Projects.Locations.BackupPlanAssociations.List(loc).Do()
		if err != nil {
			t.Fatalf("backupPlanAssociations.list: %v", err)
		}
		for _, a := range l.BackupPlanAssociations {
			if a.Resource == "projects/"+project+"/instances/"+inst {
				return a
			}
		}
		return nil
	}
	t.Cleanup(func() {
		if a := assocOf(); a != nil {
			_, _ = bdr.Projects.Locations.BackupPlanAssociations.Delete(a.Name).Do()
		}
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(loc + "/backupPlans/" + plan).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(loc + "/backupVaults/" + vault).Force(true).Do()
	})

	p := open(t)
	p.navigate("/backupdr-vaulted-backups?project=" + project)
	p.clickText("#view button", "Schedule backup")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr-vaulted-backups/create" &&
		[...document.querySelectorAll("#f-instance option")].some((o) => o.value === %q)`, inst))
	var labels string
	p.eval(`[...document.querySelectorAll("form.create-page label, form.create-page legend")].map((l) => l.textContent).join("|")`, &labels)
	for _, want := range []string{"Resource Type", "Region", "Resources", "Backup plan"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Schedule backup's labels %q lack %s", labels, want)
		}
	}
	if strings.Contains(labels, "Association ID") {
		t.Errorf("Schedule backup asks for an association ID, which Google's does not: %q", labels)
	}
	p.submitForm(map[string]string{"instance": inst, "plan": loc + "/backupPlans/" + plan}, "instance")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr-vaulted-backups" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, inst))
	a := assocOf()
	if a == nil || a.BackupPlan != loc+"/backupPlans/"+plan || a.DataSource == "" {
		t.Fatalf("backupPlanAssociations.list after Schedule backup: %+v; want %s under %s", a, inst, plan)
	}
	ds := a.DataSource[strings.LastIndex(a.DataSource, "/")+1:]
	page := "/backupdr-vaulted-backups/" + vault + "/" + ds
	p.navigate(page + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.pageAction("Create on-demand backup", map[string]string{"rule": "daily"}, "")
	var backup *backupdr.Backup
	for deadline := time.Now().Add(5 * time.Minute); backup == nil && time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		l, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(a.DataSource).Do()
		if err != nil {
			t.Fatalf("backups.list: %v", err)
		}
		for _, b := range l.Backups {
			if b.State == "ACTIVE" {
				backup = b
			}
		}
	}
	if backup == nil {
		t.Fatal("Create on-demand backup made no ACTIVE backup in five minutes")
	}
	id := backup.Name[strings.LastIndex(backup.Name, "/")+1:]

	p.navigate("/backupdr-jobs?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Succeeded"))`, id))

	p.navigate(page + "?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, id)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Restore")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	var dialog string
	p.eval(`document.querySelector(".modal.is-open").textContent`, &dialog)
	for _, want := range []string{"Choose restore destination", "Overwrite the source instance", "Overwrite an existing instance", "Restore to a new instance"} {
		if !strings.Contains(dialog, want) {
			t.Errorf("Restore's dialog %q lacks %s", dialog, want)
		}
	}
	p.clickText(".modal.is-open button", "Restore")
	p.confirmByName(p.confirmWord())
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	waitLatestSQLOp(t, svc, project, inst, "RESTORE_VOLUME")

	p.navigate(page + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") !== null`)
	where := p.findAction("Remove backup plan")
	if where == "" {
		t.Fatal("the resource's page offers no Remove backup plan")
	}
	p.clickText(where, "Remove backup plan")
	p.confirmByName(p.confirmWord())
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if a := assocOf(); a != nil {
		t.Errorf("backupPlanAssociations.list after Remove backup plan still has %s", a.Name)
	}
	if _, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.Get(backup.Name).Do(); err != nil {
		t.Errorf("the backup is gone after Remove backup plan: %v", err)
	}
}
