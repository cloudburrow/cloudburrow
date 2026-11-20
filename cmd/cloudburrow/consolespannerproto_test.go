package main

import (
	"strings"
	"testing"

	"cloud.google.com/go/spanner"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// testMusicBundle is a bundle of one message, cb.music.SingerInfo
// (singer_id int64 = 1, nationality string = 2), and one enum,
// cb.music.Genre (POP 0, JAZZ 1, ROCK 2).
func testMusicBundle(t *testing.T) spannerProtos {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("cb/music.proto"), Package: proto.String("cb.music"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("SingerInfo"), Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("singer_id"), JsonName: proto.String("singerId"), Number: proto.Int32(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			{Name: proto.String("nationality"), JsonName: proto.String("nationality"), Number: proto.Int32(2), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
		}}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("Genre"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("POP"), Number: proto.Int32(0)}, {Name: proto.String("JAZZ"), Number: proto.Int32(1)}, {Name: proto.String("ROCK"), Number: proto.Int32(2)},
		}}},
	}}}
	raw, err := proto.Marshal(fds)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := spannerProtosOf(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestSpannerProtoColumnTypes(t *testing.T) {
	ps := testMusicBundle(t)
	for in, want := range map[string]string{
		"`cb.music.SingerInfo`":        "PROTO cb.music.SingerInfo",
		"cb.music.Genre":               "ENUM cb.music.Genre",
		"PROTO<cb.music.SingerInfo>":   "PROTO cb.music.SingerInfo",
		"ARRAY<`cb.music.Genre`>":      "ARRAY ENUM cb.music.Genre",
		"ARRAY<`cb.music.SingerInfo`>": "ARRAY PROTO cb.music.SingerInfo",
		"INT64":                        "INT64 ",
	} {
		typ := ps.columnType(in)
		got := typ.GetCode().String() + " " + typ.GetProtoTypeFqn()
		if typ.GetCode() == sppb.TypeCode_ARRAY {
			got = "ARRAY " + typ.GetArrayElementType().GetCode().String() + " " + typ.GetArrayElementType().GetProtoTypeFqn()
		}
		if got != want {
			t.Errorf("columnType(%q) = %q, want %q", in, got, want)
		}
	}
	if typ := ps.columnType("`cb.music.Missing`"); typ != nil {
		t.Errorf("a type outside the bundle = %v, want nil (read-only)", typ)
	}
	if typ := (spannerProtos{}).columnType("`cb.music.Genre`"); typ != nil {
		t.Errorf("with no bundle = %v, want nil", typ)
	}
	if !ps.typed(spannerDialect{}, "`cb.music.Genre`") || ps.typed(spannerDialect{}, "INT64") {
		t.Error("a PROTO or ENUM column is a typed parameter, a GoogleSQL INT64 is not")
	}
}

func TestSpannerProtoValuesRoundTrip(t *testing.T) {
	ps := testMusicBundle(t)
	info := ps.columnType("`cb.music.SingerInfo`")
	v, err := ps.wireValue(info, `{"singerId": "7", "nationality": "FR"}`)
	if err != nil {
		t.Fatal(err)
	}
	// singer_id 7, nationality "FR", serialised in field order.
	if got, want := v.GetStringValue(), "CAcSAkZS"; got != want {
		t.Errorf("PROTO wire value = %q, want %q", got, want)
	}
	if got := ps.render(spanner.GenericColumnValue{Type: info, Value: v}); got != `{"singerId":"7","nationality":"FR"}` {
		t.Errorf("PROTO rendered = %s", got)
	}
	if _, err := ps.wireValue(info, `{"nope": 1}`); err == nil || !strings.Contains(err.Error(), "cb.music.SingerInfo") {
		t.Errorf("an unknown field = %v, want a refusal naming the message", err)
	}

	genre := ps.columnType("`cb.music.Genre`")
	for in, want := range map[string]string{"ROCK": "2", "1": "1"} {
		v, err := ps.wireValue(genre, in)
		if err != nil || v.GetStringValue() != want {
			t.Errorf("ENUM %q = %v, %v; want %s", in, v, err, want)
		}
	}
	if _, err := ps.wireValue(genre, "METAL"); err == nil || !strings.Contains(err.Error(), "POP, JAZZ, ROCK") {
		t.Errorf("an unknown name = %v, want the value names", err)
	}
	g, _ := ps.wireValue(genre, "JAZZ")
	if got := ps.render(spanner.GenericColumnValue{Type: genre, Value: g}); got != "JAZZ" {
		t.Errorf("ENUM rendered = %q, want JAZZ", got)
	}

	arr := ps.columnType("ARRAY<`cb.music.Genre`>")
	av, err := ps.wireValue(arr, `["POP", null, "ROCK"]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := ps.render(spanner.GenericColumnValue{Type: arr, Value: av}); got != `["POP",null,"ROCK"]` {
		t.Errorf("ARRAY<ENUM> rendered = %s", got)
	}
	parr := ps.columnType("ARRAY<`cb.music.SingerInfo`>")
	pv, err := ps.wireValue(parr, `[{"singerId": "1"}, null]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := ps.render(spanner.GenericColumnValue{Type: parr, Value: pv}); got != `[{"singerId":"1"},null]` {
		t.Errorf("ARRAY<PROTO> rendered = %s", got)
	}
}

func TestSpannerProtoRowFields(t *testing.T) {
	ps := testMusicBundle(t)
	cols := []spannerColumn{
		{Name: "Id", Type: "INT64", Key: 1},
		{Name: "Info", Type: "`cb.music.SingerInfo`", Nullable: true},
		{Name: "Genre", Type: "`cb.music.Genre`", Nullable: true},
	}
	fields := spannerRowFields(ps, cols, nil)
	if len(fields) != 3 {
		t.Fatalf("fields = %+v, want Id, Info and Genre", fields)
	}
	if fields[1].Type != "textarea" || !strings.Contains(fields[1].Help, "cb.music.SingerInfo") {
		t.Errorf("Info field = %+v, want a textarea for the message's JSON", fields[1])
	}
	if fields[2].Type != "select" || strings.Join(fields[2].Options, ",") != ",POP,JAZZ,ROCK" {
		t.Errorf("Genre field = %+v, want a select of NULL and the value names", fields[2])
	}
	if got := spannerRowFields(spannerProtos{}, cols, nil); len(got) != 1 {
		t.Errorf("with no bundle the PROTO and ENUM columns stay read-only: %+v", got)
	}
}
