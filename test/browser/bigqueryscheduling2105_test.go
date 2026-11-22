//go:build browser

package browser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestBigQueryStudioPipelineTaskTypesThroughThePage (#2105): a pipeline's
// Add task > Declare source, Table (Incremental table), View, Data quality
// test and Unit test, each with its target and tags; Run > Select tasks to
// run (with Include dependencies) runs the chosen table and what it
// needs; Run > Select tags to run; Run > Run unit tests runs the unit test
// alone; Trigger makes an event-based schedule; the Scheduling page lists
// it with Configuration type Trigger, its View actions disable it, and its
// details read Trigger Condition. WCAG 2.1 AA and keyboard for the Run
// pane, the Trigger form and the Scheduling page.
func TestBigQueryStudioPipelineTaskTypesThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	s := newStudio2105(t)
	ds := fmt.Sprintf("plt_%d", s.stamp)
	fixture(t, "bigquery", s.project, `{"datasetId":"`+ds+`"}`, ds)
	s.query(t, fmt.Sprintf("CREATE TABLE `%s.%s.events` AS SELECT 1 AS id, 'a' AS kind UNION ALL SELECT 2, 'b'", s.project, ds))
	name := fmt.Sprintf("browser task types %d", s.stamp)

	p := open(t)
	p.navigate("/bigquery-studio" + s.q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Pipeline")
	p.waitFor(`document.querySelector(".bq-pipeline") !== null`)

	// Declare source: the fixture table.
	studioMenu(p, ".bq-pipeline", "Add task", "Declare source")
	p.waitFor(`document.querySelector("#bq-pl-ttable") !== null`)
	p.setField("#bq-pl-tds", ds)
	p.setField("#bq-pl-ttable", "events")
	// Table: an incremental table over it, tagged nightly.
	studioMenu(p, ".bq-pipeline", "Add task", "Table")
	p.waitFor(`document.querySelector("#bq-pl-sql") !== null && document.querySelector("#bq-pl-ttable") !== null`)
	p.clickExpr(`document.querySelector('input[name="bq-pl-kind"][value="incremental"]')`)
	p.setField("#bq-pl-tds", ds)
	p.setField("#bq-pl-ttable", "kinds")
	p.setField("#bq-pl-sql", `SELECT id, kind FROM ${ref("events")}`)
	p.setField("#bq-pl-tags", "nightly")
	// View: over the table, run after it.
	studioMenu(p, ".bq-pipeline", "Add task", "View")
	p.waitFor(`document.querySelector("#bq-pl-sql") !== null && document.querySelector("h3") && [...document.querySelectorAll(".bq-pl-details h3")].some((h) => h.textContent === "View task")`)
	p.setField("#bq-pl-tds", ds)
	p.setField("#bq-pl-sql", `SELECT COUNT(*) AS n FROM ${ref("kinds")}`)
	// Data quality test: no id is null.
	studioMenu(p, ".bq-pipeline", "Add task", "Data quality test")
	p.waitFor(`[...document.querySelectorAll(".bq-pl-details h3")].some((h) => h.textContent === "Data quality test task")`)
	p.setField("#bq-pl-tds", ds)
	p.setField("#bq-pl-sql", `SELECT * FROM ${ref("kinds")} WHERE id IS NULL`)
	// Unit test: the view, with one input row standing in for kinds. The
	// incremental table is not offered: Dataform core does not test one.
	studioMenu(p, ".bq-pipeline", "Add task", "Unit test")
	p.waitFor(`document.querySelector("#bq-pl-tested") !== null`)
	var offered string
	p.eval(`[...document.querySelectorAll("#bq-pl-tested option")].map((o) => o.value).join(",")`, &offered)
	if offered != ",view_3" {
		t.Errorf("Action to test offers %q, want only view_3", offered)
	}
	p.eval(`document.querySelector("#bq-pl-tested").value = "view_3"`, nil)
	p.eval(`document.querySelector("#bq-pl-tested").dispatchEvent(new Event("change", { bubbles: true }))`, nil)
	p.clickText(".bq-pl-details button", "Add input")
	p.waitFor(`document.querySelector("#bq-pl-in-name-0") !== null`)
	p.setField("#bq-pl-in-name-0", "kinds")
	p.setField("#bq-pl-in-sql-0", "SELECT 7 AS id, 'z' AS kind")
	p.setField("#bq-pl-sql", "SELECT 1 AS n")
	var count int
	p.eval(`document.querySelectorAll(".bq-pl-task").length`, &count)
	if count != 5 {
		t.Fatalf("%d tasks in the graph, want 5", count)
	}

	// Run > Select tasks to run: the view, with its dependencies.
	studioMenu(p, ".bq-pipeline", "Run", "Select tasks to run")
	p.waitFor(`document.querySelector(".modal.is-open #bq-runsel-title") !== null`)
	var listed string
	p.eval(`[...document.querySelectorAll(".bq-runsel-table tbody tr")].map((r) => r.textContent).join(",")`, &listed)
	if strings.Contains(listed, "Unit test") || strings.Contains(listed, "Declare source") || !strings.Contains(listed, "View") {
		t.Errorf("Select tasks to run lists %s", listed)
	}
	t.Run("keyboard: Run pane", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		for _, fl := range p.dialogFailures(true) {
			t.Errorf("Select tasks to run: %s", fl)
		}
	})
	// dialogFailures ends with Escape, which closes the pane: open it again
	// once the closing one has gone.
	p.waitFor(`document.querySelector(".modal") === null`)
	studioMenu(p, ".bq-pipeline", "Run", "Select tasks to run")
	p.waitFor(`document.querySelector(".modal.is-open .bq-runsel-table") !== null`)
	p.clickExpr(`[...document.querySelectorAll(".bq-runsel-table tbody tr")].find((r) => r.textContent.includes("view_")).querySelector("input")`)
	p.clickExpr(`document.querySelector("#bq-runsel-deps")`)
	runSelected(p, "Select tasks to run", func() { studioSaveAs(p, name) })
	if state := studioWaitExecution(p, ".bq-pl-runs", 6*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the selected run is %s: %s", state, studioTaskStates(p, ".bq-pl-runs"))
	}
	tasks := studioTaskStates(p, ".bq-pl-runs")
	if !strings.Contains(tasks, "kinds") || !strings.Contains(tasks, "view") || strings.Contains(tasks, "data_quality_test") {
		t.Errorf("Select tasks to run with dependencies ran: %s", tasks)
	}
	if body := s.query(t, fmt.Sprintf("SELECT COUNT(*) AS n FROM `%s.%s.kinds`", s.project, ds)); !strings.Contains(body, "2") {
		t.Errorf("the table task's table reads %s", body)
	}

	// Run > Select tags to run: nightly is the table alone.
	studioMenu(p, ".bq-pipeline", "Run", "Select tags to run")
	p.waitFor(`document.querySelector(".bq-runsel-table") !== null`)
	p.clickExpr(`document.querySelector(".bq-runsel-table tbody input")`)
	runSelected(p, "Select tags to run", nil)
	if state := studioWaitExecution(p, ".bq-pl-runs", 6*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the tag run is %s: %s", state, studioTaskStates(p, ".bq-pl-runs"))
	}
	if tasks := studioTaskStates(p, ".bq-pl-runs"); !strings.Contains(tasks, "kinds") || strings.Contains(tasks, "view") {
		t.Errorf("Select tags to run ran: %s", tasks)
	}

	// Run > Run unit tests: All Unit Tests.
	studioMenu(p, ".bq-pipeline", "Run", "Run unit tests")
	p.waitFor(`document.querySelector('input[name="bq-runsel-scope"]') !== null`)
	p.clickExpr(`document.querySelector('input[name="bq-runsel-scope"][value="all"]')`)
	runSelected(p, "Run unit tests", nil)
	if state := studioWaitExecution(p, ".bq-pl-runs", 6*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the unit test run is %s: %s", state, studioTaskStates(p, ".bq-pl-runs"))
	}
	if tasks := studioTaskStates(p, ".bq-pl-runs"); !strings.Contains(tasks, "view_3") && !strings.Contains(tasks, "unit_test_5") || strings.Count(tasks, " | ") != 0 {
		t.Errorf("Run unit tests ran: %s", tasks)
	}

	// Trigger: on updates of events, a 3 minute minimum.
	trigger := fmt.Sprintf("on-events-%d", s.stamp)
	p.clickText(".bq-pipeline .bq-toolbar button", "Trigger")
	p.waitFor(`document.querySelector(".modal.is-open #bq-trig-name") !== null`)
	t.Run("keyboard: Trigger", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		for _, fl := range p.dialogFailures(true) {
			t.Errorf("Trigger: %s", fl)
		}
	})
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText(".bq-pipeline .bq-toolbar button", "Trigger")
	p.waitFor(`document.querySelector(".modal.is-open #bq-trig-name") !== null`)
	p.setField("#bq-trig-name", trigger)
	p.setField("#bq-trig-search", ds+".events")
	p.clickText(".modal button", "Add")
	p.waitFor(`document.querySelector(".bq-trig-chosen .mono") !== null`)
	p.clickExpr(`document.querySelector('input[name="bq-trig-cond"][value="ALL"]')`)
	p.clickText(".modal .modal-actions button", "Create schedule")
	p.waitFor(`document.querySelector("#bq-trig-name") === null`)

	// The Scheduling page lists the trigger.
	p.navigate("/bigquery-scheduling" + s.q)
	row := fmt.Sprintf(`document.querySelector('.bq-scheduling-table tr[data-schedule=%q]')`, trigger)
	p.waitForWithin(row+` !== null`, time.Minute)
	var text string
	p.eval(row+`.textContent`, &text)
	if !strings.Contains(text, "Pipeline") || !strings.Contains(text, "Trigger") || !strings.Contains(text, "Enabled") || !strings.Contains(text, "all of") {
		t.Errorf("the Scheduling row reads %q", text)
	}
	// Filter by type: Scheduled Query hides it.
	p.eval(`document.querySelector("#bq-sched-type").value = "Scheduled Query"`, nil)
	p.eval(`document.querySelector("#bq-sched-type").dispatchEvent(new Event("change", { bubbles: true }))`, nil)
	p.waitFor(row + ` === null`)
	p.eval(`document.querySelector("#bq-sched-type").value = "Pipeline"`, nil)
	p.eval(`document.querySelector("#bq-sched-type").dispatchEvent(new Event("change", { bubbles: true }))`, nil)
	p.waitFor(row + ` !== null`)
	// View actions > Disable.
	p.clickExpr(row + `.querySelector(".bq-node-menu")`)
	p.clickExpr(`[...` + row + `.querySelectorAll(".bq-menu-item")].find((b) => b.textContent === "Disable")`)
	p.waitForWithin(row+` !== null && `+row+`.textContent.includes("Disabled")`, time.Minute)
	// The details.
	p.clickExpr(row + `.querySelector(".link-button")`)
	p.waitFor(`document.querySelector(".bq-sched-detail h2") !== null`)
	var detail string
	p.eval(`document.querySelector(".bq-sched-detail").textContent`, &detail)
	if !strings.Contains(detail, "Trigger (event-based execution)") || !strings.Contains(detail, "Wait for ALL tables to update") || !strings.Contains(detail, "3m0s") {
		t.Errorf("the schedule details read %q", detail)
	}

	t.Run("keyboard: Scheduling", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.run(chromedp.Focus(chromedp.CSS(`.bq-scheduling .bq-menu-button`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.activeElement && /^(Scheduled Query|Notebook schedule)$/.test(document.activeElement.textContent)`)
		p.run(chromedp.KeyEvent(kb.Escape))
		// The details are open from above: Enter draws them anew, and Past
		// executions fills in after them. Walk once both have.
		p.eval(`document.querySelector(".bq-sched-detail h2").dataset.stale = "1"`, nil)
		p.run(chromedp.Focus(chromedp.CSS(`.bq-scheduling-table .link-button`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".bq-sched-detail h2:not([data-stale])") !== null && (() => { const r = document.querySelector(".bq-sched-runs"); return r && r.childElementCount > 0 && !r.querySelector(".spinner"); })()`)
		for _, fl := range p.keyboardFailures() {
			t.Errorf("Scheduling: %s", fl)
		}
	})
	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"BigQuery Scheduling", "/bigquery-scheduling" + s.q, row + ` !== null`, ""},
			{"BigQuery Scheduling: column display options", "/bigquery-scheduling" + s.q, row + ` !== null`,
				`[...document.querySelectorAll(".bq-scheduling .bq-toolbar button")].find((b) => b.textContent === "Column display options").click()`},
			{"BigQuery Scheduling: schedule details", "/bigquery-scheduling" + s.q, row + ` !== null`, row + `.querySelector(".link-button").click()`},
		})
	})
}

// runSelected presses Run in Run's selection pane (answering the Save
// dialog with save, when the pipeline is new) and waits for the pane to
// close on a new execution, failing with the pane's error if it shows one.
func runSelected(p *tab, pane string, save func()) {
	p.t.Helper()
	var before string
	p.eval(`(document.querySelector(".bq-pl-runs") || {}).textContent || ""`, &before)
	p.clickText(".modal .modal-actions button", "Run")
	if save != nil {
		save()
	}
	p.waitForWithin(`(() => { const d = document.querySelector(".modal.is-open"); return !d || !d.querySelector("#bq-runsel-title") || !d.querySelector(".form-error").hidden; })()`, 2*time.Minute)
	var runErr string
	p.eval(`(() => { const e = document.querySelector(".modal.is-open .form-error"); return e && !e.hidden ? e.textContent : ""; })()`, &runErr)
	if runErr != "" {
		p.t.Fatalf("%s: %s", pane, runErr)
	}
	p.waitForWithin(fmt.Sprintf(`(document.querySelector(".bq-pl-runs") || {}).textContent !== %q && document.querySelector(".bq-pl-runs .bq-exec-state") !== null`, before), 2*time.Minute)
}
