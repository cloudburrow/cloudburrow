//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	datatransfer "cloud.google.com/go/bigquery/datatransfer/apiv1"
	"cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestBigQueryStudioScheduleThroughThePage (#2094, 2026-11-23 round): the
// query editor's File > Schedule opens Google's New scheduled query pane
// (File, not a Schedule button: "click File > Schedule", scheduling-queries), whose Save makes a scheduled_query transfer config
// the official client reads back (name, a weekly schedule, the destination
// table and its write preference); the Scheduled query details page's
// Edit opens the query in the editor, and File > Schedule > Update
// scheduled query (a submenu there) saves it back (the query changed, Repeats On-demand). WCAG 2.1 AA
// for the open pane, and the pane opened, filled and closed by keyboard,
// the File menu and its Schedule submenu walked with Enter and the arrows.
func TestBigQueryStudioScheduleThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	c, err := datatransfer.NewClient(context.Background(), option.WithEndpoint(endpoint(t, envDataTransfer)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	stamp := uniqueSuffix()
	ds := fmt.Sprintf("browser_sched_%d", stamp)
	display := fmt.Sprintf("studio weekly %d", stamp)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	find := func() *datatransferpb.TransferConfig {
		it := c.ListTransferConfigs(context.Background(), &datatransferpb.ListTransferConfigsRequest{Parent: "projects/" + project})
		for {
			got, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.GetDisplayName() == display {
				return got
			}
		}
	}
	t.Cleanup(func() {
		if cfg := find(); cfg != nil {
			_ = c.DeleteTransferConfig(context.Background(), &datatransferpb.DeleteTransferConfigRequest{Name: cfg.GetName()})
		}
	})

	p := open(t)
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-new-query", "SQL query")
	p.setField(".bq-editor", "SELECT 1 AS n")
	fileMenu := func() {
		t.Helper()
		p.clickExpr(`[...document.querySelectorAll(".bq-toolbar .bq-menu-button[aria-label=File]")].find((b) => b.offsetParent !== null)`)
	}
	// Google's toolbar has no Schedule button: Schedule is in File.
	var toolbar []string
	p.eval(`[...[...document.querySelectorAll(".bq-toolbar")].find((b) => b.offsetParent !== null).querySelectorAll(".bq-menu-button")].map((b) => b.getAttribute("aria-label"))`, &toolbar)
	if strings.Join(toolbar, "|") != "Save|Share|File|Edit|Tools" {
		t.Errorf("the query editor's menus are %v, want Save, Share, File, Edit, Tools", toolbar)
	}
	fileMenu()
	// On a query that is no scheduled query's, File > Schedule is one item
	// that opens the pane; Update scheduled query is not offered.
	var fileItems []string
	p.eval(`[...document.querySelectorAll(".bq-menu:not([hidden]) > .bq-menu-item, .bq-menu:not([hidden]) > .bq-submenu-wrap > .bq-menu-item")].map((b) => b.textContent + (b.getAttribute("aria-haspopup") ? ">" : ""))`, &fileItems)
	if strings.Join(fileItems, "|") != "Schedule" {
		t.Errorf("File's items are %v, want Schedule alone (a plain item)", fileItems)
	}
	p.clickText(".bq-menu:not([hidden]) .bq-menu-item", "Schedule")
	p.waitFor(`document.querySelector("#bq-sq-pane") !== null && document.querySelector("#bq-sq-title").textContent === "New scheduled query"`)
	var labels string
	p.eval(`[...document.querySelectorAll("#bq-sq-pane label, #bq-sq-pane legend, #bq-sq-pane h3, #bq-sq-pane summary")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, want := range []string{"Name for the scheduled query", "Schedule options", "Repeats", "Start now", "Start at set time",
		"Schedule end time", "Destination for query results", "Set a destination table for query results", "Dataset name", "Table name",
		"Destination table partitioning field", "Destination table write preference", "Append to table", "Overwrite table",
		"Advanced options", "Customer-managed key", "Scheduled query credential", "Additional configurations",
		"Send email notifications", "Pub/Sub topic"} {
		if !strings.Contains("|"+labels+"|", "|"+want+"|") {
			t.Errorf("the New scheduled query pane has no %q: %s", want, labels)
		}
	}
	var repeats string
	p.eval(`document.querySelector("#bq-sq-repeats").value`, &repeats)
	if repeats != "Daily" {
		t.Errorf("Repeats defaults to %q, want Daily as Google's page says", repeats)
	}
	// A Custom schedule left empty is refused in the pane.
	p.setField("#bq-sq-name", display)
	p.setField("#bq-sq-repeats", "Custom")
	p.clickText("#bq-sq-pane button", "Save")
	p.waitFor(`document.querySelector("#bq-sq-pane .form-error").textContent === "Custom schedule is required."`)

	p.setField("#bq-sq-repeats", "Weekly")
	p.setField("#bq-sq-day", "Tuesday")
	p.setField("#bq-sq-at", "09:30")
	p.eval(`document.querySelector("#bq-sq-dest-on").click(); true`, nil)
	p.setField("#bq-sq-dataset", ds)
	p.setField("#bq-sq-table", "weekly_out")
	p.eval(`document.querySelector("#bq-sq-wd-WRITE_TRUNCATE").click(); true`, nil)
	p.clickText("#bq-sq-pane button", "Save")
	p.waitFor(`document.querySelector("#bq-sq-pane") === null`)
	cfg := find()
	if cfg == nil {
		t.Fatalf("no scheduled query %q after Save", display)
	}
	params := cfg.GetParams().GetFields()
	if cfg.GetDataSourceId() != "scheduled_query" || cfg.GetSchedule() != "every tuesday 09:30" || cfg.GetDestinationDatasetId() != ds ||
		params["query"].GetStringValue() != "SELECT 1 AS n" || params["destination_table_name_template"].GetStringValue() != "weekly_out" ||
		params["write_disposition"].GetStringValue() != "WRITE_TRUNCATE" {
		t.Errorf("the scheduled query reads back as source %q, schedule %q, dataset %q, params %v",
			cfg.GetDataSourceId(), cfg.GetSchedule(), cfg.GetDestinationDatasetId(), cfg.GetParams().AsMap())
	}

	// Its page's Edit links to the editor.
	id := cfg.GetName()[strings.LastIndex(cfg.GetName(), "/")+1:]
	if code, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery-scheduled-queries"+q+"&name="+url.QueryEscape(id), ""); code != http.StatusOK ||
		!strings.Contains(body, `"href":"/bigquery-studio?scheduledQuery=`) {
		t.Errorf("the Scheduled query details page has no Edit into the editor (%d): %.300s", code, body)
	}
	p.navigate("/bigquery-studio" + q + "&scheduledQuery=" + url.QueryEscape(cfg.GetName()))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".bq-tabs .tab")].some((b) => b.textContent.includes(%q))`, display))
	p.waitFor(`[...document.querySelectorAll(".bq-editor")].some((e) => e.offsetParent !== null && e.value === "SELECT 1 AS n")`)
	p.eval(`(() => { const e = [...document.querySelectorAll(".bq-editor")].find((x) => x.offsetParent !== null);
		e.value = "SELECT 2 AS n"; e.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	// Here File > Schedule is a submenu: Update scheduled query, then New
	// scheduled query.
	fileMenu()
	p.clickText(".bq-menu:not([hidden]) .bq-menu-parent", "Schedule ▸")
	var subItems []string
	p.eval(`[...document.querySelectorAll(".bq-submenu:not([hidden]) .bq-menu-item")].map((b) => b.textContent)`, &subItems)
	if strings.Join(subItems, "|") != "Update scheduled query|New scheduled query" {
		t.Errorf("File > Schedule's submenu is %v, want Update scheduled query, New scheduled query", subItems)
	}
	p.clickText(".bq-submenu:not([hidden]) .bq-menu-item", "Update scheduled query")
	p.waitFor(`document.querySelector("#bq-sq-title") !== null && document.querySelector("#bq-sq-title").textContent === "Update scheduled query"`)
	var filled []string
	p.eval(`["#bq-sq-name", "#bq-sq-repeats", "#bq-sq-day", "#bq-sq-at", "#bq-sq-dataset", "#bq-sq-table"].map((s) => document.querySelector(s).value)`, &filled)
	if strings.Join(filled, "|") != strings.Join([]string{display, "Weekly", "Tuesday", "09:30", ds, "weekly_out"}, "|") {
		t.Errorf("Update scheduled query is filled with %v", filled)
	}
	p.setField("#bq-sq-repeats", "On-demand")
	p.clickText("#bq-sq-pane button", "Save")
	p.waitFor(`document.querySelector("#bq-sq-pane") === null`)
	got, err := c.GetTransferConfig(context.Background(), &datatransferpb.GetTransferConfigRequest{Name: cfg.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetParams().GetFields()["query"].GetStringValue() != "SELECT 2 AS n" || !got.GetScheduleOptions().GetDisableAutoScheduling() {
		t.Errorf("after Update the query is %q and auto scheduling off is %v", got.GetParams().GetFields()["query"].GetStringValue(),
			got.GetScheduleOptions().GetDisableAutoScheduling())
	}

	// By keyboard, on this scheduled query's tab: File opens on Enter with
	// focus on Schedule, Right opens its submenu on Update scheduled query,
	// Down moves to New scheduled query, Left closes it back to Schedule,
	// and Right, Down and Enter open the pane with focus in Name; Escape
	// closes it.
	p.run(chromedp.Focus(chromedp.CSS(`.bq-split-left .bq-toolbar .bq-menu-button[aria-label=File]`)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.getAttribute("aria-label") === "Schedule"`)
	p.run(chromedp.KeyEvent(kb.ArrowRight))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Update scheduled query"`)
	p.run(chromedp.KeyEvent(kb.ArrowDown))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "New scheduled query"`)
	p.run(chromedp.KeyEvent(kb.ArrowLeft))
	p.waitFor(`document.activeElement && document.activeElement.getAttribute("aria-label") === "Schedule" && document.querySelector(".bq-submenu:not([hidden])") === null`)
	p.run(chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowDown))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "New scheduled query"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.id === "bq-sq-name"`)
	// Advanced options open, so the walk reaches their fields too.
	p.eval(`document.querySelector(".bq-sq-advanced").open = true; document.querySelector("#bq-sq-name").focus(); true`, nil)
	for _, fl := range p.keyboardFailures() {
		t.Errorf("New scheduled query: %s", fl)
	}
	p.run(chromedp.Focus(chromedp.CSS(`#bq-sq-name`)), chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector("#bq-sq-pane") === null`)

	auditScreens(t, []a11yPage{
		{"Studio New scheduled query pane", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`,
			`[...document.querySelectorAll(".bq-new-query")].find((b) => b.textContent.includes("SQL query")).click();
			 setTimeout(() => { const b = [...document.querySelectorAll(".bq-toolbar .bq-menu-button[aria-label=File]")].find((x) => x.offsetParent !== null); b.click();
			   [...document.querySelectorAll(".bq-menu-item")].find((x) => x.textContent === "Schedule" && x.offsetParent !== null).click();
			   setTimeout(() => { document.querySelector("#bq-sq-dest-on").click(); document.querySelector(".bq-sq-advanced").open = true; }, 300); }, 300)`},
	})
}

// TestDataformBigQueryStudioDataCanvasBoardThroughThePage (#2094,
// 2026-11-23 round; named for the shard that serves BigQuery and Dataform
// together, as the canvas's other test): the data canvas's board in
// Google's canvas controls: nodes placed in columns by their depth and
// joined by an edge, Zoom In, Zoom Out and the Zoom level, Zoom to Fit,
// Zoom to Selection, a node moved by its handle with the arrow keys and
// put back by Tidy canvas, Full screen and its exit; a node's menu
// (Duplicate, Debug, Run the node and the following node, a SQL node's
// Export as scheduled query), Refresh canvas, and More actions > Clear
// canvas. WCAG 2.1 AA for the board, and its controls by keyboard.
func TestDataformBigQueryStudioDataCanvasBoardThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	press := func(sel, text string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(%q)].some((b) => b.textContent.trim() === %q && !b.disabled)`, sel, text))
		p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q && !b.disabled).click(); true`, sel, text), nil)
	}
	nodeMenu := func(node, item string) {
		t.Helper()
		p.eval(fmt.Sprintf(`document.querySelector('.bq-canvas-node[data-node="%s"] .bq-canvas-bar .bq-node-menu').click(); true`, node), nil)
		press(fmt.Sprintf(`.bq-canvas-node[data-node="%s"] .bq-menu-item`, node), item)
	}
	zoom := func() string {
		var v string
		p.eval(`document.querySelector(".bq-canvas-zoom").value`, &v)
		return v
	}
	left := func(node string) float64 {
		var v float64
		p.eval(fmt.Sprintf(`parseFloat(document.querySelector('.bq-canvas-node[data-node="%s"]').style.left)`, node), &v)
		return v
	}
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Data canvas")
	p.waitFor(`document.querySelector(".bq-canvas-board") !== null`)
	var controls string
	p.eval(`[...document.querySelectorAll(".bq-canvas .bq-toolbar > button, .bq-canvas .bq-toolbar > select, .bq-canvas .bq-toolbar .bq-menu-button")].map((b) => b.getAttribute("aria-label") || b.textContent.trim()).join("|")`, &controls)
	for _, want := range []string{"Add a Search node", "Add a SQL node", "Add a Text node", "Zoom level", "Zoom to Fit", "Zoom to Selection",
		"Zoom In", "Zoom Out", "Full screen", "Tidy canvas", "Refresh canvas", "More actions"} {
		if !strings.Contains("|"+controls+"|", "|"+want+"|") {
			t.Errorf("the canvas toolbar has no %q: %s", want, controls)
		}
	}

	press(".bq-canvas .bq-toolbar button", "SQL")
	p.setField(".bq-canvas-sql textarea", "SELECT 'a' AS k, 1 AS v UNION ALL SELECT 'b', 2")
	press(".bq-canvas-sql .bq-canvas-actions button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql td")].some((c) => c.textContent === "b")`)
	press(".bq-canvas-sql .bq-canvas-actions button", "Visualize")
	p.waitFor(`document.querySelector(".bq-canvas-viz") !== null && document.querySelectorAll(".bq-canvas-edge").length === 1`)
	// The visualization is a column to the right of its SQL node.
	if l1, l2 := left("1"), left("2"); l2 <= l1 {
		t.Errorf("the Visualization node is at x=%v, its SQL node at x=%v: want it to the right", l2, l1)
	}

	// Zoom.
	if z := zoom(); z != "100" {
		t.Errorf("the canvas opens at %s%%, want 100%%", z)
	}
	press(".bq-canvas .bq-toolbar button", "Zoom In")
	p.waitFor(`document.querySelector(".bq-canvas-zoom").value === "125"`)
	press(".bq-canvas .bq-toolbar button", "Zoom Out")
	press(".bq-canvas .bq-toolbar button", "Zoom Out")
	p.waitFor(`document.querySelector(".bq-canvas-zoom").value === "75" && document.querySelector(".bq-canvas-world").style.transform === "scale(0.75)"`)
	p.setField(".bq-canvas-zoom", "50")
	p.waitFor(`document.querySelector(".bq-canvas-world").style.transform === "scale(0.5)"`)
	press(".bq-canvas .bq-toolbar button", "Zoom to Fit")
	var fits bool
	p.eval(`(() => { const b = document.querySelector(".bq-canvas-board"), s = document.querySelector(".bq-canvas-stage");
		return Number(document.querySelector(".bq-canvas-zoom").value) <= 100 && s.offsetWidth <= b.clientWidth + 1 || Number(document.querySelector(".bq-canvas-zoom").value) === 25; })()`, &fits)
	if !fits {
		t.Errorf("Zoom to Fit leaves the canvas wider than the board at %s%%", zoom())
	}
	// Zoom to Selection: the selected node at a zoom it fits.
	p.eval(`document.querySelector('.bq-canvas-node[data-node="1"] textarea').focus(); true`, nil)
	press(".bq-canvas .bq-toolbar button", "Zoom to Selection")
	p.waitFor(`document.querySelector('.bq-canvas-node[data-node="1"]').classList.contains("is-selected")`)

	// A node moved by its handle, by keyboard; Tidy canvas puts it back.
	press(".bq-canvas .bq-toolbar button", "Tidy canvas")
	before := left("2")
	p.run(chromedp.Focus(chromedp.CSS(`.bq-canvas-node[data-node="2"] .bq-canvas-move`)), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowRight))
	if after := left("2"); after != before+48 {
		t.Errorf("two ArrowRight presses moved the node from %v to %v, want %v", before, after, before+48)
	}
	press(".bq-canvas .bq-toolbar button", "Tidy canvas")
	if after := left("2"); after != before {
		t.Errorf("Tidy canvas leaves the node at %v, want %v", after, before)
	}

	// Full screen and back.
	press(".bq-canvas .bq-toolbar button", "Full screen")
	p.waitFor(`document.querySelector(".bq-canvas").classList.contains("is-fullscreen")`)
	press(".bq-canvas .bq-toolbar button", "Exit full screen")
	p.waitFor(`!document.querySelector(".bq-canvas").classList.contains("is-fullscreen")`)

	// The node menu: Duplicate, Debug, Run the node and the following node.
	nodeMenu("1", "Duplicate")
	p.waitFor(`document.querySelectorAll(".bq-canvas-sql").length === 2`)
	nodeMenu("3", "Delete")
	p.waitFor(`document.querySelectorAll(".bq-canvas-sql").length === 1`)
	// The failing run is on purpose: its 400 is expected.
	p.expected = append(p.expected, "/api/studio/bigquery?call=query")
	p.setField(".bq-canvas-sql textarea", "SELECT no_such_column")
	press(".bq-canvas-sql .bq-canvas-actions button", "Run")
	p.waitFor(`document.querySelector(".bq-canvas-sql .form-error") !== null`)
	nodeMenu("1", "Debug")
	p.waitFor(`(() => { const d = document.querySelector('.bq-canvas-node[data-node="1"] .bq-canvas-debug'); return d && !d.hidden && d.textContent.includes("Last run failed") && d.textContent.includes("no_such_column"); })()`)
	p.setField(".bq-canvas-sql textarea", "SELECT 'c' AS k, 3 AS v")
	nodeMenu("1", "Run the node and the following node")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql td")].some((c) => c.textContent === "c") && document.querySelector(".bq-canvas-viz svg.bq-chart") !== null`)
	p.setField(".bq-canvas-sql textarea", "SELECT 'd' AS k, 4 AS v")
	press(".bq-canvas .bq-toolbar button", "Refresh canvas")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql td")].some((c) => c.textContent === "d")`)
	// A SQL node's Export as scheduled query opens the New scheduled query pane.
	nodeMenu("1", "Export as scheduled query")
	p.waitFor(`document.querySelector("#bq-sq-title") !== null && document.querySelector("#bq-sq-title").textContent === "New scheduled query"`)
	press("#bq-sq-pane button", "Cancel")
	p.waitFor(`document.querySelector("#bq-sq-pane") === null`)

	// The board's controls by keyboard, then WCAG.
	for _, fl := range p.keyboardFailures() {
		t.Errorf("data canvas board: %s", fl)
	}
	auditScreens(t, []a11yPage{
		{"Studio data canvas board", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`,
			`[...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent === "Data canvas").click();
			 setTimeout(() => { [...document.querySelectorAll(".bq-canvas .bq-toolbar button")].find((b) => b.textContent === "SQL").click();
			   [...document.querySelectorAll(".bq-canvas .bq-toolbar button")].find((b) => b.textContent === "Text").click(); }, 200)`},
	})

	// More actions > Clear canvas, confirmed.
	p.eval(`document.querySelector(".bq-canvas .bq-toolbar .bq-menu-button[aria-label='More actions']").click(); true`, nil)
	press(".bq-canvas .bq-toolbar .bq-menu-item", "Clear canvas")
	p.waitFor(`document.querySelector(".modal") !== null`)
	press(".modal .modal-actions button", "Clear")
	p.waitFor(`document.querySelectorAll(".bq-canvas-node").length === 0`)
}
