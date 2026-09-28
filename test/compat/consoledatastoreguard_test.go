//go:build compat

package compat

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// TestConsoleDatastoreValueActionsRefuseAConcurrentChange (#923). Edit
// value, Add value, Remove value and Exclude from indexes address a value by
// its place in the property, and each carries the digest of the property its
// page was drawn from. An entity is written with the official Go client and
// the property's page and rows are read; then another writer changes the
// property — the official client inserts a value at the front of the array,
// and then the v1 API changes one value's index flag. Every action drawn
// before either change is refused with "changed since the page was loaded",
// and the property reads back through Lookup exactly as the other writer
// left it: Remove value on items[0] did not remove the value now there. An
// action carrying no digest is refused. A change to another property of the
// entity does not refuse an action drawn before it: Remove value then
// removes the value its row showed, and the official client reads the rest.
func TestConsoleDatastoreValueActionsRefuseAConcurrentChange(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	holder := datastore.NameKey("Holder923", "h", nil)
	when := time.Date(2026, 9, 28, 10, 11, 12, 0, time.UTC)
	put := func(items []any, note string) {
		t.Helper()
		props := datastore.PropertyList{{Name: "items", Value: items}, {Name: "note", Value: note}}
		if _, err := c.Put(ctx, holder, &props); err != nil {
			t.Fatalf("Put with the official client: %v", err)
		}
	}
	put([]any{datastore.IDKey("Order", 1, nil), "a", when}, "x")
	t.Cleanup(func() { _ = c.Delete(ctx, holder) })
	holderPB := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
		Path: []*datastorepb.Key_PathElement{{Kind: "Holder923", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}}
	lookup := func() map[string]*datastorepb.Value {
		t.Helper()
		r, err := raw.Lookup(ctx, &datastorepb.LookupRequest{ProjectId: project, Keys: []*datastorepb.Key{holderPB}})
		if err != nil || len(r.GetFound()) != 1 {
			t.Fatalf("Lookup through the v1 API = %v, %v", r, err)
		}
		return r.GetFound()[0].GetEntity().GetProperties()
	}
	entity := []string{"Holder923", holder.Encode()}
	at := func(parts ...string) []string { return append(append([]string{}, entity...), parts...) }
	// drawn is each action as the items page and its rows offer it now,
	// prefilled, by where it acts and its ID.
	type drawnAction struct {
		path   []string
		id     string
		values map[string]string
	}
	drawn := func() map[string]drawnAction {
		t.Helper()
		out := map[string]drawnAction{}
		page := consoleDatastoreValuePage(t, addr, project, at("items")...)
		for _, id := range []string{"addvalue", "excludevalues"} {
			v, ok := page.action(id)
			if !ok {
				t.Fatalf("the items page offers no %s", id)
			}
			if v["expected"] == "" {
				t.Fatalf("the items page's %s carries no digest: %v", id, v)
			}
			out["items "+id] = drawnAction{at("items"), id, v}
		}
		for _, it := range consoleDatastorePage(t, addr, project, at("items")...).listing("elements").Items {
			for _, a := range it.Actions {
				v := map[string]string{}
				for _, f := range a.Fields {
					v[f.Name] = f.Default
				}
				out[it.Name+" "+a.ID] = drawnAction{it.ActsOn, a.ID, v}
			}
		}
		return out
	}
	stale := drawn()
	for _, want := range []string{"items[0] removevalue", "items[1] editvalue", "items[0] editvalue"} {
		if stale[want].values["expected"] != stale["items addvalue"].values["expected"] {
			t.Fatalf("%s carries %v, want the property's digest as the page's actions do", want, stale[want].values)
		}
	}
	refusedAll := func(what string, want map[string]*datastorepb.Value) {
		t.Helper()
		for name, a := range map[string]drawnAction{
			"Remove value on items[0]": stale["items[0] removevalue"],
			"Edit value on items[1]":   {stale["items[1] editvalue"].path, "editvalue", with(stale["items[1] editvalue"].values, "type", "string", "value", "b")},
			"Add value at 0":           {stale["items addvalue"].path, "addvalue", with(stale["items addvalue"].values, "index", "0", "value", "z")},
			"Exclude from indexes":     {stale["items excludevalues"].path, "excludevalues", with(stale["items excludevalues"].values, "excluded", "true")},
		} {
			code, out := consoleAct(t, addr, "datastore", project, a.path, a.id, a.values)
			if code != http.StatusBadRequest || !strings.Contains(out, `property \"items\" changed since the page was loaded`) ||
				!strings.Contains(out, "reload") {
				t.Errorf("after %s, %s drawn before it = %d: %s; want it refused, saying to reload", what, name, code, out)
			}
		}
		if got := lookup(); !proto.Equal(got["items"], want["items"]) || !proto.Equal(got["note"], want["note"]) {
			t.Errorf("after the refused actions, items reads back %v through the v1 API, want it as %s left it: %v", got, what, want)
		}
	}

	// Another writer inserts a value at the front, with the official client.
	put([]any{"new", datastore.IDKey("Order", 1, nil), "a", when}, "x")
	refusedAll("the official client inserted a value at the front", lookup())

	// Another writer changes one index flag through the v1 API.
	stale = drawn()
	stored := lookup()
	stored["items"].GetArrayValue().GetValues()[3].ExcludeFromIndexes = true
	if _, err := raw.Commit(ctx, &datastorepb.CommitRequest{ProjectId: project, Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Update{
			Update: &datastorepb.Entity{Key: holderPB, Properties: stored}}}}}); err != nil {
		t.Fatalf("change items[3]'s index flag through the v1 API: %v", err)
	}
	refusedAll("the v1 API changed an index flag", lookup())

	// No digest: refused.
	fresh := drawn()
	if code, out := consoleAct(t, addr, "datastore", project, fresh["items[0] removevalue"].path, "removevalue", nil); code != http.StatusBadRequest ||
		!strings.Contains(out, "reload") {
		t.Errorf("Remove value carrying no digest = %d: %s; want it refused", code, out)
	}

	// Another property changed: the action drawn before it still applies,
	// to the value its row showed.
	if fresh["items[0] removevalue"].path[3] != "[0]" {
		t.Fatalf("items[0]'s row acts on %v", fresh["items[0] removevalue"].path)
	}
	now := lookup()
	now["note"] = &datastorepb.Value{ValueType: &datastorepb.Value_StringValue{StringValue: "y"}}
	if _, err := raw.Commit(ctx, &datastorepb.CommitRequest{ProjectId: project, Mode: datastorepb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Update{
			Update: &datastorepb.Entity{Key: holderPB, Properties: now}}}}}); err != nil {
		t.Fatalf("change note through the v1 API: %v", err)
	}
	a := fresh["items[0] removevalue"]
	if code, out := consoleAct(t, addr, "datastore", project, a.path, a.id, a.values); code != http.StatusOK {
		t.Fatalf("Remove value on items[0] after note changed = %d: %s", code, out)
	}
	var back datastore.PropertyList
	if err := c.Get(ctx, holder, &back); err != nil {
		t.Fatalf("Get with the official client: %v", err)
	}
	got := map[string]any{}
	for _, p := range back {
		got[p.Name] = p.Value
	}
	items, _ := got["items"].([]any)
	if len(items) != 3 || !keyEquals(items[0], datastore.IDKey("Order", 1, nil)) || items[1] != "a" || got["note"] != "y" {
		t.Errorf("the official client reads items %#v and note %v; want \"new\" removed, Order 1 first, and note y", got["items"], got["note"])
	}
	if ts, ok := items[2].(time.Time); !ok || !ts.Equal(when) {
		t.Errorf("the official client reads items[2] %#v, want %v", items[2], when)
	}
}

// TestConsoleDatastoreExcludeWholeArrayFromIndexes (#924). The emulator
// stores an array's values excluded from indexes after its indexed ones, so
// a value excluded before an indexed one is refused, and an array could not
// be excluded one value at a time. An entity is written with the official
// client: an indexed array of a key, a timestamp, a geopoint and a string,
// and an indexed embedded entity with a key, a timestamp and an array. On
// the array's page, Exclude from indexes, prefilled unchecked, excludes
// every value in one write: through Lookup every value is excluded, in the
// same order, and otherwise proto-equal to what was stored; the official
// client reads the array excluded, as its types. A 2,000-byte blob,
// excluded, can then be added at the front, as it could not before, and
// the client reads it first. Indexing the array again is refused while the
// blob is in it, naming it and the 1,500-byte rule, and leaves it as it
// was; with the blob removed, it indexes every value, and the array reads
// back proto-equal to what the client stored. On the embedded entity's page
// it excludes the entity and every value inside it; on the page of the
// array inside, it indexes that array's values and leaves the entity's own
// flag as it is.
func TestConsoleDatastoreExcludeWholeArrayFromIndexes(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	raw := rawDatastore(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()

	holder := datastore.NameKey("Holder924", "h", nil)
	when := time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC)
	props := datastore.PropertyList{
		{Name: "items", Value: []any{datastore.NameKey("Order", "id=7", nil), when, datastore.GeoPoint{Lat: 51.5, Lng: -0.12}, "s"}},
		{Name: "line", Value: &datastore.Entity{Key: datastore.IDKey("Line", 1, nil), Properties: []datastore.Property{
			{Name: "when", Value: when},
			{Name: "tags", Value: []any{datastore.IDKey("Order", 3, nil), "t"}},
		}}},
	}
	if _, err := c.Put(ctx, holder, &props); err != nil {
		t.Fatalf("Put with the official client: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, holder) })
	holderPB := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project},
		Path: []*datastorepb.Key_PathElement{{Kind: "Holder924", IdType: &datastorepb.Key_PathElement_Name{Name: "h"}}}}
	lookup := func() map[string]*datastorepb.Value {
		t.Helper()
		r, err := raw.Lookup(ctx, &datastorepb.LookupRequest{ProjectId: project, Keys: []*datastorepb.Key{holderPB}})
		if err != nil || len(r.GetFound()) != 1 {
			t.Fatalf("Lookup through the v1 API = %v, %v", r, err)
		}
		return r.GetFound()[0].GetEntity().GetProperties()
	}
	get := func() map[string]datastore.Property {
		t.Helper()
		var back datastore.PropertyList
		if err := c.Get(ctx, holder, &back); err != nil {
			t.Fatalf("Get with the official client: %v", err)
		}
		out := map[string]datastore.Property{}
		for _, p := range back {
			out[p.Name] = p
		}
		return out
	}
	stored := lookup()
	entity := []string{"Holder924", holder.Encode()}
	at := func(parts ...string) []string { return append(append([]string{}, entity...), parts...) }
	exclude := func(path []string, excluded bool) (int, string) {
		t.Helper()
		values, ok := consoleDatastoreValuePage(t, addr, project, path...).action("excludevalues")
		if !ok {
			t.Fatalf("%v offers no Exclude from indexes", path)
		}
		return consoleAct(t, addr, "datastore", project, path, "excludevalues", with(values, "excluded", map[bool]string{true: "true", false: "false"}[excluded]))
	}
	// flagsAs is v with every index flag inside set to excluded, an array's
	// own left unset.
	var flagsAs func(v *datastorepb.Value, excluded bool) *datastorepb.Value
	flagsAs = func(v *datastorepb.Value, excluded bool) *datastorepb.Value {
		out := proto.Clone(v).(*datastorepb.Value)
		if arr := out.GetArrayValue(); arr != nil {
			for i, e := range arr.GetValues() {
				arr.Values[i] = flagsAs(e, excluded)
			}
			return out
		}
		out.ExcludeFromIndexes = excluded
		if e := out.GetEntityValue(); e != nil {
			for n, p := range e.GetProperties() {
				e.Properties[n] = flagsAs(p, excluded)
			}
		}
		return out
	}

	// Offered on the array and the embedded entity, prefilled unchecked; not
	// on a value.
	if v, ok := consoleDatastoreValuePage(t, addr, project, at("items")...).action("excludevalues"); !ok || v["excluded"] != "false" {
		t.Fatalf("the items page offers Exclude from indexes %v (%v), want it unchecked", v, ok)
	}
	if _, ok := consoleDatastoreValuePage(t, addr, project, at("items", "[0]")...).action("excludevalues"); ok {
		t.Error("items[0], a key, is offered Exclude from indexes")
	}

	// The whole array, in one write.
	if code, out := exclude(at("items"), true); code != http.StatusOK {
		t.Fatalf("Exclude from indexes on items = %d: %s", code, out)
	}
	if got, want := lookup()["items"], flagsAs(stored["items"], true); !proto.Equal(got, want) {
		t.Fatalf("after Exclude from indexes, items reads back %v through the v1 API, want every value excluded, in order: %v", got, want)
	}
	if v, _ := consoleDatastoreValuePage(t, addr, project, at("items")...).action("excludevalues"); v["excluded"] != "true" {
		t.Errorf("Exclude from indexes is prefilled %v on an excluded array, want checked", v)
	}
	p := get()["items"]
	vs, _ := p.Value.([]any)
	if !p.NoIndex || len(vs) != 4 || !keyEquals(vs[0], datastore.NameKey("Order", "id=7", nil)) || vs[3] != "s" {
		t.Errorf("the official client reads items %#v, excluded %v; want the four values, excluded", p.Value, p.NoIndex)
	}

	// Now a 2,000-byte blob, excluded, goes first.
	big := bytes.Repeat([]byte{0xab, 0x01}, 1000)
	add, _ := consoleDatastoreValuePage(t, addr, project, at("items")...).action("addvalue")
	if add["excluded"] != "true" {
		t.Errorf("Add value on the excluded array is prefilled %v, want excluded", add)
	}
	if code, out := consoleAct(t, addr, "datastore", project, at("items"), "addvalue",
		with(add, "index", "0", "type", "blob", "value", base64.StdEncoding.EncodeToString(big))); code != http.StatusOK {
		t.Fatalf("Add value of an excluded 2,000-byte blob at 0 = %d: %s", code, out)
	}
	if vs, _ := get()["items"].Value.([]any); len(vs) != 5 || !bytes.Equal(vs[0].([]byte), big) || !keyEquals(vs[1], datastore.NameKey("Order", "id=7", nil)) {
		t.Errorf("the official client reads items %d values, want the blob first and then the key", len(vs))
	}

	// Indexed again: refused while the blob is there, then every value.
	before := lookup()["items"]
	if code, out := exclude(at("items"), false); code != http.StatusBadRequest || !strings.Contains(out, "items[0] is 2000 bytes") ||
		!strings.Contains(out, "1,500 bytes") {
		t.Errorf("indexing items with the 2,000-byte blob in it = %d: %s; want it refused naming items[0]", code, out)
	}
	if !proto.Equal(lookup()["items"], before) {
		t.Error("a refused Exclude from indexes changed items")
	}
	remove, _ := consoleDatastoreValuePage(t, addr, project, at("items", "[0]")...).action("removevalue")
	if code, out := consoleAct(t, addr, "datastore", project, at("items", "[0]"), "removevalue", remove); code != http.StatusOK {
		t.Fatalf("Remove value on items[0] = %d: %s", code, out)
	}
	if code, out := exclude(at("items"), false); code != http.StatusOK {
		t.Fatalf("Exclude from indexes cleared on items = %d: %s", code, out)
	}
	if got := lookup()["items"]; !proto.Equal(got, stored["items"]) {
		t.Errorf("indexed again, items reads back %v through the v1 API, want it as the client stored it: %v", got, stored["items"])
	}

	// The embedded entity, itself included, and then the array inside it.
	if code, out := exclude(at("line"), true); code != http.StatusOK {
		t.Fatalf("Exclude from indexes on line = %d: %s", code, out)
	}
	if got, want := lookup()["line"], flagsAs(stored["line"], true); !proto.Equal(got, want) {
		t.Errorf("after Exclude from indexes, line reads back %v through the v1 API, want it and every value inside excluded: %v", got, want)
	}
	if p := get()["line"]; !p.NoIndex {
		t.Errorf("the official client reads line %#v, want it excluded", p)
	}
	if code, out := exclude(at("line", `["tags"]`), false); code != http.StatusOK {
		t.Fatalf("Exclude from indexes cleared on line.tags = %d: %s", code, out)
	}
	want := flagsAs(stored["line"], true)
	want.GetEntityValue().Properties["tags"] = flagsAs(stored["line"].GetEntityValue().GetProperties()["tags"], false)
	if got := lookup()["line"]; !proto.Equal(got, want) {
		t.Errorf("after indexing line.tags, line reads back %v through the v1 API, want %v", got, want)
	}
}
