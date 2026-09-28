//go:build browser

package browser

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/genproto/googleapis/type/latlng"
)

// envFirestore is the Firestore emulator's address, which the emulators
// shard's browser step exports (#895): the document is written and read
// back with the official client.
const envFirestore = "CLOUDBURROW_TEST_FIRESTORE"

// TestFirestoreEditAddRemoveAndBytesValuesThroughTheBrowser (#995). A
// document written with the official client holds a map field of a
// timestamp, a geopoint and an array of a string and a timestamp, which Edit
// field cannot hold as JSON, and a bytes field. The map field's page has an
// Elements tab; meta.when's page, opened from its row, offers Edit value
// prefilled with the timestamp, and saving another one writes it. Add value
// on the map field's page adds a key; Remove value on meta.items[0], from
// its row menu, confirmed by typing meta.items[0] back, removes it. The
// client reads each change as its type and the geopoint as it wrote it. The
// bytes field's page shows its base64, and Edit field, prefilled bytes
// AAEC/w==, saved as aGk= writes the bytes "hi".
func TestFirestoreEditAddRemoveAndBytesValuesThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the document is written and read back through the emulator itself", envFirestore)
	}
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	ctx := context.Background()

	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	client, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	doc := client.Collection("holders995").Doc("h")
	when := time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC)
	geo := &latlng.LatLng{Latitude: 51.5, Longitude: -0.12}
	if _, err := doc.Set(ctx, map[string]any{
		"meta": map[string]any{"when": when, "where": geo, "items": []any{"a", when}},
		"b":    []byte{0, 1, 2, 0xff},
	}); err != nil {
		t.Fatalf("Set %s with the official client: %v", doc.Path, err)
	}
	read := func() map[string]any {
		t.Helper()
		snap, err := doc.Get(ctx)
		if err != nil {
			t.Fatalf("Get %s with the official client: %v", doc.Path, err)
		}
		return snap.Data()
	}
	openField := func(name string) {
		p.navigate("/firestore/holders995/h" + q)
		p.waitFor(`document.querySelectorAll("#view tbody tr").length === 2`)
		p.clickText("#view tbody a", name)
		p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "` + name + `"`)
	}

	// Edit value on meta.when's own page.
	openField("meta")
	p.waitFor(`document.querySelector("#tab-elements") !== null`)
	p.run(chromedp.Click(`#tab-elements`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#view tbody tr").length === 5`)
	p.clickText("#view tbody a", "meta.when")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit value")`)
	p.clickText("#view .page-actions button", "Edit value")
	p.waitFor(`document.querySelector(".modal.is-open #f-value") !== null`)
	var form struct{ Type, Value string }
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		return { Type: q("#f-type").value, Value: q("#f-value").value }; })()`, &form)
	if form.Type != "timestamp" || form.Value != "2026-09-28T10:11:12.345678Z" {
		t.Errorf("meta.when's Edit value is prefilled %s %q", form.Type, form.Value)
	}
	later := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	p.setField(".modal.is-open #f-value", later.Format(time.RFC3339))
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if ts, ok := read()["meta"].(map[string]any)["when"].(time.Time); !ok || !ts.Equal(later) {
		t.Fatalf("after Edit value, the official client reads meta.when %#v, want %v", read()["meta"], later)
	}

	// Add value on the map field's page.
	openField("meta")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Add value")`)
	p.clickText("#view .page-actions button", "Add value")
	p.waitFor(`document.querySelector(".modal.is-open #f-field") !== null`)
	p.setField(".modal.is-open #f-field", "n")
	p.setField(".modal.is-open #f-type", "number")
	p.setField(".modal.is-open #f-value", "5")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := read()["meta"].(map[string]any)["n"]; n != int64(5) {
		t.Fatalf("after Add value, the official client reads meta.n %#v, want the integer 5", n)
	}

	// Remove value on meta.items[0], from its row menu, confirmed by name.
	openField("meta")
	p.waitFor(`document.querySelector("#tab-elements") !== null`)
	p.run(chromedp.Click(`#tab-elements`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#view tbody tr").length === 6`)
	p.run(chromedp.Click(`button[aria-label="Actions for meta.items[0]"]`, chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Remove value")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "meta.items[0]", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelectorAll("#view tbody tr").length === 5`)
	meta := read()["meta"].(map[string]any)
	items, _ := meta["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("after Remove value, the official client reads meta.items %#v, want the timestamp alone", meta["items"])
	}
	if ts, ok := items[0].(time.Time); !ok || !ts.Equal(when) {
		t.Errorf("the official client reads meta.items[0] %#v, want %v", items[0], when)
	}
	if g, ok := meta["where"].(*latlng.LatLng); !ok || g.GetLatitude() != 51.5 || g.GetLongitude() != -0.12 {
		t.Errorf("the official client reads meta.where %#v, want it unchanged", meta["where"])
	}

	// The bytes field, as base64.
	openField("b")
	p.waitFor(`document.querySelector("#view").textContent.includes("AAEC/w==")`)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit field")`)
	p.clickText("#view .page-actions button", "Edit field")
	p.waitFor(`document.querySelector(".modal.is-open #f-value") !== null`)
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		return { Type: q("#f-type").value, Value: q("#f-value").value }; })()`, &form)
	if form.Type != "bytes" || form.Value != "AAEC/w==" {
		t.Errorf("b's Edit field is prefilled %s %q, want bytes AAEC/w==", form.Type, form.Value)
	}
	p.setField(".modal.is-open #f-value", "aGk=")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if b, _ := read()["b"].([]byte); !bytes.Equal(b, []byte("hi")) {
		t.Errorf("after Edit field, the official client reads b %#v, want the bytes hi", read()["b"])
	}
}
