//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestSpannerConsoleParityPages (#1859), in the emulators shard, whose
// instance serves Spanner. The Instances list has Google's columns; an
// instance's page has Google's left-hand menu, and its Create database
// makes a PostgreSQL database (read back as PostgreSQL) and a GoogleSQL one
// from a DDL template. The database's menu is Google's, with no separate
// Query tab; Spanner Studio's Explorer lists the table, Run updates the
// schema and inserts a row after a confirmation, a query's Results hold
// the row, and the Explanation tab has its plan. The table's Data
// tab inserts, edits and deletes a row, each read back through the console
// API.
func TestSpannerConsoleParityPages(t *testing.T) {
	needService(t, "spanner")
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-par-%d", time.Now().UnixNano()%1e8)
	body, _ := json.Marshal(map[string]string{"displayName": "Parity instance", "instance": inst, "edition": "Standard",
		"configType": "Regional", "regionalConfig": "regional-us-central1", "unit": "Nodes", "nodes": "1",
		"scaling": "Manual allocation", "labels": `{"env":"dev"}`})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create instance through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})

	p := open(t)
	p.navigate("/spanner" + q)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, inst))
	var headers []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean)`, &headers)
	for _, w := range []string{"Instance ID", "Instance name", "Edition", "Configuration", "Compute capacity", "Databases", "Labels"} {
		if !slices.Contains(headers, w) {
			t.Errorf("the Instances list's columns are %v; want %s among them", headers, w)
		}
	}
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("1 node") && r.textContent.includes("env: dev"))`, inst))

	// The instance's menu.
	p.clickText("#view tbody a", inst)
	p.waitFor(`document.querySelector("#view .tab-strip.side-nav[aria-orientation=vertical]") !== null`)
	menu := func() []string {
		var tabs []string
		p.eval(`[...document.querySelectorAll("#view .side-nav .tab")].map((t) => t.textContent)`, &tabs)
		return tabs
	}
	if got := menu(); len(got) < 5 || !slices.Equal(got[:5], []string{"Overview", "Backup/Restore", "Import/Export", "Monitoring", "System insights"}) {
		t.Errorf("the instance's menu is %v; want Overview, Backup/Restore, Import/Export, Monitoring, System insights first", got)
	}
	p.run(chromedp.Click(chromedp.CSS(`#tab-import-export`)))
	p.waitFor(`document.querySelector("#detail-panel").textContent.includes("Not available locally")`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-databases`)))

	// Create database: PostgreSQL with a schema.
	createDB := func(name string, pg bool, template bool, ddl string) {
		t.Helper()
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Create database")
		p.waitFor(`document.querySelector(".modal.is-open #f-database") !== null`)
		setField(p, ".modal.is-open", "f-database", name)
		if pg {
			p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-dialect-1`)))
		}
		if template {
			p.eval(`document.querySelector(".modal.is-open .form-templates").open = true`, nil)
			p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-ddl-template-0`)))
		} else {
			setField(p, ".modal.is-open", "f-ddl", ddl)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal") === null`)
	}
	createDB("pgdb", true, false, "CREATE TABLE singers (id bigint PRIMARY KEY, name varchar, tags varchar[])")
	createDB("gdb", false, true, "")
	dbs := readDataPage(t, "spanner", project, inst).rows("databases")
	if dbs["pgdb"]["Dialect"] != "PostgreSQL" || dbs["gdb"]["Dialect"] != "Google Standard SQL" {
		t.Fatalf("the instance's databases are %v; want pgdb in PostgreSQL and gdb in Google Standard SQL", dbs)
	}
	if tables := readDataPage(t, "spanner", project, inst, "pgdb").rows("tables"); tables["singers"] == nil {
		t.Errorf("the PostgreSQL database's tables are %v; want singers", tables)
	}
	if tables := readDataPage(t, "spanner", project, inst, "gdb").rows("tables"); tables["Singers"] == nil {
		t.Fatalf("the template's database's tables are %v; want Singers", tables)
	}

	// The database's menu, and Spanner Studio.
	p.navigate("/spanner/" + inst + "/gdb" + q)
	p.waitFor(`document.querySelector("#view .side-nav #tab-studio") !== null`)
	got := menu()
	wantMenu := []string{"Overview", "Spanner Studio", "Indexes", "Views", "Change streams", "Sequences", "Roles",
		"Backup/Restore", "Backup schedules", "Import/Export", "Query insights", "Lock insights", "Key Visualizer"}
	if len(got) < len(wantMenu) || !slices.Equal(got[:len(wantMenu)], wantMenu) {
		t.Errorf("the database's menu is %v; want %v first", got, wantMenu)
	}
	if slices.Contains(got, "Query") {
		t.Errorf("the database's menu %v has a Query tab beside Spanner Studio", got)
	}
	p.run(chromedp.Click(chromedp.CSS(`#tab-studio`)))
	p.waitFor(`document.querySelector("#spanner-explorer").textContent.includes("Singers")`)
	runSQL := func(sql string, confirm bool, want string) {
		t.Helper()
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#spanner-editor"); e.value = %q;
			e.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, sql), nil)
		p.run(chromedp.Click(chromedp.CSS(`#spanner-run`)))
		if confirm {
			p.waitFor(`document.querySelector(".modal.is-open #bq-write-title") !== null`)
			p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		}
		p.waitFor(fmt.Sprintf(`(document.querySelector("#spanner-results") || {}).textContent?.includes(%q)`, want))
	}
	runSQL("CREATE INDEX SingersByLastName ON Singers(LastName)", true, "Schema updated")
	p.waitFor(`document.querySelector("#spanner-explorer").textContent.includes("SingersByLastName")`)
	p.run(chromedp.Click(chromedp.CSS(`#spanner-new-tab`)))
	var tabCount int
	p.eval(`document.querySelectorAll(".spanner-editor-tabs .tab").length`, &tabCount)
	if tabCount != 2 {
		t.Errorf("New tab left %d editor tabs; want 2", tabCount)
	}
	runSQL("INSERT INTO Singers (SingerId, FirstName, LastName) VALUES (1, 'Ada', 'Lovelace')", true, "1 row(s) affected")
	runSQL("SELECT SingerId, FirstName FROM Singers", false, "Ada")
	// Run is PROFILE mode (#2029): the Explanation tab has the plan and the
	// Query summary (TestSpannerQueryPlanView draws the plan in detail).
	p.run(chromedp.Click(chromedp.CSS(`#spanner-tab-explanation`)))
	p.waitFor(`(document.querySelector("#spanner-stat-rows-returned") || {}).textContent === "1"`)
	p.waitFor(`[...document.querySelectorAll(".spanner-plan-card")].some((c) => c.textContent.includes("Table scan"))`)

	// The table's Data tab.
	p.navigate("/spanner/" + inst + "/gdb/Singers" + q + "&tab=data")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Lovelace"))`)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Insert row")
	p.waitFor(`document.querySelector(".modal.is-open #f-col_SingerId") !== null`)
	setField(p, ".modal.is-open", "f-col_SingerId", "2")
	setField(p, ".modal.is-open", "f-col_FirstName", "Grace")
	setField(p, ".modal.is-open", "f-col_LastName", "Hopper")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	rows := func() map[string]map[string]string {
		return readDataPage(t, "spanner", project, inst, "gdb", "Singers").rows("data")
	}
	if r := rows(); r["2"]["FirstName"] != "Grace" {
		t.Fatalf("after Insert row the table's rows are %v; want 2 Grace", r)
	}
	p.navigate("/spanner/" + inst + "/gdb/Singers" + q + "&tab=data")
	p.waitFor(`document.querySelector('button[aria-label="Actions for 2"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for 2"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit row")
	p.waitFor(`document.querySelector(".modal.is-open #f-col_FirstName") !== null`)
	setField(p, ".modal.is-open", "f-col_FirstName", "Gracie")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if r := rows(); r["2"]["FirstName"] != "Gracie" || r["2"]["LastName"] != "Hopper" {
		t.Fatalf("after Edit row the table's rows are %v; want 2 Gracie Hopper", r)
	}
	p.navigate("/spanner/" + inst + "/gdb/Singers" + q + "&tab=data")
	p.waitFor(`document.querySelector('button[aria-label="Actions for 2"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for 2"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete row")
	p.confirmByName("2")
	p.waitFor(`document.querySelector(".modal") === null`)
	if r := rows(); r["2"] != nil || r["1"] == nil {
		t.Errorf("after Delete row the table's rows are %v; want 1 alone", r)
	}
	if strings.Contains(fmt.Sprint(rows()), "Gracie") {
		t.Errorf("the deleted row is still read")
	}

	// A PostgreSQL database's table, with an ARRAY column, written from
	// its Data tab: an array is a JSON array.
	p.navigate("/spanner/" + inst + "/pgdb/singers" + q + "&tab=data")
	p.waitFor(`document.querySelector("#view .page-actions") !== null`)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Insert row")
	p.waitFor(`document.querySelector(".modal.is-open #f-col_tags") !== null`)
	setField(p, ".modal.is-open", "f-col_id", "7")
	setField(p, ".modal.is-open", "f-col_name", "Ada")
	setField(p, ".modal.is-open", "f-col_tags", `["x", "y"]`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	pgRows := func() map[string]map[string]string {
		return readDataPage(t, "spanner", project, inst, "pgdb", "singers").rows("data")
	}
	if r := pgRows(); r["7"]["name"] != "Ada" || !strings.Contains(r["7"]["tags"], `"y"`) {
		t.Fatalf("after Insert row in the PostgreSQL table its rows are %v; want 7 Ada [x y]", r)
	}
	p.navigate("/spanner/" + inst + "/pgdb/singers" + q + "&tab=data")
	p.waitFor(`document.querySelector('button[aria-label="Actions for 7"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for 7"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit row")
	p.waitFor(`document.querySelector(".modal.is-open #f-col_tags") !== null`)
	setField(p, ".modal.is-open", "f-col_tags", `["z", null]`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if r := pgRows(); !strings.Contains(r["7"]["tags"], `"z"`) || strings.Contains(r["7"]["tags"], `"y"`) {
		t.Errorf("after Edit row in the PostgreSQL table its rows are %v; want 7's tags [z null]", r)
	}
}
