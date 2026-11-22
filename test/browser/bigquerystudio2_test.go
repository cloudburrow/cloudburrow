//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBigQueryStudioHomeTabsVisualizationAndSavedQueries (#1861), in the
// emulators shard. Studio opens on its Home tab, in Google's sections. A
// query's results have a Visualization tab whose chart is drawn from the
// rows, with the configuration pane prefilled from the schema. Save > Save
// query keeps the query, CloudBurrow-only, and lists it under the
// Explorer's Queries node; Save query as makes a second. Open from the
// Explorer opens a dataset and a table as Studio tabs, with their pages'
// own tabs; a plain click replaces the italic preview tab. "+ Add data"
// opens Create dataset as a panel on the right, with Google's region list,
// and the dataset's Create table, also a panel, makes a table from an
// uploaded file.
func TestBigQueryStudioHomeTabsVisualizationAndSavedQueries(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_studio2_%d", stamp)
	made := fmt.Sprintf("browser_panel_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "")
		consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+made, "")
	})
	saveName := fmt.Sprintf("browser saved %d", stamp)
	t.Cleanup(func() {
		code, body := consoleDo(t, http.MethodPost, "/api/studio/bigquery?call=savedqueries&project="+url.QueryEscape(project), "{}")
		if code != http.StatusOK {
			return
		}
		for _, part := range strings.Split(body, `"id":"`)[1:] {
			id := part[:strings.Index(part, `"`)]
			if strings.Contains(part, `"name":"`+saveName) {
				consoleDo(t, http.MethodPost, "/api/studio/bigquery?call=deletequery&project="+url.QueryEscape(project), `{"id":"`+id+`"}`)
			}
		}
	})

	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected") !== null`)
	var home struct {
		Tab      string
		Sections []string
	}
	p.eval(`({ tab: document.querySelector(".bq-tabs .tab.is-selected").textContent,
		sections: [...document.querySelectorAll(".bq-home-card h3")].map((h) => h.textContent) })`, &home)
	if home.Tab != "Home" || strings.Join(home.Sections, "|") != "Check out what's new in Studio|Create new|Recent|Try with templates|Add your own data" {
		t.Errorf("Studio opens on %q with sections %q", home.Tab, home.Sections)
	}

	// Visualization of a result set.
	p.clickText(".bq-home-action", "SQL query")
	p.setField(".bq-editor", "SELECT 'north' AS region, 3 AS orders UNION ALL SELECT 'south', 5 UNION ALL SELECT 'west', 2")
	p.clickText(".bq-toolbar button", "Run")
	p.waitFor(`[...document.querySelectorAll(".bq-results-table td")].some((c) => c.textContent === "south")`)
	p.clickText(".bq-results-tabs .tab", "Visualization")
	p.waitFor(`document.querySelector("svg.bq-chart") !== null`)
	var viz struct {
		Type, Dimension string
		Bars            int
		Measures        []string
	}
	p.eval(`({ type: document.querySelector("#bq-viz-type").value, dimension: document.querySelector("#bq-viz-dimension").value,
		bars: document.querySelectorAll("svg.bq-chart rect").length - 1,
		measures: [...document.querySelectorAll(".bq-viz-measures input:checked")].map((b) => b.value) })`, &viz)
	if viz.Type != "Bar" || viz.Dimension != "region" || strings.Join(viz.Measures, ",") != "orders" || viz.Bars != 3 {
		t.Errorf("the Visualization is %+v, want a Bar chart of orders by region with 3 bars", viz)
	}
	p.setField("#bq-viz-type", "Line")
	p.waitFor(`document.querySelector("svg.bq-chart polyline") !== null`)

	// Save > Save query, then Save query as.
	p.clickExpr(`document.querySelector(".bq-toolbar .bq-menu-button[aria-label=Save]")`)
	p.clickText(".bq-menu-item", "Save query")
	p.waitFor(`document.querySelector(".modal.is-open #bq-save-title") !== null`)
	p.setField(".modal.is-open #bq-save-name", saveName)
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === ` + fmt.Sprintf("%q", saveName))
	queries := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+".queries")
	p.waitFor(queries + ` !== null`)
	p.clickExpr(queries + `.querySelector(".bq-twisty")`)
	p.waitFor(`[...` + queries + `.querySelectorAll(".bq-node-name")].some((n) => n.textContent === ` + fmt.Sprintf("%q", saveName) + `)`)
	p.clickExpr(`document.querySelector(".bq-toolbar .bq-menu-button[aria-label=Save]")`)
	p.clickText(".bq-menu-item", "Save query as")
	p.waitFor(`document.querySelector(".modal.is-open #bq-save-title") !== null`)
	p.setField(".modal.is-open #bq-save-name", saveName+" b")
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`[...` + queries + `.querySelectorAll(".bq-node-name")].filter((n) => n.textContent.startsWith(` + fmt.Sprintf("%q", saveName) + `)).length === 2`)
	if hasService(t, "dataform") {
		// Kept in Dataform as code assets (#2094).
		_, name, _ := studioAsset(t, project, saveName)
		body := studioContent(t, project, name, "UNION ALL SELECT 'south'")
		if !strings.Contains(body, "UNION ALL SELECT 'south'") {
			t.Errorf("the saved query reads back as %s", body)
		}
		t.Cleanup(func() {
			for _, e := range studioAssets(t, project) {
				if strings.HasPrefix(e.DisplayName, saveName) {
					consoleDo(t, http.MethodPost, "/api/studio/dataform?call=delete&project="+url.QueryEscape(project), `{"name":"`+e.Name+`"}`)
				}
			}
		})
	} else {
		code, body := consoleDo(t, http.MethodPost, "/api/studio/bigquery?call=savedqueries&project="+url.QueryEscape(project), "{}")
		if code != http.StatusOK || !strings.Contains(body, saveName) || !strings.Contains(body, "UNION ALL SELECT 'south'") {
			t.Errorf("the saved queries read back as %d %s", code, body)
		}
	}

	// Edit > Query settings.
	p.clickExpr(`document.querySelector(".bq-toolbar .bq-menu-button[aria-label=Edit]")`)
	p.clickText(".bq-menu-item", "Query settings")
	p.waitFor(`document.querySelector(".modal.is-open #bq-settings-title") !== null`)
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// Open from the Explorer: the dataset as a tab, with its page's tabs.
	dsNode := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+"."+ds)
	p.waitFor(dsNode + ` !== null`)
	var tabsBefore int
	p.eval(`document.querySelectorAll(".bq-tabs .tab").length`, &tabsBefore)
	p.clickExpr(dsNode + `.querySelector(".bq-node-name")`)
	p.waitFor(`location.pathname === "/bigquery-studio" && document.querySelector(".bq-tabs .tab.is-selected").textContent === ` + fmt.Sprintf("%q", ds))
	p.waitFor(`document.querySelector(".bq-resource-tab .tab-strip .tab") !== null`)
	var first string
	p.eval(`document.querySelector(".bq-resource-tab .tab-strip .tab").textContent`, &first)
	if first != "Overview" {
		t.Errorf("the dataset's tab opens on %q, want Overview", first)
	}
	var italic bool
	p.eval(`document.querySelector(".bq-tabs .tab.is-selected").classList.contains("is-preview")`, &italic)
	if !italic {
		t.Error("a resource opened with a click is not in the preview tab")
	}

	// "+ Add data": Create dataset as a right-side panel, Google's regions.
	p.clickText(".bq-explorer-head button", "+ Add data")
	p.waitFor(`document.querySelector(".modal.is-side.is-open #f-datasetId") !== null`)
	var regions int
	p.eval(`document.querySelectorAll(".modal.is-open #f-region option").length`, &regions)
	if regions < 40 {
		t.Errorf("Create dataset's Region list has %d regions, want Google's", regions)
	}
	p.setField(".modal.is-open #f-datasetId", made)
	p.clickExpr(`[...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "Region")`)
	p.waitFor(`document.querySelector(".modal.is-open #f-region").getClientRects().length > 0`)
	p.setField(".modal.is-open #f-region", "asia-northeast1")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	madeNode := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+"."+made)
	p.waitFor(madeNode + ` !== null`)
	code, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {made}}.Encode(), "")
	if code != http.StatusOK || !strings.Contains(body, "asia-northeast1") {
		t.Errorf("the panel's dataset is not in asia-northeast1: %d %s", code, body)
	}

	// Create table from Upload, in the right-side panel.
	file := filepath.Join(t.TempDir(), "people.csv")
	if err := os.WriteFile(file, []byte("name,n\nann,1\nbob,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.clickExpr(madeNode + `.querySelector(".bq-node-menu")`)
	p.clickText(".bq-menu-item", "Create table")
	p.waitFor(`document.querySelector(".modal.is-side.is-open #f-tableId") !== null`)
	p.clickExpr(`[...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "Upload")`)
	p.waitFor(`document.querySelector(".modal.is-open #f-file").getClientRects().length > 0`)
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`.modal.is-open #f-file`), []string{file}))
	p.setField(".modal.is-open #f-tableId", "uploaded")
	p.clickExpr(`document.querySelector(".modal.is-open #f-autodetect")`)
	p.setField(".modal.is-open #f-skipLeadingRows", "1")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	deadline := time.Now().Add(30 * time.Second)
	for {
		d := readDataPage(t, "bigquery", project, made, "uploaded")
		if d.summary("Rows") == "2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Create table from Upload made %+v", d)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The table opened with a click replaces the preview tab.
	p.navigate("/bigquery-studio" + q)
	p.waitFor(madeNode + ` !== null`)
	p.clickExpr(madeNode + `.querySelector(".bq-twisty")`)
	tblNode := fmt.Sprintf(`document.querySelector('[data-node=%q]')`, project+"."+made+".uploaded")
	p.waitFor(tblNode + ` !== null`)
	p.clickExpr(madeNode + `.querySelector(".bq-node-name")`)
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === ` + fmt.Sprintf("%q", made))
	p.eval(`document.querySelectorAll(".bq-tabs .tab").length`, &tabsBefore)
	p.clickExpr(tblNode + `.querySelector(".bq-node-name")`)
	p.waitFor(`document.querySelector(".bq-tabs .tab.is-selected").textContent === "uploaded"`)
	var tabsAfter int
	p.eval(`document.querySelectorAll(".bq-tabs .tab").length`, &tabsAfter)
	if tabsAfter != tabsBefore {
		t.Errorf("a second click opened a tab beside the preview tab: %d tabs, then %d", tabsBefore, tabsAfter)
	}
	p.waitFor(`[...document.querySelectorAll(".bq-resource-tab .tab-strip .tab")].map((b) => b.textContent).slice(0, 3).join("|") === "Schema|Details|Preview"`)
}
