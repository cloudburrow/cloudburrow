//go:build browser

package browser

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDatastoreAddRemoveAndBlobValuesThroughTheBrowser (#911, #912). An
// entity written with the official client holds an array of a key to Order
// named "id=7", a timestamp and a geopoint, which Edit property cannot hold
// (#894), and a blob. On the array's page, Add value inserts a key at index
// 1; on its Elements tab, Remove value on items[0], from its row menu,
// confirmed by typing items[0] back, removes it. The official client reads
// the array as Order 9, the timestamp and the geopoint. The blob's page
// shows its base64, and Edit property, prefilled blob AAEC/w==, saved as
// aGk= writes the bytes "hi". It needs CLOUDBURROW_TEST_DATASTORE, which
// the emulators shard exports (#895).
func TestDatastoreAddRemoveAndBlobValuesThroughTheBrowser(t *testing.T) {
	needService(t, "datastore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envDatastore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the entity is written and read back through the emulator itself", envDatastore)
	}
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	ctx := context.Background()

	t.Setenv("DATASTORE_EMULATOR_HOST", addr)
	client, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	holder := datastore.NameKey("Holder911", "h", nil)
	when := time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC)
	geo := datastore.GeoPoint{Lat: 51.5, Lng: -0.12}
	props := datastore.PropertyList{
		{Name: "items", Value: []any{datastore.NameKey("Order", "id=7", nil), when, geo}},
		{Name: "b", Value: []byte{0, 1, 2, 0xff}},
	}
	if _, err := client.Put(ctx, holder, &props); err != nil {
		t.Fatalf("Put %v with the official client: %v", holder, err)
	}
	read := func() map[string]any {
		t.Helper()
		var back datastore.PropertyList
		if err := client.Get(ctx, holder, &back); err != nil {
			t.Fatalf("Get %v with the official client: %v", holder, err)
		}
		out := map[string]any{}
		for _, p := range back {
			out[p.Name] = p.Value
		}
		return out
	}
	openProperty := func(name string) {
		p.navigate("/datastore/Holder911" + q)
		p.clickText("#view tbody a", "name=h")
		p.waitFor(`document.querySelectorAll("#view tbody tr").length === 2`)
		p.clickText("#view tbody a", name)
		p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "` + name + `"`)
	}

	// Add value on the array's page, at index 1.
	openProperty("items")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Add value")`)
	p.clickText("#view .page-actions button", "Add value")
	p.waitFor(`document.querySelector(".modal.is-open #f-index") !== null`)
	p.setField(".modal.is-open #f-index", "1")
	p.setField(".modal.is-open #f-type", "key")
	p.setField(".modal.is-open #f-value", "Order/id=9")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if items, _ := read()["items"].([]any); len(items) != 4 || !keyIs(items[1], datastore.IDKey("Order", 9, nil)) {
		t.Fatalf("after Add value, the official client reads items %#v, want Order 9 at index 1", read()["items"])
	}

	// Remove value on items[0], from its row menu on the Elements tab,
	// confirmed by the row's name.
	openProperty("items")
	p.waitFor(`document.querySelector("#tab-elements") !== null`)
	p.run(chromedp.Click(`#tab-elements`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#view tbody tr").length === 4`)
	p.run(chromedp.Click(`button[aria-label="Actions for items[0]"]`, chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Remove value")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "items[0]", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelectorAll("#view tbody tr").length === 3`)
	items, _ := read()["items"].([]any)
	if len(items) != 3 || !keyIs(items[0], datastore.IDKey("Order", 9, nil)) {
		t.Fatalf("after Remove value, the official client reads items %#v, want Order 9 first", read()["items"])
	}
	if ts, ok := items[1].(time.Time); !ok || !ts.Equal(when) {
		t.Errorf("the official client reads items[1] %#v, want %v", items[1], when)
	}
	if g, ok := items[2].(datastore.GeoPoint); !ok || g != geo {
		t.Errorf("the official client reads items[2] %#v, want %v", items[2], geo)
	}

	// The blob, as base64.
	openProperty("b")
	p.waitFor(`document.querySelector("#view").textContent.includes("AAEC/w==") && document.querySelector("#view").textContent.includes("blob (4 bytes)")`)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit property")`)
	p.clickText("#view .page-actions button", "Edit property")
	p.waitFor(`document.querySelector(".modal.is-open #f-value") !== null`)
	var form struct{ Type, Value string }
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		return { Type: q("#f-type").value, Value: q("#f-value").value }; })()`, &form)
	if form.Type != "blob" || form.Value != "AAEC/w==" {
		t.Errorf("b's Edit property is prefilled %s %q, want blob AAEC/w==", form.Type, form.Value)
	}
	p.setField(".modal.is-open #f-value", "aGk=")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if b, _ := read()["b"].([]byte); !bytes.Equal(b, []byte("hi")) {
		t.Errorf("after Edit property, the official client reads b %#v, want the bytes hi", read()["b"])
	}
}
