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

// TestBigQueryFlexibleNamesInsertOptionsEditTableAndViewThroughTheForms
// (#994), in the emulators shard. Create table's schema editor takes a
// flexible column name, "first name", which it used to refuse on the form,
// and the table is made with it. Insert rows with Skip invalid rows checked
// inserts the valid rows and answers with the invalid one, by its number, in
// the API's words. Edit table opens with the table's description, changes it
// and adds a label, which the table's page then shows. Create table with the
// VIEW table type and a view query makes a view, whose page offers Edit
// table and Delete table and no Insert rows.
func TestBigQueryFlexibleNamesInsertOptionsEditTableAndViewThroughTheForms(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_edit_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`","description":"x"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Create table")
	p.waitFor(`document.querySelector(".modal #f-tableId") !== null && document.querySelectorAll(".modal .schema-row").length === 1`)
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "people", chromedp.ByQuery))
	field := func(label string) string { return fmt.Sprintf(`.modal [aria-label=%q]`, label) }
	p.run(chromedp.SendKeys(field("Field 1 name"), "id", chromedp.ByQuery))
	p.setField(field("Field 1 type"), "INTEGER")
	p.setField(field("Field 1 mode"), "REQUIRED")
	p.clickText(".modal button", "Add field")
	p.run(chromedp.SendKeys(field("Field 2 name"), "first name", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	schema := readDataPage(t, "bigquery", project, ds, "people").rows("schema")
	if f, ok := schema["first name"]; !ok || f["Type"] != "STRING" {
		t.Errorf("the table's schema is %v, want the field first name", schema)
	}

	p.navigate("/bigquery/" + ds + "/people" + q)
	p.clickText("#view .page-actions button", "Insert rows")
	p.waitFor(`document.querySelector(".modal #f-rows") !== null && document.querySelector(".modal #f-skipInvalidRows") !== null`)
	p.setField(".modal #f-rows", `{"id": 1, "first name": "ann"}`+"\n"+`{"first name": "no id"}`+"\n"+`{"id": 3}`)
	p.run(chromedp.Click(`.modal #f-skipInvalidRows`, chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result-table td") !== null`)
	var cells []string
	p.eval(`[...document.querySelectorAll("#action-result-table td")].map((c) => c.textContent)`, &cells)
	if len(cells) != 2 || cells[0] != "2" || !strings.Contains(cells[1], "Missing required field: id.") {
		t.Errorf("Insert rows with Skip invalid rows answered %v, want row 2 and the API's reason", cells)
	}
	var dialog string
	p.eval(`document.querySelector(".modal").textContent`, &dialog)
	if !strings.Contains(dialog, "Inserted 2 of 3 rows; 1 skipped as invalid.") {
		t.Errorf("the result does not say how many were inserted: %.300s", dialog)
	}
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	if preview := readDataPage(t, "bigquery", project, ds, "people").rows("preview"); len(preview) != 2 {
		t.Errorf("the table previews %v, want the 2 valid rows", preview)
	}

	p.navigate("/bigquery/" + ds + "/people" + q)
	p.clickText("#view .page-actions button", "Edit table")
	p.waitFor(`document.querySelector(".modal #f-description") !== null`)
	p.setField(".modal #f-description", "people, edited")
	p.setField(".modal #f-labels", "team=data")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	_, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {ds, "people"}}.Encode(), "")
	if !strings.Contains(body, `"people, edited"`) || !strings.Contains(body, `"team: data"`) {
		t.Errorf("after Edit table the page does not show the description and label: %.600s", body)
	}

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Create table")
	p.waitFor(`document.querySelector(".modal #f-tableType") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "names", chromedp.ByQuery))
	p.setField(".modal #f-tableType", "VIEW")
	p.setField(".modal #f-viewQuery", "SELECT `first name` AS name FROM `"+project+"."+ds+".people`")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if s := readDataPage(t, "bigquery", project, ds, "names").summary("Type"); s != "VIEW" {
		t.Errorf("Create table with VIEW made a %q", s)
	}
	p.navigate("/bigquery/" + ds + "/names" + q)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit table")`)
	var buttons []string
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent)`, &buttons)
	if strings.Join(buttons, ",") != "Edit table,Delete table" {
		t.Errorf("the view's page offers %v", buttons)
	}
}

// TestBigQueryReadWriteEditorThroughTheForms (#994), in the emulators shard.
// A dataset's Query tab starts Read-only; a CREATE TABLE there is refused,
// naming the switch. Switched to Read-write, the Run button reads Run
// statement, and running a CREATE TABLE … AS SELECT asks first, naming the
// project and dataset and showing the statement; Cancel sends nothing.
// Confirmed, it answers that the statement ran, and the dataset has the
// table. An INSERT is refused on the page with the reason and #1008, and
// nothing is sent to BigQuery.
func TestBigQueryReadWriteEditorThroughTheForms(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_rw_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery/" + ds + q)
	p.waitFor(`document.querySelector("#tab-query") !== null`)
	p.run(chromedp.Click(`#tab-query`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view .query-editor") !== null && document.querySelector("#view .query-mode") !== null`)
	statement := func(sql string) {
		p.eval(`(() => { const e = document.querySelector("#view .query-editor"); e.value = ""; return true; })()`, nil)
		p.run(chromedp.SendKeys(`#view .query-editor`, sql, chromedp.ByQuery))
	}
	alert := func() string {
		p.waitFor(`[...document.querySelectorAll("#view .form-error[role=alert]")].some((e) => !e.hidden)`)
		var text string
		p.eval(`[...document.querySelectorAll("#view .form-error[role=alert]")].map((e) => e.textContent).join(" ")`, &text)
		p.forgive()
		return text
	}
	const ctas = "CREATE TABLE totals AS SELECT 'eu' AS region, 3 AS n"
	statement(ctas)
	p.clickText("#view .query-pane button.primary", "Run")
	if got := alert(); !strings.Contains(got, "Switch it to Read-write") {
		t.Errorf("a CREATE TABLE in Read-only was answered %q", got)
	}

	p.run(chromedp.Click(`#view .query-mode input[value="read-write"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view .query-pane button.primary").textContent === "Run statement"`)
	var hint string
	p.eval(`document.querySelector("#view .query-pane .card > p.unavailable").textContent`, &hint)
	if !strings.Contains(hint, "jobs.query") || !strings.Contains(hint, "#1008") {
		t.Errorf("the Read-write hint is %q", hint)
	}
	before := len(p.posts("/api/query/bigquery"))
	p.clickText("#view .query-pane button.primary", "Run statement")
	p.waitFor(`document.querySelector("#dml-confirm-title") !== null`)
	var confirm struct{ Title, Statement, Button string }
	p.eval(`({ Title: document.querySelector("#dml-confirm-title").textContent,
		Statement: document.querySelector(".modal pre").textContent,
		Button: document.querySelector(".modal button[type=submit]").textContent })`, &confirm)
	if !strings.Contains(confirm.Title, project) || !strings.Contains(confirm.Title, ds) ||
		confirm.Statement != ctas || confirm.Button != "Run statement" {
		t.Errorf("the confirmation is %+v", confirm)
	}
	p.clickText(".modal button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := len(p.posts("/api/query/bigquery")); n != before {
		t.Errorf("Cancel sent the statement (%d requests)", n-before)
	}
	p.clickText("#view .query-pane button.primary", "Run statement")
	p.waitFor(`document.querySelector("#dml-confirm-title") !== null`)
	p.run(chromedp.Click(`.modal button[type=submit]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view .query-results").textContent.includes("The statement ran")`)
	if tables := readDataPage(t, "bigquery", project, ds).rows("tables"); tables["totals"] == nil {
		t.Errorf("after the confirmed CREATE TABLE the dataset lists %v", tables)
	}

	statement("INSERT INTO totals (region, n) VALUES ('us', 1)")
	p.clickText("#view .query-pane button.primary", "Run statement")
	p.waitFor(`document.querySelector("#dml-confirm-title") !== null`)
	p.run(chromedp.Click(`.modal button[type=submit]`, chromedp.ByQuery))
	if got := alert(); !strings.Contains(got, "INSERT is DML") || !strings.Contains(got, "#1008") {
		t.Errorf("an INSERT was answered %q, want the refusal naming #1008", got)
	}
	if preview := readDataPage(t, "bigquery", project, ds, "totals").rows("preview"); len(preview) != 1 {
		t.Errorf("after the refused INSERT the table previews %v, want its 1 row", preview)
	}
}
