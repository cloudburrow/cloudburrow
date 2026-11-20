//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"github.com/chromedp/chromedp"
)

// TestBigtableStudioSavedQueries1858 (#1858), from
// https://docs.cloud.google.com/bigtable/docs/create-manage-saved-queries:
// the Query editor's Save opens Save query (a name, and a Region under
// Show advanced options); the query is listed under the Explorer's
// Queries, opens in a tab of its own, saves a new version, and is deleted
// with View actions > Delete query. Save as > Save as logical view makes
// the view the instance admin client reads. The Tables page has Status
// (Ready, each cluster's replication state) and Storage utilization. Each
// screen and dialog passes the WCAG audit.
func TestBigtableStudioSavedQueries1858(t *testing.T) {
	needService(t, "bigtable-instances")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the admin API itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	project := uniqueProject(t)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-sq-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Saved",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })
	admin, err := bigtable.NewAdminClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.CreateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "orders", "cf1"); err != nil {
		t.Fatal(err)
	}
	client, err := bigtable.NewClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for _, k := range []string{"a1", "b1"} {
		m := bigtable.NewMutation()
		m.Set("cf1", "v", bigtable.Now(), []byte("val-"+k))
		if err := client.Open("orders").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}

	p := open(t)
	q := "?project=" + project
	instPage := "/bigtable-instances/" + id + q

	// The Tables page: Status and Storage utilization.
	p.navigate(instPage + "&tab=tables")
	p.waitFor(`[...document.querySelectorAll("#view th")].some((h) => h.textContent.trim() === "Status")`)
	var heads []string
	p.eval(`[...document.querySelectorAll("#view th")].map((h) => h.textContent.trim())`, &heads)
	if !strings.Contains(strings.Join(heads, ","), "Storage utilization") {
		t.Errorf("the Tables page's columns %v have no Storage utilization", heads)
	}
	p.waitFor(`[...document.querySelectorAll("#view td")].some((c) => c.textContent.trim() === "Ready")`)

	// Save query.
	studio := instPage + "&tab=studio"
	p.navigate(studio)
	p.waitFor(`document.querySelector("#bigtable-explorer-queries") !== null && document.querySelector("#bigtable-mode-editor") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-mode-editor`)))
	p.waitFor(`document.querySelector("#bigtable-editor") !== null`)
	sql := "SELECT _key FROM orders WHERE _key = 'a1'"
	setField(p, "#view", "bigtable-editor", sql)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-editor-save`)))
	p.waitFor(`document.querySelector(".modal.is-open #bigtable-save-name") !== null`)
	var title string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if title != "Save query" {
		t.Errorf("Save opens %q, want Save query", title)
	}
	setField(p, ".modal.is-open", "bigtable-save-name", "Orders a1")
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-save-show-advanced`)))
	p.waitFor(`document.querySelector("#bigtable-save-advanced").hidden === false`)
	p.eval(`(() => { const s = document.querySelector("#bigtable-save-region"); s.value = "us-east1";
		s.dispatchEvent(new Event("change")); return s.value; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-save-submit`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`[...document.querySelectorAll("#bigtable-explorer-queries button")].some((b) => b.textContent === "Orders a1")`)
	p.waitFor(`[...document.querySelectorAll("#bigtable-query-tabs .tab")].some((b) => b.textContent === "Orders a1")`)

	// It outlives the page: opened from Queries after a reload, it runs.
	p.eval(`(() => { sessionStorage.clear(); return true })()`, nil)
	p.navigate(studio)
	p.waitFor(`[...document.querySelectorAll("#bigtable-explorer-queries button")].some((b) => b.textContent === "Orders a1")`)
	p.eval(`[...document.querySelectorAll("#bigtable-explorer-queries button")].find((b) => b.textContent === "Orders a1").click()`, nil)
	p.waitFor(`document.querySelector("#bigtable-editor")?.value === ` + jsString(sql) + ` && !document.querySelector("#bigtable-saved-note").hidden`)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-editor-run`)))
	p.waitFor(`[...document.querySelectorAll("#bigtable-results-table td")].some((c) => c.textContent === "a1")`)

	// Save as > Save as logical view.
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-editor-save-as`)))
	p.clickText(".bq-menu:not([hidden]) button", "Save as logical view")
	p.waitFor(`document.querySelector(".modal.is-open #bigtable-save-name") !== null`)
	setField(p, ".modal.is-open", "bigtable-save-name", "orders_a1")
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-save-submit`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if lv, err := iac.LogicalViewInfo(ctx, id, "orders_a1"); err != nil || lv.Query != sql {
		t.Errorf("after Save as logical view, LogicalViewInfo = %+v, %v", lv, err)
	}

	pages := []a11yPage{
		{"Bigtable Tables with Status", instPage + "&tab=tables", settled + ` && [...document.querySelectorAll("#view td")].some((c) => c.textContent.trim() === "Ready")`, ""},
		{"Bigtable Studio with saved queries", studio, settled + ` && [...document.querySelectorAll("#bigtable-explorer-queries button")].some((b) => b.textContent === "Orders a1")`, ""},
		{"Bigtable Save query dialog", studio, settled + ` && document.querySelector("#bigtable-mode-editor") !== null`,
			`(() => { document.querySelector("#bigtable-mode-editor").click();
				document.querySelector("#bigtable-editor").value = "SELECT 1"; document.querySelector("#bigtable-editor-save").click();
				return true })()`},
		{"Bigtable Delete query dialog", studio, settled + ` && [...document.querySelectorAll("#bigtable-explorer-queries button")].some((b) => b.textContent === "Orders a1")`,
			`(() => { document.querySelector("#bigtable-explorer-queries .bq-node-menu").click();
				[...document.querySelectorAll(".bq-menu:not([hidden]) button")].find((b) => b.textContent === "Delete query").click(); return true })()`},
	}
	auditScreens(t, pages)

	// Delete query, in a tab of its own: the audit took the first's time.
	p = open(t)
	p.navigate(studio)
	p.waitFor(`[...document.querySelectorAll("#bigtable-explorer-queries button")].some((b) => b.textContent === "Orders a1")`)
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-explorer-queries .bq-node-menu`)))
	p.clickText(".bq-menu:not([hidden]) button", "Delete query")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-title")?.textContent === "Delete query"`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && !document.querySelector("#bigtable-explorer-queries").textContent.includes("Orders a1")`)
}
