//go:build compat

package compat

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// datastoreValuePage is a Datastore page with its actions and edit form.
type datastoreValuePage struct {
	Unavailable string
	Summary     []struct{ Label, Value string }
	Sections    []struct {
		ID, Text, Note string
		Listing        datastoreRows
	}
	Actions []struct {
		ID                  string
		Destructive, Leaves bool
		Fields              []struct{ Name, Default string }
	}
	Edit *struct {
		Fields []struct{ Name, Default string }
	}
}

func consoleDatastoreValuePage(t *testing.T, addr, project string, path ...string) datastoreValuePage {
	t.Helper()
	var page datastoreValuePage
	q := url.Values{"project": {project}, "name": path}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "", &page)
	if page.Unavailable != "" {
		t.Fatalf("%v is unavailable: %s", path, page.Unavailable)
	}
	return page
}

// action is the page's action id's prefilled values, and whether it is
// offered.
func (p datastoreValuePage) action(id string) (map[string]string, bool) {
	for _, a := range p.Actions {
		if a.ID == id {
			out := map[string]string{}
			for _, f := range a.Fields {
				out[f.Name] = f.Default
			}
			return out, true
		}
	}
	return nil, false
}

func (p datastoreValuePage) editValues() map[string]string {
	if p.Edit == nil {
		return nil
	}
	out := map[string]string{}
	for _, f := range p.Edit.Fields {
		out[f.Name] = f.Default
	}
	return out
}

func (p datastoreValuePage) summary(label string) string {
	for _, s := range p.Summary {
		if s.Label == label {
			return s.Value
		}
	}
	return ""
}

// drawnFrom is the digest of the property the value actions on the page at
// path carry (#923), or "" when the page offers none.
func drawnFrom(t *testing.T, addr, project string, path ...string) string {
	t.Helper()
	var page datastoreValuePage
	q := url.Values{"project": {project}, "name": path}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "", &page)
	for _, a := range page.Actions {
		for _, f := range a.Fields {
			if f.Name == "expected" {
				return f.Default
			}
		}
	}
	return ""
}

// with is values with changes applied.
func with(values map[string]string, changes ...string) map[string]string {
	out := map[string]string{}
	for k, v := range values {
		out[k] = v
	}
	for i := 0; i+1 < len(changes); i += 2 {
		out[changes[i]] = changes[i+1]
	}
	return out
}

// TestConsoleDatastoreAddAndRemoveValues (#911). An entity written with the
// official Go client holds an array of a key, a timestamp, a geopoint and a
// string, beside a key to another project added through the v1 API; an
// embedded entity excluded from indexes, with a key of its own and an array
// inside; and an embedded entity with no key. An array holding a key cannot
// be rewritten with Edit property (#894), so its length could not change
// from the console. Add value inserts into the array at an index and
// appends to it, adds a property to the embedded entity, prefilled excluded
// as it is, a value to the array inside it, and a key to the entity with
// none; Remove value, a destructive confirm with no inputs, removes an
// array's value, the other project's key, an embedded entity's property, a
// value of the array inside it and its key. Every value not added or
// removed reads back proto-equal through the v1 API, and the official
// client reads the new ones as their types. What Datastore or the form
// refuses — an index past the end, an array inside an array, a reserved
// property name, a property the entity has, Add value on a value that is
// not an array or entity, and a value that is not there — is refused, the
// emulator's own message for the reserved name.
func TestConsoleDatastoreAddAndRemoveValues(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	holder := datastore.NameKey("Holder911", "h", nil)
	when := time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC)
	props := datastore.PropertyList{
		{Name: "items", Value: []any{datastore.NameKey("Order", "id=7", nil), when, datastore.GeoPoint{Lat: 51.5, Lng: -0.12}, "s"}},
		{Name: "line", Value: &datastore.Entity{Key: datastore.IDKey("Line", 1, nil), Properties: []datastore.Property{
			{Name: "when", Value: when},
			{Name: "tags", Value: []any{datastore.IDKey("Order", 3, nil), "t"}},
		}}, NoIndex: true},
		{Name: "bare", Value: &datastore.Entity{Properties: []datastore.Property{{Name: "n", Value: int64(1)}}}},
		{Name: "note", Value: "x"},
	}
	if _, err := c.Put(ctx, holder, &props); err != nil {
		t.Fatalf("Put with the official client: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, holder) })
	holderPB := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
		Path: []*datastorepb.Key_PathElement{{Kind: "Holder911", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}}
	lookup := func() map[string]*datastorepb.Value {
		t.Helper()
		r, err := raw.Lookup(ctx, &datastorepb.LookupRequest{ProjectId: project, Keys: []*datastorepb.Key{holderPB}})
		if err != nil || len(r.GetFound()) != 1 {
			t.Fatalf("Lookup through the v1 API = %v, %v", r, err)
		}
		return r.GetFound()[0].GetEntity().GetProperties()
	}
	stored := lookup()
	foreign := &datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: &datastorepb.Key{
		PartitionId: &datastorepb.PartitionId{ProjectId: "another-project"},
		Path:        []*datastorepb.Key_PathElement{{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}}}}}
	stored["items"].GetArrayValue().Values = append(stored["items"].GetArrayValue().GetValues(), foreign)
	if _, err := raw.Commit(ctx, &datastorepb.CommitRequest{ProjectId: project, Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Update{
			Update: &datastorepb.Entity{Key: holderPB, Properties: stored}}}}}); err != nil {
		t.Fatalf("add the other project's key through the v1 API: %v", err)
	}
	stored = lookup()
	entity := []string{"Holder911", holder.Encode()}
	at := func(parts ...string) []string { return append(append([]string{}, entity...), parts...) }
	// Each action carries the property as its page reads it now (#923).
	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		values = with(values, "expected", drawnFrom(t, addr, project, path...))
		if code, out := consoleAct(t, addr, "datastore", project, path, action, values); code != http.StatusOK {
			t.Fatalf("%s on %v = %d: %s", action, path, code, out)
		}
	}

	// Offered where they apply.
	itemsPage := consoleDatastoreValuePage(t, addr, project, at("items")...)
	add, ok := itemsPage.action("addvalue")
	if _, hasIndex := add["index"]; !ok || !hasIndex || add["excluded"] != "false" {
		t.Fatalf("the items page offers Add value %v (%v), want an Index and indexed", add, ok)
	}
	linePage := consoleDatastoreValuePage(t, addr, project, at("line")...)
	lineAdd, ok := linePage.action("addvalue")
	if _, hasField := lineAdd["field"]; !ok || !hasField || lineAdd["excluded"] != "true" {
		t.Fatalf("the line page offers Add value %v (%v), want a Property and excluded, as line is", lineAdd, ok)
	}
	if _, ok := consoleDatastoreValuePage(t, addr, project, at("note")...).action("addvalue"); ok {
		t.Error("a string property is offered Add value")
	}
	rows := map[string][]string{}
	for _, prop := range []string{"items", "line"} {
		for _, it := range consoleDatastorePage(t, addr, project, at(prop)...).listing("elements").Items {
			for _, a := range it.Actions {
				rows[it.Name] = append(rows[it.Name], a.ID)
			}
		}
	}
	for name, want := range map[string]string{
		"items[0]": "editvalue,removevalue", "items[4]": "removevalue",
		"line.tags": "addvalue,excludevalues,removevalue", "line.tags[0]": "editvalue,removevalue", "line.__key__": "editvalue,removevalue",
	} {
		if got := strings.Join(rows[name], ","); got != want {
			t.Errorf("%s's row offers %s, want %s", name, got, want)
		}
	}
	tagsPage := consoleDatastoreValuePage(t, addr, project, at("line", `["tags"]`)...)
	if _, ok := tagsPage.action("addvalue"); !ok {
		t.Error("line.tags's page offers no Add value")
	}
	for _, a := range tagsPage.Actions {
		// Its one field is the hidden digest of the property (#923).
		if a.ID == "removevalue" && (!a.Destructive || !a.Leaves || len(a.Fields) != 1 || a.Fields[0].Name != "expected") {
			t.Errorf("Remove value is %+v, want a destructive confirm with no inputs that leaves the page", a)
		}
	}

	// Add.
	act(at("items"), "addvalue", with(add, "index", "1", "type", "key", "value", "Customer/name=bob/Order/id=9"))
	later := time.Date(2027, 1, 2, 3, 4, 5, 6000, time.UTC)
	act(at("items"), "addvalue", with(add, "type", "timestamp", "value", "2027-01-02T03:04:05.000006Z"))
	act(at("line"), "addvalue", with(lineAdd, "field", "qty", "type", "integer", "value", "3"))
	tagsAdd, _ := tagsPage.action("addvalue")
	if tagsAdd["excluded"] != "true" {
		t.Errorf("Add value on line.tags is prefilled %v, want excluded as its values are", tagsAdd)
	}
	act(at("line", `["tags"]`), "addvalue", with(tagsAdd, "type", "geopoint", "value", "3.5, -4.25"))
	bareAdd, _ := consoleDatastoreValuePage(t, addr, project, at("bare")...).action("addvalue")
	act(at("bare"), "addvalue", with(bareAdd, "field", "__key__", "type", "key", "value", "Bare/name=b"))

	// Remove: items is now [Order id=7, Customer bob/Order 9, when, geo, s,
	// the other project's key, later].
	act(at("items", "[0]"), "removevalue", nil)
	act(at("items", "[4]"), "removevalue", nil)
	act(at("line", `["__key__"]`), "removevalue", nil)
	act(at("line", `["when"]`), "removevalue", nil)
	act(at("line", `["tags",0]`), "removevalue", nil)

	// Refused.
	for _, tc := range []struct {
		path   []string
		action string
		values map[string]string
		want   string
	}{
		{at("items"), "addvalue", with(add, "index", "99", "value", "x"), "0 to 5"},
		{at("items"), "addvalue", with(add, "type", "array", "value", "[1]"), "cannot hold an array"},
		{at("line"), "addvalue", with(lineAdd, "field", "__x__", "value", "x"), "reserved"},
		{at("line"), "addvalue", with(lineAdd, "field", "tags", "value", "x"), "already has a property"},
		{at("items", "[1]"), "addvalue", with(add, "value", "x"), "not available"},
		{at("items", "[99]"), "removevalue", nil, "not available"},
	} {
		values := with(tc.values, "expected", drawnFrom(t, addr, project, tc.path...))
		code, out := consoleAct(t, addr, "datastore", project, tc.path, tc.action, values)
		if code != http.StatusBadRequest || !strings.Contains(out, tc.want) {
			t.Errorf("%s on %v = %d: %s; want it refused naming %q", tc.action, tc.path, code, out, tc.want)
		}
	}

	// Through the v1 API: what was not added or removed is as stored.
	after := lookup()
	if !proto.Equal(after["note"], stored["note"]) || !proto.Equal(after["bare"].GetEntityValue().GetProperties()["n"], stored["bare"].GetEntityValue().GetProperties()["n"]) {
		t.Errorf("note and bare.n read back %v, %v; want them as stored", after["note"], after["bare"])
	}
	was, is := stored["items"].GetArrayValue().GetValues(), after["items"].GetArrayValue().GetValues()
	if len(is) != 5 {
		t.Fatalf("items holds %d values, want 5: %v", len(is), is)
	}
	for i := 1; i <= 3; i++ {
		if !proto.Equal(is[i], was[i]) {
			t.Errorf("items[%d] reads back %v, want %v as stored", i, is[i], was[i])
		}
	}
	gotLine := after["line"].GetEntityValue()
	wasTags := stored["line"].GetEntityValue().GetProperties()["tags"].GetArrayValue().GetValues()
	tags := gotLine.GetProperties()["tags"].GetArrayValue().GetValues()
	if gotLine.GetKey() != nil || len(gotLine.GetProperties()) != 2 || len(tags) != 2 || !proto.Equal(tags[0], wasTags[1]) {
		t.Errorf("line reads back %v, want no key, and tags [t, a geopoint] and qty", gotLine)
	}
	if !gotLine.GetProperties()["qty"].GetExcludeFromIndexes() || !tags[1].GetExcludeFromIndexes() {
		t.Errorf("line's new values are not excluded from indexes as it is: %v", gotLine)
	}

	// The official client reads the new values as their types.
	var back datastore.PropertyList
	if err := c.Get(ctx, holder, &back); err != nil {
		t.Fatalf("Get with the official client: %v", err)
	}
	got := map[string]any{}
	for _, p := range back {
		got[p.Name] = p.Value
	}
	vs, _ := got["items"].([]any)
	if len(vs) != 5 || !keyEquals(vs[0], datastore.IDKey("Order", 9, datastore.NameKey("Customer", "bob", nil))) {
		t.Fatalf("the client reads items %#v, want Customer bob/Order 9 first", got["items"])
	}
	if ts, ok := vs[4].(time.Time); !ok || !ts.Equal(later) {
		t.Errorf("the client reads items[4] %#v, want %v", vs[4], later)
	}
	line, _ := got["line"].(*datastore.Entity)
	lp := map[string]any{}
	for _, p := range line.Properties {
		lp[p.Name] = p.Value
	}
	if line.Key != nil || lp["qty"] != int64(3) {
		t.Errorf("the client reads line %#v, want no key and qty 3", line)
	}
	if tv, _ := lp["tags"].([]any); len(tv) != 2 || tv[0] != "t" || tv[1] != (datastore.GeoPoint{Lat: 3.5, Lng: -4.25}) {
		t.Errorf("the client reads line.tags %#v, want [t 3.5,-4.25]", lp["tags"])
	}
	if bare, _ := got["bare"].(*datastore.Entity); bare == nil || !bare.Key.Equal(datastore.NameKey("Bare", "b", nil)) {
		t.Errorf("the client reads bare %#v, want its key Bare b", got["bare"])
	}
}

// TestConsoleDatastoreBlobValuesAsBase64 (#912). A blob had no edit form: a
// property was shown as N bytes and a value inside an array as blob(N
// bytes). An entity written with the official Go client holds an indexed
// blob, a 2,000-byte blob excluded from indexes, a blob in an array and one
// in an embedded entity. Each is shown and prefilled as base64 (standard
// encoding, as the v1 REST API's blobValue), and Edit property and Edit
// value saved unchanged write the same bytes back: the entity reads back
// proto-equal through the v1 API. A change is read back by the official
// client as those bytes; a blob over 1,500 bytes is refused indexed, with
// the client's "too long to index", and written once excluded, top-level
// and at the end of an array. The emulator stores an array's excluded values
// after its indexed ones, so a value excluded before an indexed one is
// refused rather than moved. Add property writes a blob.
func TestConsoleDatastoreBlobValuesAsBase64(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	holder := datastore.NameKey("Holder912", "h", nil)
	big := bytes.Repeat([]byte{0xab, 0x01}, 1000)
	props := datastore.PropertyList{
		{Name: "b", Value: []byte{0, 1, 2, 0xff}},
		{Name: "big", Value: big, NoIndex: true},
		{Name: "arr", Value: []any{"x", []byte{1, 2}}},
		{Name: "ent", Value: &datastore.Entity{Properties: []datastore.Property{{Name: "bin", Value: []byte{9}}}}},
	}
	if _, err := c.Put(ctx, holder, &props); err != nil {
		t.Fatalf("Put with the official client: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, holder) })
	holderPB := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
		Path: []*datastorepb.Key_PathElement{{Kind: "Holder912", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}}
	lookup := func() map[string]*datastorepb.Value {
		t.Helper()
		r, err := raw.Lookup(ctx, &datastorepb.LookupRequest{ProjectId: project, Keys: []*datastorepb.Key{holderPB}})
		if err != nil || len(r.GetFound()) != 1 {
			t.Fatalf("Lookup through the v1 API = %v, %v", r, err)
		}
		return r.GetFound()[0].GetEntity().GetProperties()
	}
	stored := lookup()
	entity := []string{"Holder912", holder.Encode()}
	at := func(parts ...string) []string { return append(append([]string{}, entity...), parts...) }
	get := func() map[string]any {
		t.Helper()
		var back datastore.PropertyList
		if err := c.Get(ctx, holder, &back); err != nil {
			t.Fatalf("Get with the official client: %v", err)
		}
		out := map[string]any{}
		for _, p := range back {
			out[p.Name] = p
		}
		return out
	}

	// Shown and prefilled as base64.
	bPage := consoleDatastoreValuePage(t, addr, project, at("b")...)
	if bPage.summary("Type") != "blob (4 bytes)" || bPage.Sections[0].Text != "AAEC/w==" {
		t.Errorf("b's page shows type %q, value %q; want blob (4 bytes), AAEC/w==", bPage.summary("Type"), bPage.Sections[0].Text)
	}
	bEdit := bPage.editValues()
	if bEdit["type"] != "blob" || bEdit["value"] != "AAEC/w==" || bEdit["excluded"] != "false" {
		t.Errorf("b's Edit property is prefilled %v, want blob AAEC/w==, indexed", bEdit)
	}
	bigEdit := consoleDatastoreValuePage(t, addr, project, at("big")...).editValues()
	if bigEdit["value"] != base64.StdEncoding.EncodeToString(big) || bigEdit["excluded"] != "true" {
		t.Errorf("big's Edit property is prefilled excluded %q with %d characters, want its base64, excluded", bigEdit["excluded"], len(bigEdit["value"]))
	}
	cells := map[string]string{}
	for _, it := range consoleDatastorePage(t, addr, project, entity...).listing("properties").Items {
		cells[it.Name] = it.Fields["Value"]
	}
	if cells["b"] != "AAEC/w==" || !strings.HasSuffix(cells["big"], "…") || len(cells["big"]) > 100 {
		t.Errorf("the entity's page shows b %q and big %q; want b's base64 and big's shortened", cells["b"], cells["big"])
	}
	elementEdit := func(prop, name string) map[string]string {
		t.Helper()
		for _, it := range consoleDatastorePage(t, addr, project, at(prop)...).listing("elements").Items {
			if it.Name != name {
				continue
			}
			for _, a := range it.Actions {
				if a.ID == "editvalue" {
					out := map[string]string{}
					for _, f := range a.Fields {
						out[f.Name] = f.Default
					}
					return out
				}
			}
		}
		t.Fatalf("%s offers no Edit value", name)
		return nil
	}
	arrEdit := elementEdit("arr", "arr[1]")
	if arrEdit["type"] != "blob" || arrEdit["value"] != "AQI=" || arrEdit["excluded"] != "false" {
		t.Errorf("arr[1]'s Edit value is prefilled %v, want blob AQI=, indexed", arrEdit)
	}
	entEdit := elementEdit("ent", "ent.bin")
	if entEdit["value"] != "CQ==" {
		t.Errorf("ent.bin's Edit value is prefilled %v, want CQ==", entEdit)
	}

	// Saved unchanged, the same bytes.
	for _, tc := range []struct {
		prop   string
		values map[string]string
	}{{"b", bEdit}, {"big", bigEdit}} {
		if code, out := consoleEdit(t, addr, "datastore", project, at(tc.prop), tc.values); code != http.StatusOK {
			t.Fatalf("Edit property on %s saved unchanged = %d: %s", tc.prop, code, out)
		}
	}
	for _, tc := range []struct {
		path   []string
		values map[string]string
	}{{at("arr", "[1]"), arrEdit}, {at("ent", `["bin"]`), entEdit}} {
		if code, out := consoleAct(t, addr, "datastore", project, tc.path, "editvalue", tc.values); code != http.StatusOK {
			t.Fatalf("Edit value on %v saved unchanged = %d: %s", tc.path, code, out)
		}
	}
	after := lookup()
	for name, want := range stored {
		if !proto.Equal(after[name], want) {
			t.Errorf("after the edits saved unchanged, %s reads back %v through the v1 API, want %v", name, after[name], want)
		}
	}

	// Changes, read back with the official client.
	long := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 1501))
	for _, tc := range []struct {
		values map[string]string
		want   string
	}{
		{with(bEdit, "value", long), "too long to index"},
		{with(bEdit, "value", "not base64!"), "base64"},
	} {
		if code, out := consoleEdit(t, addr, "datastore", project, at("b"), tc.values); code != http.StatusBadRequest || !strings.Contains(out, tc.want) {
			t.Errorf("Edit property b = %d: %s; want it refused naming %q", code, out, tc.want)
		}
	}
	if code, out := consoleAct(t, addr, "datastore", project, at("arr", "[1]"), "editvalue", with(arrEdit, "value", long)); code != http.StatusBadRequest || !strings.Contains(out, "too long to index") {
		t.Errorf("Edit value arr[1] with an indexed 1,501-byte blob = %d: %s; want it refused", code, out)
	}
	// The emulator stores an array's excluded values after its indexed ones,
	// so arr[0] excluded before the indexed arr[1] would be moved: refused.
	if code, out := consoleAct(t, addr, "datastore", project, at("arr", "[0]"), "editvalue",
		map[string]string{"type": "blob", "value": long, "excluded": "true", "expected": arrEdit["expected"]}); code != http.StatusBadRequest || !strings.Contains(out, "reorder") {
		t.Errorf("Edit value arr[0] excluded before an indexed value = %d: %s; want it refused", code, out)
	}
	if code, out := consoleEdit(t, addr, "datastore", project, at("b"), with(bEdit, "value", long, "excluded", "true")); code != http.StatusOK {
		t.Fatalf("Edit property b with an excluded 1,501-byte blob = %d: %s", code, out)
	}
	if code, out := consoleAct(t, addr, "datastore", project, at("arr", "[1]"), "editvalue", with(arrEdit, "value", long, "excluded", "true")); code != http.StatusOK {
		t.Fatalf("Edit value arr[1] with an excluded 1,501-byte blob = %d: %s", code, out)
	}
	if code, out := consoleAct(t, addr, "datastore", project, at("ent", `["bin"]`), "editvalue", with(entEdit, "value", "aGVsbG8=")); code != http.StatusOK {
		t.Fatalf("Edit value ent.bin = %d: %s", code, out)
	}
	if code, out := consoleAct(t, addr, "datastore", project, entity, "addproperty",
		map[string]string{"field": "nb", "type": "blob", "value": "AQID"}); code != http.StatusOK {
		t.Fatalf("Add property nb = %d: %s", code, out)
	}

	got := get()
	blobOf := func(v any) []byte { b, _ := v.([]byte); return b }
	if p := got["b"].(datastore.Property); !bytes.Equal(blobOf(p.Value), bytes.Repeat([]byte{7}, 1501)) || !p.NoIndex {
		t.Errorf("the client reads b as %T of %d bytes, excluded %v; want the 1,501 bytes, excluded", p.Value, len(blobOf(p.Value)), p.NoIndex)
	}
	if p := got["nb"].(datastore.Property); !bytes.Equal(blobOf(p.Value), []byte{1, 2, 3}) || p.NoIndex {
		t.Errorf("the client reads nb as %#v", p)
	}
	arr, _ := got["arr"].(datastore.Property).Value.([]any)
	if len(arr) != 2 || arr[0] != "x" || !bytes.Equal(blobOf(arr[1]), bytes.Repeat([]byte{7}, 1501)) {
		t.Errorf("the client reads arr as %d values, want x and the long blob; the v1 API reads %v", len(arr), lookup()["arr"])
	}
	isArr := lookup()["arr"].GetArrayValue().GetValues()
	if len(isArr) != 2 || !isArr[1].GetExcludeFromIndexes() || !proto.Equal(isArr[0], stored["arr"].GetArrayValue().GetValues()[0]) {
		t.Errorf("arr reads back %v through the v1 API, want arr[0] as stored and arr[1] excluded", isArr)
	}
	ent, _ := got["ent"].(datastore.Property).Value.(*datastore.Entity)
	if bin, _ := ent.Properties[0].Value.([]byte); len(ent.Properties) != 1 || !bytes.Equal(bin, []byte("hello")) {
		t.Errorf("the client reads ent as %#v, want bin hello; the v1 API reads %v", ent, lookup()["ent"])
	}
	if !proto.Equal(lookup()["big"], stored["big"]) {
		t.Error("big changed")
	}
}
