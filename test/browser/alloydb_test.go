//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestAlloyDBThroughThePage (#2191), in the sql shard, whose instance
// serves AlloyDB. Reached from AlloyDB's menu under Databases: Clusters'
// Create cluster (a cluster and its primary instance), the cluster's page
// with its Overview, Instances, Backups, Users and Databases; Add user
// account and Create backup from the cluster's page; the backup on
// Backups. The console API reads each result back. Every screen meets
// WCAG 2.1 AA, and the Clusters list opens a cluster from the keyboard.
func TestAlloyDBThroughThePage(t *testing.T) {
	needService(t, "alloydb")
	project := uniqueProject(t)
	detail := func(provider string, path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/"+provider+"?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	p := open(t)
	p.navigateByDrawer("Databases", "/alloydb/clusters", "AlloyDB")
	p.waitFor(`[...document.querySelectorAll('a[href^="/alloydb/backups"]')].some((a) => a.textContent.includes("Backups"))`)

	p.navigate("/alloydb/clusters?project=" + project)
	p.clickText("#view button", "Create cluster")
	p.waitFor(`["f-clusterId", "f-password", "f-instanceId", "f-cpu"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"clusterId": "browser-cl", "password": "burrow-pw", "instanceId": "primary"}, "clusterId")
	cluster := "projects/" + project + "/locations/us-central1/clusters/browser-cl"
	p.waitFor(listedRow("/alloydb/clusters", "browser-cl"))
	if got := detail("alloydb", cluster); !strings.Contains(got, "PostgreSQL 16") || !strings.Contains(got, "us-central1") {
		t.Errorf("the cluster reads back as %s", got)
	}
	// The primary starts a PostgreSQL server: wait until it is Ready.
	deadline := time.Now().Add(6 * time.Minute)
	for !strings.Contains(detail("alloydb", cluster), `"Ready"`) || strings.Contains(detail("alloydb", cluster), `"Creating"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the primary is not ready: %s", detail("alloydb", cluster))
		}
		time.Sleep(2 * time.Second)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/alloydb/clusters?project=" + project)
		k.waitFor(`document.querySelector("#view tbody tr a") !== null`)
		k.tabTo(`document.activeElement.closest("#view tbody tr") !== null && document.activeElement.tagName === "A"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`decodeURIComponent(location.pathname).includes("browser-cl")`)
		k.waitFor(`[...document.querySelectorAll("#view [role=tab]")].some((t) => t.textContent.trim() === "Users")`)
		// The tab strip moves with the arrow keys, as Google's does.
		k.eval(`(() => { const t = [...document.querySelectorAll("#view [role=tab]")].find((t) => t.getAttribute("aria-selected") === "true"); t.focus(); return true; })()`, nil)
		k.run(chromedp.KeyEvent(kb.ArrowRight))
		var selected string
		k.eval(`document.activeElement.getAttribute("role") + "/" + document.activeElement.textContent.trim()`, &selected)
		if !strings.HasPrefix(selected, "tab/") || strings.HasSuffix(selected, "/Overview") {
			t.Errorf("ArrowRight on the Overview tab focused %q", selected)
		}
	})

	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.clickText(`#view [role="tab"]`, "Overview")
	p.waitFor(`document.querySelector("#view").textContent.includes("primary")`)

	where := p.findAction("Add user account")
	if where == "" {
		t.Fatal("the cluster's page offers no Add user account")
	}
	p.clickText(where, "Add user account")
	p.submitForm(map[string]string{"user": "browser-user", "password": "user-pw"}, "user")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if got := detail("alloydb", cluster); !strings.Contains(got, "browser-user") {
		t.Errorf("the user is not on the cluster's Users: %s", got)
	}

	where = p.findAction("Create backup")
	if where == "" {
		t.Fatal("the cluster's page offers no Create backup")
	}
	p.clickText(where, "Create backup")
	p.submitForm(map[string]string{"backupId": "browser-bk"}, "backupId")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.navigate("/alloydb/backups?project=" + project)
	p.waitFor(listedRow("/alloydb/backups", "browser-bk"))

	auditScreens(t, []a11yPage{
		screen("AlloyDB clusters", "/alloydb/clusters", project),
		screen("create cluster", "/alloydb/clusters/create", project),
		{"cluster", clusterPath, settled, ""},
		screen("AlloyDB backups", "/alloydb/backups", project),
	})

	b, _ := json.Marshal(map[string]any{"Path": []string{cluster}, "Action": "delete", "Values": map[string]string{}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b)); code != http.StatusOK {
		t.Errorf("Delete cluster through the console API = %d: %s", code, body)
	}
}

// TestAlloyDBReplicationAndTransfersThroughThePage (#2191), in the sql
// shard, whose instance serves AlloyDB and Cloud Storage. On a cluster's
// page: Export writes the postgres database to a Cloud Storage object,
// which the Storage client reads, and Import loads it back; Edit instance
// turns on Managed Connection Pooling, which the Instances list shows;
// Create secondary cluster makes a secondary cluster in another region,
// whose page shows it replicating the primary, and its Promote, confirmed
// by its name, makes it a primary cluster. Each screen and dialog meets
// WCAG 2.1 AA, and Export opens from the keyboard.
func TestAlloyDBReplicationAndTransfersThroughThePage(t *testing.T) {
	needService(t, "alloydb")
	needService(t, "storage")
	project := uniqueProject(t)
	gcs, _ := storageSDK(t)
	bucket := fmt.Sprintf("cb-adb-%d", time.Now().UnixNano()%1e9)
	regionalBucket(t, gcs, project, bucket)
	t.Cleanup(func() { _ = gcs.Bucket(bucket).Object("browser.sql").Delete(context.Background()) })
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/alloydb?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	ready := func(name string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		busy := func(d string) bool {
			for _, s := range []string{"CREATING", "BOOTSTRAPPING", "MAINTENANCE", "PROMOTING", "UPDATING", "Creating", "Maintenance"} {
				if strings.Contains(d, `"`+s+`"`) {
					return true
				}
			}
			return !strings.Contains(d, `"Ready"`)
		}
		for d := detail(name); busy(d); d = detail(name) {
			if time.Now().After(deadline) {
				t.Fatalf("%s is not ready: %s", name, d)
			}
			time.Sleep(2 * time.Second)
		}
	}
	act := func(path, action string, values map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": []string{path}, "Action": action, "Values": values})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b)); code != http.StatusOK {
			t.Errorf("%s %s through the console API = %d: %s", action, path, code, body)
		}
	}

	p := open(t)
	p.navigate("/alloydb/clusters/create?project=" + project)
	p.submitForm(map[string]string{"clusterId": "xfer-cl", "password": "burrow-pw", "instanceId": "primary"}, "clusterId")
	cluster := "projects/" + project + "/locations/us-central1/clusters/xfer-cl"
	secondary := "projects/" + project + "/locations/us-east1/clusters/xfer-sec"
	p.waitFor(listedRow("/alloydb/clusters", "xfer-cl"))
	ready(cluster)
	t.Cleanup(func() { act(secondary, "delete", map[string]string{}); act(cluster, "delete", map[string]string{}) })

	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate(clusterPath)
		where := k.findAction("Export")
		if where == "" {
			t.Fatal("the cluster's page offers no Export")
		}
		k.eval(fmt.Sprintf(`(() => { [...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === "Export").focus(); return true; })()`, where), nil)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-uri") !== null`)
		k.waitFor(`document.querySelector(".modal.is-open").contains(document.activeElement)`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
	})

	uri := "gs://" + bucket + "/browser.sql"
	p.pageActionWithin("Export", map[string]string{"uri": uri, "database": "postgres", "clean": "true"}, "", 3*time.Minute)
	r, err := gcs.Bucket(bucket).Object("browser.sql").NewReader(context.Background())
	if err != nil {
		t.Fatalf("the export wrote no %s: %v", uri, err)
	}
	dump, _ := io.ReadAll(r)
	r.Close()
	if !strings.Contains(string(dump), "PostgreSQL database dump") {
		t.Errorf("%s is not a PostgreSQL dump: %.200s", uri, dump)
	}
	p.pageActionWithin("Import", map[string]string{"uri": uri, "database": "postgres"}, "", 3*time.Minute)

	p.pageActionWithin("Edit instance", map[string]string{"instance": "primary", "pooling": "true", "poolFlags": "max_pool_size=10"}, "", 3*time.Minute)
	ready(cluster)
	if got := detail(cluster); !strings.Contains(got, `"Connection pooling":"Enabled"`) {
		t.Errorf("the Instances list does not show pooling enabled: %s", got)
	}

	p.pageAction("Create secondary cluster", map[string]string{"clusterId": "xfer-sec", "region": "us-east1", "instanceId": "replica"}, "")
	ready(secondary)
	if got := detail(secondary); !strings.Contains(got, `{"label":"Primary cluster","value":"`+cluster+`"}`) {
		t.Errorf("the secondary cluster's page does not name its primary: %s", got)
	}
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("xfer-sec")`)

	p.navigate("/alloydb/clusters?project=" + project)
	p.waitFor(listedRow("/alloydb/clusters", "xfer-sec"))
	p.follow(secondary)
	var secondaryPath string
	p.eval(`location.pathname + location.search`, &secondaryPath)
	// Each dialog opens from the button beside the title or over a tab.
	openAction := func(label string) string {
		return fmt.Sprintf(`(() => { for (const t of [null, ...document.querySelectorAll("#view .tab-strip [role=tab]")]) {
			if (t) t.click();
			const b = [...document.querySelectorAll("#view .page-actions button, #view .section-actions button")]
				.find((b) => b.textContent.trim() === %q && b.getClientRects().length);
			if (b) { b.click(); return; } } })()`, label)
	}
	auditScreens(t, []a11yPage{
		{"cluster with a secondary", clusterPath, settled, ""},
		{"export dialog", clusterPath, settled, openAction("Export")},
		{"import dialog", clusterPath, settled, openAction("Import")},
		{"create secondary cluster dialog", clusterPath, settled, openAction("Create secondary cluster")},
		{"secondary cluster", secondaryPath, settled, ""},
		{"switchover dialog", secondaryPath, settled, openAction("Switchover")},
	})
	// Promote, confirmed by the cluster's ID; the dialog stays open, its
	// confirmation's hint showing, while the server is promoted.
	p.navigate(secondaryPath)
	where := p.findAction("Promote")
	if where == "" {
		t.Fatal("the secondary cluster's page offers no Promote")
	}
	p.clickText(where, "Promote")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.confirmByName("xfer-sec")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, 3*time.Minute)
	ready(secondary)
	if got := detail(secondary); !strings.Contains(got, `{"label":"Cluster type","value":"Primary"}`) || strings.Contains(got, `"Primary cluster"`) {
		t.Errorf("the promoted cluster reads back as %s", got)
	}
}
