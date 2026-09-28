package main

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fsString(s string) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: s}}
}

func fsInt(n int64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: n}}
}

func fsArray(vs ...*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{ArrayValue: &firestorepb.ArrayValue{Values: vs}}}
}

func fsMap(kv map[string]*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: kv}}}
}

// firestoreNestedFixture is a map field holding every type Firestore stores,
// as the emulator returns them: an array with a map inside it, a key that is
// not a plain name, bytes, a timestamp with microseconds, a geopoint, a
// reference in this project and one in another.
func firestoreNestedFixture() *firestorepb.Value {
	return fsMap(map[string]*firestorepb.Value{
		"items": fsArray(
			fsString("a"),
			&firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{DoubleValue: 2.0}},
			fsMap(map[string]*firestorepb.Value{
				"when": {ValueType: &firestorepb.Value_TimestampValue{TimestampValue: timestamppb.New(
					time.Date(2026, 9, 28, 10, 11, 12, 345678000, time.UTC))}},
				"odd name": fsInt(7),
			}),
		),
		"blob":  {ValueType: &firestorepb.Value_BytesValue{BytesValue: []byte{0, 1, 2, 0xfe, 0xff}}},
		"where": {ValueType: &firestorepb.Value_GeoPointValue{GeoPointValue: &latlng.LatLng{Latitude: 51.5, Longitude: -0.12}}},
		"owner": {ValueType: &firestorepb.Value_ReferenceValue{
			ReferenceValue: "projects/demo/databases/(default)/documents/users/alice"}},
		"away": {ValueType: &firestorepb.Value_ReferenceValue{
			ReferenceValue: "projects/other/databases/(default)/documents/users/bob"}},
		"none": {ValueType: &firestorepb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}},
		"flag": {ValueType: &firestorepb.Value_BooleanValue{BooleanValue: true}},
	})
}

// TestFirestoreElementsAreListedAndAddressed (#995). A map field's Elements
// tab lists every value inside it, at any depth, a map or array before the
// values inside it and a map's keys in order, each labelled as Datastore's
// are and addressed by a segment that reads back to its steps. Every value
// the form can hold is offered Edit value; a map or array, and a reference
// into another project, are not.
func TestFirestoreElementsAreListedAndAddressed(t *testing.T) {
	els := firestoreElements(firestoreNestedFixture())
	var labels []string
	for _, el := range els {
		label := firestoreElementLabel("meta", el.Steps)
		labels = append(labels, label)
		steps, err := firestoreElementSteps(datastoreElementSegment(el.Steps))
		if err != nil || datastoreElementSegment(steps) != datastoreElementSegment(el.Steps) {
			t.Errorf("%s: its segment reads back as %v, %v", label, steps, err)
		}
		_, _, why, ok := formatFirestorePB("demo", el.Value)
		wantOK := !isFirestoreContainer(el.Value) && label != "meta.away"
		if ok != wantOK {
			t.Errorf("%s offers Edit value = %v (%s), want %v", label, ok, why, wantOK)
		}
	}
	want := "meta.away meta.blob meta.flag meta.items meta.items[0] meta.items[1] meta.items[2] " +
		`meta.items[2]["odd name"] meta.items[2].when meta.none meta.owner meta.where`
	if got := strings.Join(labels, " "); got != want {
		t.Errorf("the Elements tab lists\n%s\nwant\n%s", got, want)
	}
	if _, _, why, _ := formatFirestorePB("demo", firestoreNestedFixture().GetMapValue().GetFields()["away"]); !strings.Contains(why, "projects/other") {
		t.Errorf("a foreign reference's note %q does not name it", why)
	}
}

// TestFirestoreEditValueSavedUnchangedWritesTheSameValue (#995). Every value
// Edit value offers is prefilled with its type and a text that reads back to
// exactly the stored value — bytes as base64, a double keeping its point, a
// timestamp its microseconds — so saving the form unchanged leaves the field
// proto-equal to what was read.
func TestFirestoreEditValueSavedUnchangedWritesTheSameValue(t *testing.T) {
	orig := firestoreNestedFixture()
	for _, el := range firestoreElements(orig) {
		typ, raw, _, ok := formatFirestorePB("demo", el.Value)
		if !ok {
			continue
		}
		prop := proto.Clone(orig).(*firestorepb.Value)
		at, err := setFirestoreElement(prop, "demo", "meta", el.Steps, map[string]string{"type": typ, "value": raw})
		if err != nil {
			t.Errorf("%s: saving %s %q unchanged: %v", firestoreElementLabel("meta", el.Steps), typ, raw, err)
			continue
		}
		if !proto.Equal(prop, orig) {
			t.Errorf("%s: saving %s %q unchanged changed the field to %v", firestoreElementLabel("meta", el.Steps), typ, raw, prop)
		}
		if datastoreElementSegment(at) != datastoreElementSegment(el.Steps) {
			t.Errorf("%s: the change is reported at %v", firestoreElementLabel("meta", el.Steps), at)
		}
	}
	// A map, an array and another project's reference are refused, and so
	// is a map or array typed into Edit value.
	for _, c := range []struct {
		steps  []any
		values map[string]string
		want   string
	}{
		{[]any{"items"}, map[string]string{"type": "string", "value": "x"}, "value by value"},
		{[]any{"away"}, map[string]string{"type": "reference", "value": "users/bob"}, "another project"},
		{[]any{"flag"}, map[string]string{"type": "map", "value": "{}"}, "Edit value writes one value"},
		{[]any{"blob"}, map[string]string{"type": "bytes", "value": "not base64!"}, "base64"},
		{[]any{"gone"}, map[string]string{"type": "string", "value": "x"}, "holds no value"},
	} {
		prop := proto.Clone(orig).(*firestorepb.Value)
		if _, err := setFirestoreElement(prop, "demo", "meta", c.steps, c.values); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Edit value at %v with %v: %v, want an error containing %q", c.steps, c.values, err, c.want)
		}
		if !proto.Equal(prop, orig) {
			t.Errorf("a refused Edit value at %v changed the field", c.steps)
		}
	}
}

// TestFirestoreAddAndRemoveValueChangeOnlyThatValue (#995). Add value
// inserts into an array at an index or appends and adds a key to a map;
// Remove value removes an array's value, which closes up, and a map's key.
// Each changes that value only: undoing it by hand gives back the field
// proto-equal. Each reports the steps of what changed, which is what the
// write is masked to.
func TestFirestoreAddAndRemoveValueChangeOnlyThatValue(t *testing.T) {
	orig := firestoreNestedFixture()
	items := func(v *firestorepb.Value) []*firestorepb.Value {
		return v.GetMapValue().GetFields()["items"].GetArrayValue().GetValues()
	}
	for _, tc := range []struct {
		index string
		at    int
	}{{"0", 0}, {"2", 2}, {"", 3}, {" 3 ", 3}} {
		prop := proto.Clone(orig).(*firestorepb.Value)
		steps, err := addFirestoreElement(prop, "demo", "meta", []any{"items"}, map[string]string{"index": tc.index, "type": "bytes", "value": "AQID"})
		if err != nil {
			t.Fatalf("Add value at %q: %v", tc.index, err)
		}
		got := items(prop)
		if len(got) != 4 || !proto.Equal(got[tc.at], &firestorepb.Value{ValueType: &firestorepb.Value_BytesValue{BytesValue: []byte{1, 2, 3}}}) {
			t.Fatalf("Add value at %q put %v at %d", tc.index, got, tc.at)
		}
		if datastoreElementSegment(steps) != `["items"]` {
			t.Errorf("Add value into an array reports the change at %v, want the array", steps)
		}
		prop.GetMapValue().GetFields()["items"].GetArrayValue().Values = append(got[:tc.at:tc.at], got[tc.at+1:]...)
		if !proto.Equal(prop, orig) {
			t.Errorf("Add value at %q changed other values: %v", tc.index, prop)
		}
	}

	prop := proto.Clone(orig).(*firestorepb.Value)
	steps, err := addFirestoreElement(prop, "demo", "meta", []any{"items", 2}, map[string]string{"field": "qty", "type": "number", "value": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if q := items(prop)[2].GetMapValue().GetFields()["qty"]; q.GetIntegerValue() != 3 {
		t.Errorf("items[2].qty is %v, want the integer 3", q)
	}
	if datastoreElementSegment(steps) != `["items",2,"qty"]` {
		t.Errorf("Add value into a map reports the change at %v, want its new key", steps)
	}
	delete(items(prop)[2].GetMapValue().Fields, "qty")
	if !proto.Equal(prop, orig) {
		t.Errorf("Add value changed other values: %v", prop)
	}

	for _, c := range []struct {
		steps  []any
		values map[string]string
		want   string
	}{
		{[]any{"items"}, map[string]string{"type": "array", "value": "[1]"}, "an array cannot hold an array"},
		{[]any{"items"}, map[string]string{"index": "9", "type": "string", "value": "x"}, "is not one of 0 to 3"},
		{nil, map[string]string{"field": "flag", "type": "string", "value": "x"}, `already has a key "flag"`},
		{nil, map[string]string{"field": " ", "type": "string"}, "a key is required"},
		{[]any{"flag"}, map[string]string{"field": "x", "type": "string"}, "not a map or an array"},
	} {
		prop := proto.Clone(orig).(*firestorepb.Value)
		if _, err := addFirestoreElement(prop, "demo", "meta", c.steps, c.values); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Add value at %v with %v: %v, want an error containing %q", c.steps, c.values, err, c.want)
		}
		if !proto.Equal(prop, orig) {
			t.Errorf("a refused Add value at %v changed the field", c.steps)
		}
	}

	for _, tc := range []struct {
		steps, changed []any
		undo           func(p *firestorepb.Value)
	}{
		{[]any{"items", 1}, []any{"items"}, func(p *firestorepb.Value) {
			f := p.GetMapValue().GetFields()["items"].GetArrayValue()
			f.Values = append(f.Values[:1:1], append([]*firestorepb.Value{items(orig)[1]}, f.Values[1:]...)...)
		}},
		{[]any{"items", 2, "when"}, []any{"items"}, func(p *firestorepb.Value) {
			items(p)[2].GetMapValue().Fields["when"] = items(orig)[2].GetMapValue().GetFields()["when"]
		}},
		{[]any{"blob"}, []any{"blob"}, func(p *firestorepb.Value) {
			p.GetMapValue().Fields["blob"] = orig.GetMapValue().GetFields()["blob"]
		}},
	} {
		prop := proto.Clone(orig).(*firestorepb.Value)
		changed, err := removeFirestoreElement(prop, "meta", tc.steps)
		if err != nil {
			t.Fatalf("Remove value %v: %v", tc.steps, err)
		}
		if w := firestoreFieldWrite("doc", "meta", prop, changed, nil); w.GetUpdateMask().GetFieldPaths()[0] != firestoreFieldPath(append([]string{"meta"}, stringSteps(tc.changed)...)) {
			t.Errorf("Remove value %v is masked to %v", tc.steps, w.GetUpdateMask().GetFieldPaths())
		}
		tc.undo(prop)
		if !proto.Equal(prop, orig) {
			t.Errorf("Remove value %v changed more than that value: %v", tc.steps, prop)
		}
	}
}

func stringSteps(steps []any) []string {
	var out []string
	for _, s := range steps {
		if k, ok := s.(string); ok {
			out = append(out, k)
		}
	}
	return out
}

// TestFirestoreValueWritesAreMaskedToWhatChanged (#995). A value action's
// write names the field path of what changed as far as it runs through maps
// — Firestore's field paths address a map's key and not an array's index —
// quoting a key that is not a simple name, and carries the value at that path
// and nothing else of the document; a removed key is in the mask and not in
// the document, which is how Firestore deletes it. The write is
// preconditioned on the update time the document was read at.
func TestFirestoreValueWritesAreMaskedToWhatChanged(t *testing.T) {
	stamp := timestamppb.New(time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC))
	prop := firestoreNestedFixture()
	for _, c := range []struct {
		at   []any
		mask string
		// value is the path inside the written document's field, nil for none.
		value []string
	}{
		{[]any{"flag"}, "meta.flag", []string{"flag"}},
		{[]any{"items", 2, "when"}, "meta.items", []string{"items"}},
		{[]any{"items", 2, "odd name"}, "meta.items", []string{"items"}},
		{[]any{"gone"}, "meta.gone", nil},
	} {
		w := firestoreFieldWrite("projects/demo/databases/(default)/documents/c/d", "meta", prop, c.at, stamp)
		if got := w.GetUpdateMask().GetFieldPaths(); len(got) != 1 || got[0] != c.mask {
			t.Errorf("a change at %v is masked to %v, want %s", c.at, got, c.mask)
		}
		if !proto.Equal(w.GetCurrentDocument().GetUpdateTime(), stamp) {
			t.Errorf("a change at %v is not preconditioned on the read's update time", c.at)
		}
		fields := w.GetUpdate().GetFields()
		if c.value == nil {
			if len(fields) != 0 {
				t.Errorf("the removal at %v writes %v", c.at, fields)
			}
			continue
		}
		got := fields["meta"]
		want := prop
		for _, k := range c.value {
			if len(got.GetMapValue().GetFields()) != 1 {
				t.Errorf("the write at %v carries more than the changed path: %v", c.at, got)
			}
			got = got.GetMapValue().GetFields()[k]
			want = want.GetMapValue().GetFields()[k]
		}
		if !proto.Equal(got, want) {
			t.Errorf("the write at %v carries %v, want %v", c.at, got, want)
		}
	}
	for segs, want := range map[string]string{
		"a":                 "a",
		"a,b_2":             "a.b_2",
		"odd name":          "`odd name`",
		"a,2b,x`y,back\\sl": "a.`2b`.`x\\`y`.`back\\\\sl`",
	} {
		if got := firestoreFieldPath(strings.Split(segs, ",")); got != want {
			t.Errorf("field path %q = %s, want %s", segs, got, want)
		}
	}
}

// TestFirestoreValueActionsRefuseAChangedField (#995). Every value action
// carries the digest of the field as its page drew it, and one whose field
// has changed since, or that carries none, is refused.
func TestFirestoreValueActionsRefuseAChangedField(t *testing.T) {
	orig := firestoreNestedFixture()
	els := firestoreElements(orig)
	actions := firestoreElementActions("demo", els[len(els)-1], orig)
	values := map[string]string{}
	for _, f := range actions[0].Fields {
		values[f.Name] = f.Default
	}
	if err := checkFirestoreExpected(orig, "meta", values); err != nil {
		t.Errorf("an action on the field as drawn is refused: %v", err)
	}
	changed := proto.Clone(orig).(*firestorepb.Value)
	changed.GetMapValue().Fields["flag"] = fsString("no")
	if err := checkFirestoreExpected(changed, "meta", values); err == nil || !strings.Contains(err.Error(), "changed since the page was loaded") {
		t.Errorf("an action on a field changed since = %v", err)
	}
	if err := checkFirestoreExpected(orig, "meta", map[string]string{}); err == nil {
		t.Error("an action carrying no digest is accepted")
	}
	var ids []string
	for _, a := range actions {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "editvalue,removevalue" {
		t.Errorf("a geopoint's row offers %v", ids)
	}
	var container firestoreElement
	for _, el := range els {
		if isFirestoreContainer(el.Value) {
			container = el
			break
		}
	}
	ids = nil
	for _, a := range firestoreElementActions("demo", container, orig) {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "addvalue,removevalue" {
		t.Errorf("an array's row offers %v", ids)
	}
}

// TestFirestoreBytesRoundTripAsBase64 (#995). A bytes value, top level or
// inside a map, is written and prefilled as base64, standard encoding with
// padding, and what is not base64 is refused with the reason.
func TestFirestoreBytesRoundTripAsBase64(t *testing.T) {
	for _, raw := range []string{"", "AQID", "AAEC/v8=", "AA EC\n/v8="} {
		v, err := parseFirestoreValue(nil, "demo", "bytes", raw)
		if err != nil {
			t.Errorf("bytes %q: %v", raw, err)
			continue
		}
		typ, back, ok := formatFirestoreValue(v)
		if !ok || typ != "bytes" || back != strings.Join(strings.Fields(raw), "") {
			t.Errorf("bytes %q come back as %s %q (%v)", raw, typ, back, ok)
		}
	}
	for _, raw := range []string{"AQI", "not base64", "AQID===="} {
		if _, err := parseFirestoreValue(nil, "demo", "bytes", raw); err == nil || !strings.Contains(err.Error(), "base64") {
			t.Errorf("bytes %q: %v, want refused as not base64", raw, err)
		}
	}
}
