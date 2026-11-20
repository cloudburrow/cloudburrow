//go:build browser

package browser

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"github.com/chromedp/chromedp"
)

// TestDatastoreEntitiesPageAndQueryClausesThroughTheBrowser (#1846), in
// the emulators shard: the Datastore page as Google's Entities page, the
// entities of the kind its Kind picker chose; two checked rows deleted
// together with Delete; and a kind's Query by kind built with Add query
// clause (WHERE Done == boolean false) and Run.
func TestDatastoreEntitiesPageAndQueryClausesThroughTheBrowser(t *testing.T) {
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
	type task struct{ Done bool }
	keys := []*datastore.Key{datastore.NameKey("Alpha", "a1", nil),
		datastore.NameKey("Task", "t1", nil), datastore.NameKey("Task", "t2", nil), datastore.NameKey("Task", "t3", nil)}
	if _, err := dc.PutMulti(ctx, keys, []*task{{true}, {false}, {true}, {true}}); err != nil {
		t.Fatal(err)
	}
	q := "?project=" + project

	p := open(t)
	p.navigate("/datastore" + q)
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent.trim() === "name=a1")`)
	p.waitFor(`[...document.querySelectorAll("#scope-kind option")].map((o) => o.value).join(",") === "Alpha,Task"`)
	p.eval(`(() => { const s = document.querySelector("#scope-kind"); s.value = "Task";
		s.dispatchEvent(new Event("change")); return true; })()`, nil)
	p.waitFor(`new URLSearchParams(location.search).get("kind") === "Task" && ` +
		`[...document.querySelectorAll("#view tbody a")].map((a) => a.textContent.trim()).join(",") === "name=t1,name=t2,name=t3"`)

	// Check t2 and t3, and Delete them.
	for _, n := range []string{"name=t2", "name=t3"} {
		p.run(chromedp.Click(chromedp.CSS(`#view tbody input[type=checkbox][aria-label="Select ` + n + `"]`)))
	}
	// Confirmed as Google's entity delete is, nothing typed (#1931); the
	// dialog names both.
	p.clickText("#view button", "Delete")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm]") !== null`)
	p.confirmOnlyDialog()
	var detail string
	p.eval(`document.querySelector(".modal.is-open .confirm-detail").textContent`, &detail)
	if !strings.Contains(detail, "name=t2, name=t3") {
		t.Errorf("the delete dialog names %q; want name=t2 and name=t3", detail)
	}
	p.pressDelete()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var e task
		e2 := dc.Get(ctx, keys[2], &e)
		e3 := dc.Get(ctx, keys[3], &e)
		if errors.Is(e2, datastore.ErrNoSuchEntity) && errors.Is(e3, datastore.ErrNoSuchEntity) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Delete of t2 and t3: %v, %v", e2, e3)
		}
		time.Sleep(200 * time.Millisecond)
	}
	var e task
	if err := dc.Get(ctx, keys[1], &e); err != nil {
		t.Errorf("t1, not checked, after Delete: %v", err)
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].map((a) => a.textContent.trim()).join(",") === "name=t1"`)

	// Query by kind with a query clause: WHERE Done == false.
	if _, err := dc.Put(ctx, keys[2], &task{true}); err != nil {
		t.Fatal(err)
	}
	p.navigate("/datastore/Task" + q)
	p.waitFor(`document.querySelector("#tab-query") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-query`)))
	p.waitFor(`[...document.querySelectorAll(".query-form .form-tabs button")].map((b) => b.textContent).join(",") === "Query by kind,Query by GQL"`)
	p.clickText(".query-form button", "Add query clause")
	p.waitFor(`document.querySelectorAll(".query-form .group-row").length === 1`)
	p.eval(`(() => { const r = document.querySelector(".query-form .group-row");
		const set = (c, v) => { c.value = v; c.dispatchEvent(new Event("input", { bubbles: true })); c.dispatchEvent(new Event("change", { bubbles: true })); };
		const sel = r.querySelectorAll("select"), txt = r.querySelectorAll("input[type=text]");
		set(sel[0], "WHERE"); set(txt[0], "Done"); set(sel[1], "=="); set(sel[2], "boolean"); set(txt[1], "false");
		return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.query-form button[type="submit"]`)))
	p.waitFor(`document.querySelectorAll(".query-results tbody tr").length === 1`)
	var row string
	p.eval(`document.querySelector(".query-results tbody tr").textContent`, &row)
	if !strings.Contains(row, "name=t1") {
		t.Errorf("Query by kind WHERE Done == false lists %q, want name=t1", row)
	}
}
