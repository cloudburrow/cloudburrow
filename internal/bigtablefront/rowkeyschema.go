package bigtablefront

// Row key schemas (#1900). A table's row_key_schema is a Type.Struct of the
// fields its row keys are made of, each with its type and encoding, and
// the encoding that joins them (Singleton, DelimitedBytes or
// OrderedCodeBytes). The emulator (bttest) drops it, so the front keeps it:
// CreateTable stores it, GetTable returns it in the views of the table's
// schema, ListTables in SCHEMA_VIEW and FULL, and UpdateTable sets it on a
// table without one or, with ignore_warnings, clears it, as Google's
// reference allows ("Update from an empty schema to a new schema. Remove
// the existing schema. This operation requires setting the
// ignore_warnings flag to true ... Any other row key schema update
// operation ... is currently unsupported", google/bigtable/admin/v2/
// table.proto). A backup, a snapshot and a deleted table keep their
// table's schema, and the table restored from them gets it back.
//
// GoogleSQL reads the key by it (query.go): each field is a column of
// the table, in the schema's order, before the column families, and _key
// becomes a pseudocolumn, still read as the raw bytes when it is named
// but left out of SELECT * ("Structured row key queries"); a query
// that reads a field of a row whose key does not decode fails.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const rowKeySchemaPath = "row_key_schema"

// rowKeySchemas is the row key schema of each table that has one.
type rowKeySchemas struct {
	mu     sync.Mutex
	byName map[string]*btapb.Type_Struct
}

func newRowKeySchemas() *rowKeySchemas {
	return &rowKeySchemas{byName: map[string]*btapb.Type_Struct{}}
}

// get is a copy of the schema of table, or nil.
func (s *rowKeySchemas) get(table string) *btapb.Type_Struct {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rks, ok := s.byName[table]; ok {
		return proto.Clone(rks).(*btapb.Type_Struct)
	}
	return nil
}

// set keeps rks as the schema of table; nil forgets it.
func (s *rowKeySchemas) set(table string, rks *btapb.Type_Struct) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rks == nil {
		delete(s.byName, table)
		return
	}
	s.byName[table] = proto.Clone(rks).(*btapb.Type_Struct)
}

// drop forgets the schema of name and of every table under it.
func (s *rowKeySchemas) drop(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.byName {
		if t == name || strings.HasPrefix(t, name+"/") {
			delete(s.byName, t)
		}
	}
}

func (s *rowKeySchemas) reset(m map[string]*btapb.Type_Struct) {
	if m == nil {
		m = map[string]*btapb.Type_Struct{}
	}
	s.mu.Lock()
	s.byName = m
	s.mu.Unlock()
}

// --- validation ---

func badSchema(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, "Invalid row key schema: "+format, args...)
}

// checkRowKeySchema checks rks against the reference's invariants: every
// field has a non-empty name, unique, neither _key nor _timestamp nor the
// name of one of the table's column families; every field's type is a
// scalar type with an encoding, a row key's (Bytes, String, Int64 or
// Timestamp; Array, Map, Aggregate and Struct are not allowed); the struct
// has an encoding, Singleton only with one field and DelimitedBytes with
// a delimiter.
func checkRowKeySchema(rks *btapb.Type_Struct, families map[string]*btapb.ColumnFamily) error {
	if len(rks.GetFields()) == 0 {
		return badSchema("the schema must have at least one field")
	}
	seen := map[string]bool{}
	for i, f := range rks.GetFields() {
		name := f.GetFieldName()
		switch {
		case name == "":
			return badSchema("field %d has no name", i)
		case name == "_key" || name == "_timestamp":
			return badSchema("field name %q is reserved", name)
		case seen[name]:
			return badSchema("field name %q is used more than once", name)
		}
		if _, ok := families[name]; ok {
			return badSchema("field name %q conflicts with the column family %q", name, name)
		}
		seen[name] = true
		if err := checkKeyFieldType(f.GetType()); err != nil {
			return badSchema("field %q: %v", name, err)
		}
	}
	switch e := rks.GetEncoding().GetEncoding().(type) {
	case nil:
		return badSchema("the struct has no encoding: set singleton, delimited_bytes or ordered_code_bytes")
	case *btapb.Type_Struct_Encoding_Singleton_:
		if n := len(rks.GetFields()); n != 1 {
			return badSchema("the singleton encoding takes exactly one field, not %d", n)
		}
	case *btapb.Type_Struct_Encoding_DelimitedBytes_:
		if len(e.DelimitedBytes.GetDelimiter()) == 0 {
			return badSchema("the delimited_bytes encoding requires a non-empty delimiter")
		}
	case *btapb.Type_Struct_Encoding_OrderedCodeBytes_:
	default:
		return badSchema("the struct encoding %T is not supported", e)
	}
	return nil
}

func checkInt64Encoding(e *btapb.Type_Int64_Encoding) error {
	switch e.GetEncoding().(type) {
	case *btapb.Type_Int64_Encoding_BigEndianBytes_, *btapb.Type_Int64_Encoding_OrderedCodeBytes_:
		return nil
	case nil:
		return errors.New("the INT64 type has no encoding")
	}
	return fmt.Errorf("the INT64 encoding %T is not supported", e.GetEncoding())
}

func checkKeyFieldType(t *btapb.Type) error {
	switch k := t.GetKind().(type) {
	case *btapb.Type_BytesType:
		if k.BytesType.GetEncoding().GetRaw() == nil {
			return errors.New("the BYTES type has no encoding")
		}
	case *btapb.Type_StringType:
		switch k.StringType.GetEncoding().GetEncoding().(type) {
		case *btapb.Type_String_Encoding_Utf8Bytes_, *btapb.Type_String_Encoding_Utf8Raw_:
		default:
			return errors.New("the STRING type has no encoding")
		}
	case *btapb.Type_Int64Type:
		return checkInt64Encoding(k.Int64Type.GetEncoding())
	case *btapb.Type_TimestampType:
		u := k.TimestampType.GetEncoding().GetUnixMicrosInt64()
		if u == nil {
			return errors.New("the TIMESTAMP type has no encoding")
		}
		return checkInt64Encoding(u)
	case nil:
		return errors.New("the field has no type")
	case *btapb.Type_ArrayType, *btapb.Type_MapType, *btapb.Type_AggregateType, *btapb.Type_StructType:
		return fmt.Errorf("type %s is not allowed in a row key schema: only scalar types are", keyTypeName(t))
	default:
		return fmt.Errorf("type %s has no encoding a row key can use", keyTypeName(t))
	}
	return nil
}

func keyTypeName(t *btapb.Type) string {
	switch t.GetKind().(type) {
	case *btapb.Type_BytesType:
		return "BYTES"
	case *btapb.Type_StringType:
		return "STRING"
	case *btapb.Type_Int64Type:
		return "INT64"
	case *btapb.Type_Float32Type:
		return "FLOAT32"
	case *btapb.Type_Float64Type:
		return "FLOAT64"
	case *btapb.Type_BoolType:
		return "BOOL"
	case *btapb.Type_TimestampType:
		return "TIMESTAMP"
	case *btapb.Type_DateType:
		return "DATE"
	case *btapb.Type_ArrayType:
		return "ARRAY"
	case *btapb.Type_MapType:
		return "MAP"
	case *btapb.Type_AggregateType:
		return "AGGREGATE"
	case *btapb.Type_StructType:
		return "STRUCT"
	case *btapb.Type_ProtoType:
		return "PROTO"
	case *btapb.Type_EnumType:
		return "ENUM"
	}
	return fmt.Sprintf("%T", t.GetKind())
}

// --- the table admin calls ---

// modifyColumnFamilies refuses a ModifyColumnFamilies that would create a
// family named as a field of the table's row key schema, which the
// schema's field names must not collide with.
func (f *Front) modifyColumnFamilies(ctx context.Context, req *btapb.ModifyColumnFamiliesRequest) (proto.Message, error) {
	if rks := f.keys.get(req.GetName()); rks != nil {
		fields := map[string]bool{}
		for _, fl := range rks.GetFields() {
			fields[fl.GetFieldName()] = true
		}
		for _, m := range req.GetModifications() {
			if m.GetCreate() != nil && fields[m.GetId()] {
				return nil, status.Errorf(codes.InvalidArgument, "Column family name %q conflicts with a field of the table's row key schema", m.GetId())
			}
		}
	}
	return btapb.NewBigtableTableAdminClient(f.upstream).ModifyColumnFamilies(ctx, req)
}

// listTables adds what the front keeps of each table to the emulator's
// list, in the views of the table's schema.
func (f *Front) listTables(ctx context.Context, req *btapb.ListTablesRequest) (proto.Message, error) {
	resp, err := btapb.NewBigtableTableAdminClient(f.upstream).ListTables(ctx, req)
	if err != nil {
		return nil, err
	}
	if v := req.GetView(); v == btapb.Table_SCHEMA_VIEW || v == btapb.Table_FULL {
		for _, t := range resp.GetTables() {
			f.withConfig(t, v)
		}
	}
	return resp, nil
}

// updateRowKeySchema is the schema UpdateTable leaves table with, or the
// error it answers: one is set on a table without one, and cleared, with
// ignore_warnings, from a table with one.
func (f *Front) updateRowKeySchema(ctx context.Context, req *btapb.UpdateTableRequest) (*btapb.Type_Struct, error) {
	name := req.GetTable().GetName()
	cur := f.keys.get(name)
	next := req.GetTable().GetRowKeySchema()
	if next == nil {
		if cur != nil && !req.GetIgnoreWarnings() {
			return nil, status.Error(codes.InvalidArgument, "Removing the row key schema might be a backward incompatible change: set ignore_warnings to remove it")
		}
		return nil, nil
	}
	t, err := btapb.NewBigtableTableAdminClient(f.upstream).GetTable(ctx, &btapb.GetTableRequest{Name: name, View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		return nil, err
	}
	if err := checkRowKeySchema(next, t.GetColumnFamilies()); err != nil {
		return nil, err
	}
	if cur != nil && !proto.Equal(cur, next) {
		return nil, status.Errorf(codes.InvalidArgument, "Table %s already has a row key schema, which cannot be updated: remove it (with ignore_warnings) and set the new one", name)
	}
	return next, nil
}

// --- decoding a row key ---

// keyDecodeErr is the value of a key field of a row whose key does not
// decode; reading it fails the query.
type keyDecodeErr struct{ err error }

func (e keyDecodeErr) Error() string { return e.err.Error() }

// keyColumns is the SQL columns of rks's fields.
func keyColumns(rks *btapb.Type_Struct) []column {
	var cols []column
	for _, f := range rks.GetFields() {
		cols = append(cols, column{f.GetFieldName(), keySQLType(f.GetType())})
	}
	return cols
}

func keySQLType(t *btapb.Type) *btpb.Type {
	switch t.GetKind().(type) {
	case *btapb.Type_StringType:
		return stringType
	case *btapb.Type_Int64Type:
		return int64Type
	case *btapb.Type_TimestampType:
		return timestampType
	}
	return bytesType
}

// keyValues is the fields of key decoded by rks, each a keyDecodeErr if
// the key does not decode.
func keyValues(table string, rks *btapb.Type_Struct, key []byte) []any {
	vals, err := decodeRowKey(rks, key)
	if err == nil {
		return vals
	}
	e := keyDecodeErr{fmt.Errorf("The row key %q of table %s does not conform to the table's row key schema: %v", key, table, err)}
	out := make([]any, len(rks.GetFields()))
	for i := range out {
		out[i] = e
	}
	return out
}

// decodeRowKey splits key into the fields of rks, by the struct's
// encoding, and decodes each by its own.
func decodeRowKey(rks *btapb.Type_Struct, key []byte) ([]any, error) {
	fields := rks.GetFields()
	var parts [][]byte
	switch e := rks.GetEncoding().GetEncoding().(type) {
	case *btapb.Type_Struct_Encoding_Singleton_:
		parts = [][]byte{key}
	case *btapb.Type_Struct_Encoding_DelimitedBytes_:
		// The last field takes the rest of the key, delimiters and all:
		// a field's encoding, an INT64's big-endian bytes say, may hold
		// the delimiter.
		parts = bytes.SplitN(key, e.DelimitedBytes.GetDelimiter(), len(fields))
		if len(parts) != len(fields) {
			return nil, fmt.Errorf("it has %d delimited parts, and the schema %d fields", len(parts), len(fields))
		}
	case *btapb.Type_Struct_Encoding_OrderedCodeBytes_:
		var err error
		if parts, err = splitOrderedCode(key, len(fields)); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("the schema has no encoding")
	}
	vals := make([]any, len(fields))
	for i, f := range fields {
		v, err := decodeKeyField(f.GetType(), parts[i])
		if err != nil {
			return nil, fmt.Errorf("field %s: %v", f.GetFieldName(), err)
		}
		vals[i] = v
	}
	return vals, nil
}

// splitOrderedCode splits a struct's OrderedCodeBytes encoding into its n
// fields: fields are joined by {0x00, 0x01}, a null byte in a field is
// {0x00, 0xFF}, an empty field that is not omitted {0x00, 0x00}, and the
// trailing empty fields are omitted.
func splitOrderedCode(key []byte, n int) ([][]byte, error) {
	parts := [][]byte{{}}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if b != 0x00 {
			parts[len(parts)-1] = append(parts[len(parts)-1], b)
			continue
		}
		if i+1 >= len(key) {
			return nil, errors.New("it ends in an unescaped null byte")
		}
		i++
		switch key[i] {
		case 0x01:
			parts = append(parts, []byte{})
		case 0xff:
			parts[len(parts)-1] = append(parts[len(parts)-1], 0x00)
		case 0x00:
			if len(parts[len(parts)-1]) != 0 {
				return nil, errors.New("an empty-field marker inside a field")
			}
		default:
			return nil, fmt.Errorf("the null byte is followed by 0x%02x, not 0x00, 0x01 or 0xff", key[i])
		}
	}
	if len(parts) > n {
		return nil, fmt.Errorf("it has %d fields, and the schema %d", len(parts), n)
	}
	for len(parts) < n {
		parts = append(parts, []byte{})
	}
	return parts, nil
}

func decodeKeyField(t *btapb.Type, b []byte) (any, error) {
	switch k := t.GetKind().(type) {
	case *btapb.Type_BytesType:
		return append([]byte{}, b...), nil
	case *btapb.Type_StringType:
		if !utf8.Valid(b) {
			return nil, errors.New("not valid UTF-8")
		}
		return string(b), nil
	case *btapb.Type_Int64Type:
		return decodeKeyInt64(k.Int64Type.GetEncoding(), b)
	case *btapb.Type_TimestampType:
		us, err := decodeKeyInt64(k.TimestampType.GetEncoding().GetUnixMicrosInt64(), b)
		if err != nil {
			return nil, err
		}
		return time.UnixMicro(us).UTC(), nil
	}
	return nil, fmt.Errorf("type %s cannot be decoded", keyTypeName(t))
}

func decodeKeyInt64(e *btapb.Type_Int64_Encoding, b []byte) (int64, error) {
	switch e.GetEncoding().(type) {
	case *btapb.Type_Int64_Encoding_BigEndianBytes_:
		// The reference's own example decodes the four bytes
		// "\x00\x13\x00\xf3" as 1245427 (table.proto): a shorter value
		// is read as the low bytes of the eight.
		if len(b) == 0 || len(b) > 8 {
			return 0, fmt.Errorf("an INT64 of big-endian bytes is 8 bytes, not %d", len(b))
		}
		var buf [8]byte
		copy(buf[8-len(b):], b)
		return int64(binary.BigEndian.Uint64(buf[:])), nil
	case *btapb.Type_Int64_Encoding_OrderedCodeBytes_:
		v, n, err := parseOrderedInt64(b)
		if err != nil {
			return 0, err
		}
		if n != len(b) {
			return 0, fmt.Errorf("%d bytes after the INT64", len(b)-n)
		}
		return v, nil
	}
	return 0, errors.New("the INT64 has no encoding")
}

// parseOrderedInt64 reads an OrderedCode signed integer from the start of
// s, returning it and its length.
func parseOrderedInt64(s []byte) (int64, int, error) {
	bad := errors.New("not an INT64 in ordered code")
	if len(s) == 0 {
		return 0, 0, bad
	}
	var dir byte
	c := s[0]
	if c >= 0x40 && c < 0xc0 {
		return int64(int8(c ^ 0x80)), 1, nil
	}
	neg := c&0x80 == 0
	if neg {
		c, dir = ^c, 0xff
	}
	start, n := 0, 0
	if c == 0xff {
		if len(s) == 1 {
			return 0, 0, bad
		}
		start = 1
		c = s[1] ^ dir
		if c > 0xc0 {
			return 0, 0, bad
		}
		n = 7
	}
	for mask := byte(0x80); c&mask != 0; mask >>= 1 {
		c &^= mask
		n++
	}
	if len(s)-start < n {
		return 0, 0, bad
	}
	x := int64(c)
	for i := 1; i < n; i++ {
		x = x<<8 | int64(s[start+i]^dir)
	}
	if neg {
		x = ^x
	}
	return x, start + n, nil
}
