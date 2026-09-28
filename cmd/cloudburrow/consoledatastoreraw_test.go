package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// TestDatastoreKeyValuesKeepTheirProjectAndDatabase (#893).
//
// The Go client reads a key value's partition as its namespace alone, so a
// key naming an entity in another project or database read as this
// project's, and writing the entity back rewrote it. The pages now read the
// v1 API's value: a key in this project's default database, named or not, is
// a key as before; one in another project or database is shown with where it
// is, and offered no edit, since the form writes a key in this project's
// default database.
func TestDatastoreKeyValuesKeepTheirProjectAndDatabase(t *testing.T) {
	path := []*datastorepb.Key_PathElement{{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}}
	key := func(project, database, ns string) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: &datastorepb.Key{
			PartitionId: &datastorepb.PartitionId{ProjectId: project, DatabaseId: database, NamespaceId: ns}, Path: path}}}
	}
	for _, tc := range []struct {
		name  string
		value *datastorepb.Value
		shown string
		edit  bool
	}{
		{"this project", key("demo", "", ""), "Order/id=7", true},
		{"no project named", key("", "", ""), "Order/id=7", true},
		{"this project in a namespace", key("demo", "", "tenant-a"), "__namespace__/tenant-a/Order/id=7", true},
		{"another project", key("other", "", ""), "Order/id=7 (project other)", false},
		{"another database", key("demo", "db2", ""), "Order/id=7 (database db2)", false},
		{"both", key("other", "db2", "tenant-a"), "__namespace__/tenant-a/Order/id=7 (project other, database db2)", false},
	} {
		v := datastoreValueGo(tc.value, "demo")
		if typ := datastoreType(v); typ != "key" {
			t.Errorf("%s: type %q, want key", tc.name, typ)
		}
		if shown := renderDatastoreValue(v, false); shown != tc.shown {
			t.Errorf("%s: shown as %q, want %q", tc.name, shown, tc.shown)
		}
		if _, raw, ok := formatDatastoreValue(v, false); ok != tc.edit {
			t.Errorf("%s: editable %v (as %q), want %v", tc.name, ok, raw, tc.edit)
		}
		if !tc.edit && !strings.Contains(datastoreNoEditNote(v), "cannot be edited here") {
			t.Errorf("%s: the note is %q", tc.name, datastoreNoEditNote(v))
		}
		// Nested, as #894 renders it.
		arr := &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{
			Values: []*datastorepb.Value{tc.value}}}}
		if shown := renderDatastoreValue(datastoreValueGo(arr, "demo"), false); shown != "[key("+tc.shown+")]" {
			t.Errorf("%s in an array: shown as %q", tc.name, shown)
		}
	}
	if !strings.Contains(datastoreNoEditNote(datastoreValueGo(key("other", "db2", ""), "demo")), "project other, database db2") {
		t.Error("the note does not name the key's project and database")
	}
}

// TestDatastoreFormValuesAreWrittenAsTheClientWritesThem (#893).
//
// A form's value is written through the v1 API as the Go client would write
// it, and reads back as the value the form parsed: every type the form
// offers, an excluded array's flag on its elements, and the client's refusal
// of an indexed string over 1,500 bytes.
func TestDatastoreFormValuesAreWrittenAsTheClientWritesThem(t *testing.T) {
	at := time.Date(2026, 9, 27, 15, 4, 5, 123456789, time.UTC)
	for _, v := range []any{
		nil, true, int64(-3), 2.0, "x", at,
		datastore.NameKey("Order", "id=7", datastore.IDKey("Customer", 1, nil)),
		datastore.GeoPoint{Lat: 51.5, Lng: -0.12},
		[]any{int64(1), "a", []any{2.5}},
		&datastore.Entity{Properties: []datastore.Property{{Name: "a", Value: int64(1)}, {Name: "b", Value: []any{"x"}}}},
	} {
		for _, noIndex := range []bool{false, true} {
			pv, err := datastoreValuePB(v, noIndex)
			if err != nil {
				t.Errorf("%#v: %v", v, err)
				continue
			}
			back := datastorePropertiesGo(map[string]*datastorepb.Value{"p": pv}, "demo")[0]
			want := v
			if e, ok := v.(*datastore.Entity); ok && noIndex {
				// An entity in an excluded property has its own properties
				// excluded, as the client writes it.
				c := &datastore.Entity{}
				for _, p := range e.Properties {
					c.Properties = append(c.Properties, datastore.Property{Name: p.Name, Value: p.Value, NoIndex: true})
				}
				want = c
			}
			if !reflect.DeepEqual(back.Value, want) || back.NoIndex != noIndex {
				t.Errorf("%#v (excluded %v) reads back %#v (excluded %v)", v, noIndex, back.Value, back.NoIndex)
			}
		}
	}
	arr, _ := datastoreValuePB([]any{"a", "b"}, true)
	if arr.ExcludeFromIndexes || !arr.GetArrayValue().GetValues()[1].ExcludeFromIndexes {
		t.Errorf("an excluded array is written %v, want its elements excluded and not the array", arr)
	}
	// The key is written with no project or database, which the request's
	// fill in, as the client writes one.
	kv, _ := datastoreValuePB(datastore.NameKey("Order", "x", nil), false)
	if want := (&datastorepb.Key{Path: []*datastorepb.Key_PathElement{{Kind: "Order",
		IdType: &datastorepb.Key_PathElement_Name{Name: "x"}}}}); !proto.Equal(kv.GetKeyValue(), want) {
		t.Errorf("a key is written %v, want %v", kv.GetKeyValue(), want)
	}
	if _, err := datastorePropertyPB(datastore.Property{Name: "essay", Value: strings.Repeat("a", 1501)}); err == nil ||
		!strings.Contains(err.Error(), "too long to index") {
		t.Errorf("an indexed 1,501-byte string = %v, want the client's refusal", err)
	}
	if _, err := datastorePropertyPB(datastore.Property{Name: "essay", Value: strings.Repeat("a", 1501), NoIndex: true}); err != nil {
		t.Errorf("an excluded 1,501-byte string = %v", err)
	}
}

// TestNestedDatastoreValuesRenderAsTheirType (#894).
//
// A key inside an array or an embedded entity was shown in the Go client's
// String form, /Order,id=7, which does not tell a name from an ID. Nested
// values JSON cannot hold are written as their type and the form a property
// of that type shows, key(Order/name=id=7) as a key property shows
// Order/name=id=7, and an embedded entity's own key as its __key__. They stay
// read-only (TestValuesTheFormCannotHoldOfferNoEdit).
func TestNestedDatastoreValuesRenderAsTheirType(t *testing.T) {
	at := time.Date(2026, 9, 27, 15, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		v    any
		want string
	}{
		{[]any{datastore.NameKey("Order", "id=7", nil), datastore.IDKey("Order", 7, nil)},
			"[key(Order/name=id=7), key(Order/id=7)]"},
		{[]any{"Order/id=7", datastore.NameKey("Order", "a/b", datastore.NameKey("Customer", "alice", nil))},
			`["Order/id=7", key(Customer/name=alice/Order/name=a%2Fb)]`},
		{&datastore.Entity{Properties: []datastore.Property{
			{Name: "ref", Value: datastore.IDKey("Order", 7, nil)}, {Name: "n", Value: int64(2)}}},
			`{"n": 2, "ref": key(Order/id=7)}`},
		{&datastore.Entity{Key: datastore.NameKey("Line", "id=1", nil), Properties: []datastore.Property{{Name: "q", Value: 1.0}}},
			`{"__key__": key(Line/name=id=1), "q": 1.0}`},
		{[]any{at, datastore.GeoPoint{Lat: 51.5, Lng: -0.12}, []byte("abc"), nil},
			"[timestamp(2026-09-27T15:04:05Z), geopoint(51.5, -0.12), blob(YWJj), null]"},
		{[]any{[]any{datastore.IDKey("Order", 7, nil)}}, "[[key(Order/id=7)]]"},
	} {
		if got := renderDatastoreValue(tc.v, false); got != tc.want {
			t.Errorf("%#v is shown as %q, want %q", tc.v, got, tc.want)
		}
		if typ, raw, ok := formatDatastoreValue(tc.v, false); ok {
			t.Errorf("%#v offers an edit as %s %q", tc.v, typ, raw)
		}
	}
	// What JSON holds is still JSON, as before.
	if got := renderDatastoreValue([]any{int64(1), "a"}, false); got != `[1, "a"]` {
		t.Errorf("a plain array is shown as %q", got)
	}
}
