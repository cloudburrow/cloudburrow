package main

import (
	"strings"
	"testing"

	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// TestDatastoreValueActionsRefuseAChangedProperty (#923). A value action
// carries the digest of the property its page was drawn from. Two reads of
// the same property agree; a value inserted before the addressed one, a
// value removed, or an index flag changed, gives another digest, and the
// action is then refused, naming the property and saying to reload. An
// action that carries no digest is refused too.
func TestDatastoreValueActionsRefuseAChangedProperty(t *testing.T) {
	drawn := nestedFixture()
	values := map[string]string{datastoreExpectedField: datastorePropertyDigest(drawn)}
	if err := checkDatastoreExpected(proto.Clone(drawn).(*datastorepb.Value), "items", values); err != nil {
		t.Fatalf("the property as drawn is refused: %v", err)
	}
	inserted := proto.Clone(drawn).(*datastorepb.Value)
	inserted.GetArrayValue().Values = append([]*datastorepb.Value{{ValueType: &datastorepb.Value_StringValue{StringValue: "new"}}},
		inserted.GetArrayValue().GetValues()...)
	removed := proto.Clone(drawn).(*datastorepb.Value)
	removed.GetArrayValue().Values = removed.GetArrayValue().GetValues()[1:]
	flagged := proto.Clone(drawn).(*datastorepb.Value)
	flagged.GetArrayValue().GetValues()[6].GetEntityValue().GetProperties()["when"].ExcludeFromIndexes = false
	for name, changed := range map[string]*datastorepb.Value{"inserted": inserted, "removed": removed, "flag": flagged} {
		err := checkDatastoreExpected(changed, "items", values)
		if err == nil || !strings.Contains(err.Error(), `property "items" changed since the page was loaded`) ||
			!strings.Contains(err.Error(), "reload") {
			t.Errorf("%s: err %v, want the change refused, saying to reload", name, err)
		}
	}
	for _, v := range []map[string]string{nil, {datastoreExpectedField: " "}} {
		if err := checkDatastoreExpected(drawn, "items", v); err == nil || !strings.Contains(err.Error(), "reload") {
			t.Errorf("an action carrying %v: err %v, want it refused", v, err)
		}
	}
}

// TestDatastoreExcludeFromIndexesSetsEveryValue (#924). Exclude from
// indexes on an array or embedded entity sets or clears the flag on every
// value inside it, at any depth, an embedded entity's own included, and
// changes nothing else: with the flags put back, the property is
// proto-equal to what it was. It is offered, prefilled with whether every
// value is excluded, on an array or entity holding a value with a flag.
// Indexing is refused while a string or blob inside is over 1,500 bytes, an
// embedded entity in an array is refused excluded before an indexed
// neighbour (Datastore would move it), and a refused change leaves the
// property as it was.
func TestDatastoreExcludeFromIndexesSetsEveryValue(t *testing.T) {
	orig := nestedFixture()
	flags := func(v *datastorepb.Value) map[string]bool {
		out := map[string]bool{}
		for _, el := range datastoreFlagged(v) {
			out[datastoreElementSegment(el.Steps)] = el.Value.GetExcludeFromIndexes()
		}
		return out
	}
	if n := len(flags(orig)); n != 9 {
		// Seven values, the embedded entity's when, and the key in odd name;
		// the embedded entity's key and the array odd name have no flag.
		t.Fatalf("the fixture has %d flagged values, want 9: %v", n, flags(orig))
	}
	for _, excluded := range []bool{true, false} {
		prop := proto.Clone(orig).(*datastorepb.Value)
		if err := setDatastoreExcluded(prop, "items", nil, excluded); err != nil {
			t.Fatalf("Exclude from indexes %v: %v", excluded, err)
		}
		for seg, got := range flags(prop) {
			if got != excluded {
				t.Errorf("after Exclude from indexes %v, %s is excluded %v", excluded, seg, got)
			}
		}
		if a, ok := datastoreExcludeAction(prop); !ok || a.Fields[0].Default != map[bool]string{true: "true", false: "false"}[excluded] {
			t.Errorf("after Exclude from indexes %v the action is %+v (%v), want it prefilled so", excluded, a, ok)
		}
		// Only the flags changed.
		back := proto.Clone(prop).(*datastorepb.Value)
		want := flags(orig)
		for _, el := range datastoreFlagged(back) {
			el.Value.ExcludeFromIndexes = want[datastoreElementSegment(el.Steps)]
		}
		if !proto.Equal(back, orig) {
			t.Errorf("Exclude from indexes %v changed more than the flags: %v", excluded, prop)
		}
	}
	if a, _ := datastoreExcludeAction(orig); a.Fields[0].Default != "false" {
		t.Errorf("on a mixed array Exclude from indexes is prefilled %v, want unchecked", a.Fields[0].Default)
	}
	empty := &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{}}}
	if _, ok := datastoreExcludeAction(empty); ok {
		t.Error("an empty array is offered Exclude from indexes")
	}

	// Inside: the embedded entity items[6], the last value, and the array
	// odd name inside it.
	prop := proto.Clone(orig).(*datastorepb.Value)
	if err := setDatastoreExcluded(prop, "items", []any{6}, true); err != nil {
		t.Fatal(err)
	}
	line := prop.GetArrayValue().GetValues()[6]
	if !line.GetExcludeFromIndexes() || !line.GetEntityValue().GetProperties()["odd name"].GetArrayValue().GetValues()[0].GetExcludeFromIndexes() ||
		prop.GetArrayValue().GetValues()[0].GetExcludeFromIndexes() {
		t.Errorf("Exclude from indexes on items[6] set %v", prop)
	}
	prop = proto.Clone(orig).(*datastorepb.Value)
	if err := setDatastoreExcluded(prop, "items", []any{6, "odd name"}, true); err != nil {
		t.Fatal(err)
	}
	if line := prop.GetArrayValue().GetValues()[6]; line.GetExcludeFromIndexes() ||
		!line.GetEntityValue().GetProperties()["odd name"].GetArrayValue().GetValues()[0].GetExcludeFromIndexes() {
		t.Errorf("Exclude from indexes on items[6][\"odd name\"] set %v", line)
	}

	// Refused, leaving the property as it was.
	long := proto.Clone(orig).(*datastorepb.Value)
	long.GetArrayValue().Values = append(long.GetArrayValue().Values,
		&datastorepb.Value{ValueType: &datastorepb.Value_StringValue{StringValue: strings.Repeat("x", 1501)}, ExcludeFromIndexes: true})
	first := proto.Clone(orig).(*datastorepb.Value)
	first.GetArrayValue().Values = append([]*datastorepb.Value{first.GetArrayValue().GetValues()[6]}, first.GetArrayValue().GetValues()[:6]...)
	for _, tc := range []struct {
		prop     *datastorepb.Value
		steps    []any
		excluded bool
		want     string
	}{
		{long, nil, false, "items[7] is 1501 bytes"},
		{first, []any{0}, true, "reorder"},
		{orig, []any{0}, true, "not an array or an embedded entity"},
		{orig, []any{9}, true, "no value at items[9]"},
	} {
		before := proto.Clone(tc.prop).(*datastorepb.Value)
		err := setDatastoreExcluded(tc.prop, "items", tc.steps, tc.excluded)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v excluded %v: err %v, want one naming %q", tc.steps, tc.excluded, err, tc.want)
		}
		if !proto.Equal(tc.prop, before) {
			t.Errorf("%v: a refused change changed the property", tc.steps)
		}
	}
}
