package bigqueryfront

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The rows of an AppendRows request, read by the front (#1102;
// storagewrite.go says why the front reads them).
//
// A request's rows are protocol buffer messages of its writer schema, a
// DescriptorProto (proto2, self-contained: the Go client's
// adapt.NormalizeDescriptor nests every message and enum it uses in it).
// Each row is decoded with that descriptor and written as the row of
// tabledata.insertAll's JSON the column's value is: the conversions are
// BigQuery's table of the protocol buffer types each column type takes
// (https://cloud.google.com/bigquery/docs/supported-data-types, "Protocol
// Buffer data types"):
//
//	BOOL        bool, int32, int64, uint32, uint64, google.protobuf.BoolValue
//	BYTES       bytes, string, google.protobuf.BytesValue
//	DATE        int32 (days since 1970-01-01), int64, string
//	DATETIME    string, int64 (CivilTimeEncoder's packed form)
//	TIME        string, int64 (CivilTimeEncoder's packed form)
//	FLOAT       double, float, google.protobuf.DoubleValue, FloatValue
//	GEOGRAPHY   string
//	INTEGER     int32, int64, uint32, enum, google.protobuf.Int32Value,
//	            Int64Value, UInt32Value
//	JSON        string
//	NUMERIC,    int32, int64, uint32, uint64, double, float, string, bytes
//	BIGNUMERIC  (BigDecimalByteStringEncoder's form), google.protobuf.BytesValue
//	STRING      string, enum, google.protobuf.StringValue
//	TIMESTAMP   int64 (microseconds since the epoch), int32, uint32,
//	            google.protobuf.Timestamp
//	INTERVAL    string (google.protobuf.Duration: not implemented here)
//	RANGE       a message of start and end: not implemented here
//	REPEATED    a repeated field; RECORD a message
//
// The sint, sfixed and fixed kinds are the int and uint ones on the wire.
// A field the table has no column for, or of a kind its column does not
// take, is INVALID_ARGUMENT, as BigQuery refuses the writer schema.
//
// Presence is proto2's, as the Go client's own tests of the backend
// measure (managedwriter/validation_test.go): a field with no value is
// NULL, unless the descriptor gives it a default, which is written (the
// client's NormalizeDescriptor gives a proto3 field without presence its
// zero value as default, so proto3's zero values are written, not NULL); a
// repeated field with no value is an empty array.

// rowConv writes the rows of one writer schema for one table's columns.
type rowConv struct {
	md   protoreflect.MessageDescriptor
	cols []colConv
}

// colConv is one field of the writer schema and the column it writes.
type colConv struct {
	fd  protoreflect.FieldDescriptor
	col field
	// typ is the column's legacy type name (INTEGER, FLOAT, RECORD...).
	typ string
	// sub writes a RECORD column's message.
	sub *rowConv
	// wrapped is the value field of a google.protobuf wrapper message.
	wrapped protoreflect.FieldDescriptor
	// timestamp: the field is a google.protobuf.Timestamp message.
	timestamp bool
}

// writerDescriptor builds the message descriptor of a writer schema.
func writerDescriptor(dp *descriptorpb.DescriptorProto) (protoreflect.MessageDescriptor, error) {
	if dp == nil || dp.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "proto_rows.writer_schema.proto_descriptor is required, with a name")
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("cloudburrow_writer_schema.proto"),
		Syntax:      proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{dp},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: proto_rows.writer_schema: %v", err)
	}
	md := fd.Messages().ByName(protoreflect.Name(dp.GetName()))
	if md == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: no message %q", dp.GetName())
	}
	return md, nil
}

// newRowConv matches md's fields to fields, the table's columns (or a
// RECORD's); path names the RECORD in errors.
func newRowConv(md protoreflect.MessageDescriptor, fields []field, path string) (*rowConv, error) {
	rc := &rowConv{md: md}
	seen := map[string]bool{}
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		name := columnNameOf(fd)
		col, ok := fieldNamed(fields, name)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument,
				"Input schema has more fields than BigQuery schema, extra fields: '%s%s'", path, name)
		}
		if seen[strings.ToLower(col.Name)] {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: two fields write the column '%s%s'", path, col.Name)
		}
		seen[strings.ToLower(col.Name)] = true
		cc := colConv{fd: fd, col: col, typ: legacyType(col.Type)}
		where := path + col.Name
		if fd.IsMap() {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: the map field '%s' writes no BigQuery column type", where)
		}
		if fd.IsList() != strings.EqualFold(col.Mode, "REPEATED") {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: field '%s' is %s in the proto schema and %s in the table",
				where, cardinality(fd.IsList()), modeOf(col))
		}
		switch {
		case cc.typ == "RECORD":
			if fd.Kind() != protoreflect.MessageKind {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: field '%s' is a RECORD in the table and a %s in the proto schema",
					where, fd.Kind())
			}
			sub, err := newRowConv(fd.Message(), col.Fields, where+".")
			if err != nil {
				return nil, err
			}
			cc.sub = sub
		case cc.typ == "RANGE":
			return nil, status.Errorf(codes.Unimplemented, "Not implemented here: the Storage Write API's RANGE column '%s'. "+
				"Nothing was written.", where)
		case fd.Kind() == protoreflect.MessageKind:
			name := wellKnownName(fd.Message())
			switch {
			case name == "Timestamp" && cc.typ == "TIMESTAMP":
				cc.timestamp = true
			case name == "Duration" && cc.typ == "INTERVAL":
				return nil, status.Errorf(codes.Unimplemented, "Not implemented here: a google.protobuf.Duration written to the "+
					"INTERVAL column '%s' by the Storage Write API. Nothing was written. Write the interval as a string.", where)
			case wrapperTakes[cc.typ][name]:
				v := fd.Message().Fields().ByName("value")
				if v == nil {
					return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: the wrapper of '%s' has no value field", where)
				}
				cc.wrapped = v
			default:
				return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: field '%s' is a message in the proto schema "+
					"and %s in the table", where, cc.typ)
			}
		default:
			if !kindTakes(cc.typ, fd.Kind()) {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid proto schema: field '%s' of proto type %s cannot be written to "+
					"the column's type %s", where, fd.Kind(), cc.typ)
			}
		}
		rc.cols = append(rc.cols, cc)
	}
	return rc, nil
}

func cardinality(repeated bool) string {
	if repeated {
		return "repeated"
	}
	return "not repeated"
}

// columnNameOf is the column a field writes: its name, or the
// bigquery.storage.v1.column_name annotation BigQuery reads for a column
// whose name is not a proto field name.
func columnNameOf(fd protoreflect.FieldDescriptor) string {
	if opts, ok := fd.Options().(*descriptorpb.FieldOptions); ok && opts != nil && proto.HasExtension(opts, storagepb.E_ColumnName) {
		if n, _ := proto.GetExtension(opts, storagepb.E_ColumnName).(string); n != "" {
			return n
		}
	}
	return string(fd.Name())
}

// wellKnownName is the name of a google.protobuf message md is, as it is or
// as the Go client's NormalizeDescriptor nests it (google_protobuf_X), or "".
func wellKnownName(md protoreflect.MessageDescriptor) string {
	if n := string(md.FullName()); strings.HasPrefix(n, "google.protobuf.") {
		return strings.TrimPrefix(n, "google.protobuf.")
	}
	if n := string(md.Name()); strings.HasPrefix(n, "google_protobuf_") {
		return strings.TrimPrefix(n, "google_protobuf_")
	}
	return ""
}

// wrapperTakes are the google.protobuf wrappers each column type takes.
var wrapperTakes = map[string]map[string]bool{
	"BOOLEAN":    {"BoolValue": true},
	"BYTES":      {"BytesValue": true},
	"FLOAT":      {"DoubleValue": true, "FloatValue": true},
	"INTEGER":    {"Int32Value": true, "Int64Value": true, "UInt32Value": true},
	"NUMERIC":    {"BytesValue": true},
	"BIGNUMERIC": {"BytesValue": true},
	"STRING":     {"StringValue": true},
}

// Kinds by their wire type's width and sign.
func isInt32Kind(k protoreflect.Kind) bool {
	return k == protoreflect.Int32Kind || k == protoreflect.Sint32Kind || k == protoreflect.Sfixed32Kind
}
func isInt64Kind(k protoreflect.Kind) bool {
	return k == protoreflect.Int64Kind || k == protoreflect.Sint64Kind || k == protoreflect.Sfixed64Kind
}
func isUint32Kind(k protoreflect.Kind) bool {
	return k == protoreflect.Uint32Kind || k == protoreflect.Fixed32Kind
}
func isUint64Kind(k protoreflect.Kind) bool {
	return k == protoreflect.Uint64Kind || k == protoreflect.Fixed64Kind
}

// kindTakes reports whether a column of typ takes a scalar of kind k.
func kindTakes(typ string, k protoreflect.Kind) bool {
	i32, i64, u32, u64 := isInt32Kind(k), isInt64Kind(k), isUint32Kind(k), isUint64Kind(k)
	str := k == protoreflect.StringKind
	switch typ {
	case "BOOLEAN":
		return k == protoreflect.BoolKind || i32 || i64 || u32 || u64
	case "BYTES":
		return k == protoreflect.BytesKind || str
	case "DATE":
		return i32 || i64 || str
	case "DATETIME", "TIME":
		return i64 || str
	case "FLOAT":
		return k == protoreflect.DoubleKind || k == protoreflect.FloatKind
	case "GEOGRAPHY", "JSON", "INTERVAL":
		return str
	case "INTEGER":
		return i32 || i64 || u32 || k == protoreflect.EnumKind
	case "NUMERIC", "BIGNUMERIC":
		return i32 || i64 || u32 || u64 || k == protoreflect.DoubleKind || k == protoreflect.FloatKind || str || k == protoreflect.BytesKind
	case "STRING":
		return str || k == protoreflect.EnumKind
	case "TIMESTAMP":
		return i32 || i64 || u32
	}
	return false
}

// row decodes one serialized row and writes it as insertAll's JSON object.
func (rc *rowConv) row(b []byte) (map[string]any, error) {
	msg := dynamicpb.NewMessage(rc.md)
	if err := proto.Unmarshal(b, msg); err != nil {
		return nil, fmt.Errorf("the row does not decode as the writer schema's message: %v", err)
	}
	return rc.message(msg)
}

func (rc *rowConv) message(msg protoreflect.Message) (map[string]any, error) {
	out := make(map[string]any, len(rc.cols))
	for _, cc := range rc.cols {
		v, err := cc.field(msg)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", cc.col.Name, err)
		}
		out[cc.col.Name] = v
	}
	return out, nil
}

// field is the column's value in msg: NULL when the field has no value
// and no default, an array for a repeated field (above).
func (cc colConv) field(msg protoreflect.Message) (any, error) {
	fd := cc.fd
	if fd.IsList() {
		list := msg.Get(fd).List()
		out := make([]any, 0, list.Len())
		for i := 0; i < list.Len(); i++ {
			v, err := cc.value(list.Get(i))
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			if v == nil {
				return nil, fmt.Errorf("element %d is NULL, which an array cannot hold", i)
			}
			out = append(out, v)
		}
		return out, nil
	}
	if !msg.Has(fd) && (fd.Kind() == protoreflect.MessageKind || !fd.HasDefault()) {
		return nil, nil
	}
	return cc.value(msg.Get(fd))
}

// value writes one value of the field (an element of a repeated one).
func (cc colConv) value(v protoreflect.Value) (any, error) {
	fd := cc.fd
	switch {
	case cc.sub != nil:
		return cc.sub.message(v.Message())
	case cc.timestamp:
		m := v.Message()
		secs := m.Get(m.Descriptor().Fields().ByName("seconds")).Int()
		nanos := m.Get(m.Descriptor().Fields().ByName("nanos")).Int()
		if secs > math.MaxInt64/1_000_000-1 || secs < math.MinInt64/1_000_000+1 {
			return nil, fmt.Errorf("the TIMESTAMP is out of range")
		}
		return timestampText(secs*1_000_000 + nanos/1000)
	case cc.wrapped != nil:
		m := v.Message()
		return scalarValue(cc.typ, cc.wrapped, m.Get(cc.wrapped))
	}
	return scalarValue(cc.typ, fd, v)
}

// scalarValue writes a scalar v of fd's kind to a column of typ, as
// insertAll's JSON gives it.
func scalarValue(typ string, fd protoreflect.FieldDescriptor, v protoreflect.Value) (any, error) {
	k := fd.Kind()
	var (
		isInt, isUint bool
		i             int64
		u             uint64
	)
	switch {
	case isInt32Kind(k) || isInt64Kind(k):
		isInt, i = true, v.Int()
	case isUint32Kind(k) || isUint64Kind(k):
		isUint, u = true, v.Uint()
	case k == protoreflect.EnumKind:
		isInt, i = true, int64(v.Enum())
	}
	intText := func() string {
		if isUint {
			return strconv.FormatUint(u, 10)
		}
		return strconv.FormatInt(i, 10)
	}
	switch typ {
	case "BOOLEAN":
		if k == protoreflect.BoolKind {
			return v.Bool(), nil
		}
		return i != 0 || u != 0, nil
	case "INTEGER":
		return intText(), nil
	case "STRING":
		if k == protoreflect.EnumKind {
			if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
				return string(ev.Name()), nil
			}
			return intText(), nil
		}
		return v.String(), nil
	case "BYTES":
		if k == protoreflect.StringKind {
			return base64.StdEncoding.EncodeToString([]byte(v.String())), nil
		}
		return base64.StdEncoding.EncodeToString(v.Bytes()), nil
	case "FLOAT":
		f := v.Float()
		switch {
		case math.IsNaN(f):
			return "NaN", nil
		case math.IsInf(f, 1):
			return "Infinity", nil
		case math.IsInf(f, -1):
			return "-Infinity", nil
		}
		bits := 64
		if k == protoreflect.FloatKind {
			bits = 32
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, bits)), nil
	case "NUMERIC", "BIGNUMERIC":
		switch {
		case isInt || isUint:
			return intText(), nil
		case k == protoreflect.DoubleKind || k == protoreflect.FloatKind:
			f := v.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, fmt.Errorf("%v is not a %s value", f, typ)
			}
			bits := 64
			if k == protoreflect.FloatKind {
				bits = 32
			}
			return strconv.FormatFloat(f, 'f', -1, bits), nil
		case k == protoreflect.StringKind:
			return v.String(), nil
		}
		scale := 9
		if typ == "BIGNUMERIC" {
			scale = 38
		}
		return decimalBytesText(v.Bytes(), scale), nil
	case "DATE":
		if k == protoreflect.StringKind {
			return v.String(), nil
		}
		// "The valid range is -719162 (0001-01-01) to 2932896
		// (9999-12-31)" (supported-data-types).
		if i < -719162 || i > 2932896 {
			return nil, fmt.Errorf("%d days since 1970-01-01 is out of DATE's range, -719162 to 2932896", i)
		}
		return time.Unix(i*86400, 0).UTC().Format("2006-01-02"), nil
	case "TIME":
		if k == protoreflect.StringKind {
			return v.String(), nil
		}
		return packedTimeText(i)
	case "DATETIME":
		if k == protoreflect.StringKind {
			return v.String(), nil
		}
		return packedDateTimeText(i)
	case "TIMESTAMP":
		if isUint {
			i = int64(u)
		}
		return timestampText(i)
	case "GEOGRAPHY", "JSON", "INTERVAL":
		return v.String(), nil
	}
	return nil, fmt.Errorf("no conversion to %s", typ)
}

// timestampText writes us, microseconds since the epoch, as a TIMESTAMP
// insertAll takes, refusing one outside TIMESTAMP's range (years 1 to 9999).
func timestampText(us int64) (string, error) {
	t := time.UnixMicro(us).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return "", fmt.Errorf("%d microseconds since the epoch is out of TIMESTAMP's range", us)
	}
	return t.Format("2006-01-02 15:04:05.000000") + " UTC", nil
}

// CivilTimeEncoder's packed forms (the Java client's
// com.google.cloud.bigquery.storage.v1.CivilTimeEncoder, which the
// supported-data-types page names): TIME is hour<<32 | minute<<26 |
// second<<20 | microsecond; DATETIME puts year<<46 | month<<42 | day<<37
// above it.
const (
	microBits = 20
	secShift  = 20
	minShift  = 26
	hourShift = 32
	dayShift  = 37
	monShift  = 42
	yearShift = 46
)

func packedClock(p int64) (h, m, s, us int64, ok bool) {
	us = p & (1<<microBits - 1)
	s = p >> secShift & 0x3f
	m = p >> minShift & 0x3f
	h = p >> hourShift & 0x1f
	return h, m, s, us, h <= 23 && m <= 59 && s <= 59 && us <= 999999
}

func packedTimeText(p int64) (string, error) {
	h, m, s, us, ok := packedClock(p)
	if !ok || p>>dayShift != 0 {
		return "", fmt.Errorf("%d is not a TIME in CivilTimeEncoder's packed form", p)
	}
	return fmt.Sprintf("%02d:%02d:%02d.%06d", h, m, s, us), nil
}

func packedDateTimeText(p int64) (string, error) {
	h, m, s, us, ok := packedClock(p)
	d := p >> dayShift & 0x1f
	mo := p >> monShift & 0xf
	y := p >> yearShift
	if !ok || y < 1 || y > 9999 || !realDate(strconv.FormatInt(y, 10), strconv.FormatInt(mo, 10), strconv.FormatInt(d, 10)) {
		return "", fmt.Errorf("%d is not a DATETIME in CivilTimeEncoder's packed form", p)
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%06d", y, mo, d, h, m, s, us), nil
}

// decimalBytesText reads BigDecimalByteStringEncoder's form of a NUMERIC
// (scale 9) or BIGNUMERIC (scale 38): the unscaled value, two's
// complement, little-endian. It is written exactly, in decimal.
func decimalBytesText(b []byte, scale int) string {
	be := make([]byte, len(b))
	for i, c := range b {
		be[len(b)-1-i] = c
	}
	n := new(big.Int).SetBytes(be)
	if len(be) > 0 && be[0]&0x80 != 0 {
		n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(8*len(be))))
	}
	r := new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	text := r.FloatString(scale)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	if text == "-0" {
		text = "0"
	}
	return text
}
