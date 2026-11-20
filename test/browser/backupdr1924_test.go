//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestBackupDRRestoreToANewInstanceAndJobsThroughThePages (#1922, #1924),
// in the sql shard, with a Cloud SQL instance protected and backed up
// through the API:
//   - the resource's page's Restore, Choose restore destination "Restore to
//     a new instance" with an Instance ID, confirmed, creates that instance,
//     which instances.get reads RUNNABLE;
//   - with the source instance deleted, a backup of it fails;
//   - the Jobs page lists the restore job, Restore and Succeeded, with its
//     Restore resource name; Current failures counts the instance under
//     NOT_FOUND; the time range 1h still lists the restore, and a range
//     Google does not offer is refused with the ranges it does.
func TestBackupDRRestoreToANewInstanceAndJobsThroughThePages(t *testing.T) {
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
	inst, vault, plan := fmt.Sprintf("browser-bdrn-%d", n), fmt.Sprintf("browser-nvault-%d", n), fmt.Sprintf("browser-nplan-%d", n)
	target := inst + "-new"
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	wait := func(what string, op *backupdr.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for end := time.Now().Add(5 * time.Minute); !op.Done && time.Now().Before(end); time.Sleep(time.Second) {
			if op, err = bdr.Projects.Locations.Operations.Get(op.Name).Do(); err != nil {
				t.Fatalf("%s: operations.get: %v", what, err)
			}
		}
		if !op.Done || op.Error != nil {
			t.Fatalf("%s: %+v", what, op)
		}
	}
	vaultName, planName, assocName := loc+"/backupVaults/"+vault, loc+"/backupPlans/"+plan, loc+"/backupPlanAssociations/"+inst
	op, err := bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "0s"}).
		BackupVaultId(vault).Do()
	wait("backupVaults.create", op, err)
	op, err = bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName,
		ResourceType: "sqladmin.googleapis.com/Instance",
		BackupRules: []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7,
			StandardSchedule: &backupdr.StandardSchedule{RecurrenceType: "DAILY", TimeZone: "UTC",
				BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 0, EndHourOfDay: 24}}}}}).BackupPlanId(plan).Do()
	wait("backupPlans.create", op, err)
	op, err = bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: planName,
		Resource: "projects/" + project + "/instances/" + inst, ResourceType: "sqladmin.googleapis.com/Instance"}).
		BackupPlanAssociationId(inst).Do()
	wait("backupPlanAssociations.create", op, err)
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, target).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
		_, _ = bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do()
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(planName).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).IgnoreBackupPlanReferences(true).Do()
	})
	a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
	if err != nil {
		t.Fatal(err)
	}
	op, err = bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do()
	wait("triggerBackup", op, err)
	// The on-demand backup; the plan's window, the whole day, may have
	// taken a scheduled one too.
	l, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(a.DataSource).Do()
	if err != nil {
		t.Fatalf("backups.list: %v", err)
	}
	backup := ""
	for _, b := range l.Backups {
		if b.BackupType == "ON_DEMAND" && b.State == "ACTIVE" {
			backup = b.Name[strings.LastIndex(b.Name, "/")+1:]
		}
	}
	if backup == "" {
		t.Fatalf("backups.list has no ACTIVE on-demand backup: %+v", l.Backups)
	}
	ds := a.DataSource[strings.LastIndex(a.DataSource, "/")+1:]

	// Restore to a new instance, on the resource's page.
	p := open(t)
	p.navigate("/backupdr-vaulted-backups/" + vault + "/" + ds + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") !== null`)
	where := p.findAction("Restore")
	if where == "" {
		t.Fatal("the resource's page offers no Restore")
	}
	p.clickText(where, "Restore")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	var disabled bool
	p.eval(`document.querySelector('.modal.is-open input[name="f-destination-choice"][value="new"]').disabled`, &disabled)
	if disabled {
		t.Fatal("Choose restore destination draws Restore to a new instance disabled")
	}
	setField(p, ".modal.is-open", "f-backup", backup)
	p.clickText(".modal.is-open button", "Continue")
	p.waitFor(`document.querySelector('.modal.is-open input[name="f-destination-choice"][value="new"]').getClientRects().length > 0`)
	p.eval(`(() => { document.querySelector('.modal.is-open input[name="f-destination-choice"][value="new"]').click(); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-newinstance").getClientRects().length > 0`)
	setField(p, ".modal.is-open", "f-newinstance", target)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.confirmByName(p.confirmWord())
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	waitSQLInstance(t, svc, project, target, "RUNNABLE after Restore to a new instance", func(in *sqladmin.DatabaseInstance) bool {
		return in.State == "RUNNABLE" && in.DatabaseVersion == "POSTGRES_17"
	})

	// A backup of the deleted source instance fails.
	d, err := svc.Instances.Delete(project, inst).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQL(t, svc, project, d, 5*time.Minute)
	if _, err := bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do(); err == nil {
		t.Fatal("triggerBackup of the deleted instance succeeded")
	}

	restoreRow := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) &&
		r.textContent.includes("Restore") && r.textContent.includes("Succeeded"))`, target)
	p.navigate("/backupdr-jobs?project=" + project)
	p.waitForWithin(restoreRow, time.Minute)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"Current failures: NOT_FOUND", inst, "Failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Jobs page lacks %q", want)
		}
	}
	// Google's time range is a list (#1844): Last 1 hour still holds the
	// restore just run.
	p.waitFor(`[...document.querySelectorAll("#scope-range option")].map((o) => o.textContent).join(",") === ` +
		`"Last 1 hour,Last 6 hours,Last 12 hours,Last 24 hours,Last 7 days,Last 14 days"`)
	p.eval(`(() => { const s = document.querySelector("#scope-range"); s.value = "1h";
		s.dispatchEvent(new Event("change")); return true; })()`, nil)
	p.waitFor(`new URLSearchParams(location.search).get("range") === "1h"`)
	p.waitForWithin(restoreRow, time.Minute)
	p.navigate("/backupdr-jobs?project=" + project + "&range=2h")
	p.waitFor(`document.querySelector("#view").textContent.includes("1h, 6h, 12h, 24h")`)
}
