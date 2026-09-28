//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleDocPage is the part of a console detail page these tests read: the
// actions it offers and its edit form's prefilled values.
type consoleDocPage struct {
	Unavailable string
	Actions     []struct {
		ID          string
		Destructive bool
		Leaves      bool
		Fields      []struct{ Name string }
	}
	Edit *struct {
		Label  string
		Fields []struct {
			Name, Default string
			Immutable     bool
		}
	}
}

func consoleDocDetail(t *testing.T, addr, service, project string, path ...string) consoleDocPage {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	var page consoleDocPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	return page
}

// editValues are an edit form's prefilled values, as the dialog submits them.
func (p consoleDocPage) editValues(t *testing.T) map[string]string {
	t.Helper()
	if p.Edit == nil {
		t.Fatalf("the page offers no edit form (unavailable: %q)", p.Unavailable)
	}
	out := map[string]string{}
	for _, f := range p.Edit.Fields {
		out[f.Name] = f.Default
	}
	return out
}

// deleteOffered reports whether the page offers a delete the client confirms
// by the name typed back: destructive, with no inputs, leaving the page.
func (p consoleDocPage) deleteOffered(id string) bool {
	for _, a := range p.Actions {
		if a.ID == id {
			return a.Destructive && a.Leaves && len(a.Fields) == 0
		}
	}
	return false
}

func consoleAct(t *testing.T, addr, service, project string, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	return consoleDo(t, addr, http.MethodPost, "/api/actions/"+service+"?project="+url.QueryEscape(project), string(body))
}

func consoleEdit(t *testing.T, addr, service, project string, path []string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Values": values})
	return consoleDo(t, addr, http.MethodPatch, "/api/resources/"+service+"?project="+url.QueryEscape(project), string(body))
}

// TestConsoleFirestoreDocumentCreateEditDelete.
//
// The Firestore screen's writes (#796), each read back through the official
// firestore client. Start collection makes a collection's first document with
// a typed field; Add field on its page adds one of every type Google's
// console offers, and the client reads each back as that type — an integer
// as int64, 2.0 as a double, "42" as a string, a timestamp, a geopoint, a
// reference, a map and an array with their own nested types, a boolean and a
// null. A field's Edit field form saved unchanged leaves the double a double;
// changing a number to a string stores a string. Add document with no ID
// makes a second, auto-ID document, and the collection's query returns both.
// A taken ID and a geopoint out of range are refused with the emulator's
// message, and a field that exists by the console, and none changes
// anything. Delete field and Delete document, offered with no inputs so the
// page asks for the name back, remove them: the client no longer sees the
// field, and Get is NOT_FOUND.
//
// covers: google.firestore.v1.Firestore/BatchGetDocuments, google.firestore.v1.Firestore/RunQuery
func TestConsoleFirestoreDocumentCreateEditDelete(t *testing.T) {
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

	const coll = "console-widgets"
	col := c.Collection(coll)
	t.Cleanup(func() {
		docs, _ := col.Documents(ctx).GetAll()
		for _, d := range docs {
			_, _ = d.Ref.Delete(ctx)
		}
	})
	doc := col.Doc("alpha")
	read := func() map[string]any {
		t.Helper()
		snap, err := doc.Get(ctx)
		if err != nil {
			t.Fatalf("Get %s: %v", doc.Path, err)
		}
		return snap.Data()
	}

	body, _ := json.Marshal(map[string]string{
		"collection": coll, "documentId": "alpha", "field": "count", "type": "number", "value": "42"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/firestore?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console Start collection = %d: %s", code, out)
	}
	if got := read()["count"]; got != int64(42) {
		t.Fatalf("count = %#v, want the integer 42", got)
	}

	for _, f := range [][3]string{
		{"price", "number", "2.0"},
		{"label", "string", "42"},
		{"when", "timestamp", "2026-09-27T15:04:05.123456Z"},
		{"where", "geopoint", "51.5, -0.12"},
		{"owner", "reference", "users/alice"},
		{"meta", "map", `{"n": 1, "d": 2.5, "tags": ["a", true]}`},
		{"tags", "array", `["x", 3]`},
		{"flag", "boolean", "true"},
		{"gone", "null", ""},
	} {
		if code, out := consoleAct(t, addr, "firestore", project, []string{coll, "alpha"}, "addfield",
			map[string]string{"field": f[0], "type": f[1], "value": f[2]}); code != http.StatusOK {
			t.Fatalf("console Add field %s = %d: %s", f[0], code, out)
		}
	}
	data := read()
	when := time.Date(2026, 9, 27, 15, 4, 5, 123456000, time.UTC)
	for name, ok := range map[string]bool{
		"price": data["price"] == 2.0,
		"label": data["label"] == "42",
		"when":  func() bool { ts, is := data["when"].(time.Time); return is && ts.Equal(when) }(),
		"where": func() bool {
			g, is := data["where"].(*latlng.LatLng)
			return is && g.GetLatitude() == 51.5 && g.GetLongitude() == -0.12
		}(),
		"owner": func() bool {
			r, is := data["owner"].(*firestore.DocumentRef)
			return is && r.Path == c.Doc("users/alice").Path
		}(),
		"meta": func() bool {
			m, is := data["meta"].(map[string]any)
			tags, _ := m["tags"].([]any)
			return is && m["n"] == int64(1) && m["d"] == 2.5 && len(tags) == 2 && tags[0] == "a" && tags[1] == true
		}(),
		"tags": func() bool {
			a, is := data["tags"].([]any)
			return is && len(a) == 2 && a[0] == "x" && a[1] == int64(3)
		}(),
		"flag": data["flag"] == true,
		"gone": func() bool { v, has := data["gone"]; return has && v == nil }(),
	} {
		if !ok {
			t.Errorf("field %s read back through the client as %#v", name, data[name])
		}
	}

	// Edit field, saved as prefilled, keeps the double a double.
	page := consoleDocDetail(t, addr, "firestore", project, coll, "alpha", "price")
	values := page.editValues(t)
	if values["type"] != "number" || values["value"] != "2.0" {
		t.Errorf("Edit field for price is prefilled %v, want number 2.0", values)
	}
	if code, out := consoleEdit(t, addr, "firestore", project, []string{coll, "alpha", "price"}, values); code != http.StatusOK {
		t.Fatalf("console Edit field price = %d: %s", code, out)
	}
	if got := read()["price"]; got != 2.0 {
		t.Errorf("price after an unchanged edit = %#v, want the double 2.0", got)
	}
	values = consoleDocDetail(t, addr, "firestore", project, coll, "alpha", "count").editValues(t)
	values["type"], values["value"] = "string", "forty-two"
	if code, out := consoleEdit(t, addr, "firestore", project, []string{coll, "alpha", "count"}, values); code != http.StatusOK {
		t.Fatalf("console Edit field count = %d: %s", code, out)
	}
	if got := read()["count"]; got != "forty-two" {
		t.Errorf("count after an edit to a string = %#v", got)
	}

	// Refusals carry the emulator's message and change nothing.
	before := len(read())
	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/firestore?project="+project,
		`{"collection":"`+coll+`","documentId":"alpha","type":"string"}`)
	if code != http.StatusBadRequest || !strings.HasPrefix(consoleError(t, out), "AlreadyExists: ") {
		t.Errorf("a second document alpha = %d %s, want the emulator's ALREADY_EXISTS", code, out)
	}
	code, out = consoleAct(t, addr, "firestore", project, []string{coll, "alpha"}, "addfield",
		map[string]string{"field": "far", "type": "geopoint", "value": "200, 0"})
	if code != http.StatusBadRequest || !strings.HasPrefix(consoleError(t, out), "InvalidArgument: ") {
		t.Errorf("a geopoint at latitude 200 = %d %s, want the emulator's INVALID_ARGUMENT", code, out)
	}
	code, _ = consoleAct(t, addr, "firestore", project, []string{coll, "alpha"}, "addfield",
		map[string]string{"field": "flag", "type": "boolean", "value": "false"})
	if code != http.StatusBadRequest || read()["flag"] != true {
		t.Errorf("Add field over an existing field = %d, flag now %#v", code, read()["flag"])
	}
	if after := len(read()); after != before {
		t.Errorf("the refused writes changed the document: %d fields, then %d", before, after)
	}

	// Add document with no ID: Firestore's auto ID.
	if code, out := consoleAct(t, addr, "firestore", project, []string{coll}, "adddocument",
		map[string]string{"field": "n", "type": "number", "value": "1"}); code != http.StatusOK {
		t.Fatalf("console Add document = %d: %s", code, out)
	}
	docs, err := col.Documents(ctx).GetAll()
	if err != nil || len(docs) != 2 {
		t.Fatalf("the collection holds %d documents after Add document, %v; want 2", len(docs), err)
	}

	// Deletes.
	if !consoleDocDetail(t, addr, "firestore", project, coll, "alpha", "flag").deleteOffered("deletefield") {
		t.Error("a field's page does not offer Delete field confirmed by name")
	}
	if code, out := consoleAct(t, addr, "firestore", project, []string{coll, "alpha", "flag"}, "deletefield", nil); code != http.StatusOK {
		t.Fatalf("console Delete field = %d: %s", code, out)
	}
	if _, has := read()["flag"]; has {
		t.Error("flag is still there after Delete field")
	}
	if !consoleDocDetail(t, addr, "firestore", project, coll, "alpha").deleteOffered("deletedocument") {
		t.Error("a document's page does not offer Delete document confirmed by name")
	}
	if code, out := consoleAct(t, addr, "firestore", project, []string{coll, "alpha"}, "deletedocument", nil); code != http.StatusOK {
		t.Fatalf("console Delete document = %d: %s", code, out)
	}
	if _, err := doc.Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("Get after the console's Delete document = %v, want NOT_FOUND", err)
	}
	code, out = consoleAct(t, addr, "firestore", project, []string{coll, "alpha"}, "deletedocument", nil)
	if code != http.StatusBadRequest || !strings.HasPrefix(consoleError(t, out), "NotFound: ") {
		t.Errorf("deleting the deleted document = %d %s, want the emulator's NOT_FOUND", code, out)
	}
}

// TestConsoleDatastoreEntityCreateEditDelete.
//
// The Datastore screen's writes (#796), each read back through the official
// datastore client. Create entity makes a kind's first entity, keyed by name,
// with a typed property; Add property on its page adds one of every type
// Google's console offers, one excluded from indexes, and the client reads
// each back as that type with that flag — an integer, a float 2.0, a string
// "42", a timestamp, a geopoint, a key with an ancestor, an array and an
// embedded entity with their own nested types, a boolean and a null. An
// Edit property form saved unchanged keeps the float a float and the
// excluded property excluded. Create entity with no key allocates a numeric
// ID. A key already taken and a string too long to index are refused, with
// the emulator's and the client's messages. Delete property and Delete
// entity, offered with no inputs so the page asks for the name back, remove
// them: Get no longer returns the property, then answers no such entity.
//
// covers: google.datastore.v1.Datastore/Lookup, google.datastore.v1.Datastore/RunQuery
func TestConsoleDatastoreEntityCreateEditDelete(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	const kind = "ConsoleWidget"
	key := datastore.NameKey(kind, "alpha", nil)
	t.Cleanup(func() {
		keys, _ := c.GetAll(ctx, datastore.NewQuery(kind).KeysOnly(), nil)
		_ = c.DeleteMulti(ctx, keys)
	})
	read := func() map[string]datastore.Property {
		t.Helper()
		var props datastore.PropertyList
		if err := c.Get(ctx, key, &props); err != nil {
			t.Fatalf("Get %v: %v", key, err)
		}
		out := map[string]datastore.Property{}
		for _, p := range props {
			out[p.Name] = p
		}
		return out
	}

	body, _ := json.Marshal(map[string]string{
		"kind": kind, "key": "alpha", "field": "count", "type": "integer", "value": "42"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console Create entity = %d: %s", code, out)
	}
	if got := read()["count"].Value; got != int64(42) {
		t.Fatalf("count = %#v, want the integer 42", got)
	}

	for _, p := range [][4]string{
		{"ratio", "float", "2.0", ""},
		{"label", "string", "42", "true"},
		{"when", "timestamp", "2026-09-27T15:04:05.123456Z", ""},
		{"where", "geopoint", "51.5, -0.12", ""},
		{"owner", "key", "Customer/alice/Order/id=7", ""},
		{"tags", "array", `["x", 3, 1.5]`, ""},
		{"meta", "entity", `{"n": 1, "name": "m"}`, ""},
		{"flag", "boolean", "true", ""},
		{"gone", "null", "", ""},
	} {
		if code, out := consoleAct(t, addr, "datastore", project, []string{kind, "alpha"}, "addproperty",
			map[string]string{"field": p[0], "type": p[1], "value": p[2], "excluded": p[3]}); code != http.StatusOK {
			t.Fatalf("console Add property %s = %d: %s", p[0], code, out)
		}
	}
	props := read()
	when := time.Date(2026, 9, 27, 15, 4, 5, 123456000, time.UTC)
	owner := datastore.IDKey("Order", 7, datastore.NameKey("Customer", "alice", nil))
	for name, ok := range map[string]bool{
		"ratio": props["ratio"].Value == 2.0,
		"label": props["label"].Value == "42" && props["label"].NoIndex,
		"when":  func() bool { ts, is := props["when"].Value.(time.Time); return is && ts.Equal(when) }(),
		"where": props["where"].Value == datastore.GeoPoint{Lat: 51.5, Lng: -0.12},
		"owner": func() bool { k, is := props["owner"].Value.(*datastore.Key); return is && k.Equal(owner) }(),
		"tags": func() bool {
			a, is := props["tags"].Value.([]any)
			return is && len(a) == 3 && a[0] == "x" && a[1] == int64(3) && a[2] == 1.5
		}(),
		"meta": func() bool {
			e, is := props["meta"].Value.(*datastore.Entity)
			if !is {
				return false
			}
			got := map[string]any{}
			for _, p := range e.Properties {
				got[p.Name] = p.Value
			}
			return len(got) == 2 && got["n"] == int64(1) && got["name"] == "m"
		}(),
		"flag": props["flag"].Value == true,
		"gone": func() bool { p, has := props["gone"]; return has && p.Value == nil }(),
	} {
		if !ok {
			t.Errorf("property %s read back through the client as %#v", name, props[name])
		}
	}
	if props["ratio"].NoIndex {
		t.Error("ratio is excluded from indexes, although the form did not ask for it")
	}

	// Edit property, saved as prefilled, keeps the float a float and the
	// excluded property excluded.
	for _, name := range []string{"ratio", "label"} {
		values := consoleDocDetail(t, addr, "datastore", project, kind, "alpha", name).editValues(t)
		if code, out := consoleEdit(t, addr, "datastore", project, []string{kind, "alpha", name}, values); code != http.StatusOK {
			t.Fatalf("console Edit property %s = %d: %s", name, code, out)
		}
	}
	props = read()
	if props["ratio"].Value != 2.0 || !props["label"].NoIndex || props["label"].Value != "42" {
		t.Errorf("after unchanged edits: ratio %#v, label %#v", props["ratio"], props["label"])
	}

	// Refusals.
	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/datastore?project="+project,
		`{"kind":"`+kind+`","key":"alpha"}`)
	if code != http.StatusBadRequest || !strings.HasPrefix(consoleError(t, out), "AlreadyExists: ") {
		t.Errorf("a second entity alpha = %d %s, want the emulator's ALREADY_EXISTS", code, out)
	}
	code, out = consoleAct(t, addr, "datastore", project, []string{kind, "alpha"}, "addproperty",
		map[string]string{"field": "essay", "type": "string", "value": strings.Repeat("a", 1600)})
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "too long to index") {
		t.Errorf("an indexed 1,600-byte string = %d %s, want the client's refusal", code, out)
	}
	if _, has := read()["essay"]; has {
		t.Error("the refused property was written")
	}

	// Create entity with no key: a numeric ID.
	if code, out := consoleAct(t, addr, "datastore", project, []string{kind}, "createentity",
		map[string]string{"field": "n", "type": "integer", "value": "1"}); code != http.StatusOK {
		t.Fatalf("console Create entity with no key = %d: %s", code, out)
	}
	keys, err := c.GetAll(ctx, datastore.NewQuery(kind).KeysOnly(), nil)
	numeric := 0
	for _, k := range keys {
		if k.Name == "" && k.ID != 0 {
			numeric++
		}
	}
	if err != nil || len(keys) != 2 || numeric != 1 {
		t.Fatalf("the kind holds %v after Create entity with no key (%v); want alpha and one numeric ID", keys, err)
	}

	// Deletes.
	if !consoleDocDetail(t, addr, "datastore", project, kind, "alpha", "flag").deleteOffered("deleteproperty") {
		t.Error("a property's page does not offer Delete property confirmed by name")
	}
	if code, out := consoleAct(t, addr, "datastore", project, []string{kind, "alpha", "flag"}, "deleteproperty", nil); code != http.StatusOK {
		t.Fatalf("console Delete property = %d: %s", code, out)
	}
	if _, has := read()["flag"]; has {
		t.Error("flag is still there after Delete property")
	}
	if !consoleDocDetail(t, addr, "datastore", project, kind, "alpha").deleteOffered("deleteentity") {
		t.Error("an entity's page does not offer Delete entity confirmed by name")
	}
	if code, out := consoleAct(t, addr, "datastore", project, []string{kind, "alpha"}, "deleteentity", nil); code != http.StatusOK {
		t.Fatalf("console Delete entity = %d: %s", code, out)
	}
	var gone datastore.PropertyList
	if err := c.Get(ctx, key, &gone); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("Get after the console's Delete entity = %v, want no such entity", err)
	}
	code, out = consoleAct(t, addr, "datastore", project, []string{kind, "alpha"}, "deleteentity", nil)
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "no such entity") {
		t.Errorf("deleting the deleted entity = %d %s, want the client's no such entity", code, out)
	}
}
