//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// datastoreRows is a console listing's rows with what they open and the
// actions they offer.
type datastoreRows struct {
	Items []struct {
		Name    string
		Fields  map[string]string
		Opens   []string
		ActsOn  []string
		Actions []struct {
			ID     string
			Fields []struct{ Name, Default string }
		}
	}
	More   bool
	Cursor string
	Note   string
}

// datastoreSections is a console page's tabs, by ID.
type datastoreSections struct {
	Sections []struct {
		ID, Text, Note string
		Listing        datastoreRows
	}
	Summary []struct{ Label, Value string }
	Title   string
}

func (p datastoreSections) listing(id string) datastoreRows {
	for _, s := range p.Sections {
		if s.ID == id {
			return s.Listing
		}
	}
	return datastoreRows{}
}

func consoleDatastorePage(t *testing.T, addr, project string, path ...string) datastoreSections {
	t.Helper()
	var page datastoreSections
	q := url.Values{"project": {project}, "name": path}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/datastore?"+q.Encode(), "", &page)
	return page
}

// TestConsoleDatastoreListingAndQueryShowWhereAKeyIs (#904). A kind's
// entity listing and the query builder's results read through the Go client,
// whose Key has no project or database, so a key to Order 7 in
// another-project was shown in the Properties cell as Order/id=7, the key to
// this project's Order 7, and an embedded entity's own key lost where it
// was. They now run RunQuery on the v1 API, as the entity's page reads it:
// the listing, the query builder and the entity's page show each such key
// with where it is, the same way. A kind of 205 entities is paged with the
// query cursor as before.
func TestConsoleDatastoreListingAndQueryShowWhereAKeyIs(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	order7 := func(p, db string) *datastorepb.Key {
		return &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: p, DatabaseId: db},
			Path: []*datastorepb.Key_PathElement{{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}}}
	}
	keyValue := func(k *datastorepb.Key) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: k}}
	}
	holder := datastore.NameKey("Holder904", "h", nil)
	entity := &datastorepb.Entity{
		Key: &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
			Path: []*datastorepb.Key_PathElement{{Kind: "Holder904", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}},
		Properties: map[string]*datastorepb.Value{
			"other": keyValue(order7("another-project", "")),
			"mine":  keyValue(order7(project, "")),
			"refs": {ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{Values: []*datastorepb.Value{
				keyValue(order7(project, "another-db")), keyValue(order7(project, ""))}}}},
			"line": {ValueType: &datastorepb.Value_EntityValue{EntityValue: &datastorepb.Entity{
				Key:        order7("another-project", ""),
				Properties: map[string]*datastorepb.Value{"n": {ValueType: &datastorepb.Value_IntegerValue{IntegerValue: 1}}}}}},
			"note": {ValueType: &datastorepb.Value_StringValue{StringValue: "x"}},
		},
	}
	if _, err := raw.Commit(ctx, &datastorepb.CommitRequest{ProjectId: project, Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Upsert{Upsert: entity}}}}); err != nil {
		t.Fatalf("Commit through the v1 API: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, holder) })

	want := map[string]string{
		"line":  `{"__key__": key(Order/id=7 (project another-project)), "n": 1}`,
		"mine":  "Order/id=7",
		"note":  "x",
		"other": "Order/id=7 (project another-project)",
		"refs":  "[key(Order/id=7 (database another-db)), key(Order/id=7)]",
	}
	var cell []string
	for _, name := range []string{"line", "mine", "note", "other", "refs"} {
		cell = append(cell, name+": "+want[name])
	}
	wantCell := strings.Join(cell, ", ")

	// The entity's page.
	page := consoleDatastorePage(t, addr, project, "Holder904", holder.Encode())
	for _, it := range page.listing("properties").Items {
		if it.Fields["Value"] != want[it.Name] {
			t.Errorf("the entity's page shows %s as %q, want %q", it.Name, it.Fields["Value"], want[it.Name])
		}
	}
	// The kind's listing.
	kind := consoleDatastorePage(t, addr, project, "Holder904")
	if rows := kind.listing("entities").Items; len(rows) != 1 || rows[0].Fields["Properties"] != wantCell {
		t.Errorf("the kind's listing is %+v, want one row with Properties %q", rows, wantCell)
	}
	// The query builder, filtered and unfiltered.
	for _, values := range []map[string]string{{}, {"property": "note", "op": "=", "value": "x"}} {
		body, _ := json.Marshal(map[string]any{"Path": []string{"Holder904"}, "Values": values})
		var query struct{ Listing datastoreRows }
		consoleJSON(t, addr, http.MethodPost, "/api/query/datastore?project="+url.QueryEscape(project), string(body), &query)
		if rows := query.Listing.Items; len(rows) != 1 || rows[0].Fields["Properties"] != wantCell ||
			len(rows[0].Opens) != 2 || rows[0].Opens[1] != holder.Encode() {
			t.Errorf("the query builder with %v lists %+v, want one row opening %s with Properties %q", values, rows, holder.Encode(), wantCell)
		}
	}
	// A filter that matches nothing.
	body, _ := json.Marshal(map[string]any{"Path": []string{"Holder904"}, "Values": map[string]string{"property": "note", "op": "=", "value": "y"}})
	var none struct{ Listing datastoreRows }
	consoleJSON(t, addr, http.MethodPost, "/api/query/datastore?project="+url.QueryEscape(project), string(body), &none)
	if len(none.Listing.Items) != 0 {
		t.Errorf("note = y lists %+v, want nothing", none.Listing.Items)
	}

	// Paging: 205 entities, 200 on the first page and 5 after the cursor,
	// each once.
	keys := make([]*datastore.Key, 205)
	vals := make([]datastore.PropertyList, 205)
	for i := range keys {
		keys[i] = datastore.NameKey("Page904", fmt.Sprintf("e%03d", i), nil)
		vals[i] = datastore.PropertyList{{Name: "i", Value: int64(i)}}
	}
	if _, err := c.PutMulti(ctx, keys, vals); err != nil {
		t.Fatalf("PutMulti: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteMulti(ctx, keys) })
	first := consoleDatastorePage(t, addr, project, "Page904").listing("entities")
	if len(first.Items) != 200 || !first.More || first.Cursor == "" {
		t.Fatalf("the first page has %d entities, more %v, cursor %q; want 200 and a cursor", len(first.Items), first.More, first.Cursor)
	}
	if _, err := datastore.DecodeCursor(first.Cursor); err != nil {
		t.Errorf("the cursor %q is not one the client reads: %v", first.Cursor, err)
	}
	var next datastoreRows
	q := url.Values{"project": {project}, "name": {"Page904"}, "cursor": {first.Cursor}}
	consoleJSON(t, addr, http.MethodGet, "/api/page/datastore?"+q.Encode(), "", &next)
	seen := map[string]bool{}
	for _, it := range append(first.Items, next.Items...) {
		seen[it.Name] = true
	}
	if len(next.Items) != 5 || len(seen) != 205 {
		t.Errorf("the second page has %d entities and %d are listed once in all, want 5 and 205", len(next.Items), len(seen))
	}
}

// TestConsoleDatastoreEditValueInsideArraysAndEntities (#905). A key,
// timestamp or geopoint inside an array or an embedded entity was shown by
// its type and offered no edit, because Edit property's form is JSON. An
// entity written with the official Go client holds them, in an array and in
// an embedded entity with a key of its own and an array inside, beside a
// blob, and a key to another project added through the v1 API. The
// property's Elements tab lists every value inside it; Edit value on each
// one the form can hold, saved as prefilled, writes back exactly what was
// stored (the entity reads back proto-equal through the v1 API). Changing a
// key, a timestamp, a geopoint, a key in a nested array and the embedded
// entity's own key writes those values only: every other value reads back
// through the v1 API as it was, and the official client reads the new
// values as their types. The other project's key is offered no Edit value,
// and the action route refuses one. Since #911 the embedded entity's array
// is listed too, offered Add value rather than Edit value, and since #912
// the blob is shown as base64 and edited as it.
func TestConsoleDatastoreEditValueInsideArraysAndEntities(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	holder := datastore.NameKey("Holder905", "h", nil)
	when := time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC)
	props := datastore.PropertyList{
		{Name: "items", Value: []any{datastore.NameKey("Order", "id=7", nil), when, datastore.GeoPoint{Lat: 51.5, Lng: -0.12},
			"s", int64(5), []byte{1, 2}}},
		{Name: "line", Value: &datastore.Entity{Key: datastore.IDKey("Line", 1, nil), Properties: []datastore.Property{
			{Name: "when", Value: when},
			{Name: "where", Value: datastore.GeoPoint{Lat: 1, Lng: 2}},
			{Name: "tags", Value: []any{datastore.IDKey("Order", 3, nil), "t"}},
		}}, NoIndex: true},
		{Name: "note", Value: "x"},
	}
	if _, err := c.Put(ctx, holder, &props); err != nil {
		t.Fatalf("Put with the official client: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, holder) })
	holderPB := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
		Path: []*datastorepb.Key_PathElement{{Kind: "Holder905", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}}
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
	items := stored["items"].GetArrayValue()
	items.Values = append(items.Values, foreign)
	if _, err := raw.Commit(ctx, &datastorepb.CommitRequest{ProjectId: project, Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Update{
			Update: &datastorepb.Entity{Key: holderPB, Properties: stored}}}}}); err != nil {
		t.Fatalf("add the other project's key through the v1 API: %v", err)
	}
	stored = lookup()
	entity := []string{"Holder905", holder.Encode()}
	at := func(prop string) []string { return append(append([]string{}, entity...), prop) }

	type row struct {
		shown    string
		editable bool
	}
	editIndex := func(actions []struct {
		ID     string
		Fields []struct{ Name, Default string }
	}) int {
		for i, a := range actions {
			if a.ID == "editvalue" {
				return i
			}
		}
		return -1
	}
	elements := func(prop string) map[string]row {
		t.Helper()
		out := map[string]row{}
		for _, it := range consoleDatastorePage(t, addr, project, at(prop)...).listing("elements").Items {
			out[it.Name] = row{it.Fields["Value"], editIndex(it.Actions) >= 0}
		}
		return out
	}
	wantItems := map[string]row{
		"items[0]": {"Order/name=id=7", true},
		"items[1]": {"2026-09-28T10:11:12.345678Z", true},
		"items[2]": {"51.5, -0.12", true},
		"items[3]": {"s", true},
		"items[4]": {"5", true},
		"items[5]": {"AQI=", true},
		"items[6]": {"Order/id=7 (project another-project)", false},
	}
	wantLine := map[string]row{
		"line.__key__": {"Line/id=1", true},
		"line.tags":    {`[key(Order/id=3), "t"]`, false},
		"line.tags[0]": {"Order/id=3", true},
		"line.tags[1]": {"t", true},
		"line.when":    {"2026-09-28T10:11:12.345678Z", true},
		"line.where":   {"1, 2", true},
	}
	for prop, want := range map[string]map[string]row{"items": wantItems, "line": wantLine} {
		got := elements(prop)
		for name, w := range want {
			if got[name] != w {
				t.Errorf("%s's Elements tab shows %s as %+v, want %+v (all %v)", prop, name, got[name], w, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s's Elements tab lists %d values, want %d: %v", prop, len(got), len(want), got)
		}
	}

	// Saved as prefilled, every Edit value writes back what was stored.
	for _, prop := range []string{"items", "line"} {
		for _, it := range consoleDatastorePage(t, addr, project, at(prop)...).listing("elements").Items {
			i := editIndex(it.Actions)
			if i < 0 {
				continue
			}
			values := map[string]string{}
			for _, f := range it.Actions[i].Fields {
				values[f.Name] = f.Default
			}
			if code, out := consoleAct(t, addr, "datastore", project, it.ActsOn, "editvalue", values); code != http.StatusOK {
				t.Errorf("Edit value on %s saved unchanged = %d: %s", it.Name, code, out)
			}
		}
	}
	after := lookup()
	for name, want := range stored {
		if !proto.Equal(after[name], want) {
			t.Errorf("after an unchanged Edit value on every value, %s reads back %v through the v1 API, want %v", name, after[name], want)
		}
	}

	// An element's own page.
	elPage := consoleDatastorePage(t, addr, project, append(at("line"), `["tags",0]`)...)
	if elPage.Title != "line.tags[0]" || len(elPage.Sections) == 0 || elPage.Sections[0].Text != "Order/id=3" {
		t.Errorf("line.tags[0]'s page is %+v, want it headed line.tags[0] showing Order/id=3", elPage)
	}

	// Changes.
	newWhen := time.Date(2027, 1, 2, 3, 4, 5, 6000, time.UTC)
	for _, tc := range []struct {
		prop, seg, typ, value string
	}{
		{"items", "[0]", "key", "Order/id=8"},
		{"items", "[1]", "timestamp", "2027-01-02T03:04:05.000006Z"},
		{"line", `["where"]`, "geopoint", "3.5, -4.25"},
		{"line", `["tags",0]`, "key", "Customer/name=bob/Order/id=3"},
		{"line", `["__key__"]`, "key", "Line/name=two"},
	} {
		if code, out := consoleAct(t, addr, "datastore", project, append(at(tc.prop), tc.seg), "editvalue",
			map[string]string{"type": tc.typ, "value": tc.value}); code != http.StatusOK {
			t.Fatalf("Edit value on %s %s = %d: %s", tc.prop, tc.seg, code, out)
		}
	}
	// Refused: the other project's key, a type Edit value does not write,
	// the embedded entity's array as a whole, and a non-key for an entity's
	// key.
	for _, tc := range []struct {
		prop, seg, typ, value string
	}{
		{"items", "[6]", "key", "Order/id=7"},
		{"items", "[3]", "array", "[1]"},
		{"line", `["tags"]`, "string", "x"},
		{"line", `["__key__"]`, "string", "x"},
	} {
		if code, out := consoleAct(t, addr, "datastore", project, append(at(tc.prop), tc.seg), "editvalue",
			map[string]string{"type": tc.typ, "value": tc.value}); code != http.StatusBadRequest {
			t.Errorf("Edit value on %s %s as %s = %d: %s, want it refused", tc.prop, tc.seg, tc.typ, code, out)
		}
	}

	// Through the v1 API: every value not edited is as stored.
	after = lookup()
	if !proto.Equal(after["note"], stored["note"]) {
		t.Errorf("note reads back %v, want %v", after["note"], stored["note"])
	}
	gotItems, wantArr := after["items"].GetArrayValue().GetValues(), stored["items"].GetArrayValue().GetValues()
	if len(gotItems) != len(wantArr) {
		t.Fatalf("items has %d values, want %d", len(gotItems), len(wantArr))
	}
	for i := 2; i < len(wantArr); i++ {
		if !proto.Equal(gotItems[i], wantArr[i]) {
			t.Errorf("items[%d] reads back %v through the v1 API, want %v", i, gotItems[i], wantArr[i])
		}
	}
	gotLine, wantLineE := after["line"].GetEntityValue(), stored["line"].GetEntityValue()
	if !proto.Equal(gotLine.GetProperties()["when"], wantLineE.GetProperties()["when"]) ||
		!proto.Equal(gotLine.GetProperties()["tags"].GetArrayValue().GetValues()[1], wantLineE.GetProperties()["tags"].GetArrayValue().GetValues()[1]) {
		t.Errorf("line's values not edited changed: %v, want %v", gotLine, wantLineE)
	}
	if !gotLine.GetProperties()["where"].GetExcludeFromIndexes() {
		t.Error("line.where lost its exclusion from indexes")
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
	if len(vs) != 7 || !keyEquals(vs[0], datastore.IDKey("Order", 8, nil)) {
		t.Fatalf("the client reads items %#v, want Order 8 first", got["items"])
	}
	if ts, ok := vs[1].(time.Time); !ok || !ts.Equal(newWhen) {
		t.Errorf("the client reads items[1] %#v, want %v", vs[1], newWhen)
	}
	if g, ok := vs[2].(datastore.GeoPoint); !ok || g != (datastore.GeoPoint{Lat: 51.5, Lng: -0.12}) {
		t.Errorf("the client reads items[2] %#v, want it unchanged", vs[2])
	}
	line, _ := got["line"].(*datastore.Entity)
	if line == nil || !line.Key.Equal(datastore.NameKey("Line", "two", nil)) {
		t.Fatalf("the client reads line %#v, want its key Line two", got["line"])
	}
	lp := map[string]any{}
	for _, p := range line.Properties {
		lp[p.Name] = p.Value
	}
	if g, ok := lp["where"].(datastore.GeoPoint); !ok || g != (datastore.GeoPoint{Lat: 3.5, Lng: -4.25}) {
		t.Errorf("the client reads line.where %#v, want 3.5, -4.25", lp["where"])
	}
	if ts, ok := lp["when"].(time.Time); !ok || !ts.Equal(when) {
		t.Errorf("the client reads line.when %#v, want %v unchanged", lp["when"], when)
	}
	tags, _ := lp["tags"].([]any)
	if len(tags) != 2 || !keyEquals(tags[0], datastore.IDKey("Order", 3, datastore.NameKey("Customer", "bob", nil))) || tags[1] != "t" {
		t.Errorf("the client reads line.tags %#v, want [Customer/bob/Order/3 t]", lp["tags"])
	}
}
