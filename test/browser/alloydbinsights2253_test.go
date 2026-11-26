//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestAlloyDBInsightsRound3ThroughThePage2253 (#2253), in the sql shard,
// whose instance serves AlloyDB, Cloud Monitoring and Cloud Logging. The
// parts #2251 left out, each read from the instance's own server:
//   - System insights' Log errors card counts the error a statement made,
//     which Logs Explorer then shows as alloydb.googleapis.com/postgres.log;
//     the Chart list has Query latency, Wait time and Node CPU usage; the
//     events timeline has the user created;
//   - Query insights' Tags table lists the sqlcommenter tags of the
//     statements run, with the Client address filter;
//   - a query's details: its Latency (P50, P95, P99), P99 latency across
//     the cluster, its Query plan samples, and a sampled plan's operations;
//   - a tag's details, with its lock wait from a statement held behind
//     an advisory lock.
//
// Every screen meets WCAG 2.1 AA, and the keyboard subtest opens a query's
// details and a plan sample from the keyboard.
func TestAlloyDBInsightsRound3ThroughThePage2253(t *testing.T) {
	needService(t, "alloydb")
	needService(t, "monitoring-explorer")
	needService(t, "logging-buckets")
	project := uniqueProject(t)
	cluster := "projects/" + project + "/locations/us-central1/clusters/ins3-cl"
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/alloydb?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	ready := func() {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for d := detail(cluster); !strings.Contains(d, `"Ready"`) || strings.Contains(d, `"Creating"`) ||
			strings.Contains(d, `{"label":"Status","value":"Maintenance"}`); d = detail(cluster) {
			if time.Now().After(deadline) {
				t.Fatalf("the cluster is not ready: %s", d)
			}
			time.Sleep(2 * time.Second)
		}
	}
	// studio runs sql as postgres through AlloyDB Studio, its result's
	// text and whether it failed.
	studio := func(sql string) (string, bool) {
		t.Helper()
		path := "/api/studio/alloydb?project=" + url.QueryEscape(project) + "&call="
		b, _ := json.Marshal(map[string]any{"instance": cluster, "database": "postgres", "user": "postgres", "password": "burrow-pw"})
		code, body := consoleDo(t, http.MethodPost, path+"signin", string(b))
		var s struct{ Session string }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil || s.Session == "" {
			t.Errorf("Studio sign-in = %d: %s", code, body)
			return body, true
		}
		defer func() {
			b, _ := json.Marshal(map[string]any{"instance": cluster, "session": s.Session})
			_, _ = consoleDo(t, http.MethodPost, path+"signout", string(b))
		}()
		b, _ = json.Marshal(map[string]any{"instance": cluster, "session": s.Session, "sql": sql})
		code, body = consoleDo(t, http.MethodPost, path+"run", string(b))
		return body, code != http.StatusOK || strings.Contains(body, `"error"`)
	}
	run := func(sql string) string {
		t.Helper()
		body, failed := studio(sql)
		if failed {
			t.Fatalf("Studio run of %q: %s", sql, body)
		}
		return body
	}

	p := open(t)
	p.navigate("/alloydb/clusters/create?project=" + project)
	p.waitFor(`document.getElementById("f-clusterId") !== null`)
	p.submitForm(map[string]string{"clusterId": "ins3-cl", "password": "burrow-pw", "version": "POSTGRES_16", "instanceId": "primary"}, "clusterId")
	p.waitFor(listedRow("/alloydb/clusters", "ins3-cl"))
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{cluster}, "Action": "delete", "Values": map[string]string{}})
		_, _ = consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b))
	})
	ready()

	// A user account: the events timeline's USER_CREATE.
	b, _ := json.Marshal(map[string]any{"Path": []string{cluster}, "Action": "add-user",
		"Values": map[string]string{"user": "ins3-user", "type": "ALLOYDB_BUILT_IN", "password": "ins3-pw"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b)); code != http.StatusOK {
		t.Fatalf("Add user account = %d: %s", code, body)
	}

	// The workload: a tagged query (sqlcommenter), an error, and a tagged
	// statement held behind an advisory lock for four seconds. Twice, a
	// sampler's minute apart, so the latency DELTAs have a baseline.
	run("CREATE TABLE ins3_items (id int PRIMARY KEY, name text)")
	run("INSERT INTO ins3_items SELECT n, 'item-' || n FROM generate_series(1, 200) AS n")
	tagged := func(n int) string {
		return fmt.Sprintf("SELECT name FROM ins3_items WHERE id < %d /*action='list',application='shop',route='%%2Fitems',db_driver='studio'*/", n)
	}
	workload := func() {
		for i := 0; i < 6; i++ {
			run(tagged(20 + i))
		}
		if body, failed := studio("SELECT * FROM ins3_missing_table"); !failed {
			t.Fatalf("the missing table's SELECT did not fail: %s", body)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = studio("SELECT pg_advisory_lock(2253), pg_sleep(4), pg_advisory_unlock(2253)")
		}()
		time.Sleep(time.Second)
		run("SELECT pg_advisory_lock(2253), pg_advisory_unlock(2253) /*application='locker'*/")
		wg.Wait()
	}
	workload()
	time.Sleep(70 * time.Second)
	workload()

	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	clusterPath = strings.Split(clusterPath, "&tab=")[0]

	// Query insights' Tags table: the tagged statements' application,
	// route and action, called at least twelve times.
	tagsPath := clusterPath + "&tab=query-insights&subtab=qt-primary"
	deadline := time.Now().Add(3 * time.Minute)
	for d := detail(cluster); !regexp.MustCompile(`"Application":"shop"[^}]*"Times called":"(1[2-9]|[2-9][0-9])"|"Times called":"(1[2-9]|[2-9][0-9])"[^}]*"Application":"shop"`).MatchString(d); d = detail(cluster) {
		if time.Now().After(deadline) {
			t.Fatalf("the Tags table has no shop tag called 12 times: %s", d)
		}
		time.Sleep(5 * time.Second)
	}
	p.navigate(tagsPath)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("shop") && r.textContent.includes("/items"))`)
	var heads []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter((h) => h)`, &heads)
	for _, want := range []string{"Action", "Controller", "Framework", "Route", "Application", "DB Driver", "Database", "Load by total time", "Avg execution time (ms)", "Times called"} {
		if !strings.Contains(strings.Join(heads, "|"), want) {
			t.Errorf("the Tags table has no %s column: %v", want, heads)
		}
	}
	var menus []string
	p.eval(`[...document.querySelectorAll("#view .list-menus .list-menu-label > span")].map((m) => m.textContent.trim())`, &menus)
	if !strings.Contains(strings.Join(menus, "|"), "Client address") {
		t.Errorf("the Tags table has no Client address filter: %v", menus)
	}

	// The locker tag's details: its lock wait, sampled from
	// pg_stat_activity while it waited.
	var lockerHref string
	p.eval(`(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("locker")); return r ? r.querySelector("a").getAttribute("href") : ""; })()`, &lockerHref)
	if lockerHref == "" {
		t.Fatal("the Tags table has no locker row")
	}
	p.navigate(lockerHref)
	p.waitFor(`document.querySelector("#view").textContent.includes("Database load, specific tags") && document.querySelector("#view").textContent.includes("locker")`)
	tagPath := lockerHref

	// A query's details, from the Queries table.
	qiPath := clusterPath + "&tab=query-insights&subtab=qi-primary"
	p.navigate(qiPath)
	findQuery := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("FROM ins3_items WHERE id <"))`
	p.waitFor(findQuery + ` !== undefined && ` + findQuery + `.querySelector("a") !== null`)
	var queryHref string
	p.eval(findQuery+`.querySelector("a").getAttribute("href")`, &queryHref)
	p.navigate(queryHref)
	p.waitFor(`document.querySelector("#view").textContent.includes("Database load, specific query")`)
	// Its path's last segment is the query_hash.
	m := regexp.MustCompile(`/query/primary/([0-9]+)\?`).FindStringSubmatch(queryHref)
	if m == nil {
		t.Fatalf("the query's link carries no query hash: %s", queryHref)
	}
	hash := m[1]
	queryPath := []string{cluster, "query", "primary", hash}
	// Latency: P50, P95 and P99 series, and the primary's P99 across the
	// cluster, once the sampler has its two readings.
	deadline = time.Now().Add(4 * time.Minute)
	for d := detail(queryPath...); !strings.Contains(d, `"P99 (ms)":"`); d = detail(queryPath...) {
		if time.Now().After(deadline) {
			t.Fatalf("the query's P99 latency across the cluster has no reading: %s", d)
		}
		time.Sleep(10 * time.Second)
	}
	d := detail(queryPath...)
	for _, want := range []string{`"label":"P50"`, `"label":"P95"`, `"label":"P99"`, `"seriesTitle":"Latency"`, `P99 latency across the cluster`, `"Query plan samples"`} {
		if !strings.Contains(d, want) {
			t.Errorf("the query's details lack %s: %s", want, d)
		}
	}
	p.navigate(queryHref + "&tab=query-latency")
	p.waitFor(`document.querySelector("#view .query-load-chart h2") !== null && document.querySelector("#view .query-load-chart h2").textContent === "Latency"`)
	// Query plan samples: auto_explain's, at most five a minute.
	plansPath := queryHref + "&tab=query-plans"
	p.navigate(plansPath)
	findPlan := `[...document.querySelectorAll("#view tbody tr a")].find((a) => /UTC$/.test(a.textContent.trim()))`
	p.waitForWithin(findPlan+` !== undefined`, 2*time.Minute)
	var planHref string
	p.eval(findPlan+`.getAttribute("href")`, &planHref)
	p.navigate(planHref)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].length > 0 && document.querySelector("#view").textContent.includes("Latency (ms)")`)
	var ops []string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent)`, &ops)
	if !strings.Contains(strings.Join(ops, "|"), "Scan") {
		t.Errorf("the plan has no scan operation: %v", ops)
	}

	// System insights: Log errors counts the missing table's errors and
	// opens Logs Explorer on them.
	siCluster := clusterPath + "&tab=system-insights&subtab=si-cluster"
	deadline = time.Now().Add(2 * time.Minute)
	logErrors := regexp.MustCompile(`"label":"Log errors","value":"([0-9]+)"`)
	for d := detail(cluster); ; d = detail(cluster) {
		if m := logErrors.FindStringSubmatch(d); m != nil {
			if n, _ := strconv.Atoi(m[1]); n >= 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Log errors does not count the two errors: %s", d)
		}
		time.Sleep(5 * time.Second)
	}
	if d := detail(cluster); !strings.Contains(d, "USER_CREATE on ins3-user") {
		t.Errorf("the events timeline has no USER_CREATE: %s", d)
	}
	p.navigate(siCluster)
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.closest(".properties, dl, .property") && /^[0-9]+$/.test(a.textContent.trim()) && a.getAttribute("href").startsWith("/logs?"))`)
	var logsHref string
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.getAttribute("href").startsWith("/logs?") && /^[0-9]+$/.test(a.textContent.trim())).getAttribute("href")`, &logsHref)
	p.navigate(logsHref)
	p.waitForWithin(`document.querySelector("#view").textContent.includes("ins3_missing_table")`, 2*time.Minute)
	var charts []string
	p.navigate(clusterPath + "&tab=system-insights&subtab=si-primary")
	p.waitFor(`document.querySelector("#view #overview-chart-metric") !== null`)
	p.eval(`[...document.querySelectorAll("#view #overview-chart-metric option")].map((o) => o.textContent)`, &charts)
	for _, want := range []string{"Query latency - P99", "Wait time", "Wait count", "Node CPU usage", "Node database availability"} {
		if !strings.Contains(strings.Join(charts, "|"), want) {
			t.Errorf("the primary's Chart list has no %s: %v", want, charts)
		}
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		// A query's details, opened from the Queries table by the keyboard.
		k.navigate(qiPath)
		k.waitFor(findQuery + ` !== undefined`)
		k.tabTo(`document.activeElement.tagName === "A" && document.activeElement.closest("tbody") !== null && document.activeElement.textContent.includes("FROM ins3_items")`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#view").textContent.includes("Database load, specific query")`)
		// A plan sample, by the keyboard.
		k.navigate(plansPath)
		k.waitFor(findPlan + ` !== undefined`)
		k.tabTo(`document.activeElement.tagName === "A" && /UTC$/.test(document.activeElement.textContent.trim())`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#view").textContent.includes("Latency (ms)")`)
	})

	auditScreens(t, []a11yPage{
		{"System insights, cluster, with Log errors", siCluster, settled, ""},
		{"System insights, primary, with its latency and node charts", clusterPath + "&tab=system-insights&subtab=si-primary", settled, ""},
		{"Query insights tags", tagsPath, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
		{"a query's details", queryHref, settled, ""},
		{"a query's latency", queryHref + "&tab=query-latency", settled, ""},
		{"a query's plan samples", plansPath, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
		{"a sampled plan", planHref, settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
		{"a tag's details", tagPath, settled, ""},
	})
}
