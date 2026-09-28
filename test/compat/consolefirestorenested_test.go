//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"
)

// consoleFirestoreElements reads a Firestore field or element page's
// Elements tab through the console API: each row's name, where it opens,
// and its actions with their prefilled values, as the page draws them.
type consoleElementRow struct {
	Name    string
	Fields  map[string]string
	Opens   []string
	Actions []struct {
		ID     string
		Fields []struct{ Name, Default string }
	}
}

func consoleFirestoreElements(t *testing.T, addr, project string, path ...string) []consoleElementRow {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/firestore?"+q.Encode(), "")
	var page struct {
		Unavailable string
		Sections    []struct {
			ID      string
			Listing struct{ Items []consoleElementRow }
		}
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil || page.Unavailable != "" {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	for _, s := range page.Sections {
		if s.ID == "elements" {
			return s.Listing.Items
		}
	}
	t.Fatalf("the page %v has no Elements tab: %s", path, body)
	return nil
}

// action is the row's action id with its prefilled values, and false when
// the row does not offer it.
func (r consoleElementRow) action(id string) (map[string]string, bool) {
	for _, a := range r.Actions {
		if a.ID == id {
			values := map[string]string{}
			for _, f := range a.Fields {
				values[f.Name] = f.Default
			}
			return values, true
		}
	}
	return nil, false
}

func elementRow(t *testing.T, rows []consoleElementRow, name string) consoleElementRow {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("the Elements tab has no row %s", name)
	return consoleElementRow{}
}

// TestConsoleFirestoreEditValuesInsideMapsAndArrays.
//
// Firestore's Elements tab and per-value writes (#995), through the console
// API, read back with the official firestore client. A document written
// with the client holds a map field of an array (a string, a double, a map
// of a timestamp and a key that is not a plain name), bytes, a geopoint, a
// reference in this project and one in another, a boolean and a null, and
// fields beside it. The map field's page lists every value inside it, and
// Edit value saved as prefilled on every value it offers the form for
// leaves the document exactly as the client wrote it, reference in another
// project included, which is offered no edit. Then Edit value changes the
// timestamp inside the array's map, the bytes (as base64) and the geopoint;
// Add value inserts a reference into the array at index 1 and adds a key to
// the map; Remove value removes the array's first value: the client reads
// each new value as its type, and every other value and field as it wrote
// it. An action drawn before another writer changed the field is refused
// with the console's message, and the change is not made. A top-level bytes
// field is added with Add field as base64 and its Edit field is prefilled
// with it.
//
// covers: google.firestore.v1.Firestore/BatchGetDocuments, google.firestore.v1.Firestore/Commit
func TestConsoleFirestoreEditValuesInsideMapsAndArrays(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()

	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()
	const coll = "console-nested"
	doc := c.Collection(coll).Doc("n1")
	t.Cleanup(func() { _, _ = doc.Delete(ctx) })

	when := time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC)
	away := &firestore.DocumentRef{Path: "projects/another-project/databases/(default)/documents/users/bob", ID: "bob"}
	if _, err := doc.Set(ctx, map[string]any{
		"meta": map[string]any{
			"items": []any{"a", 2.0, map[string]any{"when": when, "odd name": int64(7)}},
			"blob":  []byte{0, 1, 2, 0xfe, 0xff},
			"where": &latlng.LatLng{Latitude: 51.5, Longitude: -0.12},
			"owner": c.Doc("users/alice"),
			"away":  away,
			"flag":  true,
			"none":  nil,
		},
		"title": "untouched",
		"count": int64(3),
	}); err != nil {
		t.Fatalf("Set with the official client: %v", err)
	}
	read := func() map[string]any {
		t.Helper()
		snap, err := doc.Get(ctx)
		if err != nil {
			t.Fatalf("Get with the official client: %v", err)
		}
		return snap.Data()
	}
	orig := read()
	if ref, _ := orig["meta"].(map[string]any)["away"].(*firestore.DocumentRef); ref == nil || ref.Path != away.Path {
		t.Fatalf("the emulator did not keep the other project's reference: %#v", orig["meta"])
	}
	fieldPath := []string{coll, "n1", "meta"}
	act := func(path []string, action string, values map[string]string) (int, string) {
		t.Helper()
		return consoleAct(t, addr, "firestore", project, path, action, values)
	}

	rows := consoleFirestoreElements(t, addr, project, fieldPath...)
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	want := "meta.away meta.blob meta.flag meta.items meta.items[0] meta.items[1] meta.items[2] " +
		`meta.items[2]["odd name"] meta.items[2].when meta.none meta.owner meta.where`
	if got := strings.Join(names, " "); got != want {
		t.Errorf("the Elements tab lists\n%s\nwant\n%s", got, want)
	}

	// Every Edit value, saved as prefilled, is a no-op.
	for _, r := range rows {
		values, ok := r.action("editvalue")
		if r.Name == "meta.away" || r.Name == "meta.items" || r.Name == "meta.items[2]" {
			if ok {
				t.Errorf("%s offers Edit value", r.Name)
			}
			continue
		}
		if !ok {
			t.Errorf("%s offers no Edit value", r.Name)
			continue
		}
		if code, out := act(r.Opens, "editvalue", values); code != http.StatusOK {
			t.Fatalf("Edit value on %s, as prefilled, = %d: %s", r.Name, code, out)
		}
		// Read again, so the rows carry the field as it now is.
		rows = consoleFirestoreElements(t, addr, project, fieldPath...)
	}
	if got := read(); !reflect.DeepEqual(got, orig) {
		t.Errorf("after every Edit value saved as prefilled the client reads\n%#v\nwant\n%#v", got, orig)
	}
	if blob := elementRow(t, rows, "meta.blob"); blob.Fields["Value"] != "AAEC/v8=" {
		t.Errorf("meta.blob is listed as %q, want its base64", blob.Fields["Value"])
	}

	// Changes, each to one value.
	edit := func(name, typ, value string) {
		t.Helper()
		r := elementRow(t, consoleFirestoreElements(t, addr, project, fieldPath...), name)
		values, _ := r.action("editvalue")
		values["type"], values["value"] = typ, value
		if code, out := act(r.Opens, "editvalue", values); code != http.StatusOK {
			t.Fatalf("Edit value on %s = %d: %s", name, code, out)
		}
	}
	later := time.Date(2027, 1, 2, 3, 4, 5, 6000, time.UTC)
	edit("meta.items[2].when", "timestamp", later.Format(time.RFC3339Nano))
	edit("meta.blob", "bytes", "aGk=")
	edit("meta.where", "geopoint", "-33.9, 18.4")

	fieldRows := consoleFirestoreElements(t, addr, project, fieldPath...)
	items, _ := elementRow(t, fieldRows, "meta.items").action("addvalue")
	items["index"], items["type"], items["value"] = "1", "reference", "users/carol"
	if code, out := act(elementRow(t, fieldRows, "meta.items").Opens, "addvalue", items); code != http.StatusOK {
		t.Fatalf("Add value into meta.items = %d: %s", code, out)
	}
	// Add value on the field's own page, a new key of the map.
	add := elementValues(t, addr, project, fieldPath)
	add["field"], add["type"], add["value"] = "added", "number", "5"
	if code, out := act(fieldPath, "addvalue", add); code != http.StatusOK {
		t.Fatalf("Add value on the field's page = %d: %s", code, out)
	}
	first := elementRow(t, consoleFirestoreElements(t, addr, project, fieldPath...), "meta.items[0]")
	rm, _ := first.action("removevalue")
	if code, out := act(first.Opens, "removevalue", rm); code != http.StatusOK {
		t.Fatalf("Remove value on meta.items[0] = %d: %s", code, out)
	}

	got := read()
	meta, _ := got["meta"].(map[string]any)
	gotItems, _ := meta["items"].([]any)
	if len(gotItems) != 3 {
		t.Fatalf("the client reads meta.items %#v, want three values", meta["items"])
	}
	if r, ok := gotItems[0].(*firestore.DocumentRef); !ok || r.Path != c.Doc("users/carol").Path {
		t.Errorf("meta.items[0] is %#v, want a reference to users/carol", gotItems[0])
	}
	if gotItems[1] != 2.0 {
		t.Errorf("meta.items[1] is %#v, want the double 2.0", gotItems[1])
	}
	inner, _ := gotItems[2].(map[string]any)
	if ts, ok := inner["when"].(time.Time); !ok || !ts.Equal(later) || inner["odd name"] != int64(7) {
		t.Errorf("meta.items[2] is %#v, want when %v and odd name 7", gotItems[2], later)
	}
	if b, _ := meta["blob"].([]byte); !bytes.Equal(b, []byte("hi")) {
		t.Errorf("meta.blob is %#v, want the bytes hi", meta["blob"])
	}
	if g, _ := meta["where"].(*latlng.LatLng); g.GetLatitude() != -33.9 || g.GetLongitude() != 18.4 {
		t.Errorf("meta.where is %#v, want -33.9, 18.4", meta["where"])
	}
	if meta["added"] != int64(5) {
		t.Errorf("meta.added is %#v, want the integer 5", meta["added"])
	}
	origMeta := orig["meta"].(map[string]any)
	for _, k := range []string{"owner", "away", "flag", "none"} {
		if !reflect.DeepEqual(meta[k], origMeta[k]) {
			t.Errorf("meta.%s is %#v, want it unchanged, %#v", k, meta[k], origMeta[k])
		}
	}
	if got["title"] != "untouched" || got["count"] != int64(3) || len(got) != 3 {
		t.Errorf("the document's other fields are %#v", got)
	}

	// A row drawn before another writer changed the field is refused.
	stale := elementRow(t, consoleFirestoreElements(t, addr, project, fieldPath...), "meta.items[0]")
	staleValues, _ := stale.action("removevalue")
	if _, err := doc.Update(ctx, []firestore.Update{{FieldPath: []string{"meta", "flag"}, Value: false}}); err != nil {
		t.Fatal(err)
	}
	if code, out := act(stale.Opens, "removevalue", staleValues); code != http.StatusBadRequest ||
		!strings.Contains(consoleError(t, out), "changed since the page was loaded") {
		t.Errorf("a stale Remove value = %d: %s, want refused as changed since the page was loaded", code, out)
	}
	if n := len(read()["meta"].(map[string]any)["items"].([]any)); n != 3 {
		t.Errorf("the refused Remove value left meta.items with %d values, want 3", n)
	}

	// Bytes at the top level, as base64.
	if code, out := act([]string{coll, "n1"}, "addfield", map[string]string{"field": "raw", "type": "bytes", "value": "AQID"}); code != http.StatusOK {
		t.Fatalf("Add field raw as bytes = %d: %s", code, out)
	}
	if b, _ := read()["raw"].([]byte); !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Errorf("raw is %#v, want the bytes 1, 2, 3", read()["raw"])
	}
	values := consoleDocDetail(t, addr, "firestore", project, coll, "n1", "raw").editValues(t)
	if values["type"] != "bytes" || values["value"] != "AQID" {
		t.Errorf("raw's Edit field is prefilled %v, want bytes AQID", values)
	}
}

// elementValues are the Add value inputs of a field page's own action, with
// its digest, as the dialog would submit them.
func elementValues(t *testing.T, addr, project string, path []string) map[string]string {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/firestore?"+q.Encode(), "")
	var page struct {
		Actions []struct {
			ID     string
			Fields []struct{ Name, Default string }
		}
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	for _, a := range page.Actions {
		if a.ID == "addvalue" {
			out := map[string]string{}
			for _, f := range a.Fields {
				out[f.Name] = f.Default
			}
			return out
		}
	}
	t.Fatalf("the page %v offers no Add value", path)
	return nil
}
