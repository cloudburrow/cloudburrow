//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// studio2105 is what the #2105 tests share: the project, a stamp for
// their names, the Dataform asset cleanup and a BigQuery query.
type studio2105 struct {
	project, q string
	stamp      int64
}

func newStudio2105(t *testing.T) studio2105 {
	t.Helper()
	project := servedProject(t)
	s := studio2105{project: project, q: "?project=" + url.QueryEscape(project), stamp: uniqueSuffix()}
	df := "/api/studio/dataform?project=" + url.QueryEscape(project) + "&call=delete"
	t.Cleanup(func() {
		for _, e := range studioAssets(t, project) {
			if strings.Contains(e.DisplayName, fmt.Sprint(s.stamp)) {
				consoleDo(t, http.MethodPost, df, `{"name":"`+e.Name+`"}`)
			}
		}
	})
	return s
}

func (s studio2105) query(t *testing.T, sql string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"query": sql, "write": true})
	code, body := consoleDo(t, http.MethodPost, "/api/studio/bigquery?project="+url.QueryEscape(s.project)+"&call=query", string(b))
	if code != http.StatusOK {
		t.Fatalf("%s = %d: %s", sql, code, body)
	}
	return body
}

// studioMenu opens a text menu button (Schedule ▾, Add task ▾, Run ▾) in scope
// and picks item.
func studioMenu(p *tab, scope, button, item string) {
	p.t.Helper()
	p.clickText(scope+" .bq-menu-button", button+" ▾")
	p.clickText(scope+" .bq-menu-item", item)
}

// studioSaveAs answers the Save dialog of a new code asset.
func studioSaveAs(p *tab, name string) {
	p.t.Helper()
	p.waitFor(`document.querySelector("#bq-asset-name") !== null`)
	p.setField("#bq-asset-name", name)
	p.clickText(".modal .modal-actions button", "Save")
	p.waitFor(`document.querySelector("#bq-asset-name") === null`)
	// The dialog closes before the save answers: the selected tab takes the
	// saved name once it has.
	p.waitForWithin(fmt.Sprintf(`[...document.querySelectorAll("[role=tab].is-selected")].some((n) => n.textContent.includes(%q))`, name), time.Minute)
}

// studioWaitExecution waits for the execution shown in scope to finish and
// returns its state.
func studioWaitExecution(p *tab, scope string, limit time.Duration) string {
	p.t.Helper()
	p.waitForWithin(fmt.Sprintf(`(() => { const s = document.querySelector(%q); return s && /SUCCEEDED|FAILED|CANCELLED/.test(s.textContent); })()`, scope+" .bq-exec-state"), limit)
	var state string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent.trim()`, scope+" .bq-exec-state"), &state)
	return state
}

func studioTaskStates(p *tab, scope string) string {
	p.t.Helper()
	var s string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((r) => r.textContent).join(" | ")`, scope+" .bq-exec-tasks tbody tr"), &s)
	return s
}

// TestBigQueryStudioScheduledNotebookThroughThePage (#2105): a saved
// notebook's Schedule > Create schedule (Schedule notebook: Schedule name,
// Authentication, Notebook options' Cloud Storage bucket, Schedule
// frequency) makes a schedule Dataform runs; View schedules lists it, its
// Run starts an execution whose state and task are shown, and View output
// notebook reads the executed notebook from the bucket. WCAG 2.1 AA for
// the dialog and the schedules list, and both reached by keyboard.
func TestBigQueryStudioScheduledNotebookThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	needService(t, "storage")
	s := newStudio2105(t)
	bucket := fmt.Sprintf("nb-sched-%d", s.stamp)
	fixture(t, "storage", s.project, `{"name":"`+bucket+`"}`, bucket)
	nbName := fmt.Sprintf("scheduled notebook %d", s.stamp)

	p := open(t)
	p.navigate("/bigquery-studio" + s.q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null`)
	// Schedule needs a saved notebook.
	var disabled bool
	p.clickText(".bq-notebook .bq-menu-button", "Schedule ▾")
	p.eval(`[...document.querySelectorAll(".bq-notebook .bq-menu-item")].find((b) => b.textContent === "Create schedule").disabled`, &disabled)
	if !disabled {
		t.Error("Create schedule is offered for an unsaved notebook")
	}
	p.clickText(".bq-notebook .bq-menu-button", "Schedule ▾")
	p.clickText(".bq-notebook .bq-toolbar button", "+ Code")
	p.setField(".bq-nb-cell:last-child .bq-nb-source", "print(6 * 7)")
	p.clickText(".bq-notebook .bq-toolbar button", "Save")
	studioSaveAs(p, nbName)
	_, repo, _ := studioAsset(t, s.project, nbName)

	studioMenu(p, ".bq-notebook", "Schedule", "Create schedule")
	p.waitFor(`document.querySelector("#bq-sched-name") !== null`)
	var title string
	p.eval(`document.querySelector("#bq-sched-title").textContent`, &title)
	if title != "Schedule notebook" {
		t.Errorf("the dialog is titled %q, want Schedule notebook", title)
	}
	for _, id := range []string{"bq-sched-template", "bq-sched-bucket", "bq-sched-repeats", "bq-sched-at", "bq-sched-zone"} {
		var ok bool
		p.eval(fmt.Sprintf(`document.querySelector("#%s") !== null`, id), &ok)
		if !ok {
			t.Errorf("Schedule notebook has no #%s", id)
		}
	}
	// The bucket is required.
	p.setField("#bq-sched-name", "nightly")
	p.clickText(".modal .modal-actions button", "Create schedule")
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	p.setField("#bq-sched-bucket", "gs://"+bucket)
	p.setField("#bq-sched-repeats", "daily")
	p.setField("#bq-sched-at", "06:30")
	p.setField("#bq-sched-zone", "Europe/Paris")
	p.clickText(".modal .modal-actions button", "Create schedule")
	p.waitFor(`document.querySelector("#bq-sched-name") === null`)
	code, body := consoleDo(t, http.MethodPost, "/api/studio/dataform?project="+url.QueryEscape(s.project)+"&call=schedules", `{"name":"`+repo+`"}`)
	if code != http.StatusOK || !strings.Contains(body, `"cron":"30 6 * * *"`) || !strings.Contains(body, `"timeZone":"Europe/Paris"`) || !strings.Contains(body, "gs://"+bucket) {
		t.Fatalf("the schedule = %d: %s", code, body)
	}

	// View schedules: the schedule, Run, the execution and its output.
	studioMenu(p, ".bq-notebook", "Schedule", "View schedules")
	p.waitFor(`document.querySelector(".bq-schedules tr[data-schedule='nightly']") !== null`)
	p.clickText(".bq-schedules tr[data-schedule='nightly'] .bq-node-menu", "")
	p.clickText(".bq-schedules .bq-menu-item", "Run")
	if state := studioWaitExecution(p, ".bq-sched-runs", 10*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the run is %s: %s", state, studioTaskStates(p, ".bq-sched-runs"))
	}
	if tasks := studioTaskStates(p, ".bq-sched-runs"); !strings.Contains(tasks, "Notebook") || !strings.Contains(tasks, "SUCCEEDED") {
		t.Errorf("the run's tasks: %s", tasks)
	}
	p.clickText(".bq-sched-runs button", "View output notebook")
	p.waitForWithin(`(() => { const o = document.querySelector(".bq-exec-outputs"); return o && (o.textContent.includes("42") || o.querySelector(".form-error")); })()`, time.Minute)
	var out string
	p.eval(`document.querySelector(".bq-exec-outputs").textContent`, &out)
	if !strings.Contains(out, "42") || !strings.Contains(out, "gs://"+bucket+"/") {
		t.Errorf("the output notebook shows %q", out)
	}
	// Past executions lists the run.
	p.clickText(".bq-sched-runs button", "Back to executions")
	p.waitFor(`document.querySelectorAll(".bq-executions tbody tr").length >= 1`)
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		// Schedule ▾ opens by Enter, its first item by Enter, Escape closes.
		p.run(chromedp.Focus(chromedp.CSS(`.bq-notebook .bq-menu-button`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.activeElement && document.activeElement.textContent === "Create schedule"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector("#bq-sched-name") !== null && document.activeElement === document.querySelector("#bq-sched-name")`)
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal") === null`)
		for _, fl := range p.keyboardFailures() {
			t.Errorf("notebook with Schedule: %s", fl)
		}
	})
	link := "/bigquery-studio?" + url.Values{"project": {s.project}, "savedQuery": {repo}}.Encode()
	ready := `document.querySelector(".bq-notebook .bq-menu-button") !== null`
	pick := func(item string) string {
		return fmt.Sprintf(`(() => { const b = [...document.querySelectorAll(".bq-notebook .bq-menu-button")].find((x) => x.textContent === "Schedule ▾"); b.click();
			[...document.querySelectorAll(".bq-notebook .bq-menu-item")].find((x) => x.textContent === %q).click(); })()`, item)
	}
	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Studio Schedule notebook dialog", link, ready, pick("Create schedule")},
			{"Studio notebook schedules dialog", link, ready, pick("View schedules")},
		})
	})
}

// TestBigQueryStudioPipelineThroughThePage (#2105): Create new > Pipeline
// opens the pipeline editor; Add task > Query and Add task > Notebook (Run
// after the query), Settings' Cloud Storage bucket, then Run > Run all
// tasks saves the pipeline (a Dataform repository of definitions/*.sqlx
// and definitions/actions.yaml), runs it, and shows each task's state; the
// query's table is read back. WCAG 2.1 AA and the editor by keyboard.
func TestBigQueryStudioPipelineThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	needService(t, "storage")
	s := newStudio2105(t)
	ds := fmt.Sprintf("pipeline_%d", s.stamp)
	fixture(t, "bigquery", s.project, `{"datasetId":"`+ds+`"}`, ds)
	bucket := fmt.Sprintf("pl-out-%d", s.stamp)
	fixture(t, "storage", s.project, `{"name":"`+bucket+`"}`, bucket)
	name := fmt.Sprintf("browser pipeline %d", s.stamp)

	p := open(t)
	p.navigate("/bigquery-studio" + s.q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Pipeline")
	p.waitFor(`document.querySelector(".bq-pipeline") !== null`)
	studioMenu(p, ".bq-pipeline", "Add task", "Query")
	p.waitFor(`document.querySelector("#bq-pl-sql") !== null`)
	p.setField("#bq-pl-sql", fmt.Sprintf("CREATE OR REPLACE TABLE `%s.%s.answer` AS SELECT 42 AS answer", s.project, ds))
	studioMenu(p, ".bq-pipeline", "Add task", "Notebook")
	p.waitFor(`document.querySelector("#bq-pl-code") !== null`)
	p.setField("#bq-pl-code", "print('from the pipeline')")
	var after string
	p.eval(`[...document.querySelectorAll(".bq-pl-after input:checked")].map((i) => i.value).join(",")`, &after)
	if after != "query_1" {
		t.Errorf("the notebook task runs after %q, want query_1", after)
	}
	// Run with a notebook task and no bucket asks for Settings.
	studioMenu(p, ".bq-pipeline", "Run", "Run all tasks")
	p.waitFor(`document.querySelector("#bq-pl-set-bucket") !== null`)
	p.setField("#bq-pl-set-bucket", "gs://"+bucket)
	p.clickText(".bq-pipeline [role=tab]", "Pipeline Viewer")
	p.waitFor(`document.querySelectorAll(".bq-pl-task").length === 2`)
	studioMenu(p, ".bq-pipeline", "Run", "Run all tasks")
	studioSaveAs(p, name)
	if state := studioWaitExecution(p, ".bq-pl-runs", 10*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the run is %s: %s", state, studioTaskStates(p, ".bq-pl-runs"))
	}
	tasks := studioTaskStates(p, ".bq-pl-runs")
	// One row a task, each SUCCEEDED (a row may show the state twice).
	rows := strings.Split(tasks, " | ")
	if len(rows) != 2 || !strings.Contains(rows[0], "SUCCEEDED") || !strings.Contains(rows[1], "SUCCEEDED") || strings.Contains(tasks, "FAILED") ||
		!strings.Contains(tasks, "Query") || !strings.Contains(tasks, "Notebook") {
		t.Errorf("the run's tasks: %s", tasks)
	}
	if body := s.query(t, fmt.Sprintf("SELECT answer FROM `%s.%s.answer`", s.project, ds)); !strings.Contains(body, "42") {
		t.Errorf("the query task's table reads %s", body)
	}
	kind, repo, _ := studioAsset(t, s.project, name)
	if kind != "pipeline" {
		t.Errorf("the pipeline is listed as a %q", kind)
	}
	// Executions lists the run.
	p.clickText(".bq-pipeline .bq-toolbar button", "Executions")
	p.waitFor(`document.querySelectorAll(".bq-pl-runs .bq-executions tbody tr").length >= 1`)

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.run(chromedp.Focus(chromedp.CSS(`.bq-pipeline .bq-menu-button`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.activeElement && document.activeElement.textContent === "Query"`)
		p.run(chromedp.KeyEvent(kb.Escape))
		p.run(chromedp.Focus(chromedp.CSS(`.bq-pl-task`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".bq-pl-task").getAttribute("aria-pressed") === "true"`)
		for _, fl := range p.keyboardFailures() {
			t.Errorf("pipeline editor: %s", fl)
		}
	})
	link := "/bigquery-studio?" + url.Values{"project": {s.project}, "savedQuery": {repo}}.Encode()
	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Studio pipeline editor", link, `document.querySelectorAll(".bq-pl-task").length === 2`, ""},
			{"Studio pipeline settings", link, `document.querySelectorAll(".bq-pl-task").length === 2`,
				`[...document.querySelectorAll(".bq-pipeline [role=tab]")].find((b) => b.textContent === "Settings").click()`},
		})
	})
}

// TestBigQueryStudioDataPreparationThroughThePage (#2105): Create new >
// Data preparation opens its editor: Source, Add step > Filter and
// Transformation, Destination with Write mode Append, the SQLX view (a
// Dataform dataPreparation action), then Run saves it and runs it into the
// destination, read back. WCAG 2.1 AA and the editor by keyboard.
func TestBigQueryStudioDataPreparationThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	s := newStudio2105(t)
	ds := fmt.Sprintf("dataprep_%d", s.stamp)
	fixture(t, "bigquery", s.project, `{"datasetId":"`+ds+`"}`, ds)
	s.query(t, fmt.Sprintf("CREATE TABLE `%s.%s.src` AS SELECT * FROM UNNEST([STRUCT(1 AS v), STRUCT(2 AS v), STRUCT(3 AS v)])", s.project, ds))
	name := fmt.Sprintf("browser data preparation %d", s.stamp)

	p := open(t)
	p.navigate("/bigquery-studio" + s.q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Data preparation")
	p.waitFor(`document.querySelector(".bq-dataprep") !== null && document.querySelector("#bq-dp-source") !== null`)
	p.setField("#bq-dp-source", ds+".src")
	studioMenu(p, ".bq-dataprep", "Add step", "Filter")
	p.waitFor(`document.querySelector("#bq-dp-expr") !== null`)
	p.setField("#bq-dp-expr", "v > 1")
	studioMenu(p, ".bq-dataprep", "Add step", "Transformation")
	p.waitFor(`document.querySelector("#bq-dp-col") !== null`)
	p.setField("#bq-dp-col", "w")
	p.setField("#bq-dp-expr", "v * 10")
	p.clickText(".bq-dataprep .bq-toolbar button", "Destination")
	p.waitFor(`document.querySelector("#bq-dp-dest") !== null`)
	p.setField("#bq-dp-dest", ds+".prepared")
	p.setField("#bq-dp-mode", "APPEND")
	var steps int
	p.eval(`document.querySelectorAll(".bq-dp-steps li").length`, &steps)
	if steps != 4 {
		t.Errorf("Steps lists %d entries, want Source, two steps and Destination", steps)
	}
	p.clickText(".bq-dataprep [role=tab]", "SQLX")
	p.waitFor(`(document.querySelector(".bq-dp-sqlx") || {}).textContent?.includes("dataPreparation")`)
	var sqlx string
	p.eval(`document.querySelector(".bq-dp-sqlx").textContent`, &sqlx)
	for _, want := range []string{`mode: "APPEND"`, "|> WHERE v > 1", "|> EXTEND v * 10 AS `w`"} {
		if !strings.Contains(sqlx, want) {
			t.Errorf("the SQLX lacks %s:\n%s", want, sqlx)
		}
	}
	p.clickText(".bq-dataprep-tab > .bq-toolbar button", "Run")
	studioSaveAs(p, name)
	if state := studioWaitExecution(p, ".bq-dp-runs", 5*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the run is %s: %s", state, studioTaskStates(p, ".bq-dp-runs"))
	}
	if body := s.query(t, fmt.Sprintf("SELECT COUNT(*) AS n, SUM(w) AS s FROM `%s.%s.prepared`", s.project, ds)); !strings.Contains(body, "50") {
		t.Errorf("the destination reads %s, want 2 rows summing to 50", body)
	}
	kind, repo, _ := studioAsset(t, s.project, name)
	if kind != "data_preparation" {
		t.Errorf("the data preparation is listed as a %q", kind)
	}

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		// The views' tabs move with the arrow keys; a step opens by Enter.
		// ArrowLeft selects the tab before the selected one, wrapping.
		var want string
		p.eval(`(() => { const t = [...document.querySelectorAll(".bq-dataprep [role=tab]")]; const i = t.findIndex((n) => n.getAttribute("aria-selected") === "true");
			return t[(i + t.length - 1) % t.length].textContent; })()`, &want)
		p.run(chromedp.Focus(chromedp.CSS(`.bq-dataprep [role=tab][aria-selected=true]`)), chromedp.KeyEvent(kb.ArrowLeft))
		p.waitFor(fmt.Sprintf(`document.querySelector(".bq-dataprep [role=tab][aria-selected=true]").textContent === %q`, want))
		p.run(chromedp.Focus(chromedp.CSS(`.bq-dp-step`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector("#bq-dp-source") !== null`)
		for _, fl := range p.keyboardFailures() {
			t.Errorf("data preparation editor: %s", fl)
		}
	})
	link := "/bigquery-studio?" + url.Values{"project": {s.project}, "savedQuery": {repo}}.Encode()
	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Studio data preparation editor", link, `document.querySelectorAll(".bq-dp-step").length === 4`, ""},
			{"Studio data preparation graph", link, `document.querySelectorAll(".bq-dp-step").length === 4`,
				`[...document.querySelectorAll(".bq-dataprep [role=tab]")].find((b) => b.textContent === "Graph").click()`},
		})
	})
}

// TestBigQueryStudioDataPreparationUpsertValidationFlatten (#2105): Add
// step > Validation (Failed validation rows go to error table), Flatten of
// a RECORD (its nested column generated as PARENT_FIELD) and Unnest of an
// array; Destination with Write mode Upsert and its Merge keys, and an
// error table. The SQLX holds Dataform core's MERGE and ${EXPECT}; Run
// merges into the existing destination and the failing row lands in the
// error table. More > Open YAML opens a YAML data preparation (its source
// and destination, the SQLX view showing its YAML), and Migrate to SQLX
// makes it a SQLX one. WCAG 2.1 AA and the new form by keyboard.
func TestBigQueryStudioDataPreparationUpsertValidationFlatten(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	s := newStudio2105(t)
	ds := fmt.Sprintf("dataprep_up_%d", s.stamp)
	fixture(t, "bigquery", s.project, `{"datasetId":"`+ds+`"}`, ds)
	s.query(t, fmt.Sprintf("CREATE TABLE `%s.%s.src` AS SELECT 1 AS id, 2 AS v, STRUCT(10 AS a) AS s, [1, 2] AS arr UNION ALL SELECT 2, 0, STRUCT(20), [3] UNION ALL SELECT 3, 5, STRUCT(30), [4]", s.project, ds))
	s.query(t, fmt.Sprintf("CREATE TABLE `%s.%s.up` AS SELECT 1 AS id, 0 AS v, 0 AS s_a, 1 AS arr", s.project, ds))
	name := fmt.Sprintf("browser upsert data preparation %d", s.stamp)

	p := open(t)
	p.navigate("/bigquery-studio" + s.q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Data preparation")
	p.waitFor(`document.querySelector(".bq-dataprep") !== null && document.querySelector("#bq-dp-source") !== null`)
	p.setField("#bq-dp-source", ds+".src")
	studioMenu(p, ".bq-dataprep", "Add step", "Validation")
	p.waitFor(`document.querySelector("#bq-dp-expr") !== null && document.querySelector("#bq-dp-validation").checked`)
	p.setField("#bq-dp-expr", "v > 1")
	studioMenu(p, ".bq-dataprep", "Add step", "Flatten")
	p.waitFor(`[...document.querySelectorAll("#bq-dp-col option")].some((o) => o.value === "s")`)
	p.setField("#bq-dp-col", "s")
	p.waitFor(`document.querySelector("#bq-dp-expr").value.includes("AS ` + "`s_a`" + `")`)
	studioMenu(p, ".bq-dataprep", "Add step", "Unnest")
	p.waitFor(`[...document.querySelectorAll("#bq-dp-col option")].some((o) => o.value === "arr")`)
	p.setField("#bq-dp-col", "arr")
	p.clickText(".bq-dataprep .bq-toolbar button", "Destination")
	p.waitFor(`document.querySelector("#bq-dp-dest") !== null`)
	p.setField("#bq-dp-dest", ds+".up")
	p.setField("#bq-dp-mode", "MERGE")
	p.waitFor(`!document.querySelector("#bq-dp-merge-keys").closest(".form-field").hidden`)
	p.setField("#bq-dp-merge-keys", "id, arr")
	p.setField("#bq-dp-errors", ds+".up_errors")
	var labels []string
	p.eval(`[...document.querySelectorAll(".bq-dp-step")].map((b) => b.textContent)`, &labels)
	if len(labels) != 5 || !strings.HasPrefix(labels[1], "Validation: v > 1") || !strings.HasPrefix(labels[2], "Flatten: s") || !strings.Contains(labels[4], "(Upsert)") {
		t.Errorf("Steps = %q", labels)
	}
	p.clickText(".bq-dataprep [role=tab]", "SQLX")
	p.waitFor(`(document.querySelector(".bq-dp-sqlx") || {}).textContent?.includes("dataPreparation")`)
	var sqlx string
	p.eval(`document.querySelector(".bq-dp-sqlx").textContent`, &sqlx)
	for _, want := range []string{`mode: "MERGE", uniqueKey: ["id", "arr"]`, "|> ${EXPECT} v > 1", "|> SELECT * EXCEPT (`s`), `s`.`a` AS `s_a`", "CROSS JOIN UNNEST("} {
		if !strings.Contains(sqlx, want) {
			t.Errorf("the SQLX lacks %s:\n%s", want, sqlx)
		}
	}
	p.clickText(".bq-dataprep-tab > .bq-toolbar button", "Run")
	studioSaveAs(p, name)
	if state := studioWaitExecution(p, ".bq-dp-runs", 5*time.Minute); state != "SUCCEEDED" {
		t.Fatalf("the run is %s: %s", state, studioTaskStates(p, ".bq-dp-runs"))
	}
	if body := s.query(t, fmt.Sprintf("SELECT COUNT(*) AS n, SUM(v) AS sv, SUM(s_a) AS sa FROM `%s.%s.up`", s.project, ds)); !strings.Contains(body, `"3"`) || !strings.Contains(body, `"9"`) || !strings.Contains(body, `"50"`) {
		t.Errorf("the merged destination reads %s, want 3 rows, v summing to 9 and s_a to 50", body)
	}
	if body := s.query(t, fmt.Sprintf("SELECT error FROM `%s.%s.up_errors`", s.project, ds)); !strings.Contains(body, "Validation failed: v > 1") && !strings.Contains(body, `Validation failed: v \u003e 1`) {
		t.Errorf("the error table reads %s", body)
	}

	// More > Open YAML: a YAML data preparation's source and destination.
	studioMenu(p, ".bq-dataprep", "More", "Open YAML")
	p.waitFor(`document.querySelector("#bq-dp-yaml-text") !== null`)
	p.setField("#bq-dp-yaml-text", "nodes:\n- id: node1\n  source: {table: {dataset: "+ds+", table: src}}\n- id: node2\n  source: {nodeId: node1}\n  destination: {table: {dataset: "+ds+", table: fromyaml}}\n")
	p.clickText(".bq-dp-form button", "Open")
	p.waitFor(`document.querySelector(".bq-dp-yaml button") !== null`)
	p.eval(`[...document.querySelectorAll(".bq-dp-step")].map((b) => b.textContent)`, &labels)
	if len(labels) != 2 || labels[0] != "Source: "+ds+".src" || !strings.HasPrefix(labels[1], "Destination: "+ds+".fromyaml") {
		t.Errorf("the YAML data preparation's steps = %q", labels)
	}
	p.clickText(".bq-dataprep [role=tab]", "Data")
	p.clickText(".bq-dataprep [role=tab]", "SQLX")
	p.waitFor(`(document.querySelector(".bq-dp-sqlx") || {}).textContent?.includes("nodeId: node1")`)

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		// Migrate to SQLX by Enter.
		p.run(chromedp.Focus(chromedp.CSS(`.bq-dp-yaml button`)), chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".bq-dp-yaml button") === null`)
		for _, fl := range p.keyboardFailures() {
			t.Errorf("data preparation editor: %s", fl)
		}
	})
	p.waitFor(`(document.querySelector(".bq-dp-sqlx") || {}).textContent?.includes("dataPreparation")`)
	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Studio data preparation Open YAML", "/bigquery-studio" + s.q, `document.querySelector(".bq-home") !== null`,
				`[...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent.includes("Data preparation")).click();
				const until = async (f) => { for (let i = 0; i < 200 && !f(); i++) await new Promise((r) => setTimeout(r, 50)); };
				await until(() => [...document.querySelectorAll(".bq-dataprep .bq-menu-item")].some((b) => b.textContent === "Open YAML"));
				[...document.querySelectorAll(".bq-dataprep .bq-menu-item")].find((b) => b.textContent === "Open YAML").click();
				await until(() => document.querySelector("#bq-dp-yaml-text") !== null)`},
		})
	})
}
