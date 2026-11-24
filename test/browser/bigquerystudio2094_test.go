//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataformBigQueryStudioDataCanvasNodesThroughThePage (#2094, 2026-11-22
// round), with BigQuery and Dataform served (a data canvas is a Dataform
// code asset: without Dataform, Studio's Home draws Data canvas disabled,
// so the test is named for the run shard's browser step, the one instance
// that serves both): the data canvas's Destination node (Save
// to table into a new dataset, read back through the API, the SQL toggle
// following a re-run, Detach into a table node), its Insights node (the
// whole result's statistics), Visualization's Auto-Chart, Pie and Heat map,
// Export as notebook, the Data Canvas Assistant (its Settings and a
// question, answered by the local model or with its reason) and Clear
// canvas; and the Map's Natural Earth base map. WCAG 2.1 AA for the canvas
// and the open assistant, and the canvas's controls reached by keyboard.
func TestDataformBigQueryStudioDataCanvasNodesThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	stamp := uniqueSuffix()
	ds := fmt.Sprintf("canvas_dest_%d", stamp)
	bq := "/api/studio/bigquery?project=" + url.QueryEscape(project) + "&call=query"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"query": fmt.Sprintf("DROP SCHEMA IF EXISTS `%s.%s` CASCADE", project, ds), "write": true})
		consoleDo(t, http.MethodPost, bq, string(b))
	})
	rows := func(sql string) string {
		b, _ := json.Marshal(map[string]any{"query": sql})
		code, body := consoleDo(t, http.MethodPost, bq, string(b))
		if code != http.StatusOK {
			t.Fatalf("%s = %d: %s", sql, code, body)
		}
		return body
	}

	p := open(t)
	// press clicks a canvas button by script: the canvas is long, and a
	// mouse click can land on the console's header drawn over it.
	press := func(sel, text string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(%q)].some((b) => b.textContent.trim() === %q && !b.disabled)`, sel, text))
		p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q && !b.disabled).click(); true`, sel, text), nil)
	}
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Data canvas")
	p.waitFor(`document.querySelector(".bq-canvas") !== null`)
	press(".bq-canvas .bq-toolbar button", "SQL")
	p.setField(".bq-canvas-sql textarea", "SELECT 'a' AS k, 'x' AS g, 1 AS v UNION ALL SELECT 'b', 'x', 2 UNION ALL SELECT 'c', 'y', 3")
	press(".bq-canvas-sql .bq-canvas-actions button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql td")].some((c) => c.textContent === "c")`)

	// Destination: a new dataset and table, saved and read back.
	press(".bq-canvas-sql .bq-canvas-actions button", "Save to table")
	p.waitFor(`document.querySelector(".bq-canvas-dest") !== null`)
	p.setField(".bq-canvas-dest-fields input[id$='-ds']", ds)
	p.setField(".bq-canvas-dest-fields input[id$='-tb']", "letters")
	press(".bq-canvas-dest button", "Save")
	p.waitForWithin(`document.querySelector(".bq-canvas-dest [role=status]").textContent.startsWith("Saved to")`, time.Minute)
	if body := rows(fmt.Sprintf("SELECT SUM(v) AS s FROM `%s.%s.letters`", project, ds)); !strings.Contains(body, `"6"`) && !strings.Contains(body, `:6`) {
		t.Errorf("the Destination's table reads back as %s", body)
	}
	// The SQL toggle is on: a re-run of the SQL node rewrites the table.
	p.setField(".bq-canvas-sql textarea", "SELECT 'a' AS k, 'x' AS g, 10 AS v")
	press(".bq-canvas-sql .bq-canvas-actions button", "Run")
	ok := false
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if body := rows(fmt.Sprintf("SELECT SUM(v) AS s FROM `%s.%s.letters`", project, ds)); strings.Contains(body, `"10"`) || strings.Contains(body, `:10`) {
			ok = true
			break
		}
	}
	if !ok {
		t.Error("the Destination's table did not follow its SQL node's run")
	}
	p.setField(".bq-canvas-sql textarea", "SELECT 'a' AS k, 'x' AS g, 1 AS v UNION ALL SELECT 'b', 'x', 2 UNION ALL SELECT 'c', 'y', 3")
	press(".bq-canvas-sql .bq-canvas-actions button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-canvas-sql td")].some((c) => c.textContent === "c")`)

	// Insights: the result's statistics.
	press(".bq-canvas-sql .bq-canvas-actions button", "Generate insights")
	press(".bq-canvas-node.bq-canvas-insights button", "Run")
	p.waitForWithin(`(() => { const n = document.querySelector(".bq-canvas-node.bq-canvas-insights"); return n && n.textContent.includes("3 rows, 3 columns") && n.textContent.includes("3 distinct"); })()`, time.Minute)
	var insight string
	p.eval(`document.querySelector(".bq-canvas-node.bq-canvas-insights").textContent`, &insight)
	if !strings.Contains(insight, "min 1") || !strings.Contains(insight, "max 3") {
		t.Errorf("Insights say %q, want v's min 1 and max 3", insight)
	}

	// Visualization: Auto-Chart picks the pie of three categories; Heat
	// map and Pie draw.
	press(".bq-canvas-sql .bq-canvas-actions button", "Visualize")
	p.waitFor(`document.querySelector(".bq-canvas-viz svg.bq-chart") !== null`)
	var label string
	p.eval(`document.querySelector(".bq-canvas-viz svg.bq-chart").getAttribute("aria-label")`, &label)
	if !strings.HasPrefix(label, "Pie chart") {
		t.Errorf("Auto-Chart drew %q, want a pie of three categories", label)
	}
	p.setField(".bq-canvas-viz select", "Heat map")
	p.waitFor(`(document.querySelector(".bq-canvas-viz svg.bq-chart") || { getAttribute: () => "" }).getAttribute("aria-label").startsWith("Heat map")`)
	var cells int
	p.eval(`document.querySelectorAll(".bq-canvas-viz svg.bq-chart rect").length - 1`, &cells)
	if cells != 3 {
		t.Errorf("the heat map draws %d cells, want 3", cells)
	}

	// Detach: the Destination becomes a table node of its table.
	press(".bq-canvas-dest button", "Detach")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".bq-canvas-table .mono")].some((n) => n.textContent === %q)`, project+"."+ds+".letters"))

	// The Data Canvas Assistant: Settings, then a question.
	press(".bq-canvas .bq-toolbar button", "Open Data Canvas Assistant")
	p.waitFor(`!document.querySelector(".bq-canvas-assistant").hidden`)
	press(".bq-canvas-assistant button", "Settings")
	p.waitFor(`document.querySelector("#bq-ca-instructions") !== null`)
	p.setField("#bq-ca-instructions", "Use GoogleSQL.")
	p.setField("#bq-ca-question", "How many letters are there?")
	press(".bq-canvas-assistant button", "Send")
	p.waitForWithin(`document.querySelectorAll(".bq-canvas-msg.is-assistant").length === 1`, 3*time.Minute)
	var reply string
	p.eval(`document.querySelector(".bq-canvas-msg.is-assistant").textContent`, &reply)
	if !hasService(t, "playground") && !strings.Contains(reply, "local model") {
		t.Errorf("the assistant without a local model says %q", reply)
	}

	// The canvas's controls by keyboard: More actions opens on Enter with
	// focus on Clear canvas, Enter asks, Escape leaves the canvas as it was.
	p.run(chromedp.Focus(chromedp.CSS(`.bq-canvas .bq-toolbar .bq-menu-button[aria-label='More actions']`)), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Clear canvas"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") !== null`)
	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelectorAll(".bq-canvas-node").length > 0`)

	auditScreens(t, []a11yPage{
		{"Studio data canvas with its nodes", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`,
			`[...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent === "Data canvas").click();
			 setTimeout(() => { [...document.querySelectorAll(".bq-canvas .bq-toolbar button")].find((b) => b.textContent === "Open Data Canvas Assistant").click(); }, 100)`},
	})
	for _, fl := range p.keyboardFailures() {
		t.Errorf("data canvas: %s", fl)
	}

	// Export as notebook: a notebook tab with the SQL as a %%bigquery cell.
	press(".bq-canvas-sql .bq-canvas-actions button", "Export as notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null && document.querySelector(".bq-nb-source").value.startsWith("%%bigquery\nSELECT 'a'")`)
	p.eval(`[...document.querySelectorAll(".bq-tabs .tab")].find((b) => b.textContent.includes("canvas")).click(); true`, nil)

	// Clear canvas, confirmed.
	p.waitFor(`document.querySelector(".bq-canvas") !== null`)
	p.eval(`document.querySelector(".bq-canvas .bq-toolbar .bq-menu-button[aria-label='More actions']").click(); true`, nil)
	press(".bq-canvas .bq-toolbar .bq-menu-item", "Clear canvas")
	p.waitFor(`document.querySelector(".modal") !== null`)
	press(".modal .modal-actions button", "Clear")
	p.waitFor(`document.querySelectorAll(".bq-canvas-node").length === 0`)

	// The Map's base map: Natural Earth's land under the features.
	p.eval(`window.scrollTo(0, 0); document.querySelectorAll(".bq-workspace, #main, main").forEach((n) => { n.scrollTop = 0; }); true`, nil)
	p.clickText(".bq-new-query", "SQL query")
	p.setField(".bq-editor", "SELECT 'here' AS name, ST_GEOGPOINT(-122.08, 37.42) AS loc")
	p.clickText(".bq-toolbar button", "Run")
	p.waitForWithin(`document.querySelector(".bq-results-tabs") !== null`, time.Minute)
	p.clickText(".bq-results-tabs .tab", "Visualization")
	p.waitFor(`document.querySelector("svg.bq-map .bq-map-land path") !== null`)
	var land int
	p.eval(`document.querySelector("svg.bq-map .bq-map-land path").getAttribute("d").length`, &land)
	if land < 100 {
		t.Errorf("the base map's land path is %d bytes", land)
	}
}

// TestBigQueryStudioNotebookRuntimeThroughThePage (#2094): a notebook's
// Connect starts its runtime (CloudBurrow's stand-in for a Colab
// Enterprise runtime: IPython with Google's BigQuery client and
// bigquery-magics, built on the first connect), and its cells run there:
// Python state kept between cells, %%bigquery into a DataFrame, the
// client's queries on this instance's BigQuery, a DataFrame drawn as a
// table, an error's traceback, no route out but BigQuery's, Interrupt
// execution, Restart session (the variables gone) and Disconnect and
// delete runtime (its containers gone). The image is about 1 GB, so the
// test runs only with CLOUDBURROW_TEST_NOTEBOOK_RUNTIME=1, as the Spark
// procedures' tests run with CLOUDBURROW_TEST_SPARK.
func TestBigQueryStudioNotebookRuntimeThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	if os.Getenv("CLOUDBURROW_TEST_NOTEBOOK_RUNTIME") == "" {
		t.Skip("CLOUDBURROW_TEST_NOTEBOOK_RUNTIME is not set: the notebook runtime image is built on the first Connect")
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := openWithin(t, 30*time.Minute)
	press := func(sel, text string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(%q)].some((b) => b.textContent.trim() === %q && !b.disabled && !b.hidden)`, sel, text))
		p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q && !b.disabled && !b.hidden).click(); true`, sel, text), nil)
	}
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null && document.querySelector(".bq-nb-runtime").textContent !== ""`)
	// runtimeContainers are the notebook runtimes' containers on this
	// host; ours are the ones Connect adds.
	runtimeContainers := func() map[string]bool {
		out, _ := exec.Command("docker", "ps", "-a", "--filter", "name=cb-nb-", "--format", "{{.Names}}").CombinedOutput()
		m := map[string]bool{}
		for _, name := range strings.Fields(string(out)) {
			m[name] = true
		}
		return m
	}
	before := runtimeContainers()
	press(".bq-notebook .bq-toolbar button", "Connect")
	p.waitForWithin(`document.querySelector(".bq-nb-runtime").textContent.startsWith("Connected") || document.querySelector(".bq-nb-runtime").className.includes("form-error")`, 20*time.Minute)
	var status string
	p.eval(`document.querySelector(".bq-nb-runtime").textContent`, &status)
	if !strings.HasPrefix(status, "Connected to a runtime: Python 3.12") {
		t.Fatalf("Connect: %q", status)
	}
	ours := map[string]bool{}
	for name := range runtimeContainers() {
		if !before[name] {
			ours[name] = true
		}
	}
	if len(ours) != 2 {
		t.Errorf("Connect started %v, want the runtime and its relay", ours)
	}

	cell := func(code string) string {
		t.Helper()
		press(".bq-notebook .bq-toolbar button", "+ Code")
		p.setField(".bq-nb-cell:last-child .bq-nb-source", code)
		press(".bq-nb-cell:last-child button", "Run cell")
		// Done when the cell shows its execution count (a cell may print
		// nothing) or an error.
		p.waitForWithin(`(() => { const c = document.querySelector(".bq-nb-cell:last-child");
			return !c.querySelector(".bq-nb-output .spinner") && (/\[\d+\]/.test(c.querySelector(".bq-nb-count").textContent) || c.querySelector(".bq-nb-output .form-error") !== null); })()`, 3*time.Minute)
		var out string
		p.eval(`document.querySelector(".bq-nb-cell:last-child .bq-nb-output").textContent`, &out)
		return out
	}
	cell("x = 41")
	if out := cell("x + 1"); !strings.Contains(out, "42") {
		t.Errorf("state between cells: x + 1 shows %q", out)
	}
	cell("%%bigquery df\nSELECT 6 * 7 AS answer")
	if out := cell("int(df.answer[0])"); !strings.Contains(out, "42") {
		t.Errorf("%%%%bigquery df: df.answer[0] shows %q", out)
	}
	if out := cell("from google.cloud import bigquery\nlist(bigquery.Client().query('SELECT 7 AS s').result())[0].s"); !strings.Contains(out, "7") {
		t.Errorf("bigquery.Client().query: %q", out)
	}
	cell("import pandas as pd\npd.DataFrame({'a': [1, 2]})")
	var frames int
	p.eval(`document.querySelectorAll(".bq-nb-cell:last-child iframe.bq-nb-html").length`, &frames)
	if frames != 1 {
		t.Errorf("a DataFrame is drawn in %d frames, want 1", frames)
	}
	if out := cell("1/0"); !strings.Contains(out, "ZeroDivisionError") {
		t.Errorf("an error shows %q", out)
	}
	// No route out: only this instance's BigQuery is reached.
	if out := cell("import socket\ntry:\n    socket.create_connection(('example.com', 80), 5)\n    print('reached')\nexcept OSError as e:\n    print('refused', type(e).__name__)"); !strings.Contains(out, "refused") {
		t.Errorf("the runtime reached the internet: %q", out)
	}

	// Interrupt execution stops a long cell.
	press(".bq-notebook .bq-toolbar button", "+ Code")
	p.setField(".bq-nb-cell:last-child .bq-nb-source", "import time\ntime.sleep(120)")
	press(".bq-nb-cell:last-child button", "Run cell")
	time.Sleep(2 * time.Second)
	press(".bq-notebook .bq-toolbar button", "Interrupt execution")
	p.waitForWithin(`document.querySelector(".bq-nb-cell:last-child .bq-nb-output").textContent.includes("KeyboardInterrupt")`, time.Minute)

	// The notebook's controls by keyboard: Shift+Enter runs a cell.
	press(".bq-notebook .bq-toolbar button", "+ Code")
	p.setField(".bq-nb-cell:last-child .bq-nb-source", "'by ' + 'keyboard'")
	p.run(chromedp.Focus(chromedp.CSS(".bq-nb-cell:last-child .bq-nb-source")), chromedp.KeyEvent(kb.Enter, chromedp.KeyModifiers(chromedp.ModifierShift)))
	p.waitForWithin(`document.querySelector(".bq-nb-cell:last-child .bq-nb-output").textContent.includes("by keyboard")`, time.Minute)
	for _, fl := range p.keyboardFailures() {
		t.Errorf("connected notebook: %s", fl)
	}
	for _, fl := range p.contrastFailures() {
		t.Errorf("connected notebook: %s", fl)
	}
	for _, u := range p.unnamedControls() {
		t.Errorf("connected notebook: an unnamed %s", u)
	}

	// Restart session: a new kernel, x gone.
	press(".bq-notebook .bq-toolbar button", "Restart session")
	p.waitForWithin(`document.querySelector(".bq-nb-runtime").textContent.startsWith("Connected")`, 3*time.Minute)
	if out := cell("x"); !strings.Contains(out, "NameError") {
		t.Errorf("after Restart session, x shows %q", out)
	}

	// Disconnect and delete runtime: its containers are gone.
	press(".bq-notebook .bq-toolbar button", "Disconnect and delete runtime")
	p.waitFor(`document.querySelector(".bq-nb-runtime").textContent === "Not connected."`)
	after := runtimeContainers()
	for name := range ours {
		if after[name] {
			t.Errorf("container %s outlived Disconnect and delete runtime", name)
		}
	}
}
