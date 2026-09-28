package bigqueryfront

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
)

// Avro read sessions (#1098).
//
// The emulator writes a session's Avro schema as a record named after the
// table, in the namespace "<project>.<dataset>", with a RECORD column's
// record named after the column (types/avro.go, TableToAVRO,
// marshalAVROType). Avro names are [A-Za-z_][A-Za-z0-9_]*, and a
// namespace is such names joined by dots
// (https://avro.apache.org/docs/1.11.1/specification/#names), so a
// project ID with a hyphen, as every CloudBurrow project ID has, makes the
// schema invalid: the emulator's own ReadRows then failed to build its
// goavro codec (storageread.go), and a client's Avro library would refuse
// the schema too. Nor are two records of one full name allowed, which the
// emulator writes for two RECORD columns of one name at different depths.
//
// So the front gives the client the emulator's schema with every name
// made valid (bigQueryAvroSchema): each character that may not be in a
// name becomes '_', a name that would begin with a digit begins with '_',
// and a nested record whose name another record has already gets "_2",
// "_3"... Nothing else of the schema is changed: fields keep their names
// (BigQuery column names are valid Avro names), types, logical types and
// the order they are written in. The front writes the rows in that schema
// itself (avroRows, storagerows.go): the binary encoding of the
// specification, rows one after another, as BigQuery's AvroRows
// serialized_binary_rows are.

// bigQueryAvroSchema is the emulator's Avro schema with valid names
// (above), or the schema as it came when it is not a JSON record.
func bigQueryAvroSchema(schema string) string {
	var root map[string]any
	if json.Unmarshal([]byte(schema), &root) != nil || root["type"] != "record" {
		return schema
	}
	if ns, ok := root["namespace"].(string); ok {
		parts := strings.Split(ns, ".")
		for i, p := range parts {
			parts[i] = avroName(p)
		}
		root["namespace"] = strings.Join(parts, ".")
	}
	name, _ := root["name"].(string)
	name = avroName(name)
	root["name"] = name
	used := map[string]bool{name: true}
	renameRecords(root["fields"], used)
	b, err := json.Marshal(root)
	if err != nil {
		return schema
	}
	return string(b)
}

// renameRecords gives each record within v a valid name no other has.
func renameRecords(v any, used map[string]bool) {
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			renameRecords(x, used)
		}
	case map[string]any:
		if v["type"] == "record" {
			name, _ := v["name"].(string)
			base := avroName(name)
			name = base
			for i := 2; used[name]; i++ {
				name = base + "_" + strconv.Itoa(i)
			}
			used[name] = true
			v["name"] = name
			renameRecords(v["fields"], used)
			return
		}
		for _, k := range []string{"type", "items", "fields"} {
			renameRecords(v[k], used)
		}
	}
}

// avroName is s with each character an Avro name may not have made '_'.
func avroName(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// avroType is a type of an Avro schema, as far as the front writes it.
type avroType struct {
	// kind is a primitive type's name, "record", "array" or "union".
	kind    string
	logical string
	// scale is a decimal's.
	scale    int
	fields   []avroField
	items    *avroType
	branches []*avroType
}

type avroField struct {
	name string
	typ  *avroType
}

// parseAvroType reads the type v of a schema read from JSON.
func parseAvroType(v any) (*avroType, error) {
	switch v := v.(type) {
	case string:
		switch v {
		case "null", "boolean", "int", "long", "float", "double", "bytes", "string":
			return &avroType{kind: v}, nil
		}
		return nil, fmt.Errorf("unsupported Avro type %q", v)
	case []any:
		t := &avroType{kind: "union"}
		for _, b := range v {
			bt, err := parseAvroType(b)
			if err != nil {
				return nil, err
			}
			t.branches = append(t.branches, bt)
		}
		return t, nil
	case map[string]any:
		kind, _ := v["type"].(string)
		switch kind {
		case "record":
			fields, _ := v["fields"].([]any)
			t := &avroType{kind: "record"}
			for _, f := range fields {
				fm, _ := f.(map[string]any)
				name, _ := fm["name"].(string)
				ft, err := parseAvroType(fm["type"])
				if err != nil {
					return nil, fmt.Errorf("field %s: %w", name, err)
				}
				t.fields = append(t.fields, avroField{name: name, typ: ft})
			}
			return t, nil
		case "array":
			it, err := parseAvroType(v["items"])
			if err != nil {
				return nil, err
			}
			return &avroType{kind: "array", items: it}, nil
		}
		t, err := parseAvroType(v["type"])
		if err != nil {
			return nil, err
		}
		t.logical, _ = v["logicalType"].(string)
		if s, ok := v["scale"].(float64); ok {
			t.scale = int(s)
		}
		return t, nil
	}
	return nil, fmt.Errorf("unsupported Avro type %v", v)
}

// avroRows writes rows in the binary encoding of an Avro record schema.
type avroRows struct {
	schema string
	root   *avroType
}

func newAvroRows(schema string) (*avroRows, error) {
	var v any
	if err := json.Unmarshal([]byte(schema), &v); err != nil {
		return nil, err
	}
	root, err := parseAvroType(v)
	if err != nil {
		return nil, err
	}
	if root.kind != "record" {
		return nil, fmt.Errorf("the schema is a %s, not a record", root.kind)
	}
	return &avroRows{schema: schema, root: root}, nil
}

func (a *avroRows) schemaOf(resp *storagepb.ReadRowsResponse) {
	resp.Schema = &storagepb.ReadRowsResponse_AvroSchema{AvroSchema: &storagepb.AvroSchema{Schema: a.schema}}
}

func (a *avroRows) encode(rows []queryRow) (*storagepb.ReadRowsResponse, error) {
	var buf []byte
	for i, row := range rows {
		for j, f := range a.root.fields {
			var err error
			if buf, err = appendAvro(buf, f.typ, row.F[j].V); err != nil {
				return nil, fmt.Errorf("row %d, column %s: %w", i, f.name, err)
			}
		}
	}
	return &storagepb.ReadRowsResponse{Rows: &storagepb.ReadRowsResponse_AvroRows{AvroRows: &storagepb.AvroRows{
		SerializedBinaryRows: buf,
		RowCount:             int64(len(rows)),
	}}}, nil
}

// appendAvro appends v, a value as the REST API gives it (storagerows.go),
// encoded as t, to buf.
func appendAvro(buf []byte, t *avroType, v any) ([]byte, error) {
	switch t.kind {
	case "union":
		for i, b := range t.branches {
			if (v == nil) == (b.kind == "null") {
				buf = binary.AppendVarint(buf, int64(i))
				return appendAvro(buf, b, v)
			}
		}
		return nil, fmt.Errorf("no branch of the union for %v", v)
	case "null":
		if v != nil {
			return nil, fmt.Errorf("a value for null")
		}
		return buf, nil
	case "array":
		// BigQuery has no NULL array: an empty one reads as NULL.
		items, ok := v.([]any)
		if v != nil && !ok {
			return nil, fmt.Errorf("a REPEATED value of %T", v)
		}
		if len(items) > 0 {
			buf = binary.AppendVarint(buf, int64(len(items)))
			for _, it := range items {
				var err error
				if buf, err = appendAvro(buf, t.items, cellValue(it)); err != nil {
					return nil, err
				}
			}
		}
		return binary.AppendVarint(buf, 0), nil
	case "record":
		cells, err := recordCells(v)
		if err != nil {
			return nil, err
		}
		if len(cells) != len(t.fields) {
			return nil, fmt.Errorf("a RECORD of %d fields, want %d", len(cells), len(t.fields))
		}
		for i, f := range t.fields {
			if buf, err = appendAvro(buf, f.typ, cellValue(cells[i])); err != nil {
				return nil, fmt.Errorf("%s: %w", f.name, err)
			}
		}
		return buf, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("a %s value of %T", t.kind, v)
	}
	switch t.kind {
	case "boolean":
		x, err := strconv.ParseBool(s)
		if err != nil {
			return nil, err
		}
		if x {
			return append(buf, 1), nil
		}
		return append(buf, 0), nil
	case "int":
		if t.logical == "date" {
			d, err := time.Parse("2006-01-02", s)
			if err != nil {
				return nil, err
			}
			return binary.AppendVarint(buf, d.Unix()/86400), nil
		}
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, err
		}
		return binary.AppendVarint(buf, n), nil
	case "long":
		switch t.logical {
		case "timestamp-micros":
			ts, err := parseTimestamp(s)
			if err != nil {
				return nil, err
			}
			return binary.AppendVarint(buf, ts.Unix()*1e6+int64(ts.Nanosecond()/1000)), nil
		case "time-micros":
			us, err := parseTimeOfDay(s)
			if err != nil {
				return nil, err
			}
			return binary.AppendVarint(buf, us), nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		return binary.AppendVarint(buf, n), nil
	case "double":
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(x)), nil
	case "float":
		x, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint32(buf, math.Float32bits(float32(x))), nil
	case "string":
		buf = binary.AppendVarint(buf, int64(len(s)))
		return append(buf, s...), nil
	case "bytes":
		var b []byte
		if t.logical == "decimal" {
			var err error
			if b, err = decimalBytes(s, t.scale); err != nil {
				return nil, err
			}
		} else {
			var err error
			if b, err = base64.StdEncoding.DecodeString(s); err != nil {
				return nil, err
			}
		}
		buf = binary.AppendVarint(buf, int64(len(b)))
		return append(buf, b...), nil
	}
	return nil, fmt.Errorf("unsupported Avro type %s", t.kind)
}

// decimalBytes is the Avro decimal of s at scale: its unscaled value as
// a big-endian two's-complement integer.
func decimalBytes(s string, scale int) ([]byte, error) {
	n, err := unscaled(s, scale)
	if err != nil {
		return nil, err
	}
	if n.Sign() >= 0 {
		b := n.Bytes()
		if len(b) == 0 || b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return b, nil
	}
	// Negative: 2^(8k) + n in k bytes, the fewest that hold n.
	k := (n.BitLen() + 8) / 8
	m := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), uint(8*k)), n)
	b := m.Bytes()
	for len(b) < k {
		b = append([]byte{0xff}, b...)
	}
	return b, nil
}

// unscaled is the decimal text s times 10^scale, exactly (the digits
// past scale dropped; BigQuery's text has none).
func unscaled(s string, scale int) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("unreadable decimal %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
	return new(big.Int).Quo(r.Num(), r.Denom()), nil
}
