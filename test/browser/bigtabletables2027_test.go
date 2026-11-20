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

// TestBigtableTablesAndExplorerActions2027 (#2027): the Tables page lists
// a table's replications behind the arrow beside its ID ("Click the arrow
// next to the table ID to expand a list of replications of the table",
// https://docs.cloud.google.com/bigtable/docs/managing-tables), each
// cluster with its replication state; Storage utilization is the table's
// size; a table's ID opens it in Bigtable Studio, in a query tab of its
// own; the Explorer's View actions are Google's
// (https://docs.cloud.google.com/bigtable/docs/manage-data-using-console),
// with Prevent deletion read back by the admin client, View a sample query
// in an editor tab, View backups on the Backups page, and Monitor and
// Export to Cloud Storage disabled with the reason. Each screen and dialog
// passes the WCAG audit.
func TestBigtableTablesAndExplorerActions2027(t *testing.T) {
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
	id := fmt.Sprintf("br-rep-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstanceWithClusters(ctx, &bigtable.InstanceWithClustersConfig{InstanceID: id, DisplayName: "Replicated",
		Clusters: []bigtable.ClusterConfig{
			{ClusterID: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD},
			{ClusterID: id + "-c2", Zone: "us-east1-b", NumNodes: 1, StorageType: bigtable.SSD},
		}}); err != nil {
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
	t.Cleanup(func() {
		_ = admin.UpdateTableWithDeletionProtection(context.Background(), "orders", bigtable.Unprotected)
	})
	client, err := bigtable.NewClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	m := bigtable.NewMutation()
	m.Set("cf1", "v", bigtable.Now(), []byte(strings.Repeat("x", 4096)))
	if err := client.Open("orders").Apply(ctx, "a1", m); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	q := "?project=" + project
	instPage := "/bigtable-instances/" + id + q
	tablesPage := instPage + "&tab=tables"

	// The Tables page: the arrow lists each cluster with its state.
	p.navigate(tablesPage)
	expander := `#view button.row-expander[aria-label="Show replications of orders"]`
	p.waitFor(`document.querySelector(` + jsString(expander) + `) !== null`)
	var size string
	p.eval(`(() => { const heads = [...document.querySelectorAll("#view th")].map((h) => h.textContent.trim());
		const row = document.querySelector(`+jsString(expander)+`).closest("tr");
		return row.children[heads.indexOf("Storage utilization")].textContent.trim(); })()`, &size)
	if !strings.HasSuffix(size, "KiB") {
		t.Errorf("Storage utilization of a 4 KiB table = %q", size)
	}
	var n int
	p.eval(`document.querySelectorAll("#view tr.child-row").length`, &n)
	if n != 0 {
		t.Errorf("%d replication rows before the arrow is opened", n)
	}
	p.run(chromedp.Click(chromedp.CSS(expander)))
	p.waitFor(`document.querySelectorAll("#view tr.child-row").length === 2`)
	var children []string
	p.eval(`[...document.querySelectorAll("#view tr.child-row")].map((r) => r.textContent.replace(/\s+/g, " ").trim())`, &children)
	if len(children) != 2 || !strings.Contains(children[0], id+"-c1") || !strings.Contains(children[0], "Ready") ||
		!strings.Contains(children[1], id+"-c2") {
		t.Errorf("the replications of orders = %q", children)
	}
	var expanded string
	p.eval(`document.querySelector('#view button.row-expander').getAttribute("aria-expanded")`, &expanded)
	if expanded != "true" {
		t.Errorf("the open arrow's aria-expanded = %q", expanded)
	}

	// A table's ID opens it in Bigtable Studio, in a builder tab on it.
	p.eval(`[...document.querySelectorAll("#view td a")].find((a) => a.textContent === "orders").click()`, nil)
	p.waitFor(`location.search.includes("tab=studio") && [...document.querySelectorAll("#bigtable-query-tabs .tab")].some((b) => b.textContent === "orders" && b.getAttribute("aria-selected") === "true")`)
	p.waitFor(`document.querySelector("#bigtable-builder-table")?.value === "orders"`)

	// The Explorer's View actions for the table, in Google's order.
	studio := instPage + "&tab=studio"
	menu := `#bigtable-explorer .spanner-explorer-table .bq-node-menu`
	openMenu := func() {
		p.navigate(studio)
		p.waitFor(`document.querySelector(` + jsString(menu) + `) !== null`)
		p.run(chromedp.Click(chromedp.CSS(menu)))
		p.waitFor(`document.querySelector(".bq-menu:not([hidden])") !== null`)
	}
	openMenu()
	var labels []string
	p.eval(`[...document.querySelectorAll(".bq-menu:not([hidden]) button")].map((b) => b.textContent)`, &labels)
	want := "Query,Open in query editor,Create a new table,Create a backup,Prevent deletion,Edit,Delete,View a sample query,View backups,Monitor by viewing system insights,Export to Cloud Storage,Open table page"
	if got := strings.Join(labels, ","); got != want {
		t.Errorf("View actions = %s\nwant %s", got, want)
	}
	var disabled []string
	p.eval(`[...document.querySelectorAll(".bq-menu:not([hidden]) button[disabled]")].map((b) => b.textContent + ": " + b.title)`, &disabled)
	if len(disabled) != 2 || !strings.Contains(disabled[0], "Cloud Monitoring") || !strings.Contains(disabled[1], "Dataflow") {
		t.Errorf("the disabled actions = %q", disabled)
	}

	// Prevent deletion.
	p.clickText(".bq-menu:not([hidden]) button", "Prevent deletion")
	p.waitFor(`document.querySelector(".modal.is-open #f-protected") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-protected`)))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if info, err := admin.TableInfo(ctx, "orders"); err != nil || info.DeletionProtection != bigtable.Protected {
		t.Errorf("after Prevent deletion, TableInfo = %+v, %v", info, err)
	}

	// View a sample query.
	openMenu()
	p.clickText(".bq-menu:not([hidden]) button", "View a sample query")
	p.waitFor("document.querySelector(\"#bigtable-editor\")?.value === \"SELECT _key, `cf1` FROM `orders` LIMIT 10\"")
	p.run(chromedp.Click(chromedp.CSS(`#bigtable-editor-run`)))
	p.waitFor(`[...document.querySelectorAll("#bigtable-results-table td")].some((c) => c.textContent === "a1")`)

	// View backups.
	openMenu()
	p.clickText(".bq-menu:not([hidden]) button", "View backups")
	p.waitFor(`location.search.includes("tab=backups")`)

	pages := []a11yPage{
		{"Bigtable Tables with replications", tablesPage, settled + ` && document.querySelector(` + jsString(expander) + `) !== null`,
			`(() => { document.querySelector(` + jsString(expander) + `).click(); return true })()`},
		{"Bigtable Studio Explorer table actions", studio, settled + ` && document.querySelector(` + jsString(menu) + `) !== null`,
			`(() => { document.querySelector(` + jsString(menu) + `).click(); return true })()`},
		{"Bigtable Explorer Prevent deletion dialog", studio, settled + ` && document.querySelector(` + jsString(menu) + `) !== null`,
			`(() => { document.querySelector(` + jsString(menu) + `).click();
				[...document.querySelectorAll(".bq-menu:not([hidden]) button")].find((b) => b.textContent === "Prevent deletion").click(); return true })()`},
	}
	auditScreens(t, pages)
}
