package main

// PROTO and ENUM columns on a table's Data tab (#2017). Google documents a
// database's proto bundle as the descriptors it is given with CREATE PROTO
// BUNDLE (CreateDatabase's or UpdateDatabaseDdl's proto_descriptors), which
// GetDatabaseDdl returns, and a PROTO or ENUM column's value as a typed
// value of TypeCode PROTO or ENUM naming the type by proto_type_fqn: a PROTO
// travels as the base64 of the serialised message, an ENUM as its number
// (google/spanner/v1/type.proto; protocol-buffers). Measured on the
// emulator (cloud-spanner-emulator 1.5.58, 2026-11-20,
// TestSpannerProtoBundleColumns): it keeps the bundle, returns the
// descriptors from GetDatabaseDdl, binds both parameter types, and its
// INFORMATION_SCHEMA.COLUMNS spells such a column's SPANNER_TYPE as the
// type's backquoted full name ("`cb.music.SingerInfo`",
// "ARRAY<`cb.music.Genre`>"), so whether it is a PROTO or an ENUM is read
// from the bundle.
//
// The Data tab reads the bundle, shows a PROTO value as the message's JSON
// (protojson, Google's JSON mapping of a message) and an ENUM by its value
// name, and writes them back as typed parameters: the JSON is parsed
// against the column's message descriptor and serialised, and an ENUM is
// chosen by name. A value whose descriptor is not in the bundle is shown as
// it travels (base64, or the number), and its column is not written.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/spanner"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// spannerProtos is a database's proto bundle; the zero value has none.
type spannerProtos struct {
	files *protoregistry.Files
	types *dynamicpb.Types
}

// spannerProtosFor reads the bundle of dbName when one of cols is not a
// scalar type or an ARRAY of one (a PROTO or ENUM column), and returns
// none otherwise, or when the bundle cannot be read: those columns then
// stay read-only.
func (p spannerProvider) spannerProtosFor(ctx context.Context, dbName string, cols []spannerColumn) spannerProtos {
	need := false
	for _, c := range cols {
		if spannerColumnType(c.Type) == nil {
			need = true
		}
	}
	if !need {
		return spannerProtos{}
	}
	admin, err := database2AdminClient(ctx, p.endpoint)
	if err != nil {
		return spannerProtos{}
	}
	defer admin.Close()
	ddl, err := admin.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: dbName})
	if err != nil {
		return spannerProtos{}
	}
	ps, _ := spannerProtosOf(ddl.GetProtoDescriptors())
	return ps
}

// spannerProtosOf parses a serialised FileDescriptorSet.
func spannerProtosOf(raw []byte) (spannerProtos, error) {
	if len(raw) == 0 {
		return spannerProtos{}, nil
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		return spannerProtos{}, err
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return spannerProtos{}, err
	}
	return spannerProtos{files: files, types: dynamicpb.NewTypes(files)}, nil
}

func (ps spannerProtos) message(fqn string) protoreflect.MessageDescriptor {
	if ps.files == nil {
		return nil
	}
	d, err := ps.files.FindDescriptorByName(protoreflect.FullName(fqn))
	if err != nil {
		return nil
	}
	m, _ := d.(protoreflect.MessageDescriptor)
	return m
}

func (ps spannerProtos) enum(fqn string) protoreflect.EnumDescriptor {
	if ps.files == nil {
		return nil
	}
	d, err := ps.files.FindDescriptorByName(protoreflect.FullName(fqn))
	if err != nil {
		return nil
	}
	e, _ := d.(protoreflect.EnumDescriptor)
	return e
}

// protoType is a column type naming a bundle's message or enum: the
// backquoted full name the emulator's catalogue gives, the bare name, or
// PROTO<name> / ENUM<name>.
func (ps spannerProtos) protoType(t string) *sppb.Type {
	name := strings.Trim(strings.TrimSpace(t), "`")
	for _, pre := range []string{"PROTO<", "ENUM<"} {
		if strings.HasPrefix(strings.ToUpper(name), pre) && strings.HasSuffix(name, ">") {
			name = strings.Trim(name[len(pre):len(name)-1], "` ")
		}
	}
	if ps.message(name) != nil {
		return &sppb.Type{Code: sppb.TypeCode_PROTO, ProtoTypeFqn: name}
	}
	if ps.enum(name) != nil {
		return &sppb.Type{Code: sppb.TypeCode_ENUM, ProtoTypeFqn: name}
	}
	return nil
}

// columnType is spannerColumnType with the bundle's PROTO and ENUM types,
// and ARRAYs of them.
func (ps spannerProtos) columnType(t string) *sppb.Type {
	if typ := spannerColumnType(t); typ != nil {
		return typ
	}
	s := strings.TrimSpace(t)
	if strings.HasPrefix(strings.ToUpper(s), "ARRAY<") && strings.HasSuffix(s, ">") {
		el := ps.protoType(s[len("ARRAY<") : len(s)-1])
		if el == nil {
			return nil
		}
		return &sppb.Type{Code: sppb.TypeCode_ARRAY, ArrayElementType: el}
	}
	return ps.protoType(s)
}

// editable says whether the Data tab's forms can write a column of type t.
func (ps spannerProtos) editable(t string) bool { return ps.columnType(t) != nil }

// typed says whether a column is written as a typed parameter rather than
// through spannerCastExpr: every column of a PostgreSQL database, an
// ARRAY, a PROTO and an ENUM.
func (ps spannerProtos) typed(d spannerDialect, t string) bool {
	switch ps.columnType(t).GetCode() {
	case sppb.TypeCode_ARRAY, sppb.TypeCode_PROTO, sppb.TypeCode_ENUM:
		return true
	}
	return d.pg
}

// enumNames are an enum's value names, in their declared order.
func enumNames(e protoreflect.EnumDescriptor) []string {
	var out []string
	for i := 0; i < e.Values().Len(); i++ {
		out = append(out, string(e.Values().Get(i).Name()))
	}
	return out
}

// protoWire is raw, a message's JSON or an enum value's name (or number),
// as Spanner's wire value of t: the base64 of the serialised message, or
// the number as a decimal string.
func (ps spannerProtos) protoWire(t *sppb.Type, raw string) (*structpb.Value, error) {
	fqn := t.GetProtoTypeFqn()
	if t.GetCode() == sppb.TypeCode_ENUM {
		e := ps.enum(fqn)
		if e == nil {
			return nil, fmt.Errorf("the database's proto bundle has no enum %s", fqn)
		}
		s := strings.TrimSpace(raw)
		if v := e.Values().ByName(protoreflect.Name(s)); v != nil {
			return structpb.NewStringValue(strconv.FormatInt(int64(v.Number()), 10)), nil
		}
		if n, err := strconv.ParseInt(s, 10, 32); err == nil && e.Values().ByNumber(protoreflect.EnumNumber(n)) != nil {
			return structpb.NewStringValue(strconv.FormatInt(n, 10)), nil
		}
		return nil, fmt.Errorf("%q is not a value of %s: one of %s", raw, fqn, strings.Join(enumNames(e), ", "))
	}
	md := ps.message(fqn)
	if md == nil {
		return nil, fmt.Errorf("the database's proto bundle has no message %s", fqn)
	}
	m := dynamicpb.NewMessage(md)
	if err := (protojson.UnmarshalOptions{Resolver: ps.types}).Unmarshal([]byte(raw), m); err != nil {
		return nil, fmt.Errorf("a %s is written as the message's JSON, such as {\"field\": value}: %v", fqn, err)
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("serialising the %s: %v", fqn, err)
	}
	return structpb.NewStringValue(base64.StdEncoding.EncodeToString(b)), nil
}

// renderProto is a PROTO or ENUM value (or an ARRAY of them) as the Data
// tab shows it, and false when v is not one, or its type is not in the
// bundle.
func (ps spannerProtos) renderProto(t *sppb.Type, v *structpb.Value) (any, bool) {
	if _, ok := v.GetKind().(*structpb.Value_NullValue); ok {
		return nil, true
	}
	switch t.GetCode() {
	case sppb.TypeCode_ARRAY:
		el := t.GetArrayElementType()
		if el.GetCode() != sppb.TypeCode_PROTO && el.GetCode() != sppb.TypeCode_ENUM {
			return nil, false
		}
		out := []any{}
		for _, e := range v.GetListValue().GetValues() {
			r, ok := ps.renderProto(el, e)
			if !ok {
				return nil, false
			}
			out = append(out, r)
		}
		return out, true
	case sppb.TypeCode_ENUM:
		e := ps.enum(t.GetProtoTypeFqn())
		n, err := strconv.ParseInt(v.GetStringValue(), 10, 32)
		if e == nil || err != nil {
			return nil, false
		}
		if ev := e.Values().ByNumber(protoreflect.EnumNumber(n)); ev != nil {
			return string(ev.Name()), true
		}
		// A number the enum does not declare reads as itself.
		return v.GetStringValue(), true
	case sppb.TypeCode_PROTO:
		md := ps.message(t.GetProtoTypeFqn())
		b, err := base64.StdEncoding.DecodeString(v.GetStringValue())
		if md == nil || err != nil {
			return nil, false
		}
		m := dynamicpb.NewMessage(md)
		if err := (proto.UnmarshalOptions{Resolver: ps.types}).Unmarshal(b, m); err != nil {
			return nil, false
		}
		j, err := protojson.MarshalOptions{Resolver: ps.types}.Marshal(m)
		if err != nil {
			return nil, false
		}
		// protojson's spacing varies by build: compact it, so a row reads
		// the same each time.
		var buf bytes.Buffer
		if json.Compact(&buf, j) != nil {
			return nil, false
		}
		return json.RawMessage(buf.Bytes()), true
	}
	return nil, false
}

// render is a Data tab cell: a PROTO as its JSON, an ENUM by name, an
// ARRAY of them as a JSON array, anything else as renderSpannerValue.
func (ps spannerProtos) render(v spanner.GenericColumnValue) string {
	if ps.files != nil {
		if r, ok := ps.renderProto(v.Type, v.Value); ok {
			switch x := r.(type) {
			case nil:
				return "—"
			case string:
				return x
			case json.RawMessage:
				return string(x)
			default:
				b, err := json.Marshal(x)
				if err == nil {
					return string(b)
				}
			}
		}
	}
	return renderSpannerValue(v)
}
