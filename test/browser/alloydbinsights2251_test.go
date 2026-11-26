//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestAlloyDBInsightsThroughThePage2251 (#2251, round 2), in the sql shard,
// whose instance serves AlloyDB and Cloud Monitoring. Google's AlloyDB
// console:
//   - Query insights: the primary's queries from pg_stat_statements, the
//     query this test ran listed with the times it was called, the Database
//     and User filters and the Database load chart; its Edit (Query
//     lengths, Query plan sampling, ...), ending in Update instance, read
//     back;
//   - the Overview's Maintenance window Edit (a day and an hour, UTC),
//     ending in Update, read back;
//   - an instance row's Edit, whose database flags picker (Add flag, a flag
//     from the New database flag list, its value) sets a flag the server
//     then runs with;
//   - System insights: the Cluster (all instances) and primary views, their
//     summary cards from the instance's Cloud Monitoring series once the
//     sampler has read the server, the Chart list, and the events timeline;
//   - Create backup's Description and Labels, and the Backups page's Edit
//     labels, read back.
//
// Every screen and dialog meets WCAG 2.1 AA, and the keyboard subtest
// opens the Maintenance Edit and Query insights' Edit from the keyboard.
func TestAlloyDBInsightsThroughThePage2251(t *testing.T) {
	needService(t, "alloydb")
	needService(t, "monitoring-explorer")
	project := uniqueProject(t)
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/alloydb?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	cluster := "projects/" + project + "/locations/us-central1/clusters/ins-cl"
	ready := func() {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for d := detail(cluster); !strings.Contains(d, `"Ready"`) || strings.Contains(d, `"Creating"`) ||
			strings.Contains(d, `"Status":"Maintenance"`) || strings.Contains(d, `{"label":"Status","value":"Maintenance"}`); d = detail(cluster) {
			if time.Now().After(deadline) {
				t.Fatalf("the cluster is not ready: %s", d)
			}
			time.Sleep(2 * time.Second)
		}
	}
	// run runs sql as postgres through AlloyDB Studio and answers its
	// result's text.
	run := func(sql string) string {
		t.Helper()
		path := "/api/studio/alloydb?project=" + url.QueryEscape(project) + "&call="
		b, _ := json.Marshal(map[string]any{"instance": cluster, "database": "postgres", "user": "postgres", "password": "burrow-pw"})
		code, body := consoleDo(t, http.MethodPost, path+"signin", string(b))
		var s struct{ Session string }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil || s.Session == "" {
			t.Fatalf("Studio sign-in = %d: %s", code, body)
		}
		defer func() {
			b, _ := json.Marshal(map[string]any{"instance": cluster, "session": s.Session})
			_, _ = consoleDo(t, http.MethodPost, path+"signout", string(b))
		}()
		b, _ = json.Marshal(map[string]any{"instance": cluster, "session": s.Session, "sql": sql})
		code, body = consoleDo(t, http.MethodPost, path+"run", string(b))
		if code != http.StatusOK || strings.Contains(body, `"error"`) {
			t.Fatalf("Studio run of %q = %d: %s", sql, code, body)
		}
		return body
	}

	p := open(t)
	p.navigate("/alloydb/clusters/create?project=" + project)
	p.waitFor(`document.getElementById("f-clusterId") !== null`)
	p.submitForm(map[string]string{"clusterId": "ins-cl", "password": "burrow-pw", "version": "POSTGRES_16", "instanceId": "primary"}, "clusterId")
	p.waitFor(listedRow("/alloydb/clusters", "ins-cl"))
	t.Cleanup(func() {
		for _, b := range []string{"ins-bk"} {
			bb, _ := json.Marshal(map[string]any{"Path": []string{b}, "Action": "delete", "Values": map[string]string{}})
			_, _ = consoleDo(t, http.MethodPost, "/api/actions/alloydb-backups?project="+url.QueryEscape(project), string(bb))
		}
		b, _ := json.Marshal(map[string]any{"Path": []string{cluster}, "Action": "delete", "Values": map[string]string{}})
		_, _ = consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b))
	})
	ready()

	// A workload the server's pg_stat_statements records: one query run
	// three times.
	run("CREATE TABLE ins_items (id int PRIMARY KEY, name text)")
	run("INSERT INTO ins_items SELECT n, 'item-' || n FROM generate_series(1, 50) AS n")
	for i := 0; i < 3; i++ {
		run(fmt.Sprintf("SELECT name FROM ins_items WHERE id < %d", 10+i))
	}

	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	clusterPath = strings.Split(clusterPath, "&tab=")[0]

	// Query insights: the primary's queries, Google's columns, the query
	// above called three times.
	qiPath := clusterPath + "&tab=query-insights&subtab=qi-primary"
	p.navigate(qiPath)
	p.waitFor(`document.querySelector("#subtab-qi-primary") !== null`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("FROM ins_items WHERE id <"))`)
	var heads []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter((h) => h)`, &heads)
	for _, want := range []string{"Query string", "Database", "User", "Load by total time", "Avg execution time (ms)", "Times called", "Avg rows fetched"} {
		if !strings.Contains(strings.Join(heads, "|"), want) {
			t.Errorf("Query insights has no %s column: %v", want, heads)
		}
	}
	var called string
	p.eval(`(() => { const hs = [...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim());
		const i = hs.indexOf("Times called");
		const r = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("FROM ins_items WHERE id <"));
		return r.children[i].textContent.trim(); })()`, &called)
	if n, err := strconv.Atoi(called); err != nil || n < 3 {
		t.Errorf("the query run three times reads Times called %q", called)
	}
	p.waitFor(`document.querySelector("#view .query-load-chart h2").textContent === "Database load - all top queries"`)
	p.waitFor(`[...document.querySelectorAll("#view .list-menus select, #view .list-menus button")].length >= 2`)

	// Query insights' Edit, read back from the instance.
	p.pageAction("Edit", map[string]string{"queryStringLength": "2048", "queryPlansPerMinute": "10", "recordApplicationTags": "false"}, "")
	ready()
	if d := detail(cluster); !strings.Contains(d, `"name":"queryStringLength","label":"Query lengths"`) ||
		!strings.Contains(d, `"default":"2048"`) {
		t.Errorf("Query lengths does not read back as 2048: %s", d)
	}

	// Maintenance: Edit beside the window, a day and an hour, Update.
	p.navigate(clusterPath + "&tab=overview")
	p.waitFor(`document.querySelector("#view a.property-action[aria-label='Edit maintenance window']") !== null`)
	p.eval(`document.querySelector("#view a.property-action[aria-label='Edit maintenance window']").click()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-day") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-day"); f.value = "TUESDAY"; f.dispatchEvent(new Event("change", { bubbles: true }));
		const h = document.querySelector(".modal.is-open #f-hour"); h.value = "23"; h.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Update")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	if d := detail(cluster); !strings.Contains(d, `{"label":"Maintenance window","value":"Tuesday, 23:00 UTC"`) {
		t.Errorf("the maintenance window does not read back: %s", d)
	}
	ready()

	// An instance row's Edit: Add flag, work_mem from the list, its value,
	// Update instance; the server then runs with it.
	p.navigate(clusterPath + "&tab=instances")
	openInstanceEdit := `(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "primary"));
		r.querySelector(".overflow-trigger").click();
		[...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Edit").click(); })()`
	p.waitFor(`document.querySelector("#view tbody tr .overflow-trigger") !== null`)
	p.eval(openInstanceEdit, nil)
	p.waitFor(`document.querySelector(".modal.is-open .group-add") !== null`)
	p.clickText(".modal.is-open .group-add", "Add flag")
	p.eval(`(() => { const rows = document.querySelectorAll(".modal.is-open .group-row"); const r = rows[rows.length - 1];
		const s = r.querySelector("select"); if (![...s.options].some((o) => o.value === "work_mem")) return false;
		s.value = "work_mem"; s.dispatchEvent(new Event("change", { bubbles: true }));
		const v = r.querySelector("input"); v.value = "8192"; v.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Update instance")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, 2*time.Minute)
	ready()
	deadline := time.Now().Add(5 * time.Minute)
	for got := run("SHOW work_mem"); !strings.Contains(got, "8MB"); got = run("SHOW work_mem") {
		if time.Now().After(deadline) {
			t.Fatalf("the server does not run with work_mem set: %s", got)
		}
		time.Sleep(5 * time.Second)
		ready()
	}

	// Create backup with its Description and Labels.
	p.navigate(clusterPath + "&tab=backups")
	p.pageActionWithin("Create backup", map[string]string{"backupId": "ins-bk", "description": "before the flags", "labels": "env=test"}, "", 5*time.Minute)
	backups := func() string {
		code, body := consoleDo(t, http.MethodGet, "/api/resources/alloydb-backups?project="+url.QueryEscape(project), "")
		if code != http.StatusOK {
			t.Fatalf("list the backups = %d: %s", code, body)
		}
		return body
	}
	deadline = time.Now().Add(5 * time.Minute)
	for b := backups(); !strings.Contains(b, "ins-bk") || !strings.Contains(b, "env: test") || !strings.Contains(b, "Ready"); b = backups() {
		if time.Now().After(deadline) {
			t.Fatalf("the backup is not listed ready with its label: %s", b)
		}
		time.Sleep(3 * time.Second)
	}
	// The Backups page's Edit labels.
	openEditLabels := `(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("ins-bk"));
		r.querySelector(".overflow-trigger").click();
		[...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Edit labels").click(); })()`
	p.navigate("/alloydb/backups?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ins-bk"))`)
	p.eval(openEditLabels, nil)
	p.waitFor(`document.querySelector(".modal.is-open #f-labels") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-labels"); f.value = "env=prod\nteam=db"; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Save")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	if b := backups(); !strings.Contains(b, "env: prod") || !strings.Contains(b, "team: db") {
		t.Errorf("the backup's labels do not read back: %s", b)
	}

	// System insights: the sampler reads the server every 60 seconds;
	// once it has twice, the summary's CPU utilization is a reading.
	deadline = time.Now().Add(6 * time.Minute)
	for d := detail(cluster); !strings.Contains(d, `"label":"CPU utilization - P99 | P50","value":"`) ||
		strings.Contains(d, `"label":"CPU utilization - P99 | P50","value":"No readings yet`); d = detail(cluster) {
		if time.Now().After(deadline) {
			t.Fatalf("System insights has no CPU readings: %s", d)
		}
		time.Sleep(10 * time.Second)
	}
	if d := detail(cluster); !strings.Contains(d, `"label":"Peak connections","value":"`) || !strings.Contains(d, "INSTANCE_UPDATE on primary") {
		t.Errorf("System insights' summary or events timeline: %s", d)
	}
	siPath := clusterPath + "&tab=system-insights&subtab=si-primary"
	p.navigate(siPath)
	p.waitFor(`document.querySelector("#subtab-si-cluster") !== null && document.querySelector("#subtab-si-primary") !== null`)
	p.waitFor(`document.querySelector("#view #overview-chart-metric") !== null && document.querySelector("#view").textContent.includes("Events timeline")`)
	var charts []string
	p.eval(`[...document.querySelectorAll("#view #overview-chart-metric option")].map((o) => o.textContent)`, &charts)
	for _, want := range []string{"Max CPU utilization", "Available memory", "Number of connections", "Transaction ID utilization"} {
		if !strings.Contains(strings.Join(charts, "|"), want) {
			t.Errorf("the primary's Chart list has no %s: %v", want, charts)
		}
	}
	p.navigate(clusterPath + "&tab=system-insights&subtab=si-cluster")
	p.waitFor(`document.querySelector("#view").textContent.includes("Active node count") && document.querySelector("#view").textContent.includes("Storage")`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		// The Maintenance window's Edit, reached and opened by the keyboard.
		k.navigate(clusterPath + "&tab=overview")
		k.waitFor(`document.querySelector("#view a.property-action[aria-label='Edit maintenance window']") !== null`)
		k.tabTo(`document.activeElement.getAttribute("aria-label") === "Edit maintenance window"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-day") !== null`)
		k.waitFor(`document.querySelector(".modal.is-open").contains(document.activeElement)`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
		// Query insights' Edit, by the keyboard.
		k.navigate(qiPath)
		k.waitFor(`document.querySelector("#view .section-actions button") !== null`)
		k.tabTo(`document.activeElement.tagName === "BUTTON" && document.activeElement.closest(".section-actions") !== null && document.activeElement.textContent.trim() === "Edit"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-queryStringLength") !== null`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
	})

	openMaintenance := `document.querySelector("#view a.property-action[aria-label='Edit maintenance window']").click()`
	openQIEdit := `[...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent.trim() === "Edit").click()`
	auditScreens(t, []a11yPage{
		{"AlloyDB overview with its chart and maintenance", clusterPath + "&tab=overview", settled + ` && document.querySelector("#view #overview-chart-metric") !== null`, ""},
		{"maintenance window dialog", clusterPath + "&tab=overview", settled, openMaintenance},
		{"System insights, cluster", clusterPath + "&tab=system-insights&subtab=si-cluster", settled, ""},
		{"System insights, primary", siPath, settled, ""},
		{"Query insights", qiPath, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
		{"Query insights edit dialog", qiPath, settled + ` && document.querySelector("#view .section-actions button") !== null`, openQIEdit},
		{"instance edit dialog with flags", clusterPath + "&tab=instances", settled + ` && document.querySelector("#view tbody tr .overflow-trigger") !== null`, openInstanceEdit},
		screen("AlloyDB backups", "/alloydb/backups", project),
		{"edit backup labels dialog", "/alloydb/backups?project=" + project, settled + ` && document.querySelector("#view tbody tr .overflow-trigger") !== null`, openEditLabels},
	})
}
