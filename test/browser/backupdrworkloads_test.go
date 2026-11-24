//go:build browser

package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
)

// envAlloyDBBrowser is AlloyDB's endpoint, gRPC and JSON on one port, in
// the sql shard's browser step (#1524).
const envAlloyDBBrowser = "CLOUDBURROW_TEST_ALLOYDB"

// TestBackupDRAlloyDBRestoresThroughThePages (#1524), in the sql shard,
// whose instance serves Backup and DR beside AlloyDB. An AlloyDB cluster is
// made on AlloyDB's Create cluster page, and a vault and an AlloyDB plan
// with log retention through the API. On Google's pages:
//   - Create Backup plan's Resource Type offers AlloyDB, enabled;
//   - Vaulted backups' Schedule backup, Resource Type AlloyDB, offers the
//     cluster under Clusters and applies the plan;
//   - Filter table > Resource type AlloyDB lists it;
//   - the cluster's page: Create on-demand backup takes a backup; Restore
//     (the cluster ID pre-filled with "-restore-" and a time) and
//     Point-in-time restore each make a new cluster, read back through the
//     console API with the backup or the data source it came from.
//
// The rows' data is checked by TestBackupDRAlloyDBRoundTrip (compat).
// Each screen meets WCAG 2.1 AA, and Vaulted backups opens the cluster's
// page from the keyboard.
func TestBackupDRAlloyDBRestoresThroughThePages(t *testing.T) {
	needService(t, "backupdr")
	needService(t, "alloydb")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := servedProject(t)
	loc := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	cl, vault, plan := fmt.Sprintf("bdr-cl-%d", n), fmt.Sprintf("bdr-avault-%d", n), fmt.Sprintf("bdr-aplan-%d", n)
	cluster := loc + "/clusters/" + cl
	restored, pitr := cl+"-r", cl+"-p"
	detail := func(provider string, path ...string) (int, string) {
		t.Helper()
		q := "project=" + project
		for _, s := range path {
			q += "&name=" + s
		}
		return consoleDo(t, http.MethodGet, "/api/detail/"+provider+"?"+q, "")
	}
	act := func(provider, action string, path []string) {
		t.Helper()
		body := fmt.Sprintf(`{"Path":[%q],"Action":%q,"Values":{}}`, strings.Join(path, `","`), action)
		_, _ = consoleDo(t, http.MethodPost, "/api/actions/"+provider+"?project="+project, body)
	}
	assocOf := func() *backupdr.BackupPlanAssociation {
		t.Helper()
		l, err := bdr.Projects.Locations.BackupPlanAssociations.List(loc).Do()
		if err != nil {
			t.Fatalf("backupPlanAssociations.list: %v", err)
		}
		for _, a := range l.BackupPlanAssociations {
			if a.Resource == cluster {
				return a
			}
		}
		return nil
	}
	t.Cleanup(func() {
		if a := assocOf(); a != nil {
			_, _ = bdr.Projects.Locations.BackupPlanAssociations.Delete(a.Name).Do()
		}
		for _, c := range []string{cluster, loc + "/clusters/" + restored, loc + "/clusters/" + pitr} {
			act("alloydb", "delete", []string{c})
		}
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(loc + "/backupPlans/" + plan).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(loc + "/backupVaults/" + vault).Force(true).Do()
	})

	// The cluster, on AlloyDB's page, and its primary ready.
	p := open(t)
	p.navigate("/alloydb/clusters?project=" + project)
	p.clickText("#view button", "Create cluster")
	p.submitForm(map[string]string{"clusterId": cl, "password": "burrow-pw", "instanceId": "primary"}, "clusterId")
	p.waitFor(listedRow("/alloydb/clusters", cl))
	for deadline := time.Now().Add(6 * time.Minute); ; time.Sleep(2 * time.Second) {
		_, got := detail("alloydb", cluster)
		if strings.Contains(got, `"Ready"`) && !strings.Contains(got, `"Creating"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the primary is not ready: %s", got)
		}
	}

	if _, err := bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).
		BackupVaultId(vault).Do(); err != nil {
		t.Fatalf("backupVaults.create: %v", err)
	}
	if _, err := bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: loc + "/backupVaults/" + vault,
		ResourceType: "alloydb.googleapis.com/Cluster", LogRetentionDays: 7,
		BackupRules: []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7,
			StandardSchedule: &backupdr.StandardSchedule{RecurrenceType: "DAILY", TimeZone: "UTC",
				BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 0, EndHourOfDay: 24}}}}}).BackupPlanId(plan).Do(); err != nil {
		t.Fatalf("backupPlans.create: %v", err)
	}

	// Create Backup plan offers AlloyDB.
	radios := `[...document.querySelectorAll('input[type="radio"][name$="resourceType-choice"]')]`
	p.navigate("/backupdr-plans/create?project=" + project)
	p.waitFor(radios + `.length > 0`)
	var enabled string
	p.eval(radios+`.filter((r) => !r.disabled).map((r) => r.value).join(",")`, &enabled)
	if !strings.Contains(enabled, "alloydb.googleapis.com/Cluster") {
		t.Errorf("Create Backup plan's enabled Resource Types: %s", enabled)
	}

	// Schedule backup: Resource Type AlloyDB, the cluster, the plan.
	p.navigate("/backupdr-vaulted-backups/create?project=" + project)
	p.waitFor(radios + `.some((r) => r.value === "alloydb.googleapis.com/Cluster" && !r.disabled)`)
	p.eval(radios+`.find((r) => r.value === "alloydb.googleapis.com/Cluster").click()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-cluster option")].some((o) => o.value === %q) &&
		document.querySelector("#f-cluster").getClientRects().length > 0`, cl))
	p.submitForm(map[string]string{"cluster": cl, "plan": loc + "/backupPlans/" + plan}, "cluster")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr-vaulted-backups" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, cl))
	a := assocOf()
	if a == nil || a.BackupPlan != loc+"/backupPlans/"+plan || a.DataSource == "" {
		t.Fatalf("backupPlanAssociations.list after Schedule backup: %+v", a)
	}
	ds := a.DataSource[strings.LastIndex(a.DataSource, "/")+1:]
	list := "/backupdr-vaulted-backups?project=" + project
	p.navigate(list + "&type=alloydb.googleapis.com/Cluster")
	p.waitFor(rowReady(cl))

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate(list + "&type=alloydb.googleapis.com/Cluster")
		k.waitFor(rowReady(cl))
		k.tabTo(fmt.Sprintf(`document.activeElement.tagName === "A" && document.activeElement.closest("#view tbody tr") !== null &&
			document.activeElement.textContent.includes(%q)`, cl))
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(fmt.Sprintf(`decodeURIComponent(location.pathname).includes(%q)`, ds))
		k.waitFor(`document.querySelector("#view h1") !== null`)
	})

	// Create on-demand backup on the cluster's page.
	page := "/backupdr-vaulted-backups/" + vault + "/" + ds + "?project=" + project
	p.navigate(page)
	p.waitFor(`document.querySelector("#view h1") !== null`)
	var summary string
	p.eval(`document.querySelector("#view").textContent`, &summary)
	if !strings.Contains(summary, "AlloyDB") || !strings.Contains(summary, "Cluster UID") {
		t.Errorf("the cluster's page: %q", summary)
	}
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
	mid := time.Now().UTC().Add(2 * time.Second)
	time.Sleep(4 * time.Second)

	// Restore from the backup's row: a new cluster.
	p.navigate(page)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, id)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Restore")
	p.waitFor(`document.querySelector(".modal.is-open #f-name") !== null`)
	var name string
	p.eval(`document.querySelector(".modal.is-open #f-name").value`, &name)
	if !strings.HasPrefix(name, cl+"-restore-") {
		t.Errorf("the new cluster's ID is pre-filled %q, want %s-restore- and a time", name, cl)
	}
	setField(p, ".modal.is-open", "f-name", restored)
	p.clickText(".modal.is-open button", "Restore")
	p.confirmByName(p.confirmWord())
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	waitCluster := func(id, want string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			var code int
			if code, got = detail("alloydb", loc+"/clusters/"+id); code == http.StatusOK && strings.Contains(got, `"Ready"`) {
				break
			}
		}
		if !strings.Contains(got, `"Ready"`) {
			t.Fatalf("the cluster %s restored through the page: %s", id, got)
		}
		// The cluster as AlloyDB's REST API answers it, with the source
		// published alloydbpb lacks.
		r, err := http.Get("http://" + endpoint(t, envAlloyDBBrowser) + "/v1/" + loc + "/clusters/" + id)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), want) {
			t.Errorf("the restored cluster %s does not name %s: %s", id, want, b)
		}
	}
	waitCluster(restored, backup.Name)

	// Point-in-time restore: a new cluster at a time after the backup.
	p.navigate(page)
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.pageActionWithin("Point-in-time restore", map[string]string{"time": mid.Format(time.RFC3339), "name": pitr}, p.confirmWord(), 2*time.Minute)
	waitCluster(pitr, a.DataSource)

	openRestore := fmt.Sprintf(`%s.querySelector(".overflow-trigger").click();
		[...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Restore").click()`, find)
	auditScreens(t, []a11yPage{
		{"Vaulted backups, Resource type AlloyDB", list + "&type=alloydb.googleapis.com/Cluster", rowReady(cl), ""},
		{"Schedule backup", "/backupdr-vaulted-backups/create?project=" + project, `document.querySelector("form") !== null`, ""},
		{"an AlloyDB cluster's vaulted backups", page, settled, ""},
		{"Restore an AlloyDB backup dialog", page, settled + ` && document.querySelector("#view tbody tr") !== null`, openRestore},
	})
}
