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
)

// TestBigQueryStudioLeftPaneJobHistoryOverviewAndDestination (#1861), in
// the emulators shard. Studio's left pane has Google's tabs, Explorer, Data,
// Files and Source control: Data lists the data hierarchy without the
// saved queries, Files lists the saved queries and Source control says it
// is not served. The Explorer's Job history opens a Job history tab with
// Personal and Project history. A dataset's Overview holds its Tables,
// Routines and Models. Create table's Destination is Project, Dataset and
// Table, and the table is made in the Dataset chosen there. A query job's
// page names its user and the bytes it processed and billed.
func TestBigQueryStudioLeftPaneJobHistoryOverviewAndDestination(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_studio3_%d", stamp)
	other := fmt.Sprintf("browser_studio3b_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	studio := "/api/studio/bigquery?project=" + url.QueryEscape(project) + "&call="
	for _, d := range []string{ds, other} {
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+d+`"}`); code != http.StatusOK {
			t.Fatalf("create dataset %s through the console API = %d: %s", d, code, body)
		}
		name := d
		t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+name, "") })
	}
	saveName := fmt.Sprintf("browser files %d", stamp)
	code, body := consoleDo(t, http.MethodPost, studio+"savequery", `{"name":"`+saveName+`","query":"SELECT 1"}`)
	if code != http.StatusOK {
		t.Fatalf("savequery = %d: %s", code, body)
	}
	var sq struct{ ID string }
	if err := json.Unmarshal([]byte(body), &sq); err != nil || sq.ID == "" {
		t.Fatalf("savequery answered %s", body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodPost, studio+"deletequery", `{"id":"`+sq.ID+`"}`) })

	// The left pane's tabs, in Google's order.
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelectorAll(".bq-pane-tabs .tab").length === 4`)
	var panes []string
	p.eval(`[...document.querySelectorAll(".bq-pane-tabs .tab")].map((b) => b.textContent)`, &panes)
	if strings.Join(panes, "|") != "Explorer|Data|Files|Source control" {
		t.Errorf("the left pane's tabs are %q", panes)
	}
	queries := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+".queries")
	dsNode := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+"."+ds)
	p.waitFor(queries + ` !== null && ` + dsNode + ` !== null`)

	// Data: the datasets, no code assets.
	p.clickText(".bq-pane-tabs .tab", "Data")
	p.waitFor(`document.querySelector(".bq-tree[aria-label=Data]") !== null && ` + dsNode + ` !== null`)
	var hasQueries bool
	p.eval(queries+` !== null`, &hasQueries)
	if hasQueries {
		t.Error("the Data pane lists the saved queries, which Google's leaves out")
	}

	// Files: the saved query, opened as its tab.
	p.clickText(".bq-pane-tabs .tab", "Files")
	p.clickText(".bq-files .bq-node-name", saveName)
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === ` + fmt.Sprintf("%q", saveName))

	// Source control: Dataform's repositories where Dataform is served
	// (#2094), otherwise not served, and says why.
	p.clickText(".bq-pane-tabs .tab", "Source control")
	p.waitFor(`document.querySelector("#bq-pane .unavailable") !== null`)
	var source string
	p.eval(`document.querySelector("#bq-pane").textContent`, &source)
	if hasService(t, "dataform") {
		if !strings.Contains(source, "Dataform repositories") {
			t.Errorf("Source control says %q", source)
		}
	} else if !strings.Contains(source, "Not available locally") || !strings.Contains(source, "Dataform") {
		t.Errorf("Source control says %q", source)
	}

	// Explorer > Job history: a tab with Personal and Project history.
	if code, body := consoleDo(t, http.MethodPost, studio+"query", `{"query":"SELECT 17 AS n"}`); code != http.StatusOK {
		t.Fatalf("a Studio query = %d: %s", code, body)
	}
	p.clickText(".bq-pane-tabs .tab", "Explorer")
	p.clickText(".bq-explorer-jobs", "Job history")
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === "Job history" &&
		document.querySelectorAll(".bq-history-table tbody tr").length > 0`)
	var views []string
	p.eval(`[...document.querySelectorAll(".bq-history-tabs .tab")].map((b) => b.textContent)`, &views)
	if strings.Join(views, "|") != "Personal history|Project history" {
		t.Errorf("Job history's views are %q", views)
	}
	var tabsBefore, tabsAfter int
	p.eval(`document.querySelectorAll(".bq-tabs .tab").length`, &tabsBefore)
	p.clickText(".bq-explorer-jobs", "Job history")
	p.eval(`document.querySelectorAll(".bq-tabs .tab").length`, &tabsAfter)
	if tabsAfter != tabsBefore {
		t.Errorf("a second Job history opened another tab: %d, then %d", tabsBefore, tabsAfter)
	}

	// A query job's page: User, Bytes processed and Bytes billed.
	p.waitFor(`document.querySelector(".bq-history-table tbody tr a") !== null`)
	var jobHref string
	p.eval(`document.querySelector(".bq-history-table tbody tr a").getAttribute("href")`, &jobHref)
	p.navigate(jobHref)
	p.waitFor(`[...document.querySelectorAll("#view dt")].some((d) => d.textContent === "Bytes processed")`)
	var labels []string
	p.eval(`[...document.querySelectorAll("#view dt")].map((d) => d.textContent)`, &labels)
	got := strings.Join(labels, "|")
	for _, want := range []string{"User", "Bytes processed", "Bytes billed", "Duration"} {
		if !strings.Contains(got, want) {
			t.Errorf("the query job's details have no %s: %q", want, labels)
		}
	}

	// The dataset's Overview holds Tables, Routines and Models.
	p.navigate("/bigquery/" + ds + q)
	p.waitFor(`document.querySelector("#view .sub-tabs .tab") !== null`)
	var dsTabs struct{ Top, Sub []string }
	p.eval(`({ top: [...document.querySelectorAll("#view .tab-strip:not(.sub-tabs) > .tab")].filter((b) => !b.hidden).map((b) => b.textContent),
		sub: [...document.querySelectorAll("#view .sub-tabs .tab")].map((b) => b.textContent) })`, &dsTabs)
	if len(dsTabs.Top) < 2 || strings.Join(dsTabs.Top[:2], "|") != "Overview|Details" {
		t.Errorf("the dataset's tabs are %q, want Overview, Details first", dsTabs.Top)
	}
	if strings.Join(dsTabs.Sub, "|") != "Tables|Routines|Models" {
		t.Errorf("the dataset's Overview holds %q", dsTabs.Sub)
	}

	// Create table: Destination's Dataset chooses where the table goes.
	p.clickText("#view .page-actions button", "Create table")
	p.waitFor(`document.querySelector(".modal.is-open #f-tableDataset") !== null`)
	var dest struct{ Project, Dataset string }
	p.eval(`({ project: document.querySelector(".modal.is-open #f-tableProject").value,
		dataset: document.querySelector(".modal.is-open #f-tableDataset").value })`, &dest)
	if dest.Project != project || dest.Dataset != ds {
		t.Errorf("Destination opens on project %q, dataset %q", dest.Project, dest.Dataset)
	}
	p.setField(".modal.is-open #f-tableDataset", other)
	p.setField(".modal.is-open #f-tableId", "chosen")
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open .schema-row:nth-child(1) .schema-name`), "id"))
	p.setField(".modal.is-open .schema-row:nth-child(1) .schema-type", "INTEGER")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, ok := readDataPage(t, "bigquery", project, other).rows("tables")["chosen"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Create table did not make the table in the Destination's dataset: %v", readDataPage(t, "bigquery", project, other).rows("tables"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, ok := readDataPage(t, "bigquery", project, ds).rows("tables")["chosen"]; ok {
		t.Error("the table was made in the dataset the form was opened on, not the one chosen")
	}
}

// TestBigQueryStudioPanesMeetWCAG (#1861), in the emulators shard: Studio
// with each of its left pane's tabs, the Job history tab, a dataset's
// Overview and Create table pass the console's WCAG 2.1 AA audit.
func TestBigQueryStudioPanesMeetWCAG(t *testing.T) {
	needService(t, "bigquery")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	ds := fmt.Sprintf("a11y_studio_%d", uniqueSuffix())
	fixture(t, "bigquery", project, `{"datasetId":"`+ds+`"}`, ds)
	studio := "/bigquery-studio" + q
	tree := `document.querySelector(".bq-tree") !== null`
	pane := func(label string) string {
		return `[...document.querySelectorAll(".bq-pane-tabs .tab")].find((b) => b.textContent === "` + label + `").click()`
	}
	auditScreens(t, []a11yPage{
		{"Studio Explorer", studio, tree, ""},
		{"Studio Data pane", studio, tree, pane("Data")},
		{"Studio Files pane", studio, tree, pane("Files")},
		{"Studio Source control pane", studio, tree, pane("Source control")},
		{"Studio Job history tab", studio, tree, `document.querySelector(".bq-explorer-jobs").click()`},
		{"dataset Overview", "/bigquery/" + ds + q, settled + ` && document.querySelector("#view .sub-tabs") !== null`, ""},
		{"Create table dialog", "/bigquery/" + ds + q, settled,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Create table").click()`},
	})
}
