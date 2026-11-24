//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestBigQueryStudioAutocompleteAndQueryModeThroughThePage (#2094,
// 2026-11-23 third round): the query editor's Tools > Parser-based
// auto-completion and Edit > Query mode, as Google's docs give them
// (https://docs.cloud.google.com/bigquery/docs/running-queries and
// continuous-queries).
//
// Auto-completion is on: typed by keyboard, "SELECT * FROM " suggests the
// project and the dataset, Enter inserts "dataset." and suggests its
// table, Enter inserts it; Ctrl+Space in the select list suggests the
// table's columns first, and "alias." its columns. Turned off in Tools,
// typing suggests nothing. Edit > Query mode > Optional job creation,
// confirmed, runs a SELECT without a job (Job information: No job
// created, and no new job in Personal history's API list); Continuous
// query, confirmed, refuses a continuous INSERT with the CONTINUOUS
// reservation rule. WCAG 2.1 AA with the suggestions open and with the
// confirmation open; the menus, items and suggestions by keyboard.
func TestBigQueryStudioAutocompleteAndQueryModeThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	ds := fmt.Sprintf("browser_ac_%d", uniqueSuffix())
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	stmt, _ := json.Marshal(map[string]any{"Path": []string{ds}, "Mode": "read-write",
		"Statement": "CREATE TABLE " + ds + ".orders AS SELECT 1 AS order_id, 'x' AS buyer_name, 2.5 AS total"})
	if code, body := consoleDo(t, http.MethodPost, "/api/query/bigquery"+q, string(stmt)); code != http.StatusOK {
		t.Fatalf("CREATE TABLE through the console API = %d: %s", code, body)
	}

	p := open(t)
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.eval(`localStorage.removeItem("cloudburrow.bigquery.autocomplete"); true`, nil)
	p.clickText(".bq-new-query", "SQL query")
	p.waitFor(`[...document.querySelectorAll(".bq-editor")].some((e) => e.offsetParent !== null)`)
	p.eval(`[...document.querySelectorAll(".bq-editor")].find((e) => e.offsetParent !== null).id = "ac-editor"; true`, nil)
	const editor = "#ac-editor"
	menu := func(label string) {
		t.Helper()
		p.clickExpr(fmt.Sprintf(`[...document.querySelectorAll(".bq-toolbar .bq-menu-button[aria-label=%s]")].find((b) => b.offsetParent !== null)`, label))
		p.waitFor(`document.querySelector(".bq-menu:not([hidden])") !== null`)
	}
	items := func() []string {
		var out []string
		p.eval(`[...document.querySelectorAll(".bq-menu:not([hidden]) > .bq-menu-item, .bq-menu:not([hidden]) > .bq-submenu-wrap > .bq-menu-item")].map((b) => b.getAttribute("aria-label") || b.textContent.replace("✓", ""))`, &out)
		return out
	}
	shown := func() []string {
		var out []string
		p.eval(`[...document.querySelectorAll(".bq-sql-completions:not([hidden]) li")].map((li) => li.firstChild.textContent)`, &out)
		return out
	}
	waitShown := func(label string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".bq-sql-completions:not([hidden]) li")].some((li) => li.firstChild.textContent === %q)`, label))
	}
	value := func() string {
		var v string
		p.eval(`document.querySelector("#ac-editor").value`, &v)
		return v
	}

	// Tools > Parser-based auto-completion, checked by default.
	menu("Tools")
	if got := items(); strings.Join(got, "|") != "Parser-based auto-completion" {
		t.Errorf("Tools' items are %v, want Parser-based auto-completion", got)
	}
	var checked string
	p.eval(`document.querySelector(".bq-menu:not([hidden]) [role=menuitemcheckbox]").getAttribute("aria-checked")`, &checked)
	if checked != "true" {
		t.Errorf("Parser-based auto-completion is aria-checked=%q, want true by default", checked)
	}
	p.run(chromedp.KeyEvent(kb.Escape))

	// Typed: the project and the dataset after FROM.
	p.run(chromedp.Focus(chromedp.CSS(editor)), chromedp.SendKeys(editor, "SELECT * FROM "))
	waitShown(ds)
	if got := shown(); len(got) < 2 || got[0] != project {
		t.Errorf("after FROM the suggestions are %v, want the project first, then its datasets", got)
	}
	var role string
	p.eval(`document.querySelector("#ac-editor").getAttribute("aria-expanded") + "|" + document.querySelector(".bq-sql-completions").getAttribute("role")`, &role)
	if role != "true|listbox" {
		t.Errorf("the editor and its suggestions are %q, want an expanded listbox", role)
	}
	for _, fl := range p.contrastFailures() {
		t.Errorf("suggestions: %s", fl)
	}
	for _, u := range p.unnamedControls() {
		t.Errorf("suggestions: an unnamed %s", u)
	}
	// Down to the dataset, Enter inserts it with its dot and offers its
	// table; Enter inserts the table.
	var idx int
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(".bq-sql-completions li")].findIndex((li) => li.firstChild.textContent === %q)`, ds), &idx)
	for i := 0; i < idx; i++ {
		p.run(chromedp.KeyEvent(kb.ArrowDown))
	}
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector("#ac-editor").value === %q`, "SELECT * FROM "+ds+"."))
	waitShown("orders")
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector("#ac-editor").value === %q`, "SELECT * FROM "+ds+".orders"))

	// Ctrl+Space in the select list: the table's columns, first.
	p.eval(`(() => { const e = document.querySelector("#ac-editor"); e.focus(); e.setSelectionRange(7, 8); return true; })()`, nil)
	p.run(chromedp.KeyEvent(kb.Backspace))
	p.run(chromedp.KeyEvent(" ", chromedp.KeyModifiers(2))) // Ctrl+Space
	waitShown("order_id")
	if got := shown(); len(got) < 3 || got[0] != "buyer_name" || got[1] != "order_id" || got[2] != "total" {
		t.Errorf("Ctrl+Space in the select list: %v, want buyer_name, order_id, total first", got)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector(".bq-sql-completions").hidden`)

	// An alias's columns after "o.".
	p.setField(editor, "")
	p.run(chromedp.Focus(chromedp.CSS(editor)), chromedp.SendKeys(editor, "SELECT o.tot"))
	p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#ac-editor"); const at = e.value.length;
		e.value += %q; e.setSelectionRange(at, at); return true; })()`, " FROM "+ds+".orders o"), nil)
	p.run(chromedp.KeyEvent(" ", chromedp.KeyModifiers(2)))
	waitShown("total")
	if got := shown(); len(got) != 1 {
		t.Errorf("o.tot: %v, want total alone", got)
	}
	p.run(chromedp.KeyEvent(kb.Tab))
	if v := value(); v != "SELECT o.total FROM "+ds+".orders o" {
		t.Errorf("Tab inserted: %q", v)
	}

	// Turned off: typing suggests nothing.
	menu("Tools")
	p.clickExpr(`document.querySelector(".bq-menu:not([hidden]) [role=menuitemcheckbox]")`)
	p.setField(editor, "")
	p.run(chromedp.Focus(chromedp.CSS(editor)), chromedp.SendKeys(editor, "SELECT * FROM "))
	time.Sleep(800 * time.Millisecond)
	if got := shown(); len(got) != 0 {
		t.Errorf("with Parser-based auto-completion off, typing suggests %v", got)
	}
	menu("Tools")
	p.eval(`document.querySelector(".bq-menu:not([hidden]) [role=menuitemcheckbox]").getAttribute("aria-checked")`, &checked)
	if checked != "false" {
		t.Errorf("after deselecting, aria-checked=%q", checked)
	}
	p.run(chromedp.KeyEvent(kb.Escape))

	// Edit > Query mode: Optional job creation and Continuous query, by
	// keyboard from the Edit menu.
	editBtn := `[...document.querySelectorAll(".bq-toolbar .bq-menu-button[aria-label=Edit]")].find((b) => b.offsetParent !== null)`
	p.eval(editBtn+`.id = "ac-edit"; true`, nil)
	p.run(chromedp.Focus(chromedp.CSS("#ac-edit")), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Query settings"`)
	if got := items(); strings.Join(got, "|") != "Query settings|Query mode|Clear query" {
		t.Errorf("Edit's items are %v, want Query settings, Query mode, Clear query", got)
	}
	p.run(chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.ArrowRight))
	p.waitFor(`document.activeElement && document.activeElement.getAttribute("role") === "menuitemcheckbox"`)
	var modes []string
	p.eval(`[...document.querySelectorAll(".bq-submenu:not([hidden]) .bq-menu-item")].map((b) => b.textContent.replace("✓", "") + ":" + b.getAttribute("aria-checked"))`, &modes)
	if strings.Join(modes, "|") != "Optional job creation:false|Continuous query:false" {
		t.Errorf("Query mode's modes are %v", modes)
	}
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	var heading string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &heading)
	if heading != "Switch to Optional job creation?" {
		t.Errorf("the confirmation is %q", heading)
	}
	// Measured once the dialog has finished opening.
	p.waitFor(`document.getAnimations().every((a) => a.playState !== "running")`)
	for _, fl := range p.keyboardFailures() {
		t.Errorf("Query mode confirmation: %s", fl)
	}
	for _, fl := range p.contrastFailures() {
		t.Errorf("Query mode confirmation: %s", fl)
	}
	p.pressConfirm("Confirm")
	p.waitFor(`[...document.querySelectorAll(".bq-mode")].some((m) => m.offsetParent !== null && m.textContent === "Optional job creation")`)

	jobsBefore := studioJobCount(t, q)
	p.setField(editor, "SELECT 42 AS answer")
	p.clickExpr(`[...document.querySelectorAll(".bq-toolbar button.primary")].find((b) => b.textContent === "Run" && b.offsetParent !== null)`)
	p.waitForWithin(`[...document.querySelectorAll(".bq-results")].some((r) => r.offsetParent !== null && r.textContent.includes("without creating a job"))`, 2*time.Minute)
	var info string
	p.clickExpr(`[...document.querySelectorAll(".bq-results [role=tab]")].find((b) => b.textContent === "Job information" && b.offsetParent !== null)`)
	p.waitFor(`[...document.querySelectorAll(".bq-job-info")].some((d) => d.offsetParent !== null)`)
	p.eval(`[...document.querySelectorAll(".bq-job-info")].find((d) => d.offsetParent !== null).textContent`, &info)
	if !strings.Contains(info, "Query ID") || !strings.Contains(info, "No job created") {
		t.Errorf("Job information: %q, want a Query ID and No job created", info)
	}
	if after := studioJobCount(t, q); after != jobsBefore {
		t.Errorf("Optional job creation made a job: %d jobs before, %d after", jobsBefore, after)
	}

	// Continuous query: an INSERT is refused by the reservation rule, a
	// 400 the page expects.
	p.expected = append(p.expected, "/api/studio/bigquery?call=query")
	p.run(chromedp.Focus(chromedp.CSS("#ac-edit")), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Query settings"`)
	p.run(chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	p.pressConfirm("Confirm")
	p.waitFor(`[...document.querySelectorAll(".bq-mode")].some((m) => m.offsetParent !== null && m.textContent === "Continuous query")`)
	p.setField(editor, "INSERT INTO "+ds+".orders SELECT 2, 'y', 1.0")
	p.clickExpr(`[...document.querySelectorAll(".bq-toolbar button.primary")].find((b) => b.textContent === "Run" && b.offsetParent !== null)`)
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	p.clickText(".modal.is-open button", "Run statement")
	p.waitForWithin(`[...document.querySelectorAll(".bq-results .form-error")].some((r) => r.offsetParent !== null)`, 2*time.Minute)
	var refusal string
	p.eval(`[...document.querySelectorAll(".bq-results .form-error")].find((r) => r.offsetParent !== null).textContent`, &refusal)
	if !strings.Contains(refusal, "CONTINUOUS assignment") {
		t.Errorf("a continuous query: %q, want the CONTINUOUS reservation rule", refusal)
	}
	// Choosing the checked mode again goes back to a query job.
	p.run(chromedp.Focus(chromedp.CSS("#ac-edit")), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Query settings"`)
	p.run(chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`![...document.querySelectorAll(".bq-mode")].some((m) => m.offsetParent !== null)`)

	auditScreens(t, []a11yPage{
		{"Studio query editor suggestions", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`,
			`[...document.querySelectorAll(".bq-new-query")].find((b) => b.textContent.includes("SQL query")).click()`},
	})
}

// studioJobCount is the number of the project's jobs Studio's Personal
// history lists.
func studioJobCount(t *testing.T, q string) int {
	t.Helper()
	code, body := consoleDo(t, http.MethodPost, "/api/studio/bigquery"+q+"&call=jobs", `{"all":true}`)
	if code != http.StatusOK {
		t.Fatalf("Studio jobs = %d: %s", code, body)
	}
	var got struct{ Jobs []json.RawMessage }
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Studio jobs answered %s", body)
	}
	return len(got.Jobs)
}

// TestBigQuerySparkNotebookManageSessionsThroughThePage (#2094, in the
// spark shard): an Apache Spark notebook's runtime selector has Manage
// sessions, whose Active sessions dialog lists the project's Spark Connect
// session and terminates it with the terminate icon and Terminate
// (https://docs.cloud.google.com/bigquery/docs/use-spark, End the Spark
// session). WCAG 2.1 AA and keyboard for the dialog.
func TestBigQuerySparkNotebookManageSessionsThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	needService(t, "dataproc-sessions")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a session runs in the pinned apache/spark image")
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	id := fmt.Sprintf("studio-sess-%d", uniqueSuffix())
	full := "projects/" + project + "/locations/us-central1/sessions/" + id
	values, _ := json.Marshal(map[string]string{"sessionId": id, "region": "us-central1", "runtimeVersion": "3.0"})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/dataproc-sessions"+q, string(values)); code != http.StatusOK {
		t.Fatalf("create a session = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodPost, "/api/actions/dataproc-sessions"+q, fmt.Sprintf(`{"Path":[%q],"Action":"delete"}`, full))
	})
	state := func() string {
		code, body := consoleDo(t, http.MethodGet, "/api/resources/dataproc-sessions"+q, "")
		if code != http.StatusOK {
			t.Fatalf("list sessions = %d: %s", code, body)
		}
		var l struct {
			Items []struct{ Name, Status string }
		}
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			if it.Name == id {
				return it.Status
			}
		}
		return ""
	}
	deadline := time.Now().Add(15 * time.Minute)
	for st := state(); st != "ACTIVE"; st = state() {
		if st == "FAILED" || time.Now().After(deadline) {
			t.Fatalf("the session is %q, want ACTIVE", st)
		}
		time.Sleep(3 * time.Second)
	}

	p := openWithin(t, 30*time.Minute)
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Apache Spark notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null`)
	more := `.bq-notebook .bq-toolbar button[aria-label="Additional connection options"]`
	p.run(chromedp.Focus(chromedp.CSS(more)), chromedp.KeyEvent(kb.Enter))
	// Focus is on the first item that is enabled: Connect to a runtime
	// when Colab Enterprise's runtimes are served, else Manage sessions.
	p.waitFor(`document.activeElement && document.activeElement.closest(".bq-menu:not([hidden])") !== null`)
	var opts []string
	p.eval(`[...document.querySelectorAll(".bq-menu:not([hidden]) > .bq-menu-item")].map((b) => b.textContent)`, &opts)
	if strings.Join(opts, "|") != "Connect to a runtime|Manage sessions" {
		t.Errorf("the runtime selector's items are %v", opts)
	}
	var first string
	p.eval(`document.activeElement.textContent`, &first)
	if first != "Manage sessions" {
		p.run(chromedp.KeyEvent(kb.ArrowDown))
	}
	p.waitFor(`document.activeElement.textContent === "Manage sessions"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#bq-sessions-title") !== null`)
	var title string
	p.eval(`document.querySelector("#bq-sessions-title").textContent`, &title)
	if title != "Active sessions" {
		t.Errorf("the dialog is %q, want Active sessions", title)
	}
	term := fmt.Sprintf(`.bq-sessions button[aria-label="Terminate session %s"]`, id)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, term))
	p.waitFor(`document.getAnimations().every((a) => a.playState !== "running")`)
	for _, fl := range p.keyboardFailures() {
		t.Errorf("Active sessions: %s", fl)
	}
	for _, fl := range p.contrastFailures() {
		t.Errorf("Active sessions: %s", fl)
	}
	for _, u := range p.unnamedControls() {
		t.Errorf("Active sessions: an unnamed %s", u)
	}
	p.run(chromedp.Focus(chromedp.CSS(term)), chromedp.KeyEvent(kb.Enter))
	// The confirmation opens over Active sessions, which stays open.
	p.confirmOnlyDialogLabelled("Terminate")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open form[data-confirm] .modal-actions button.danger`)))
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") === null && document.querySelector("#bq-sessions-title") !== null`)
	deadline = time.Now().Add(3 * time.Minute)
	for st := state(); st != "TERMINATED" && st != "TERMINATING"; st = state() {
		if time.Now().After(deadline) {
			t.Fatalf("after Terminate the session is %q", st)
		}
		time.Sleep(2 * time.Second)
	}
	p.waitForWithin(fmt.Sprintf(`document.querySelector(%q) === null`, term), time.Minute)

	auditScreens(t, []a11yPage{
		{"Studio Active sessions", "/bigquery-studio" + q, `document.querySelector(".bq-tree") !== null`,
			`[...document.querySelectorAll(".bq-home-action")].find((b) => b.textContent.trim() === "Apache Spark notebook").click();
			 setTimeout(() => { document.querySelector(` + fmt.Sprintf("%q", more) + `).click();
			   [...document.querySelectorAll(".bq-menu-item")].find((x) => x.textContent === "Manage sessions" && x.offsetParent !== null).click(); }, 300)`},
	})
}
