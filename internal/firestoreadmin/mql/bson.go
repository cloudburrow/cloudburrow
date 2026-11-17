package mql

// Firestore with MongoDB compatibility (#1703): BSON values and documents
// as Firestore values and documents, as Google documents the mapping
// ("BSON and Cloud Firestore type compatibility",
// https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/supported-data-types-drivers):
//
//   - Double, String, Object, Array, Binary of subtype 0, Boolean, Null,
//     64-bit Integer and Date map to the Firestore value of their kind;
//   - 32-bit Integer, Binary of another subtype, ObjectId, Timestamp,
//     Regular Expression, MinKey, MaxKey and Decimal128 are maps of one
//     key: __int__, __binary__ (the subtype byte first), __oid__,
//     __request_timestamp__, __regex__, __min__, __max__ and
//     __decimal128__;
//   - a Firestore GeoPoint, Vector or Reference reads as an object of one
//     key, __fs_geopoint__, __fs_vector__ or __fs_ref__, and such an
//     object is written back as that value.
//
// The Firestore emulator accepts the special maps only when it runs as an
// Enterprise edition emulator, which internal/components starts it as.
//
// A collection is a root collection of the database, and a document's
// _id is its document ID. Google does not document how a non-string _id
// is named, so CloudBurrow names it with a prefix of its own (docID).

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Google's documented limits (behavior differences, quotas).
const (
	mongoMaxDocSize  = 16 << 20
	mongoMaxDepth    = 20
	mongoMaxNameSize = 1500
	mongoMaxIDSize   = 1500
)

var reservedNameRe = regexp.MustCompile(`^__.*__$`)

// mongoError is a MongoDB command error: its code, code name and message.
type mongoError struct {
	Code int32
	Name string
	Msg  string
}

func (e *mongoError) Error() string { return e.Msg }

func merr(code int32, name, format string, args ...any) *mongoError {
	return &mongoError{Code: code, Name: name, Msg: fmt.Sprintf(format, args...)}
}

func badValue(format string, args ...any) *mongoError {
	return merr(2, "BadValue", format, args...)
}

// checkFieldName refuses a field name Google does not support.
func checkFieldName(name string) error {
	switch {
	case name == "":
		return badValue("Empty field names are not supported.")
	case !utf8.ValidString(name):
		return badValue("Field names must be valid UTF-8.")
	case len(name) > mongoMaxNameSize:
		return badValue("Field name %.40q... is longer than %d bytes.", name, mongoMaxNameSize)
	case reservedNameRe.MatchString(name):
		return badValue("Field name %q is not supported: names matching __.*__ are reserved.", name)
	}
	return nil
}

// checkCollectionName refuses a collection name Google does not support.
func checkCollectionName(name string) error {
	switch {
	case name == "":
		return merr(73, "InvalidNamespace", "Invalid namespace specified: the collection name is empty.")
	case !utf8.ValidString(name), strings.ContainsRune(name, 0), strings.Contains(name, "$"), strings.Contains(name, "/"),
		strings.HasPrefix(name, "system."), strings.Contains(name, ".system."), name == ".", name == "..":
		return merr(73, "InvalidNamespace", "Invalid collection name: %q.", name)
	case len(name) > mongoMaxNameSize:
		return merr(73, "InvalidNamespace", "Collection name is longer than %d bytes.", mongoMaxNameSize)
	case reservedNameRe.MatchString(name):
		return merr(73, "InvalidNamespace", "Collection names matching __.*__ are not supported: %q.", name)
	}
	return nil
}

var (
	minDate = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	maxDate = time.Date(9999, 12, 31, 23, 59, 59, 999e6, time.UTC).UnixMilli()
)

func special(key string, v *firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: map[string]*firestorepb.Value{key: v}}}}
}

func fsInt(i int64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: i}}
}

func fsString(s string) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: s}}
}

var fsNull = &firestorepb.Value{ValueType: &firestorepb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}}

func fsMap(fields map[string]*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: fields}}}
}

// toFirestore is a BSON value as a Firestore value. depth is the nesting
// depth of the value's container.
func toFirestore(v any, depth int) (*firestorepb.Value, error) {
	switch x := v.(type) {
	case nil, bson.Null:
		return fsNull, nil
	case bool:
		return &firestorepb.Value{ValueType: &firestorepb.Value_BooleanValue{BooleanValue: x}}, nil
	case int32:
		return special("__int__", fsInt(int64(x))), nil
	case int64:
		return fsInt(x), nil
	case int:
		return fsInt(int64(x)), nil
	case float64:
		if math.IsNaN(x) {
			x = math.NaN()
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{DoubleValue: x}}, nil
	case string:
		return fsString(x), nil
	case bson.ObjectID:
		return special("__oid__", fsString(x.Hex())), nil
	case bson.DateTime:
		ms := int64(x)
		if ms < minDate || ms > maxDate {
			return nil, badValue("Date values must fall in [0001-01-01T00:00:00Z, 9999-12-31T23:59:59Z].")
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_TimestampValue{TimestampValue: timestamppb.New(time.UnixMilli(ms).UTC())}}, nil
	case bson.Binary:
		if x.Subtype == 0 {
			return &firestorepb.Value{ValueType: &firestorepb.Value_BytesValue{BytesValue: x.Data}}, nil
		}
		return special("__binary__", &firestorepb.Value{ValueType: &firestorepb.Value_BytesValue{BytesValue: append([]byte{x.Subtype}, x.Data...)}}), nil
	case bson.Regex:
		if err := checkRegexOptions(x.Options); err != nil {
			return nil, err
		}
		return special("__regex__", fsMap(map[string]*firestorepb.Value{"pattern": fsString(x.Pattern), "options": fsString(x.Options)})), nil
	case bson.Decimal128:
		s := x.String()
		switch {
		case x.IsNaN():
			s = "NaN"
		case x.IsInf() > 0:
			s = "Infinity"
		case x.IsInf() < 0:
			s = "-Infinity"
		}
		return special("__decimal128__", fsString(s)), nil
	case bson.Timestamp:
		return special("__request_timestamp__", fsMap(map[string]*firestorepb.Value{"seconds": fsInt(int64(x.T)), "increment": fsInt(int64(x.I))})), nil
	case bson.MinKey:
		return special("__min__", fsNull), nil
	case bson.MaxKey:
		return special("__max__", fsNull), nil
	case bson.A:
		if depth+1 > mongoMaxDepth {
			return nil, badValue("The maximum nesting depth of fields is %d.", mongoMaxDepth)
		}
		vals := make([]*firestorepb.Value, len(x))
		for i, e := range x {
			fv, err := toFirestore(e, depth+1)
			if err != nil {
				return nil, err
			}
			vals[i] = fv
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{ArrayValue: &firestorepb.ArrayValue{Values: vals}}}, nil
	case bson.D:
		if depth+1 > mongoMaxDepth {
			return nil, badValue("The maximum nesting depth of fields is %d.", mongoMaxDepth)
		}
		if fv, ok, err := firestoreOnly(x); ok || err != nil {
			return fv, err
		}
		fields, err := toFirestoreFields(x, depth+1)
		if err != nil {
			return nil, err
		}
		return fsMap(fields), nil
	case bson.Undefined, bson.JavaScript, bson.CodeWithScope, bson.Symbol, bson.DBPointer:
		return nil, badValue("The %s BSON type is not supported.", bsonTypeName(v))
	}
	return nil, badValue("Unsupported value of type %T.", v)
}

// toFirestoreFields is a BSON document's fields as Firestore fields.
func toFirestoreFields(d bson.D, depth int) (map[string]*firestorepb.Value, error) {
	fields := make(map[string]*firestorepb.Value, len(d))
	for _, e := range d {
		if err := checkFieldName(e.Key); err != nil {
			return nil, err
		}
		if _, dup := fields[e.Key]; dup {
			return nil, badValue("Duplicate field name %q.", e.Key)
		}
		fv, err := toFirestore(e.Value, depth)
		if err != nil {
			return nil, err
		}
		fields[e.Key] = fv
	}
	return fields, nil
}

// firestoreOnly is the Firestore GeoPoint, Vector or Reference an object
// of one key, __fs_geopoint__, __fs_vector__ or __fs_ref__, stands for.
func firestoreOnly(d bson.D) (*firestorepb.Value, bool, error) {
	if len(d) != 1 {
		return nil, false, nil
	}
	switch d[0].Key {
	case "__fs_geopoint__":
		g, ok := d[0].Value.(bson.D)
		lat, lok := numberOf(lookup(g, "latitude"))
		lng, gok := numberOf(lookup(g, "longitude"))
		if !ok || !lok || !gok {
			return nil, true, badValue("__fs_geopoint__ needs numeric latitude and longitude.")
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_GeoPointValue{GeoPointValue: &latlng.LatLng{Latitude: lat, Longitude: lng}}}, true, nil
	case "__fs_vector__":
		a, ok := d[0].Value.(bson.A)
		if !ok {
			return nil, true, badValue("__fs_vector__ needs an array of numbers.")
		}
		vals := make([]*firestorepb.Value, len(a))
		for i, e := range a {
			f, ok := numberOf(e)
			if !ok {
				return nil, true, badValue("__fs_vector__ needs an array of numbers.")
			}
			vals[i] = &firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{DoubleValue: f}}
		}
		return fsMap(map[string]*firestorepb.Value{"__type__": fsString("__vector__"),
			"value": {ValueType: &firestorepb.Value_ArrayValue{ArrayValue: &firestorepb.ArrayValue{Values: vals}}}}), true, nil
	case "__fs_ref__":
		s, ok := d[0].Value.(string)
		if !ok {
			return nil, true, badValue("__fs_ref__ needs a document name.")
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: s}}, true, nil
	}
	return nil, false, nil
}

// fromFirestore is a Firestore value as a BSON value.
func fromFirestore(v *firestorepb.Value) any {
	switch x := v.GetValueType().(type) {
	case *firestorepb.Value_NullValue:
		return nil
	case *firestorepb.Value_BooleanValue:
		return x.BooleanValue
	case *firestorepb.Value_IntegerValue:
		return x.IntegerValue
	case *firestorepb.Value_DoubleValue:
		return x.DoubleValue
	case *firestorepb.Value_TimestampValue:
		return bson.DateTime(x.TimestampValue.AsTime().UnixMilli())
	case *firestorepb.Value_StringValue:
		return x.StringValue
	case *firestorepb.Value_BytesValue:
		return bson.Binary{Subtype: 0, Data: x.BytesValue}
	case *firestorepb.Value_ReferenceValue:
		return bson.D{{Key: "__fs_ref__", Value: x.ReferenceValue}}
	case *firestorepb.Value_GeoPointValue:
		return bson.D{{Key: "__fs_geopoint__", Value: bson.D{{Key: "latitude", Value: x.GeoPointValue.GetLatitude()}, {Key: "longitude", Value: x.GeoPointValue.GetLongitude()}}}}
	case *firestorepb.Value_ArrayValue:
		out := make(bson.A, len(x.ArrayValue.GetValues()))
		for i, e := range x.ArrayValue.GetValues() {
			out[i] = fromFirestore(e)
		}
		return out
	case *firestorepb.Value_MapValue:
		return fromFirestoreMap(x.MapValue.GetFields())
	}
	return nil
}

func fromFirestoreMap(f map[string]*firestorepb.Value) any {
	if len(f) == 1 {
		for k, v := range f {
			switch k {
			case "__int__":
				return int32(v.GetIntegerValue())
			case "__oid__":
				var id bson.ObjectID
				if b, err := hex.DecodeString(v.GetStringValue()); err == nil && len(b) == 12 {
					copy(id[:], b)
				}
				return id
			case "__binary__":
				b := v.GetBytesValue()
				if len(b) == 0 {
					return bson.Binary{}
				}
				return bson.Binary{Subtype: b[0], Data: b[1:]}
			case "__regex__":
				m := v.GetMapValue().GetFields()
				return bson.Regex{Pattern: m["pattern"].GetStringValue(), Options: m["options"].GetStringValue()}
			case "__decimal128__":
				d, _ := bson.ParseDecimal128(v.GetStringValue())
				return d
			case "__request_timestamp__":
				m := v.GetMapValue().GetFields()
				return bson.Timestamp{T: uint32(m["seconds"].GetIntegerValue()), I: uint32(m["increment"].GetIntegerValue())}
			case "__min__":
				return bson.MinKey{}
			case "__max__":
				return bson.MaxKey{}
			}
		}
	}
	if len(f) == 2 && f["__type__"].GetStringValue() == "__vector__" {
		var a bson.A
		for _, e := range f["value"].GetArrayValue().GetValues() {
			a = append(a, fromFirestore(e))
		}
		return bson.D{{Key: "__fs_vector__", Value: a}}
	}
	return fromFirestoreFields(f)
}

// fromFirestoreFields is Firestore fields as a BSON document, in field
// name order (a Firestore map keeps no order), with names escaped as
// __r:name__ read back as __name__.
func fromFirestoreFields(f map[string]*firestorepb.Value) bson.D {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := make(bson.D, 0, len(keys))
	for _, k := range keys {
		name := k
		if strings.HasPrefix(k, "__r:") && strings.HasSuffix(k, "__") && len(k) > 6 {
			name = "__" + k[4:len(k)-2] + "__"
		}
		d = append(d, bson.E{Key: name, Value: fromFirestore(f[k])})
	}
	return d
}

// checkRegexOptions enforces Google's rule: options are i, m, s, u or x,
// in alphabetical order, without repeats.
func checkRegexOptions(o string) error {
	last := rune(0)
	for _, r := range o {
		if !strings.ContainsRune("imsux", r) || r <= last {
			return badValue("Regular expression options must be valid (\"i\", \"m\", \"s\", \"u\", or \"x\") and provided in alphabetical order without repeats: %q.", o)
		}
		last = r
	}
	return nil
}

// checkID enforces Google's rules for a top-level _id.
func checkID(v any, top bool) error {
	switch x := v.(type) {
	case bson.ObjectID, int32, int64, float64, bson.Binary, bool:
		return nil
	case string:
		if !utf8.ValidString(x) {
			return badValue("A string _id must be valid UTF-8.")
		}
		if reservedNameRe.MatchString(x) {
			return badValue("A string _id can't match the regular expression __.*__: %q.", x)
		}
		return nil
	case bson.D:
		for _, e := range x {
			if err := checkID(e.Value, false); err != nil {
				return err
			}
		}
		return nil
	case bson.A:
		if top {
			break
		}
		for _, e := range x {
			if err := checkID(e, false); err != nil {
				return err
			}
		}
		return nil
	}
	return badValue("The _id field must be an ObjectId, String, 64-bit integer, 32-bit integer, Double, Binary, Boolean or Object; %s is not supported.", bsonTypeName(v))
}

// docIDPrefix starts every document ID that is not a string _id as it is.
const docIDPrefix = "~"

// docID is the Firestore document ID of an _id. A string that is a valid
// document ID is the ID itself, so a document a Firestore client writes
// reads back with its ID as its _id; any other _id is named by its type
// and value after docIDPrefix.
func docID(id any) (string, error) {
	if err := checkID(id, true); err != nil {
		return "", err
	}
	if raw, err := bson.Marshal(bson.D{{Key: "v", Value: id}}); err == nil && len(raw)-8 > mongoMaxIDSize {
		return "", badValue("The _id must be no larger than %d bytes.", mongoMaxIDSize)
	}
	switch x := id.(type) {
	case string:
		if x != "" && x != "." && x != ".." && !strings.Contains(x, "/") && !strings.HasPrefix(x, docIDPrefix) {
			return x, nil
		}
		return docIDPrefix + "s:" + base64.RawURLEncoding.EncodeToString([]byte(x)), nil
	case bson.ObjectID:
		return docIDPrefix + "o:" + x.Hex(), nil
	case int32:
		return docIDPrefix + "i:" + strconv.FormatInt(int64(x), 10), nil
	case int64:
		return docIDPrefix + "l:" + strconv.FormatInt(x, 10), nil
	case float64:
		if math.IsNaN(x) {
			return "", badValue("A NaN _id is not supported.")
		}
		return docIDPrefix + "d:" + strconv.FormatFloat(x, 'g', -1, 64), nil
	}
	raw, err := bson.Marshal(bson.D{{Key: "v", Value: id}})
	if err != nil {
		return "", badValue("The _id cannot be encoded: %v", err)
	}
	return docIDPrefix + "v:" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// numericAliases are the document IDs of the other numeric types an
// _id equals: 5, Long(5) and 5.0 are one _id to MongoDB.
func numericAliases(id any) []string {
	f, ok := numberOf(id)
	if !ok || math.IsNaN(f) {
		return nil
	}
	var out []string
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		i := int64(f)
		if i >= math.MinInt32 && i <= math.MaxInt32 {
			out = append(out, docIDPrefix+"i:"+strconv.FormatInt(i, 10))
		}
		out = append(out, docIDPrefix+"l:"+strconv.FormatInt(i, 10))
	}
	out = append(out, docIDPrefix+"d:"+strconv.FormatFloat(f, 'g', -1, 64))
	return out
}

// idOf is the _id a document ID names (docID).
func idOf(docID string) any {
	if !strings.HasPrefix(docID, docIDPrefix) || len(docID) < 3 || docID[2] != ':' {
		return docID
	}
	body := docID[3:]
	switch docID[1] {
	case 's':
		if b, err := base64.RawURLEncoding.DecodeString(body); err == nil {
			return string(b)
		}
	case 'o':
		if id, err := bson.ObjectIDFromHex(body); err == nil {
			return id
		}
	case 'i':
		if i, err := strconv.ParseInt(body, 10, 32); err == nil {
			return int32(i)
		}
	case 'l':
		if i, err := strconv.ParseInt(body, 10, 64); err == nil {
			return i
		}
	case 'd':
		if f, err := strconv.ParseFloat(body, 64); err == nil {
			return f
		}
	case 'v':
		if b, err := base64.RawURLEncoding.DecodeString(body); err == nil {
			var d bson.D
			if bson.Unmarshal(b, &d) == nil && len(d) == 1 {
				return d[0].Value
			}
		}
	}
	return docID
}

// toFirestoreDoc is a BSON document's fields, its _id left out, as
// Firestore fields, with Google's size limit enforced.
func toFirestoreDoc(d bson.D) (map[string]*firestorepb.Value, error) {
	if raw, err := bson.Marshal(d); err != nil {
		return nil, badValue("The document cannot be encoded: %v", err)
	} else if len(raw) > mongoMaxDocSize {
		return nil, merr(10334, "BSONObjectTooLarge", "The document is %d bytes; the maximum document size is 16 MiB.", len(raw))
	}
	rest := make(bson.D, 0, len(d))
	for _, e := range d {
		if e.Key != "_id" {
			rest = append(rest, e)
		}
	}
	return toFirestoreFields(rest, 0)
}

// fromFirestoreDoc is a Firestore document as a BSON document, _id first.
func fromFirestoreDoc(doc *firestorepb.Document) bson.D {
	name := doc.GetName()
	id := idOf(name[strings.LastIndex(name, "/")+1:])
	return append(bson.D{{Key: "_id", Value: id}}, fromFirestoreFields(doc.GetFields())...)
}

// bsonTypeName is the MongoDB alias of v's BSON type.
func bsonTypeName(v any) string {
	switch v.(type) {
	case nil, bson.Null:
		return "null"
	case missingT:
		return "missing"
	case bool:
		return "bool"
	case int32:
		return "int"
	case int64, int:
		return "long"
	case float64:
		return "double"
	case string:
		return "string"
	case bson.D:
		return "object"
	case bson.A:
		return "array"
	case bson.ObjectID:
		return "objectId"
	case bson.DateTime:
		return "date"
	case bson.Binary:
		return "binData"
	case bson.Regex:
		return "regex"
	case bson.Decimal128:
		return "decimal"
	case bson.Timestamp:
		return "timestamp"
	case bson.MinKey:
		return "minKey"
	case bson.MaxKey:
		return "maxKey"
	case bson.Undefined:
		return "undefined"
	case bson.JavaScript:
		return "javascript"
	case bson.CodeWithScope:
		return "javascriptWithScope"
	case bson.Symbol:
		return "symbol"
	case bson.DBPointer:
		return "dbPointer"
	}
	return fmt.Sprintf("%T", v)
}
