//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDatastoreQueryOperatorsAndGQLCompletionThroughTheBrowser (#1888,
// #1846), in the emulators shard: Query by kind's Add query clause with
// Google's !=, IN and NOT_IN, which the Datastore front serves; and Query
// by GQL's Ctrl+Space, which offers the namespace's kinds at the cursor,
// Enter putting the chosen one in the query; Google's Run and Clear on
// Query by kind and Run query on Query by GQL.
func TestDatastoreQueryOperatorsAndGQLCompletionThroughTheBrowser(t *testing.T) {
	needService(t, "datastore")
	dsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envDatastore)), "http://")
	if dsAddr == "" {
		t.Skipf("%s is needed: the entities are written through Datastore", envDatastore)
	}
	ctx := context.Background()
	project := uniqueProject(t)
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	type row struct{ N int64 }
	var keys []*datastore.Key
	var rows []*row
	for i := int64(1); i <= 4; i++ {
		keys = append(keys, datastore.NameKey("Opsk", fmt.Sprintf("o%d", i), nil))
		rows = append(rows, &row{i})
	}
	keys = append(keys, datastore.NameKey("Zeta", "z1", nil))
	rows = append(rows, &row{9})
	if _, err := dc.PutMulti(ctx, keys, rows); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/datastore/Opsk?project=" + project)
	p.waitFor(`document.querySelector("#tab-query") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-query`)))
	p.waitFor(`[...document.querySelectorAll(".query-form .form-tabs button")].map((b) => b.textContent).join(",") === "Query by kind,Query by GQL"`)
	// Google's buttons: Run and Clear on Query by kind.
	p.waitFor(`document.querySelector(".query-form button[type=submit]").textContent === "Run" && ` +
		`[...document.querySelectorAll(".query-form .card-actions button")].some((b) => b.textContent === "Clear")`)
	p.clickText(".query-form button", "Add query clause")
	p.waitFor(`document.querySelectorAll(".query-form .group-row").length === 1`)
	var ops string
	p.eval(`[...document.querySelector(".query-form .group-row").querySelectorAll("select")[1].options].map((o) => o.value).join(",")`, &ops)
	if !strings.Contains(ops, "==,!=,<,<=,>,>=,IN,NOT_IN") {
		t.Errorf("the clause's operators are %s, want Google's ==, !=, <, <=, >, >=, IN and NOT_IN", ops)
	}
	for _, c := range []struct{ op, value, want string }{
		{"IN", "1, 3", "name=o1,name=o3"},
		{"!=", "2", "name=o1,name=o3,name=o4"},
		{"NOT_IN", "1,4", "name=o2,name=o3"},
	} {
		p.eval(fmt.Sprintf(`(() => { const r = document.querySelector(".query-form .group-row");
			const set = (c, v) => { c.value = v; c.dispatchEvent(new Event("input", { bubbles: true })); c.dispatchEvent(new Event("change", { bubbles: true })); };
			const sel = r.querySelectorAll("select"), txt = r.querySelectorAll("input[type=text]");
			set(sel[0], "WHERE"); set(txt[0], "N"); set(sel[1], %q); set(sel[2], "integer"); set(txt[1], %q);
			document.querySelector(".query-results").replaceChildren();
			return true; })()`, c.op, c.value), nil)
		p.run(chromedp.Click(chromedp.CSS(`.query-form button[type="submit"]`)))
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".query-results tbody tr")].map((r) => r.querySelector("a") ? r.querySelector("a").textContent.trim() : "").join(",") === %q`, c.want))
	}

	// Query by GQL: Ctrl+Space after FROM offers the kinds; Down and
	// Enter choose Zeta.
	p.clickText(".query-form .form-tabs button", "Query by GQL")
	p.waitFor(`document.querySelector(".query-form button[type=submit]").textContent === "Run query"`)
	p.waitFor(`document.querySelector(".query-form textarea[name=gql]") !== null && document.querySelector(".query-form textarea[name=gql]").offsetParent !== null`)
	p.eval(`(() => { const a = document.querySelector(".query-form textarea[name=gql]"); a.value = "SELECT * FROM ";
		a.focus(); a.setSelectionRange(a.value.length, a.value.length); return true; })()`, nil)
	p.run(chromedp.KeyEvent(" ", chromedp.KeyModifiers(chromedp.ModifierCtrl)))
	p.waitFor(`[...document.querySelectorAll(".query-form .completions li")].map((l) => l.textContent).join(",") === "Opsk,Zeta"`)
	p.run(chromedp.KeyEvent(kb.ArrowDown))
	p.waitFor(`document.querySelector(".query-form .completions li.active").textContent === "Zeta"`)
	// Enter as a keydown alone: chromedp's key event also types the
	// carriage return as a separate char event, which a keydown's
	// preventDefault does not reach.
	p.eval(`document.querySelector(".query-form textarea[name=gql]").dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }))`, nil)
	p.waitFor(`document.querySelector(".query-form textarea[name=gql]").value === "SELECT * FROM Zeta" && document.querySelector(".query-form .completions").hidden`)
	// A prefix narrows them: "Op" offers Opsk alone.
	p.eval(`(() => { const a = document.querySelector(".query-form textarea[name=gql]"); a.value = "SELECT * FROM Op";
		a.focus(); a.setSelectionRange(a.value.length, a.value.length); return true; })()`, nil)
	p.run(chromedp.KeyEvent(" ", chromedp.KeyModifiers(chromedp.ModifierCtrl)))
	p.waitFor(`[...document.querySelectorAll(".query-form .completions li")].map((l) => l.textContent).join(",") === "Opsk"`)
	p.eval(`document.querySelector(".query-form textarea[name=gql]").dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }))`, nil)
	p.waitFor(`document.querySelector(".query-form textarea[name=gql]").value === "SELECT * FROM Opsk"`)
	p.eval(`(() => { const a = document.querySelector(".query-form textarea[name=gql]"); a.value += " WHERE N IN ARRAY(2, 4)";
		a.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector(".query-results").replaceChildren(); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.query-form button[type="submit"]`)))
	p.waitFor(`[...document.querySelectorAll(".query-results tbody tr")].map((r) => r.querySelector("a") ? r.querySelector("a").textContent.trim() : "").join(",") === "name=o2,name=o4"`)
}
