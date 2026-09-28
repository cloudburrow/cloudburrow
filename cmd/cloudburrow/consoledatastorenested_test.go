package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// nestedFixture is a property holding a value of each kind inside an array
// and an embedded entity, stored as the emulator returns them: keys with the
// project named, index flags on the elements, a meaning on one.
func nestedFixture() *datastorepb.Value {
	key := func(project string, id int64) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: &datastorepb.Key{
			PartitionId: &datastorepb.PartitionId{ProjectId: project},
			Path:        []*datastorepb.Key_PathElement{{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: id}}}}}}
	}
	ts := &datastorepb.Value{ValueType: &datastorepb.Value_TimestampValue{TimestampValue: &timestamppb.Timestamp{Seconds: 1790000000, Nanos: 123456000}},
		ExcludeFromIndexes: true}
	geo := &datastorepb.Value{ValueType: &datastorepb.Value_GeoPointValue{GeoPointValue: &latlng.LatLng{Latitude: 51.5, Longitude: -0.12}}}
	blob := &datastorepb.Value{ValueType: &datastorepb.Value_BlobValue{BlobValue: []byte{1, 2}}}
	num := &datastorepb.Value{ValueType: &datastorepb.Value_IntegerValue{IntegerValue: 5}, Meaning: 21}
	line := &datastorepb.Value{ValueType: &datastorepb.Value_EntityValue{EntityValue: &datastorepb.Entity{
		Key: key("demo", 9).GetKeyValue(),
		Properties: map[string]*datastorepb.Value{
			"when": proto.Clone(ts).(*datastorepb.Value),
			"odd name": {ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{
				Values: []*datastorepb.Value{key("demo", 3)}}}},
		}}}}
	return &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{
		Values: []*datastorepb.Value{key("demo", 7), ts, geo, key("another-project", 7), blob, num, line}}}}
}

// TestDatastoreElementsAreListedAndAddressed (#905). Every value inside an
// array or embedded entity is listed, at any depth, by a label its row and
// page share and an address (its steps as JSON) that reads back to it; a key
// in another project and a blob are offered no Edit value.
func TestDatastoreElementsAreListedAndAddressed(t *testing.T) {
	type row struct {
		label, seg, shown string
		editable          bool
	}
	var got []row
	for _, el := range datastoreElements(nestedFixture()) {
		v := datastoreValueGo(el.Value, "demo")
		_, _, ok := datastoreElementForm(v, isDatastoreKeyStep(el.Steps))
		seg := datastoreElementSegment(el.Steps)
		back, err := parseDatastoreElement(seg)
		if err != nil || !reflect.DeepEqual(back, el.Steps) {
			t.Errorf("%s reads back as %v, %v; want %v", seg, back, err, el.Steps)
		}
		got = append(got, row{datastoreElementLabel("items", el.Steps), seg, renderDatastoreValue(v, el.Value.GetExcludeFromIndexes()), ok})
	}
	want := []row{
		{"items[0]", "[0]", "Order/id=7", true},
		{"items[1]", "[1]", "2026-09-21T14:13:20.123456Z", true},
		{"items[2]", "[2]", "51.5, -0.12", true},
		{"items[3]", "[3]", "Order/id=7 (project another-project)", false},
		{"items[4]", "[4]", renderValue([]byte{1, 2}), false},
		{"items[5]", "[5]", "5", true},
		{"items[6].__key__", `[6,"__key__"]`, "Order/id=9", true},
		{`items[6]["odd name"][0]`, `[6,"odd name",0]`, "Order/id=3", true},
		{"items[6].when", `[6,"when"]`, "2026-09-21T14:13:20.123456Z", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("elements:\n got %v\nwant %v", got, want)
	}
	for _, bad := range []string{"", "[]", "[-1]", "[1.5]", "[true]", "{}", "[0] [1]"} {
		if _, err := parseDatastoreElement(bad); err == nil {
			t.Errorf("%q reads as an element's address", bad)
		}
	}
}

// TestDatastoreEditValueWritesOnlyThatValue (#905). Edit value saved as
// prefilled writes back exactly what was stored, the whole property proto-
// equal, for every value the form offers; a change writes that value as the
// client writes it, with its own index flag, and leaves every other value in
// the property untouched. A key in another project, a blob, an array or
// entity type, and a non-key for an entity's key are refused.
func TestDatastoreEditValueWritesOnlyThatValue(t *testing.T) {
	orig := nestedFixture()
	for _, el := range datastoreElements(orig) {
		v := datastoreValueGo(el.Value, "demo")
		fields, _, ok := datastoreElementForm(v, isDatastoreKeyStep(el.Steps))
		if !ok {
			continue
		}
		values := map[string]string{}
		for _, f := range fields {
			values[f.Name] = f.Default
		}
		parsed, err := parseDatastoreElementValue(values, isDatastoreKeyStep(el.Steps))
		if err != nil {
			t.Errorf("%v: the prefilled form does not read back: %v", el.Steps, err)
			continue
		}
		prop := proto.Clone(orig).(*datastorepb.Value)
		if err := setDatastoreElement(prop, "demo", "items", el.Steps, parsed); err != nil {
			t.Errorf("%v: %v", el.Steps, err)
			continue
		}
		if !proto.Equal(prop, orig) {
			t.Errorf("Edit value on %v saved unchanged rewrote the property:\n got %v\nwant %v", el.Steps, prop, orig)
		}
	}

	// A change.
	when := time.Date(2027, 1, 2, 3, 4, 5, 6000, time.UTC)
	prop := proto.Clone(orig).(*datastorepb.Value)
	if err := setDatastoreElement(prop, "demo", "items", []any{6, "when"}, when); err != nil {
		t.Fatal(err)
	}
	if err := setDatastoreElement(prop, "demo", "items", []any{0}, datastore.NameKey("Order", "id=8", nil)); err != nil {
		t.Fatal(err)
	}
	gotWhen := prop.GetArrayValue().GetValues()[6].GetEntityValue().GetProperties()["when"]
	if !gotWhen.GetTimestampValue().AsTime().Equal(when) || !gotWhen.GetExcludeFromIndexes() {
		t.Errorf("items[6].when is %v, want %v, still excluded from indexes", gotWhen, when)
	}
	gotKey := prop.GetArrayValue().GetValues()[0].GetKeyValue()
	if gotKey.GetPath()[0].GetName() != "id=8" || gotKey.GetPartitionId().GetProjectId() != "demo" {
		t.Errorf("items[0] is %v, want Order/name=id=8 in demo", gotKey)
	}
	back := proto.Clone(prop).(*datastorepb.Value)
	back.GetArrayValue().Values[0] = orig.GetArrayValue().GetValues()[0]
	back.GetArrayValue().GetValues()[6].GetEntityValue().Properties["when"] = orig.GetArrayValue().GetValues()[6].GetEntityValue().GetProperties()["when"]
	if !proto.Equal(back, orig) {
		t.Errorf("Edit value changed more than the two values: %v", prop)
	}

	for _, tc := range []struct {
		steps  []any
		values map[string]string
		want   string
	}{
		{[]any{3}, map[string]string{"type": "key", "value": "Order/id=7"}, "another-project"},
		{[]any{4}, map[string]string{"type": "string", "value": "x"}, "blob"},
		{[]any{0}, map[string]string{"type": "array", "value": "[1]"}, "Edit property"},
		{[]any{6, "__key__"}, map[string]string{"type": "string", "value": "x"}, "is a key"},
		{[]any{9}, map[string]string{"type": "string", "value": "x"}, "no value at items[9]"},
		{[]any{6, "missing"}, map[string]string{"type": "string", "value": "x"}, "no value at items[6].missing"},
	} {
		prop := proto.Clone(orig).(*datastorepb.Value)
		parsed, err := parseDatastoreElementValue(tc.values, isDatastoreKeyStep(tc.steps))
		if err == nil {
			err = setDatastoreElement(prop, "demo", "items", tc.steps, parsed)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %v: err %v, want one naming %q", tc.steps, tc.values, err, tc.want)
		}
		if !proto.Equal(prop, orig) {
			t.Errorf("%v: a refused Edit value changed the property", tc.steps)
		}
	}
}

// TestDatastoreEmbeddedEntityKeyKeepsItsPartition (#904). An embedded
// entity's own key in another project or database is shown with where it is,
// as a key value is, where the Go client's Entity.Key dropped it.
func TestDatastoreEmbeddedEntityKeyKeepsItsPartition(t *testing.T) {
	v := &datastorepb.Value{ValueType: &datastorepb.Value_EntityValue{EntityValue: &datastorepb.Entity{
		Key: &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: "other", DatabaseId: "db2"},
			Path: []*datastorepb.Key_PathElement{{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}}},
		Properties: map[string]*datastorepb.Value{"n": {ValueType: &datastorepb.Value_IntegerValue{IntegerValue: 1}}}}}}
	got := datastoreValueGo(v, "demo")
	if typ := datastoreType(got); typ != "entity" {
		t.Errorf("type %q, want entity", typ)
	}
	if s, want := renderDatastoreValue(got, false), `{"__key__": key(Order/id=7 (project other, database db2)), "n": 1}`; s != want {
		t.Errorf("rendered %q, want %q", s, want)
	}
	arr := &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{Values: []*datastorepb.Value{v}}}}
	if s, want := renderDatastoreValue(datastoreValueGo(arr, "demo"), false), `[{"__key__": key(Order/id=7 (project other, database db2)), "n": 1}]`; s != want {
		t.Errorf("in an array, rendered %q, want %q", s, want)
	}
	if _, _, ok := formatDatastoreValue(got, false); ok {
		t.Error("an embedded entity with another project's key is offered Edit property")
	}
	for _, el := range datastoreElements(v) {
		if isDatastoreKeyStep(el.Steps) {
			if _, _, ok := datastoreElementForm(datastoreValueGo(el.Value, "demo"), true); ok {
				t.Error("another project's embedded entity key is offered Edit value")
			}
		}
	}
}

// TestDatastoreQueriesAreWrittenAsTheClientWritesThem (#904). The kind
// listing and the query builder run through RunQuery; the cursor is written
// and read as the client's Cursor is, so a link issued before still pages
// on, and a filter and an order are the client's.
func TestDatastoreQueriesAreWrittenAsTheClientWritesThem(t *testing.T) {
	for _, raw := range [][]byte{{0}, {1, 2, 3}, []byte("a cursor of some length \xff\xfe")} {
		s := encodeDatastoreCursor(raw)
		c, err := datastore.DecodeCursor(s)
		if err != nil || c.String() != s {
			t.Errorf("the client reads %q as %v, %v", s, c, err)
		}
		back, err := decodeDatastoreCursor(s)
		if err != nil || string(back) != string(raw) {
			t.Errorf("%q reads back as %v, %v; want %v", s, back, err, raw)
		}
	}
	if _, err := decodeDatastoreCursor("not base64!"); err == nil {
		t.Error("a cursor that is not base64 reads")
	}

	f, err := datastoreFilterPB(`"odd name"`, ">=", 3.0)
	if err != nil {
		t.Fatal(err)
	}
	pf := f.GetPropertyFilter()
	if pf.GetProperty().GetName() != "odd name" || pf.GetOp() != datastorepb.PropertyFilter_GREATER_THAN_OR_EQUAL || pf.GetValue().GetDoubleValue() != 3 {
		t.Errorf("filter %v", pf)
	}
	for _, bad := range []struct{ field, op string }{{"", "="}, {"n", "!"}, {`"unterminated`, "="}} {
		if _, err := datastoreFilterPB(bad.field, bad.op, 1.0); err == nil {
			t.Errorf("filter %q %q is accepted", bad.field, bad.op)
		}
	}
	o, err := datastoreOrderPB("-n")
	if err != nil || o.GetProperty().GetName() != "n" || o.GetDirection() != datastorepb.PropertyOrder_DESCENDING {
		t.Errorf("order -n = %v, %v", o, err)
	}
	if _, err := datastoreOrderPB("+n"); err == nil {
		t.Error("order +n is accepted, which the client refuses")
	}
}
