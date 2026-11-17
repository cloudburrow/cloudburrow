//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFirestoreEnterpriseOnlyValues (#1703): Google's "Supported data
// types" (https://docs.cloud.google.com/firestore/docs/concepts/data-types)
// marks 32-bit integer, BSON timestamp, bytes with subtype, Decimal128, max
// key, min key, ObjectId and regular expression "Supported only in
// Enterprise edition", and says a Standard database's array cannot hold an
// array. The emulator runs as an Enterprise one for Firestore with MongoDB
// compatibility, so the front keeps a Standard database to Standard's
// values: through the official Firestore client, `(default)` refuses each
// INVALID_ARGUMENT, as the Standard emulator does, also as an arrayUnion
// operand, while an Enterprise database stores each and reads it back.
func TestFirestoreEnterpriseOnlyValues(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	std := firestoreClient(t, h)
	enterpriseDatabase(t, h, "edition-ent")
	ent, err := firestore.NewClientWithDatabase(ctx, h.Project(), "edition-ent")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = ent.Close() })

	values := []struct {
		name string
		v    any
		msg  string
	}{
		{"int32", firestore.BSONInt32(7), "Value type INT is not allowed"},
		{"objectId", firestore.BSONObjectID("507f1f77bcf86cd799439011"), "Value type OBJECT_ID is not allowed"},
		{"decimal128", firestore.BSONDecimal128("1.25"), "Value type DECIMAL128 is not allowed"},
		{"bsonTimestamp", firestore.BSONTimestamp{Seconds: 1700000000, Increment: 3}, "Value type REQUEST_TIMESTAMP is not allowed"},
		{"regex", firestore.BSONRegex{Pattern: "^a", Options: "i"}, "Value type REGEX is not allowed"},
		{"minKey", firestore.BSONMinKey{}, "Value type MIN_KEY is not allowed"},
		{"maxKey", firestore.BSONMaxKey{}, "Value type MAX_KEY is not allowed"},
		{"binary", firestore.BSONBinary{Subtype: 4, Data: []byte{1, 2, 3}}, "Value type BYTES_WITH_SUBTYPE is not allowed"},
		{"inArray", []any{firestore.BSONInt32(7)}, "Value type INT is not allowed"},
		{"inMap", map[string]any{"a": firestore.BSONDecimal128("2")}, "Value type DECIMAL128 is not allowed"},
		{"nestedArray", []any{[]any{int64(1), int64(2)}}, "Nested arrays are not allowed"},
	}
	for _, c := range values {
		_, err := std.Collection("edition").Doc(c.name).Set(ctx, map[string]any{"v": c.v})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), c.msg) {
			t.Errorf("Standard: Set(%s) = %v, want INVALID_ARGUMENT %q", c.name, err, c.msg)
		}

		ref := ent.Collection("edition").Doc(c.name)
		if _, err := ref.Set(ctx, map[string]any{"v": c.v}); err != nil {
			t.Errorf("Enterprise: Set(%s) = %v, want it stored", c.name, err)
			continue
		}
		snap, err := ref.Get(ctx)
		if err != nil {
			t.Errorf("Enterprise: Get(%s) = %v", c.name, err)
			continue
		}
		if got := snap.Data()["v"]; !reflect.DeepEqual(got, c.v) {
			t.Errorf("Enterprise: %s read back as %#v, want %#v", c.name, got, c.v)
		}
	}

	// A transform's operand is a value too.
	for _, op := range []any{firestore.BSONInt32(7), []any{int64(1)}} {
		if _, err := std.Collection("edition").Doc("union").Set(ctx, map[string]any{"v": firestore.ArrayUnion(op)}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("Standard: arrayUnion(%#v) = %v, want INVALID_ARGUMENT", op, err)
		}
		if _, err := ent.Collection("edition").Doc("union").Set(ctx, map[string]any{"v": firestore.ArrayUnion(op)}); err != nil {
			t.Errorf("Enterprise: arrayUnion(%#v) = %v, want it applied", op, err)
		}
	}

	// Standard's own values still pass: a 64-bit integer, bytes, an
	// array of maps holding arrays, and a map named like no BSON type.
	if _, err := std.Collection("edition").Doc("plain").Set(ctx, map[string]any{
		"i": int64(7), "b": []byte{1, 2}, "a": []any{map[string]any{"x": []any{int64(1)}}}, "m": map[string]any{"int": int64(1)},
	}); err != nil {
		t.Errorf("Standard: Set(Standard values) = %v", err)
	}
}
