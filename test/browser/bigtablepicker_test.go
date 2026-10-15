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

// TestBigtableInstancePickerThroughTheBrowser (#1360). An instance is made
// with the official InstanceAdminClient in a new project. On the Bigtable
// screen its picker offers it beside the default instance; choosing it puts
// ?instance= in the address and shows its empty table list, and Create table
// there makes a table the admin client reads in that instance and not in the
// default one. A row written with the data client on that instance shows on
// the table's page, reached from the list with the instance still in the
// address, and Delete all rows, confirmed by the table's name, leaves the
// data client reading none. The instance's page on the instances screen has
// a Tables tab whose row opens the table on the Bigtable screen with the
// instance chosen. Back on the list, choosing the default instance takes
// ?instance= out of the address.
func TestBigtableInstancePickerThroughTheBrowser(t *testing.T) {
	needService(t, "bigtable")
	needService(t, "bigtable-instances")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envBigtable)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the Bigtable front itself", envBigtable)
	}
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := context.Background()
	project := uniqueProject(t)
	iac, err := bigtable.NewInstanceAdminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	id := fmt.Sprintf("br-pick-%d", time.Now().UnixNano()%1e8)
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "Picked",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatalf("CreateInstance with the official client: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), id) })
	admin, err := bigtable.NewAdminClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	defAdmin, err := bigtable.NewAdminClient(ctx, project, bigtableInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = defAdmin.Close() })
	table := fmt.Sprintf("picked-%d", time.Now().UnixNano()%1e8)

	p := open(t)
	q := "?project=" + project
	p.navigate("/bigtable" + q)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#scope-instance option")].map((o) => o.value).join(",") === %q`,
		bigtableInstanceID+","+id))
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#scope-instance"); s.value = %q;
		s.dispatchEvent(new Event("change")); return true; })()`, id), nil)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("instance") === %q`, id))
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create table")`)
	var selected string
	p.eval(`document.querySelector("#scope-instance").value`, &selected)
	if selected != id {
		t.Errorf("after choosing %s the picker shows %q", id, selected)
	}

	p.clickText(`#view .state button.primary`, "Create table")
	p.waitFor(`document.querySelector(".modal #f-table") !== null`)
	p.setField(".modal #f-table", table)
	p.setField(".modal #f-families", "cf")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if tables, err := admin.Tables(ctx); err != nil || strings.Join(tables, ",") != table {
		t.Fatalf("after Create table in %s, its tables are %v, %v", id, tables, err)
	}
	if tables, err := defAdmin.Tables(ctx); err != nil || len(tables) != 0 {
		t.Errorf("after Create table in %s, the default instance's tables are %v, %v, want none", id, tables, err)
	}

	bigtableWrite(t, client, table, "r1", map[string][]byte{"cf:v": []byte("hello")})
	p.navigate("/bigtable" + q + "&instance=" + id)
	p.clickText("#view td a", table)
	p.waitFor(fmt.Sprintf(`location.pathname === "/bigtable/%s" && new URLSearchParams(location.search).get("instance") === %q`, table, id))
	p.waitFor(`[...document.querySelectorAll("#view td a")].some((a) => a.textContent === "r1")`)
	p.clickText("#view .page-actions button", "Delete all rows")
	p.confirmByName(table)
	p.waitFor(`document.querySelector(".modal") === null`)
	if keys := bigtableRowKeys(t, client, table); len(keys) != 0 {
		t.Errorf("after Delete all rows in %s, ReadRows reads %v, want none", id, keys)
	}

	p.navigate("/bigtable-instances/" + id + q)
	p.waitFor(`document.querySelector("#tab-tables") !== null`)
	p.run(chromedp.Click(`#tab-tables`, chromedp.ByQuery))
	p.clickText("#view td a", table)
	p.waitFor(fmt.Sprintf(`location.pathname === "/bigtable/%s" && new URLSearchParams(location.search).get("instance") === %q`, table, id))
	p.waitFor(`document.querySelector("#tab-families") !== null`)

	p.navigate("/bigtable" + q + "&instance=" + id)
	p.waitFor(`document.querySelector("#scope-instance") !== null`)
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#scope-instance"); s.value = %q;
		s.dispatchEvent(new Event("change")); return true; })()`, bigtableInstanceID), nil)
	p.waitFor(`!new URLSearchParams(location.search).has("instance")`)
}
