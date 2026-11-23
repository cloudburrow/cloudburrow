//go:build browser

package browser

import (
	"encoding/json"
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
