package bigtablefront

import (
	"fmt"
	"math"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// PROTO and ENUM values in GoogleSQL for Bigtable (#2113), as "Query
// protobuf data" documents them (docs read 2026-11-21:
// https://docs.cloud.google.com/bigtable/docs/query-protobuf-data): a
// BYTES value CAST to bundle_id.package.Message, the bundle one of the FROM
// table's schema bundles, is that message; its fields are read with dot
// notation, nested messages too, in the SELECT list, WHERE and aggregates,
// and expression.* gives one column per top-level field. A query returns a
// message as Type.proto_type (bytes_value) and an enum as Type.enum_type
// (int_value), as the data API's types.proto says, and takes parameters of
// those types.
//
// Field reads follow GoogleSQL's protocol buffer rules: a scalar field not
// set reads as its default, a message field not set reads NULL, a repeated
// field is an ARRAY (empty when unset), and has_<field> is whether a field
// with presence is set. Unsigned 64-bit fields read as INT64, since
// Bigtable's types have no UINT64. A message is neither comparable nor
// groupable; an enum compares by number, and with a string literal by
// name. CAST converts BYTES and STRING (text format) to and from a message,
// and an enum to and from INT64 and STRING.

// protoTypePrefix marks a CAST target the parser read as a dotted path.
const protoTypePrefix = "\x00proto:"

// protoCatalog is the schema bundles a query's FROM table has.
type protoCatalog struct {
	table string
	files map[string]*protoregistry.Files // by bundle ID
}

// newProtoCatalog builds the catalog of table from its bundles.
func newProtoCatalog(table string, bundles map[string][]byte) *protoCatalog {
	c := &protoCatalog{table: table, files: map[string]*protoregistry.Files{}}
	for id, raw := range bundles {
		var set descriptorpb.FileDescriptorSet
		if proto.Unmarshal(raw, &set) != nil {
			continue
		}
		if f, err := protodesc.NewFiles(&set); err == nil {
			c.files[id] = f
		}
	}
	return c
}

func protoMsgType(bundle, name string) *btpb.Type {
	return &btpb.Type{Kind: &btpb.Type_ProtoType{ProtoType: &btpb.Type_Proto{SchemaBundleId: bundle, MessageName: name}}}
}

func protoEnumType(bundle, name string) *btpb.Type {
	return &btpb.Type{Kind: &btpb.Type_EnumType{EnumType: &btpb.Type_Enum{SchemaBundleId: bundle, EnumName: name}}}
}

// isProtoMsg is whether t is a schema bundle's message (not GEOGRAPHY).
func isProtoMsg(t *btpb.Type) bool { return t.GetProtoType() != nil && !isGeography(t) }

func isProtoEnum(t *btpb.Type) bool { return t.GetEnumType() != nil }

// protoKind is how errors name a message or enum type.
func protoKind(t *btpb.Type) string {
	if p := t.GetProtoType(); p != nil {
		return p.GetSchemaBundleId() + "." + p.GetMessageName()
	}
	e := t.GetEnumType()
	return e.GetSchemaBundleId() + "." + e.GetEnumName()
}

// resolve is the type a CAST's dotted path names.
func (c *protoCatalog) resolve(path string) (*btpb.Type, error) {
	i := strings.Index(path, ".")
	if i <= 0 || i == len(path)-1 {
		return nil, fmt.Errorf("Type not found: %s", path)
	}
	bundle, name := path[:i], path[i+1:]
	d, err := c.find(bundle, name)
	if err != nil {
		return nil, err
	}
	switch d.(type) {
	case protoreflect.MessageDescriptor:
		return protoMsgType(bundle, name), nil
	case protoreflect.EnumDescriptor:
		return protoEnumType(bundle, name), nil
	}
	return nil, fmt.Errorf("Type not found: %s", path)
}

func (c *protoCatalog) find(bundle, name string) (protoreflect.Descriptor, error) {
	if c == nil {
		return nil, fmt.Errorf("Type not found: %s.%s", bundle, name)
	}
	f, ok := c.files[bundle]
	if !ok {
		return nil, fmt.Errorf("Type not found: %s.%s: table %s has no schema bundle %s", bundle, name, c.table, bundle)
	}
	d, err := f.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, fmt.Errorf("Type not found: %s.%s", bundle, name)
	}
	return d, nil
}

// message is the descriptor of a message type.
func (c *protoCatalog) message(t *btpb.Type) (protoreflect.MessageDescriptor, error) {
	p := t.GetProtoType()
	d, err := c.find(p.GetSchemaBundleId(), p.GetMessageName())
	if err != nil {
		return nil, err
	}
	m, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("Type not found: %s", protoKind(t))
	}
	return m, nil
}

// enum is the descriptor of an enum type.
func (c *protoCatalog) enum(t *btpb.Type) (protoreflect.EnumDescriptor, error) {
	e := t.GetEnumType()
	d, err := c.find(e.GetSchemaBundleId(), e.GetEnumName())
	if err != nil {
		return nil, err
	}
	en, ok := d.(protoreflect.EnumDescriptor)
	if !ok {
		return nil, fmt.Errorf("Type not found: %s", protoKind(t))
	}
	return en, nil
}

// checkType is whether every message and enum t holds is in the catalog.
func (c *protoCatalog) checkType(t *btpb.Type) error {
	switch {
	case isProtoMsg(t):
		_, err := c.message(t)
		return err
	case isProtoEnum(t):
		_, err := c.enum(t)
		return err
	case t.GetArrayType() != nil:
		return c.checkType(t.GetArrayType().GetElementType())
	}
	return nil
}

// fieldType is the SQL type of a message's field.
func fieldType(bundle string, fd protoreflect.FieldDescriptor) *btpb.Type {
	var t *btpb.Type
	switch fd.Kind() {
	case protoreflect.BoolKind:
		t = boolType
	case protoreflect.EnumKind:
		t = protoEnumType(bundle, string(fd.Enum().FullName()))
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind, protoreflect.Int64Kind,
		protoreflect.Sint64Kind, protoreflect.Sfixed64Kind, protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		t = int64Type
	case protoreflect.FloatKind:
		t = float32Type
	case protoreflect.DoubleKind:
		t = float64Type
	case protoreflect.StringKind:
		t = stringType
	case protoreflect.BytesKind:
		t = bytesType
	case protoreflect.MessageKind, protoreflect.GroupKind:
		t = protoMsgType(bundle, string(fd.Message().FullName()))
	}
	if fd.IsList() || fd.IsMap() {
		return arrayType(t)
	}
	return t
}

// protoField is the field access of a message's field, or has_<field>.
type protoField struct {
	md  protoreflect.MessageDescriptor
	fd  protoreflect.FieldDescriptor
	has bool
}

// lookupField finds name in md, as GoogleSQL does: case-insensitively,
// and has_<field> for a field with presence.
func lookupField(md protoreflect.MessageDescriptor, name string) (*protoField, error) {
	find := func(n string) protoreflect.FieldDescriptor {
		if fd := md.Fields().ByName(protoreflect.Name(n)); fd != nil {
			return fd
		}
		for i := 0; i < md.Fields().Len(); i++ {
			if fd := md.Fields().Get(i); strings.EqualFold(string(fd.Name()), n) {
				return fd
			}
		}
		return nil
	}
	if fd := find(name); fd != nil {
		return &protoField{md: md, fd: fd}, nil
	}
	if len(name) > 4 && strings.EqualFold(name[:4], "has_") {
		if fd := find(name[4:]); fd != nil {
			if fd.IsList() || fd.IsMap() {
				return nil, fmt.Errorf("Protocol buffer %s field %s is repeated; has_%s is not allowed", md.FullName(), fd.Name(), fd.Name())
			}
			if !fd.HasPresence() {
				return nil, fmt.Errorf("Protocol buffer %s field %s has no presence; has_%s is not allowed", md.FullName(), fd.Name(), fd.Name())
			}
			return &protoField{md: md, fd: fd, has: true}, nil
		}
	}
	return nil, fmt.Errorf("Protocol buffer %s does not have a field named %s", md.FullName(), name)
}

// read is the field of the serialized message raw.
func (pf *protoField) read(raw []byte) (any, error) {
	m := dynamicpb.NewMessage(pf.md)
	if err := proto.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("Error parsing proto %s: %v", pf.md.FullName(), err)
	}
	if pf.has {
		return m.Has(pf.fd), nil
	}
	fd := pf.fd
	v := m.Get(fd)
	switch {
	case fd.IsMap():
		out := []any{}
		var err error
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			e := dynamicpb.NewMessage(fd.Message())
			e.Set(fd.MapKey(), k.Value())
			e.Set(fd.MapValue(), mv)
			var b []byte
			b, err = proto.MarshalOptions{Deterministic: true}.Marshal(e)
			if err != nil {
				return false
			}
			out = append(out, b)
			return true
		})
		return out, err
	case fd.IsList():
		l := v.List()
		out := make([]any, 0, l.Len())
		for i := 0; i < l.Len(); i++ {
			e, err := scalarValue(fd, l.Get(i))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case fd.Message() != nil && !m.Has(fd):
		return nil, nil
	}
	return scalarValue(fd, v)
}

// scalarValue is one value of fd as the engine holds it.
func scalarValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) (any, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return v.Bool(), nil
	case protoreflect.EnumKind:
		return int64(v.Enum()), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind, protoreflect.Int64Kind,
		protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int(), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint()), nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float(), nil
	case protoreflect.StringKind:
		return v.String(), nil
	case protoreflect.BytesKind:
		return v.Bytes(), nil
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return proto.MarshalOptions{Deterministic: true}.Marshal(v.Message().Interface())
	}
	return nil, fmt.Errorf("unsupported protocol buffer field kind %s", fd.Kind())
}

// protoCast is a CAST to or from a message or enum type, or nil, false
// when neither side is one.
func protoCast(c *protoCatalog, from, to *btpb.Type) (func(any) (any, error), bool, error) {
	if !isProtoMsg(from) && !isProtoEnum(from) && !isProtoMsg(to) && !isProtoEnum(to) {
		return nil, false, nil
	}
	invalid := func() (func(any) (any, error), bool, error) {
		return nil, true, fmt.Errorf("Invalid cast from %s to %s", kind(from), kind(to))
	}
	if sameType(from, to) {
		return func(v any) (any, error) { return v, nil }, true, nil
	}
	switch {
	case isProtoMsg(to):
		md, err := c.message(to)
		if err != nil {
			return nil, true, err
		}
		switch {
		case isKind(from, "BYTES"):
			return func(v any) (any, error) {
				b := asBytes(v)
				if err := proto.Unmarshal(b, dynamicpb.NewMessage(md)); err != nil {
					return nil, fmt.Errorf("Error parsing proto %s: %v", md.FullName(), err)
				}
				return b, nil
			}, true, nil
		case isKind(from, "STRING"):
			return func(v any) (any, error) {
				m := dynamicpb.NewMessage(md)
				if err := prototext.Unmarshal(asBytes(v), m); err != nil {
					return nil, fmt.Errorf("Error parsing proto %s: %v", md.FullName(), err)
				}
				return proto.MarshalOptions{Deterministic: true}.Marshal(m)
			}, true, nil
		}
		return invalid()
	case isProtoMsg(from):
		md, err := c.message(from)
		if err != nil {
			return nil, true, err
		}
		switch {
		case isKind(to, "BYTES"):
			return func(v any) (any, error) { return asBytes(v), nil }, true, nil
		case isKind(to, "STRING"):
			return func(v any) (any, error) {
				m := dynamicpb.NewMessage(md)
				if err := proto.Unmarshal(asBytes(v), m); err != nil {
					return nil, fmt.Errorf("Error parsing proto %s: %v", md.FullName(), err)
				}
				b, err := prototext.MarshalOptions{}.Marshal(m)
				return string(b), err
			}, true, nil
		}
		return invalid()
	case isProtoEnum(to):
		ed, err := c.enum(to)
		if err != nil {
			return nil, true, err
		}
		switch {
		case isKind(from, "INT64"):
			return func(v any) (any, error) {
				n := v.(int64)
				if n < math.MinInt32 || n > math.MaxInt32 || ed.Values().ByNumber(protoreflect.EnumNumber(n)) == nil {
					return nil, fmt.Errorf("Out of range cast of integer %d to enum type %s", n, kind(to))
				}
				return n, nil
			}, true, nil
		case isKind(from, "STRING"):
			return func(v any) (any, error) {
				ev := ed.Values().ByName(protoreflect.Name(string(asBytes(v))))
				if ev == nil {
					return nil, fmt.Errorf("Out of range cast of string '%s' to enum type %s", asBytes(v), kind(to))
				}
				return int64(ev.Number()), nil
			}, true, nil
		}
		return invalid()
	default: // from an enum
		ed, err := c.enum(from)
		if err != nil {
			return nil, true, err
		}
		switch {
		case isKind(to, "INT64"):
			return func(v any) (any, error) { return v, nil }, true, nil
		case isKind(to, "STRING"):
			return func(v any) (any, error) {
				ev := ed.Values().ByNumber(protoreflect.EnumNumber(v.(int64)))
				if ev == nil {
					return nil, fmt.Errorf("Out of range cast of integer %d to STRING for enum type %s", v, kind(from))
				}
				return string(ev.Name()), nil
			}, true, nil
		}
		return invalid()
	}
}

// coerceEnumLiteral turns a string literal compared with an enum into the
// enum's value, as GoogleSQL coerces literals.
func coerceEnumLiteral(c *protoCatalog, x expr, enum *btpb.Type) (expr, bool, error) {
	l, ok := x.(*literal)
	if !ok || l.v == nil || !isKind(l.t, "STRING") {
		return x, false, nil
	}
	ed, err := c.enum(enum)
	if err != nil {
		return nil, false, err
	}
	ev := ed.Values().ByName(protoreflect.Name(l.v.(string)))
	if ev == nil {
		return nil, false, fmt.Errorf("Could not cast literal \"%s\" to type %s", l.v, kind(enum))
	}
	return &literal{v: int64(ev.Number()), t: enum}, true, nil
}

// starFields is the columns expression.* gives for a value of type t.
func starFields(c *protoCatalog, x expr, t *btpb.Type) ([]selectItem, error) {
	var out []selectItem
	switch {
	case t.GetStructType() != nil:
		for _, f := range t.GetStructType().GetFields() {
			if f.GetFieldName() == "" {
				return nil, fmt.Errorf("Star expansion of a STRUCT with an anonymous field is not allowed")
			}
			out = append(out, selectItem{e: &field{x: x, name: f.GetFieldName()}, alias: f.GetFieldName()})
		}
	case isProtoMsg(t):
		md, err := c.message(t)
		if err != nil {
			return nil, err
		}
		for i := 0; i < md.Fields().Len(); i++ {
			n := string(md.Fields().Get(i).Name())
			out = append(out, selectItem{e: &field{x: x, name: n}, alias: n})
		}
	default:
		return nil, fmt.Errorf("Dot-star is not supported for type %s", kind(t))
	}
	return out, nil
}
