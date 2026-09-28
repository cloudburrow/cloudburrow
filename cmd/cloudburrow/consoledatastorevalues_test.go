package main

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// TestDatastoreAddAndRemoveValueChangeOnlyThatValue (#911). Add value
// inserts into an array at an index or appends, and adds a property or a key
// to an embedded entity; Remove value removes an array's value, which closes
// up, an embedded entity's property and its key. Each changes that value
// only: undoing it by hand gives back the property proto-equal. What the
// form cannot mean is refused, and a refused change leaves the property as
// it was.
func TestDatastoreAddAndRemoveValueChangeOnlyThatValue(t *testing.T) {
	orig := nestedFixture()
	items := func(v *datastorepb.Value) []*datastorepb.Value { return v.GetArrayValue().GetValues() }
	line := func(v *datastorepb.Value) *datastorepb.Entity { return items(v)[6].GetEntityValue() }

	// Insert at 0, at 3 and append; each new value is as the client writes
	// it, with the form's index flag.
	for _, tc := range []struct {
		index string
		at    int
	}{{"0", 0}, {"3", 3}, {"", 7}, {" 7 ", 7}} {
		prop := proto.Clone(orig).(*datastorepb.Value)
		excluded := tc.at == 7 // an excluded value goes after the indexed ones
		if err := addDatastoreElement(prop, "items", nil, map[string]string{"index": tc.index, "type": "blob", "value": "AQID",
			"excluded": strconv.FormatBool(excluded)}); err != nil {
			t.Fatalf("Add value at %q: %v", tc.index, err)
		}
		got := items(prop)
		if len(got) != 8 || !proto.Equal(got[tc.at], &datastorepb.Value{ValueType: &datastorepb.Value_BlobValue{BlobValue: []byte{1, 2, 3}}, ExcludeFromIndexes: excluded}) {
			t.Fatalf("Add value at %q put %v at %d", tc.index, got, tc.at)
		}
		prop.GetArrayValue().Values = append(got[:tc.at:tc.at], got[tc.at+1:]...)
		if !proto.Equal(prop, orig) {
			t.Errorf("Add value at %q changed other values: %v", tc.index, prop)
		}
	}

	// A property of an embedded entity inside the array, and inside an
	// array inside that entity.
	prop := proto.Clone(orig).(*datastorepb.Value)
	if err := addDatastoreElement(prop, "items", []any{6}, map[string]string{"field": "qty", "type": "integer", "value": "3"}); err != nil {
		t.Fatal(err)
	}
	if err := addDatastoreElement(prop, "items", []any{6, "odd name"}, map[string]string{"type": "key", "value": "Order/id=4"}); err != nil {
		t.Fatal(err)
	}
	if q := line(prop).GetProperties()["qty"]; q.GetIntegerValue() != 3 || q.GetExcludeFromIndexes() {
		t.Errorf("items[6].qty is %v, want 3, indexed", q)
	}
	odd := line(prop).GetProperties()["odd name"].GetArrayValue()
	if len(odd.GetValues()) != 2 || odd.GetValues()[1].GetKeyValue().GetPath()[0].GetId() != 4 {
		t.Errorf("items[6][\"odd name\"] is %v, want Order 4 appended", odd)
	}
	delete(line(prop).Properties, "qty")
	odd.Values = odd.Values[:1]
	if !proto.Equal(prop, orig) {
		t.Errorf("Add value changed other values: %v", prop)
	}

	// Remove each kind of value, then check only it went.
	for _, tc := range []struct {
		steps []any
		undo  func(p *datastorepb.Value)
	}{
		{[]any{1}, func(p *datastorepb.Value) {
			p.GetArrayValue().Values = append(items(p)[:1:1], append([]*datastorepb.Value{items(orig)[1]}, items(p)[1:]...)...)
		}},
		{[]any{6, "when"}, func(p *datastorepb.Value) { line(p).Properties["when"] = line(orig).GetProperties()["when"] }},
		{[]any{6, "__key__"}, func(p *datastorepb.Value) { line(p).Key = line(orig).GetKey() }},
		{[]any{6, "odd name", 0}, func(p *datastorepb.Value) {
			line(p).GetProperties()["odd name"].GetArrayValue().Values = line(orig).GetProperties()["odd name"].GetArrayValue().GetValues()
		}},
		{[]any{6}, func(p *datastorepb.Value) { p.GetArrayValue().Values = append(items(p), items(orig)[6]) }},
	} {
		prop := proto.Clone(orig).(*datastorepb.Value)
		if err := removeDatastoreElement(prop, "items", tc.steps); err != nil {
			t.Fatalf("Remove value %v: %v", tc.steps, err)
		}
		if proto.Equal(prop, orig) {
			t.Fatalf("Remove value %v changed nothing", tc.steps)
		}
		tc.undo(prop)
		if !proto.Equal(prop, orig) {
			t.Errorf("Remove value %v changed more than that value: %v", tc.steps, prop)
		}
	}

	// A key for an embedded entity that has none.
	prop = proto.Clone(orig).(*datastorepb.Value)
	line(prop).Key = nil
	if err := addDatastoreElement(prop, "items", []any{6}, map[string]string{"field": "__key__", "type": "key", "value": "Line/name=l"}); err != nil {
		t.Fatal(err)
	}
	if k := line(prop).GetKey(); len(k.GetPath()) != 1 || k.GetPath()[0].GetName() != "l" {
		t.Errorf("items[6]'s key is %v, want Line/name=l", k)
	}

	// Refused, leaving the property as it was.
	big := strings.Repeat("A", 2004) // 1,503 bytes
	for _, tc := range []struct {
		steps  []any
		values map[string]string
		remove bool
		want   string
	}{
		{nil, map[string]string{"index": "8", "type": "string", "value": "x"}, false, "0 to 7"},
		{nil, map[string]string{"index": "-1", "type": "string", "value": "x"}, false, "0 to 7"},
		{nil, map[string]string{"type": "array", "value": "[1]"}, false, "cannot hold an array"},
		{nil, map[string]string{"type": "blob", "value": big}, false, "too long to index"},
		// Datastore would move an excluded value before an indexed one to
		// the end of the array.
		{nil, map[string]string{"index": "0", "type": "string", "value": "x", "excluded": "true"}, false, "reorder"},
		{nil, map[string]string{"type": "integer", "value": "x"}, false, "64-bit integer"},
		{[]any{0}, map[string]string{"type": "string", "value": "x"}, false, "not an array or an embedded entity"},
		{[]any{6}, map[string]string{"field": "when", "type": "string", "value": "x"}, false, "already has a property"},
		{[]any{6}, map[string]string{"field": "__key__", "type": "key", "value": "A/id=1"}, false, "already has a key"},
		{[]any{6}, map[string]string{"field": "__key__", "type": "string", "value": "x"}, false, "is a key"},
		{[]any{6}, map[string]string{"field": " ", "type": "string", "value": "x"}, false, "name is required"},
		{[]any{9}, map[string]string{"type": "string", "value": "x"}, false, "no value at items[9]"},
		{[]any{9}, nil, true, "no value at items[9]"},
		{[]any{6, "missing"}, nil, true, "no value at items[6].missing"},
		{[]any{0, "__key__"}, nil, true, "no value at items[0].__key__"},
		{nil, nil, true, "Delete property"},
	} {
		prop := proto.Clone(orig).(*datastorepb.Value)
		var err error
		if tc.remove {
			err = removeDatastoreElement(prop, "items", tc.steps)
		} else {
			err = addDatastoreElement(prop, "items", tc.steps, tc.values)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %v: err %v, want one naming %q", tc.steps, tc.values, err, tc.want)
		}
		if !proto.Equal(prop, orig) {
			t.Errorf("%v: a refused change changed the property", tc.steps)
		}
	}
	// Excluded, the long blob is added.
	prop = proto.Clone(orig).(*datastorepb.Value)
	if err := addDatastoreElement(prop, "items", nil, map[string]string{"type": "blob", "value": big, "excluded": "true"}); err != nil {
		t.Errorf("an excluded 1,503-byte blob is refused: %v", err)
	}
}

// TestDatastoreValueActionsAreOfferedWhereTheyApply (#911, #912, #923,
// #924). A value the form can hold is offered Edit value and Remove value;
// an array or embedded entity inside a property Add value, Exclude from
// indexes and Remove value; a key in another project Remove value only.
// Every one carries the property's digest in a hidden field, Remove value
// nothing else, so it is still a confirm with no inputs. Add value's index
// flag is prefilled as its neighbours'.
func TestDatastoreValueActionsAreOfferedWhereTheyApply(t *testing.T) {
	drawn := nestedFixture()
	digest := datastorePropertyDigest(drawn)
	got := map[string][]string{}
	for _, el := range datastoreElements(drawn) {
		var ids []string
		for _, a := range datastoreElementActions("demo", el, drawn) {
			ids = append(ids, a.ID)
			last := a.Fields[len(a.Fields)-1]
			if last.Name != datastoreExpectedField || last.Type != "hidden" || last.Default != digest {
				t.Errorf("%s on %v carries %+v last, want the property's digest %s, hidden", a.ID, el.Steps, last, digest)
			}
			if a.ID == "removevalue" && (!a.Destructive || !a.Leaves || len(a.Fields) != 1) {
				t.Errorf("Remove value on %v is %+v, want a destructive confirm with only the hidden digest", el.Steps, a)
			}
		}
		got[datastoreElementLabel("items", el.Steps)] = ids
	}
	edit := []string{"editvalue", "removevalue"}
	container := []string{"addvalue", "excludevalues", "removevalue"}
	want := map[string][]string{
		"items[0]": edit, "items[1]": edit, "items[2]": edit,
		"items[3]":                {"removevalue"},
		"items[4]":                edit, // a blob, as base64 (#912)
		"items[5]":                edit,
		"items[6]":                container,
		"items[6].__key__":        edit,
		`items[6]["odd name"]`:    container,
		`items[6]["odd name"][0]`: edit,
		"items[6].when":           edit,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("actions:\n got %v\nwant %v", got, want)
	}

	defaults := func(target *datastorepb.Value) map[string]string {
		out := map[string]string{}
		for _, f := range datastoreAddValueAction(target).Fields {
			out[f.Name] = f.Default
		}
		return out
	}
	excludedArr := &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{
		Values: []*datastorepb.Value{{ValueType: &datastorepb.Value_StringValue{StringValue: "x"}, ExcludeFromIndexes: true}}}}}
	if d := defaults(excludedArr); d["excluded"] != "true" || d["type"] != "string" {
		t.Errorf("Add value on an excluded array is prefilled %v, want excluded", d)
	}
	if _, has := defaults(excludedArr)["index"]; !has {
		t.Error("Add value on an array has no Index")
	}
	entity := nestedFixture().GetArrayValue().GetValues()[6]
	if d := defaults(entity); d["excluded"] != "false" {
		t.Errorf("Add value on an indexed embedded entity is prefilled %v, want indexed", d)
	}
	if _, has := defaults(entity)["field"]; !has {
		t.Error("Add value on an embedded entity has no Property")
	}
}

// TestDatastoreBlobValuesRoundTripAsBase64 (#912). A blob inside a property
// is prefilled as base64 with its index flag, and saved unchanged writes the
// same bytes back, its meaning included; a blob over 1,500 bytes must be
// excluded from indexes, and Edit value's Exclude from indexes does it.
func TestDatastoreBlobValuesRoundTripAsBase64(t *testing.T) {
	blob := &datastorepb.Value{ValueType: &datastorepb.Value_BlobValue{BlobValue: []byte{0, 1, 0xff}}, Meaning: 16}
	orig := &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{
		Values: []*datastorepb.Value{blob}}}}
	fields, _, ok := datastoreElementForm(datastoreValueGo(blob, "demo"), false, false)
	if !ok {
		t.Fatal("a blob is offered no Edit value")
	}
	values := map[string]string{}
	for _, f := range fields {
		values[f.Name] = f.Default
	}
	if values["type"] != "blob" || values["value"] != "AAH/" || values["excluded"] != "false" {
		t.Errorf("Edit value on a blob is prefilled %v, want blob AAH/, indexed", values)
	}
	parsed, err := parseDatastoreElementValue(values, false)
	if err != nil {
		t.Fatal(err)
	}
	prop := proto.Clone(orig).(*datastorepb.Value)
	if err := setDatastoreElement(prop, "demo", "b", []any{0}, parsed, datastoreExcludedValue(values)); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(prop, orig) {
		t.Errorf("Edit value on a blob saved unchanged wrote %v, want %v", prop, orig)
	}

	long := strings.Repeat("AAAA", 501) // 1,503 bytes
	values = map[string]string{"type": "blob", "value": long, "excluded": "false"}
	parsed, _ = parseDatastoreElementValue(values, false)
	if err := setDatastoreElement(proto.Clone(orig).(*datastorepb.Value), "demo", "b", []any{0}, parsed, datastoreExcludedValue(values)); err == nil ||
		!strings.Contains(err.Error(), "too long to index") {
		t.Errorf("an indexed 1,503-byte blob: err %v, want too long to index", err)
	}
	values["excluded"] = "true"
	prop = proto.Clone(orig).(*datastorepb.Value)
	if err := setDatastoreElement(prop, "demo", "b", []any{0}, parsed, datastoreExcludedValue(values)); err != nil {
		t.Fatal(err)
	}
	if v := prop.GetArrayValue().GetValues()[0]; len(v.GetBlobValue()) != 1503 || !v.GetExcludeFromIndexes() {
		t.Errorf("the excluded long blob is %d bytes, excluded %v", len(v.GetBlobValue()), v.GetExcludeFromIndexes())
	}

	// Excluded before an indexed value of its array, it would be moved to
	// the end, so it is refused there.
	two := proto.Clone(orig).(*datastorepb.Value)
	two.GetArrayValue().Values = append(two.GetArrayValue().Values, &datastorepb.Value{ValueType: &datastorepb.Value_StringValue{StringValue: "x"}})
	before := proto.Clone(two).(*datastorepb.Value)
	if err := setDatastoreElement(two, "demo", "b", []any{0}, parsed, datastoreExcludedValue(values)); err == nil || !strings.Contains(err.Error(), "reorder") {
		t.Errorf("an excluded blob before an indexed value: err %v, want it refused", err)
	}
	if !proto.Equal(two, before) {
		t.Error("a refused Edit value changed the array")
	}

	// A top-level blob property: as the client writes it, and shown as
	// base64, a listing's cell shortened.
	pv, err := datastorePropertyPB(datastore.Property{Name: "b", Value: []byte(long)})
	if err == nil || !strings.Contains(err.Error(), "too long to index") {
		t.Errorf("an indexed 1,503-byte blob property: %v, %v", pv, err)
	}
	if pv, err := datastorePropertyPB(datastore.Property{Name: "b", Value: []byte{1, 2}}); err != nil || string(pv.GetBlobValue()) != "\x01\x02" {
		t.Errorf("a blob property is written %v, %v", pv, err)
	}
	if s := renderDatastoreValue([]byte{1, 2}, false); s != "AQI=" {
		t.Errorf("a blob renders %q, want AQI=", s)
	}
	if s := datastoreRowValue(make([]byte, 300), true); len([]rune(s)) != 78 || !strings.HasSuffix(s, "…") {
		t.Errorf("a 300-byte blob's cell is %q, want it shortened", s)
	}
	if typ := datastoreType([]byte{1, 2}); typ != "blob (2 bytes)" {
		t.Errorf("a blob's type is %q", typ)
	}
}
