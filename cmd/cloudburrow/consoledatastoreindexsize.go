package main

import (
	"context"

	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
)

// The Indexes page's Entries and Size (#1846). Google's Indexes page shows
// each index's status, "the amount of storage space used by the index and
// the number of entries in each index"
// (https://cloud.google.com/datastore/docs/console/managing-datastore). The
// emulator keeps no statistics, so they are computed from the kind's
// entities, in every namespace, by Google's own rules
// (https://cloud.google.com/datastore/docs/concepts/storage-size): an
// entity has one entry for each combination of the indexed properties'
// values (each value of an array one), none when a property is missing or
// excluded from indexes, and one per ancestor for an index of all
// ancestors; "the size of a composite index entry is the sum of: the key
// size of the indexed entity, the sum of the indexed property values, 32
// bytes".

// datastoreStringSize is a string's size: its UTF-8 bytes, plus 1.
func datastoreStringSize(s string) int64 { return int64(len(s)) + 1 }

// datastoreKeySize is a key's size: its namespace's string size (if not
// the default), its path's (integer IDs 8 bytes each), and 16 bytes.
func datastoreKeySize(k *datastorepb.Key) int64 {
	n := int64(16)
	if ns := k.GetPartitionId().GetNamespaceId(); ns != "" {
		n += datastoreStringSize(ns)
	}
	for _, e := range k.GetPath() {
		n += datastoreStringSize(e.GetKind())
		if e.GetName() != "" {
			n += datastoreStringSize(e.GetName())
		} else {
			n += 8
		}
	}
	return n
}

// datastoreValueSize is a value's size by Google's table.
func datastoreValueSize(v *datastorepb.Value) int64 {
	switch x := v.GetValueType().(type) {
	case *datastorepb.Value_ArrayValue:
		var n int64
		for _, e := range x.ArrayValue.GetValues() {
			n += datastoreValueSize(e)
		}
		return n
	case *datastorepb.Value_BlobValue:
		return int64(len(x.BlobValue))
	case *datastorepb.Value_BooleanValue, *datastorepb.Value_NullValue:
		return 1
	case *datastorepb.Value_DoubleValue, *datastorepb.Value_IntegerValue, *datastorepb.Value_TimestampValue:
		return 8
	case *datastorepb.Value_EntityValue:
		return datastoreEntitySize(x.EntityValue)
	case *datastorepb.Value_GeoPointValue:
		return 16
	case *datastorepb.Value_KeyValue:
		return datastoreKeySize(x.KeyValue)
	case *datastorepb.Value_StringValue:
		return datastoreStringSize(x.StringValue)
	}
	return 0
}

// datastoreEntitySize is an entity's size: its key's, its properties'
// (each name's string size and value's), and 32 bytes.
func datastoreEntitySize(e *datastorepb.Entity) int64 {
	n := int64(32)
	if e.GetKey() != nil {
		n += datastoreKeySize(e.GetKey())
	}
	for name, v := range e.GetProperties() {
		n += datastoreStringSize(name) + datastoreValueSize(v)
	}
	return n
}

// datastoreIndexedValues are the values of a property an index holds: an
// array's indexed elements, or the value if it is indexed.
func datastoreIndexedValues(v *datastorepb.Value) []*datastorepb.Value {
	if v == nil {
		return nil
	}
	if a, ok := v.GetValueType().(*datastorepb.Value_ArrayValue); ok {
		var out []*datastorepb.Value
		for _, e := range a.ArrayValue.GetValues() {
			if !e.GetExcludeFromIndexes() {
				out = append(out, e)
			}
		}
		return out
	}
	if v.GetExcludeFromIndexes() {
		return nil
	}
	return []*datastorepb.Value{v}
}

// datastoreIndexEntries are the entries an entity has in a composite
// index, and their size.
func datastoreIndexEntries(ix *dsadminpb.Index, e *datastorepb.Entity) (entries, size int64) {
	combos := int64(1)
	var valueSums []int64
	var counts []int64
	for _, pr := range ix.GetProperties() {
		vals := datastoreIndexedValues(e.GetProperties()[pr.GetName()])
		if len(vals) == 0 {
			return 0, 0
		}
		var sum int64
		for _, v := range vals {
			sum += datastoreValueSize(v)
		}
		combos *= int64(len(vals))
		valueSums = append(valueSums, sum)
		counts = append(counts, int64(len(vals)))
	}
	// Every combination: the key and 32 bytes each, and each property's
	// value sizes once for every combination of the others.
	size = combos * (datastoreKeySize(e.GetKey()) + 32)
	for i, sum := range valueSums {
		size += sum * (combos / counts[i])
	}
	if ix.GetAncestor() == dsadminpb.Index_ALL_ANCESTORS {
		depth := int64(len(e.GetKey().GetPath()))
		return combos * depth, size * depth
	}
	return combos, size
}

// datastoreIndexStats are a composite index's entries and size over the
// kind's entities in every namespace given; ok is false when they could not
// all be read.
func datastoreIndexStats(ctx context.Context, c datastorepb.DatastoreClient, project string, namespaces []string, ix *dsadminpb.Index) (entries, size int64, ok bool) {
	for _, ns := range namespaces {
		var start []byte
		for {
			res, err := runEntityQuery(ctx, c, project, ns,
				&datastorepb.Query{Kind: []*datastorepb.KindExpression{{Name: ix.GetKind()}}}, 1000, start)
			if err != nil {
				return 0, 0, false
			}
			for _, e := range res.Entities {
				n, s := datastoreIndexEntries(ix, e)
				entries, size = entries+n, size+s
			}
			if len(res.Entities) < 1000 || len(res.Cursor) == 0 {
				break
			}
			start = res.Cursor
		}
	}
	return entries, size, true
}
