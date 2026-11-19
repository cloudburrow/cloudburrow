package main

import (
	"testing"

	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
)

// TestDatastoreIndexEntriesFollowGooglesStorageSizes (#1846): a composite
// index's entries and size, by Google's storage size rules.
func TestDatastoreIndexEntriesFollowGooglesStorageSizes(t *testing.T) {
	str := func(s string) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_StringValue{StringValue: s}}
	}
	num := func(n int64) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_IntegerValue{IntegerValue: n}}
	}
	key := &datastorepb.Key{Path: []*datastorepb.Key_PathElement{{Kind: "Task", IdType: &datastorepb.Key_PathElement_Name{Name: "t1"}}}}
	// Key: "Task" 5 + "t1" 3 + 16 = 24.
	if got := datastoreKeySize(key); got != 24 {
		t.Errorf("key size = %d, want 24", got)
	}
	ix := &dsadminpb.Index{Kind: "Task", Properties: []*dsadminpb.Index_IndexedProperty{{Name: "done"}, {Name: "tags"}}}
	e := &datastorepb.Entity{Key: key, Properties: map[string]*datastorepb.Value{
		"done": num(1),
		"tags": {ValueType: &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{Values: []*datastorepb.Value{str("a"), str("bc")}}}},
	}}
	// Two entries (one per tag), each 24 + 32 + 8 + the tag (2 or 3).
	if n, size := datastoreIndexEntries(ix, e); n != 2 || size != 2*(24+32+8)+2+3 {
		t.Errorf("entries, size = %d, %d; want 2, %d", n, size, 2*(24+32+8)+2+3)
	}
	excluded := num(1)
	excluded.ExcludeFromIndexes = true
	e.Properties["done"] = excluded
	if n, _ := datastoreIndexEntries(ix, e); n != 0 {
		t.Errorf("an unindexed property's entity has %d entries, want 0", n)
	}
	delete(e.Properties, "done")
	if n, _ := datastoreIndexEntries(ix, e); n != 0 {
		t.Errorf("a missing property's entity has %d entries, want 0", n)
	}
	child := &datastorepb.Entity{Key: &datastorepb.Key{Path: []*datastorepb.Key_PathElement{
		{Kind: "List", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}, {Kind: "Task", IdType: &datastorepb.Key_PathElement_Name{Name: "t1"}}}},
		Properties: map[string]*datastorepb.Value{"done": num(0), "tags": str("x")}}
	ix.Ancestor = dsadminpb.Index_ALL_ANCESTORS
	if n, _ := datastoreIndexEntries(ix, child); n != 2 {
		t.Errorf("an ancestor index's entries of a child = %d, want 2", n)
	}
}
