//go:build browser

package browser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLConsoleBackupLocation (#1956), in the sql shard: on a created
// PostgreSQL instance's page, Edit's Backup location (Google's Edit backup
// settings, Advanced options) sets settings.backupConfiguration.location,
// which the official client reads back; Create backup then takes a backup
// there, and the Backups page's Location column says "Region:
// europe-west1".
func TestCloudSQLConsoleBackupLocation(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	name := fmt.Sprintf("browser-bkloc-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
			BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)

	p := open(t)
	page := "/cloudsql-instances/" + name + "?project=" + project
	p.navigate(page)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit")`)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-backupLocation") !== null`)
	var label string
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-backupLocation"); f.value = "europe-west1";
		f.dispatchEvent(new Event("change", { bubbles: true })); return f.options[f.selectedIndex].textContent; })()`, &label)
	if !strings.HasPrefix(label, "Region: europe-west1") {
		t.Errorf("the chosen option reads %q; want Region: europe-west1 (...)", label)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		in, err := svc.Instances.Get(project, name).Do()
		if err == nil && in.Settings.BackupConfiguration != nil && in.Settings.BackupConfiguration.Location == "europe-west1" && in.State == "RUNNABLE" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Edit, backupConfiguration is %+v (%v)", in.Settings.BackupConfiguration, err)
		}
	}

	p.navigate(page)
	p.pageAction("Create backup", map[string]string{"description": "browser-located"}, "")
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
		runs, err := svc.BackupRuns.List(project, name).Do()
		if err == nil && len(runs.Items) > 0 && runs.Items[0].Status == "SUCCESSFUL" {
			if runs.Items[0].Location != "europe-west1" {
				t.Fatalf("the backup is in %q, want europe-west1", runs.Items[0].Location)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Create backup made no SUCCESSFUL backup (%v)", err)
		}
	}
	p.navigate(page)
	p.clickText("#view [role=tab]", "Backups")
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-located")`)
	var shown bool
	p.eval(`document.querySelector("#view").textContent.includes("Region: europe-west1")`, &shown)
	if !shown {
		t.Error("the Backups page's Location column does not say Region: europe-west1")
	}
}
