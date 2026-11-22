//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBigQueryStudioExplorerEditorAndHistory (#1861), in the emulators
// shard. BigQuery's menu is Google's (Studio, Search, Datasets, Job
// history, Data transfers, Scheduled queries, Analytics Hub, Dataform,
// Partner Center, Settings), and the drawer's BigQuery row opens Studio.
// In Studio a CREATE TABLE … AS SELECT asks to be confirmed and then runs;
// the Explorer shows the project, the dataset and the table, its search
// narrows the tree and a star survives a redraw; the table's View actions
// Query opens a new SQL query tab; the validator's dry run says how much
// the query will process; Run shows Query results with Job information,
// Results, Visualization, JSON, Execution details and Execution graph; Edit >
// Query settings' destination table writes the results to a table; Schedule is
// drawn disabled with its reason; the Explorer's Job history opens a tab
// whose Personal and Project history list the job, with Open as new query. A page CloudBurrow does not serve says
// "Not available locally", and the table's page has Google's tabs in
// Google's order.
func TestBigQueryStudioExplorerEditorAndHistory(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_studio_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector("#product-nav a[aria-current=page]") !== null`)
	var menu []string
	p.eval(`[...document.querySelectorAll("#product-nav li a")].map((a) => a.textContent)`, &menu)
	want := []string{"Studio", "Search", "Datasets", "Job history", "Data transfers", "Scheduled queries",
		"Analytics Hub", "Partner Center", "Settings"}
	// Dataform's three pages are in the menu only where it is served
	// (#2105), which the emulators shard's instance is not.
	if hasService(t, "dataform") {
		want = append(want[:7:7], "Dataform", "Dataform folders", "Dataform settings", "Partner Center", "Settings")
	}
	if strings.Join(menu, "|") != strings.Join(want, "|") {
		t.Errorf("BigQuery's menu is %q, want %q", menu, want)
	}
	var current string
	p.eval(`document.querySelector("#product-nav a[aria-current=page]").textContent`, &current)
	if current != "Studio" {
		t.Errorf("Studio's menu marks %q", current)
	}

	// The Explorer: the project, then the dataset. Studio opens on Home,
	// whose Create new has SQL query.
	node := func(key string) string { return fmt.Sprintf(`document.querySelector('[data-node=%q]')`, key) }
	p.waitFor(node(project) + ` !== null`)
	p.waitFor(node(project+"."+ds) + ` !== null`)
	p.clickText(".bq-home-action", "SQL query")
	p.waitFor(`document.querySelector(".bq-editor") !== null`)

	// A statement that changes data is confirmed first.
	create := fmt.Sprintf("CREATE TABLE `%s.%s.people` AS SELECT 1 AS id, 'ann' AS name UNION ALL SELECT 2, 'bob'", project, ds)
	p.setField(".bq-editor", create)
	p.clickText(".bq-toolbar button", "Run")
	p.waitFor(`document.querySelector(".modal.is-open #bq-write-title") !== null`)
	p.clickText(".modal.is-open button", "Run statement")
	p.waitFor(`document.querySelector(".bq-results-title") !== null || document.querySelector(".bq-results .form-error") !== null`)
	var failed string
	p.eval(`(document.querySelector(".bq-results .form-error") || {}).textContent || ""`, &failed)
	if failed != "" {
		t.Fatalf("CREATE TABLE AS SELECT from Studio failed: %s", failed)
	}

	// Refresh contents shows the table; the dataset opens to it.
	p.clickExpr(node(project) + `.querySelector(".bq-node-menu")`)
	p.clickText(".bq-menu-item", "Refresh contents")
	p.waitFor(node(project+"."+ds) + ` !== null`)
	p.clickExpr(node(project+"."+ds) + `.querySelector(".bq-twisty")`)
	tableKey := project + "." + ds + ".people"
	p.waitFor(node(tableKey) + ` !== null`)

	// Search narrows the tree; a star is kept.
	p.setField(".bq-explorer-search", "no_such_resource_here")
	p.waitFor(node(tableKey) + ` === null`)
	p.setField(".bq-explorer-search", "people")
	p.waitFor(node(tableKey) + ` !== null`)
	p.clickExpr(node(tableKey) + `.querySelector(".bq-star")`)
	p.waitFor(node(tableKey) + `.querySelector(".bq-star").getAttribute("aria-pressed") === "true"`)
	p.setField(".bq-explorer-search", "")
	p.clickExpr(`document.querySelector("#bq-starred-only")`)
	p.waitFor(node(tableKey) + ` !== null && ` + node(project+"."+ds) + ` !== null`)
	p.clickExpr(`document.querySelector("#bq-starred-only")`)

	// The table's View actions: Query opens a new tab with its SELECT.
	p.clickExpr(node(tableKey) + `.querySelector(".bq-node-menu")`)
	var items []string
	p.eval(node(tableKey)+`.querySelectorAll(".bq-menu-item").length ? [...`+node(tableKey)+`.querySelectorAll(".bq-menu-item")].map((b) => b.textContent) : []`, &items)
	if strings.Join(items, "|") != "Open|Open in new tab|Query|Query in split tab|Copy ID|Delete" {
		t.Errorf("a table's View actions are %q", items)
	}
	p.clickText(".bq-menu-item", "Query")
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === "people"`)
	var sql string
	p.eval(`document.querySelector(".bq-editor").value`, &sql)
	if !strings.Contains(sql, "SELECT * FROM `"+tableKey+"`") {
		t.Errorf("Query opened the editor on %q", sql)
	}

	// The validator dry-runs the statement.
	p.setField(".bq-editor", fmt.Sprintf("SELECT id, name FROM `%s` ORDER BY id", tableKey))
	p.waitFor(`document.querySelector(".bq-validator").textContent.includes("This query will process")`)
	var validator string
	p.eval(`document.querySelector(".bq-validator").textContent`, &validator)
	if !strings.HasSuffix(validator, "when run.") {
		t.Errorf("the validator says %q", validator)
	}

	// Schedule is Google's, disabled with its reason.
	for _, label := range []string{"Schedule"} {
		var why string
		p.eval(fmt.Sprintf(`(() => { const b = [...document.querySelectorAll(".bq-toolbar button")].find((n) => n.textContent === %q);
			return b && b.disabled ? b.title : ""; })()`, label), &why)
		if !strings.Contains(why, "Not available locally") {
			t.Errorf("%s is not drawn disabled with a reason: %q", label, why)
		}
	}

	// Run: Query results with Google's tabs, on Results.
	p.clickText(".bq-toolbar button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-results-table td")].some((c) => c.textContent === "ann")`)
	var tabs []string
	p.eval(`[...document.querySelectorAll(".bq-results-tabs .tab")].map((b) => b.textContent)`, &tabs)
	if strings.Join(tabs, "|") != "Job information|Results|Visualization|JSON|Execution details|Execution graph" {
		t.Errorf("Query results' tabs are %q", tabs)
	}
	p.clickText(".bq-results-tabs .tab", "JSON")
	p.waitFor(`document.querySelector(".bq-json") !== null && document.querySelector(".bq-json").textContent.includes('"name": "bob"')`)
	p.clickText(".bq-results-tabs .tab", "Job information")
	p.waitFor(`[...document.querySelectorAll(".bq-job-info dt")].some((d) => d.textContent === "Bytes processed")`)
	var info string
	p.eval(`document.querySelector(".bq-job-info").textContent`, &info)
	for _, label := range []string{"Job ID", "Location", "Creation time", "Duration", "Job priority", "Use legacy SQL"} {
		if !strings.Contains(info, label) {
			t.Errorf("Job information has no %s: %q", label, info)
		}
	}
	p.clickText(".bq-results-tabs .tab", "Execution details")
	p.waitFor(`document.querySelector(".bq-results-body").textContent.length > 0`)

	// Edit > Query settings, where Google's docs put it: a destination table.
	p.clickExpr(`document.querySelector(".bq-toolbar .bq-menu-button[aria-label=Edit]")`)
	p.clickText(".bq-menu-item", "Query settings")
	p.waitFor(`document.querySelector(".modal.is-open #bq-settings-title") !== null`)
	p.clickExpr(`document.querySelector(".modal.is-open input[name=bq-dest][value=table]")`)
	p.setField(".modal.is-open #bq-dest-dataset", ds)
	p.setField(".modal.is-open #bq-dest-table", "people_copy")
	p.clickExpr(`document.querySelector(".modal.is-open input[name=bq-priority][value=BATCH]")`)
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.clickText(".bq-toolbar button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-job-info dd, .bq-results-table td")].some((c) => c.textContent === "ann" || c.textContent.endsWith("people_copy"))`)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := readDataPage(t, "bigquery", project, ds).rows("tables")["people_copy"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a destination table set in Query settings was not written: %v", readDataPage(t, "bigquery", project, ds).rows("tables"))
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The job Run ran, named in its Job information.
	var jobID string
	p.eval(`[...document.querySelectorAll(".bq-results-tabs .tab")].find((b) => b.textContent === "Job information").click()`, nil)
	p.waitFor(`document.querySelector(".bq-job-info") !== null`)
	p.eval(`(() => { const d = [...document.querySelectorAll(".bq-job-info dt")].find((n) => n.textContent === "Job ID");
		const v = d ? d.nextElementSibling.textContent : ""; return v.slice(v.lastIndexOf(".") + 1); })()`, &jobID)
	if jobID == "" {
		t.Fatalf("Job information names no job")
	}

	// Personal and Project history, in the Job history tab the Explorer's
	// Job history opens.
	p.clickText(".bq-explorer-jobs", "Job history")
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === "Job history"`)
	p.waitFor(`[...document.querySelectorAll(".bq-history-table td")].length > 0`)
	p.clickText(".bq-history-tabs .tab", "Project history")
	p.waitFor(`document.querySelector(".bq-history-tabs .tab.is-selected").textContent === "Project history" &&
		document.querySelectorAll(".bq-history-table tbody tr").length > 0`)
	p.clickExpr(`document.querySelector(".bq-history-table tbody tr .bq-node-menu")`)
	var jobItems []string
	p.eval(`[...document.querySelector(".bq-history-table tbody tr").querySelectorAll(".bq-menu-item")].map((b) => b.textContent)`, &jobItems)
	if strings.Join(jobItems, "|") != "Show job details|Open as new query" {
		t.Errorf("a query job's actions are %q", jobItems)
	}
	var state string
	p.eval(`document.querySelector(".bq-history-table tbody tr").textContent`, &state)
	if !strings.Contains(state, "Succeeded") {
		t.Errorf("the newest job's row has no Succeeded state: %q", state)
	}

	// The job's page: Open as new query opens Studio on its statement.
	p.navigate("/bigquery-jobs/" + url.PathEscape(jobID) + q)
	p.clickText("#view .page-actions button", "Open as new query")
	p.waitFor(`location.pathname === "/bigquery-studio" && document.querySelector(".bq-editor") !== null &&
		document.querySelector(".bq-editor").value.includes("ORDER BY id")`)

	// A page CloudBurrow does not serve, in Google's place.
	// (Analytics Hub is served since #1960, Dataform since #2105.)
	p.navigate("/bigquery-partner-center" + q)
	p.waitFor(`document.querySelector("#view .state h2") !== null`)
	var unavailable string
	p.eval(`document.querySelector("#view .state").textContent`, &unavailable)
	if !strings.Contains(unavailable, "Not available locally") {
		t.Errorf("Partner Center says %q", unavailable)
	}

	// The table's tabs, in Google's order.
	p.navigate("/bigquery/" + ds + "/people" + q)
	p.waitFor(`document.querySelectorAll("#view .tab-strip .tab").length >= 6`)
	var tableTabs []string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((b) => b.textContent)`, &tableTabs)
	if len(tableTabs) < 6 || strings.Join(tableTabs[:6], "|") != "Schema|Details|Preview|Lineage|Data profile|Data quality" {
		t.Errorf("the table's tabs are %q", tableTabs)
	}
}

// clickExpr clicks the element a JavaScript expression names, as a person
// would, once it is drawn and not inert: a dialog on its way out is inert
// (openModal) and still drawn for its exit, holding the same IDs as the
// next one, so a click meant for the new dialog's button could otherwise be
// aimed at the old one's.
//
// The element is found, scrolled to the middle of the viewport and aimed at
// in one evaluation, which answers the point to click only when the topmost
// element there is the element, one of its descendants, an ancestor (an
// input drawn by its label or container) or one of its labels. Anything
// else (gone, inert, covered by the dialog replacing it, without a box) is
// looked for again within waitFor's 30 seconds (main CI 44926873355). An
// inline element that wraps, as a long ID's link does in a narrow column,
// is aimed at the middle of one of its own line boxes: the middle of its
// bounding box can fall between its lines, on the cell around it, which
// counted as an ancestor and took the click instead of the link (main CI
// 44966435794: Scheduled queries' config ID on four lines). The
// mouse click at that point is sent once and never retried: the page has
// it then, and a second one could submit a form twice. Clicking by a mark
// (chromedp.Click on [data-cb-click]) instead waited on a node the page
// could replace until the test's deadline, and a click retried after a
// timeout may already have landed.
func (p *tab) clickExpr(expr string) {
	p.t.Helper()
	ready := `(() => { const n = ` + expr + `; return !!n && n.isConnected && !n.closest("[inert]") && n.getClientRects().length > 0; })()`
	aim := `(() => { const n = ` + expr + `;
		if (!n || !n.isConnected || n.closest("[inert]")) return null;
		n.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
		const root = n.getRootNode();
		for (const b of n.getClientRects()) {
			if (!b.width || !b.height) continue;
			const x = b.left + b.width / 2, y = b.top + b.height / 2, hit = root.elementFromPoint(x, y);
			if (hit && n.contains(hit)) return { X: x, Y: y };
		}
		const r = n.getBoundingClientRect();
		if (!r.width || !r.height) return null;
		const x = r.left + r.width / 2, y = r.top + r.height / 2, hit = root.elementFromPoint(x, y);
		const reaches = !!hit && (n.contains(hit) || hit.contains(n) || [...(n.labels || [])].some((l) => l.contains(hit)));
		return reaches ? { X: x, Y: y } : null; })()`
	deadline := time.Now().Add(30 * time.Second)
	for {
		p.waitForWithin(ready, max(time.Until(deadline), time.Second))
		var at *struct{ X, Y float64 }
		p.eval(aim, &at)
		if at != nil {
			p.run(chromedp.MouseClickXY(at.X, at.Y))
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("browser: %s was never where a click would reach it", strings.Join(strings.Fields(expr), " "))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBigQueryCreateDatasetAndTableAsGoogles (#1861), in the emulators
// shard. Create dataset is a page of Google's fields: Location type
// (Region or Multi-region), Link to an external dataset drawn disabled with
// its reason, and Advanced options, whose Enable table expiration reveals
// Default maximum table age in days; what it sets is on the dataset's
// Details. From the Explorer, the dataset's Create table opens Google's
// sections, Create table from with Google Cloud Storage revealing its URI,
// and a schema written with Edit as text, partitioned and clustered, makes
// the table.
func TestBigQueryCreateDatasetAndTableAsGoogles(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_create_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery/create" + q)
	p.waitFor(`document.querySelector("#view #f-datasetId") !== null`)
	var labels []string
	p.eval(`[...document.querySelectorAll("#view fieldset legend, #view label")].map((n) => n.textContent.trim())`, &labels)
	all := strings.Join(labels, "|")
	for _, want := range []string{"Location type", "Region", "Multi-region", "Link to an external dataset", "Encryption",
		"Enable case insensitive table names", "Default collation", "Enable table expiration", "Default rounding mode",
		"Storage billing model", "Time travel window (days)"} {
		if !strings.Contains(all, want) {
			t.Errorf("Create dataset has no %q: %q", want, all)
		}
	}
	for _, gone := range []string{"Description", "Labels"} {
		if strings.Contains("|"+all+"|", "|"+gone+"|") {
			t.Errorf("Create dataset asks for %s, which Google's form does not", gone)
		}
	}
	var external bool
	p.eval(`document.querySelector("#view #f-externalDataset").disabled`, &external)
	if !external {
		t.Errorf("Link to an external dataset is not drawn disabled")
	}
	p.setField("#view #f-datasetId", ds)
	p.clickExpr(`[...document.querySelectorAll("#view input[type=radio]")].find((r) => r.value === "Region")`)
	p.waitFor(`document.querySelector("#view #f-region").getClientRects().length > 0`)
	p.setField("#view #f-region", "europe-west1")
	p.clickExpr(`document.querySelector("#view #f-enableTableExpiration")`)
	p.waitFor(`document.querySelector("#view #f-tableExpirationDays").getClientRects().length > 0`)
	p.setField("#view #f-tableExpirationDays", "3")
	p.setField("#view #f-defaultCollation", "und:ci")
	p.clickExpr(`document.querySelector("#view form button[type=submit]")`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {ds}}.Encode(), "")
		if code == http.StatusOK && strings.Contains(body, "europe-west1") {
			for _, want := range []string{`"3 days"`, `"und:ci"`} {
				if !strings.Contains(body, want) {
					t.Errorf("the dataset's Details have no %s: %s", want, body)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Create dataset made no dataset in europe-west1: %d %s", code, body)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Create table from the Explorer.
	p.navigate("/bigquery-studio" + q)
	node := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+"."+ds)
	p.waitFor(node + ` !== null`)
	p.clickExpr(node + `.querySelector(".bq-node-menu")`)
	p.clickText(".bq-menu-item", "Create table")
	p.waitFor(`document.querySelector(".modal.is-open #f-tableId") !== null`)
	var legends []string
	p.eval(`[...document.querySelectorAll(".modal.is-open legend, .modal.is-open h3, .modal.is-open h2")].map((n) => n.textContent.trim())`, &legends)
	if !strings.Contains(strings.Join(legends, "|"), "Create table from") {
		t.Errorf("Create table has no Create table from: %q", legends)
	}
	p.clickExpr(`[...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "Google Cloud Storage")`)
	p.waitFor(`document.querySelector(".modal.is-open #f-uris").getClientRects().length > 0`)
	p.clickExpr(`[...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "Empty table")`)
	p.waitFor(`document.querySelector(".modal.is-open #f-uris").getClientRects().length === 0`)
	p.setField(".modal.is-open #f-tableId", "events")
	p.clickExpr(`document.querySelector(".modal.is-open .schema-text-toggle")`)
	p.waitFor(`!document.querySelector(".modal.is-open .schema-text").hidden`)
	p.setField(".modal.is-open .schema-text",
		`[{"name":"ts","type":"TIMESTAMP","mode":"REQUIRED"},{"name":"name","type":"STRING"},{"name":"n","type":"INTEGER"}]`)
	p.setField(".modal.is-open #f-partitionType", "DAY")
	p.setField(".modal.is-open #f-partitionField", "ts")
	p.setField(".modal.is-open #f-clusteringFields", "name")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	deadline = time.Now().Add(15 * time.Second)
	for {
		d := readDataPage(t, "bigquery", project, ds, "events")
		if d.summary("Fields") == "3" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Create table with a schema written as text made %+v", d)
		}
		time.Sleep(300 * time.Millisecond)
	}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {ds, "events"}}.Encode(), "")
	if code != http.StatusOK || !strings.Contains(body, "DAY") || !strings.Contains(body, "name") {
		t.Errorf("the table's page shows no DAY partitioning or clustering: %d %s", code, body)
	}

	// The dataset's page opens on Overview, as Google's.
	p.navigate("/bigquery/" + ds + q)
	p.waitFor(`document.querySelector("#view .tab-strip .tab") !== null`)
	var first string
	p.eval(`document.querySelector("#view .tab-strip .tab").textContent`, &first)
	if first != "Overview" {
		t.Errorf("the dataset's first tab is %q, want Overview", first)
	}
}
