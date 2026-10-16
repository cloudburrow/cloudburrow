package pubsubfront

import (
	"math"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func mustParseProto(t *testing.T, def string) protoreflect.MessageDescriptor {
	t.Helper()
	md, err := parseProtoSchema(def)
	if err != nil {
		t.Fatalf("parseProtoSchema: %v", err)
	}
	return md
}

func TestParseProtoSchemaProto3(t *testing.T) {
	md := mustParseProto(t, `
syntax = "proto3";
package burrow.v1;
option go_package = "example.com/burrow";
// A burrow.
/* With tunnels,
   and dwellers. */
message Burrow {
  option deprecated = false;
  enum Kind { KIND_UNSPECIFIED = 0; DEN = 1; WARREN = 2; }
  message Tunnel {
    string name = 1;
    int64 length = 2;
    Kind kind = 3;
  }
  string name = 1;
  optional int32 depth = 2;
  repeated Tunnel tunnels = 3;
  map<string, Tunnel> by_name = 4;
  Kind kind = 5;
  oneof dweller {
    string fox = 6;
    bytes rabbit = 7;
  }
  reserved 8, 10 to 12, 100 to max;
  reserved "old", "older";
  burrow.v1.Burrow.Kind other_kind = 9;
  .burrow.v1.Burrow.Tunnel main = 13 [deprecated = true, json_name = "mainTunnel", (custom).opt = { a: 1 }];
  repeated double weights = 14;
}`)
	if md.FullName() != "burrow.v1.Burrow" {
		t.Fatalf("FullName = %s", md.FullName())
	}
	f := md.Fields()
	if d := f.ByName("depth"); !d.HasPresence() || d.ContainingOneof() == nil || !d.ContainingOneof().IsSynthetic() {
		t.Errorf("depth: want a proto3 optional field in a synthetic oneof")
	}
	if tun := f.ByName("tunnels"); !tun.IsList() || tun.Message().FullName() != "burrow.v1.Burrow.Tunnel" {
		t.Errorf("tunnels = %v", tun)
	}
	if m := f.ByName("by_name"); !m.IsMap() || m.MapKey().Kind() != protoreflect.StringKind ||
		m.MapValue().Message().FullName() != "burrow.v1.Burrow.Tunnel" {
		t.Errorf("by_name = %v", m)
	}
	if k := f.ByName("kind"); k.Enum() == nil || k.Enum().FullName() != "burrow.v1.Burrow.Kind" {
		t.Errorf("kind = %v", k)
	}
	if k := md.Messages().ByName("Tunnel").Fields().ByName("kind"); k.Enum().FullName() != "burrow.v1.Burrow.Kind" {
		t.Errorf("Tunnel.kind resolves to %v, want the enclosing message's Kind", k.Enum().FullName())
	}
	if o := md.Oneofs().ByName("dweller"); o == nil || o.Fields().Len() != 2 {
		t.Errorf("oneof dweller = %v", o)
	}
	if k := f.ByName("other_kind"); k.Enum().FullName() != "burrow.v1.Burrow.Kind" {
		t.Errorf("other_kind = %v", k)
	}
	if m := f.ByName("main"); m.JSONName() != "mainTunnel" || m.Message().FullName() != "burrow.v1.Burrow.Tunnel" {
		t.Errorf("main = %v", m)
	}
	if w := f.ByName("weights"); !w.IsPacked() {
		t.Errorf("weights: proto3 packs repeated scalars")
	}
	if !md.ReservedRanges().Has(11) || !md.ReservedRanges().Has(maxFieldNumber) || md.ReservedRanges().Has(13) {
		t.Errorf("reserved ranges = %v", md.ReservedRanges())
	}
	if !md.ReservedNames().Has("older") {
		t.Errorf("reserved names = %v", md.ReservedNames())
	}
}

func TestParseProtoSchemaProto2(t *testing.T) {
	md := mustParseProto(t, `
syntax = "proto2";
message Den {
  required string name = 1;
  optional int32 depth = 2 [default = -3];
  optional string motto = 3 [default = "dig\tdeep" " and wide"];
  optional bytes seal = 4 [default = "\001\x02z"];
  optional Size size = 5 [default = LARGE];
  repeated int32 marks = 6 [packed = true];
  enum Size { option allow_alias = true; SMALL = 1; LITTLE = 1; LARGE = 2; }
  optional double ratio = 7 [default = -inf];
  optional uint32 hex = 8 [default = 0x10];
  oneof door { string front = 9; string back = 10; }
}`)
	f := md.Fields()
	if f.ByName("name").Cardinality() != protoreflect.Required {
		t.Errorf("name is not required")
	}
	if got := f.ByName("depth").Default().Int(); got != -3 {
		t.Errorf("depth default = %d", got)
	}
	if got := f.ByName("motto").Default().String(); got != "dig\tdeep and wide" {
		t.Errorf("motto default = %q", got)
	}
	if got := string(f.ByName("seal").Default().Bytes()); got != "\x01\x02z" {
		t.Errorf("seal default = %q", got)
	}
	if got := f.ByName("size").Default().Enum(); got != 2 {
		t.Errorf("size default = %d", got)
	}
	if !f.ByName("marks").IsPacked() {
		t.Errorf("marks is not packed")
	}
	if got := f.ByName("ratio").Default().Float(); !math.IsInf(got, -1) {
		t.Errorf("ratio default = %v", got)
	}
	if got := f.ByName("hex").Default().Uint(); got != 16 {
		t.Errorf("hex default = %d", got)
	}

	// No syntax statement is proto2.
	md = mustParseProto(t, `message M { optional string s = 1; }`)
	if md.ParentFile().Syntax() != protoreflect.Proto2 {
		t.Errorf("syntax = %v, want proto2", md.ParentFile().Syntax())
	}
}

func TestParseProtoSchemaRefuses(t *testing.T) {
	for _, tc := range []struct{ name, def, want string }{
		{"import", `syntax = "proto3"; import "other.proto"; message M {}`, "imports are not supported"},
		{"two messages", `syntax = "proto3"; message A {} message B {}`, "exactly one top-level type"},
		{"top-level enum", `syntax = "proto3"; enum E { Z = 0; } message A { E e = 1; }`, "exactly one top-level type"},
		{"no message", `syntax = "proto3";`, "exactly one top-level type"},
		{"syntax error", `syntax = "proto3"; message M { string name = ; }`, "1:46: expected a number"},
		{"duplicate numbers", `syntax = "proto3"; message M { string a = 1; string b = 1; }`, "conflicting fields"},
		{"unknown type", `syntax = "proto3"; message M { Missing m = 1; }`, `"Missing" is not defined`},
		{"nested unknown", `syntax = "proto3"; message M { message N {} N.X m = 1; }`, `"N.X" resolves to "M.N.X"`},
		{"proto3 required", `syntax = "proto3"; message M { required string a = 1; }`, "required fields are not allowed"},
		{"proto3 default", `syntax = "proto3"; message M { string a = 1 [default = "x"]; }`, "default values are not allowed"},
		{"proto3 enum zero", `syntax = "proto3"; message M { enum E { A = 1; } E e = 1; }`, "zero"},
		{"proto2 no label", `syntax = "proto2"; message M { string a = 1; }`, `expected "required"`},
		{"unknown syntax", `syntax = "proto4"; message M {}`, `unknown syntax "proto4"`},
		{"group", `syntax = "proto2"; message M { optional group G = 1 { } }`, "groups are not supported"},
		{"service", `syntax = "proto3"; message M {} service S {}`, "service is not supported"},
		{"extensions", `syntax = "proto2"; message M { extensions 100 to 200; }`, "extensions is not supported"},
		{"unterminated comment", "syntax = \"proto3\";\nmessage M { /* x }", "2:13: unterminated comment"},
		{"unterminated string", `syntax = "proto3`, "unterminated string"},
		{"float map key", `syntax = "proto3"; message M { map<double, string> m = 1; }`, "a map's key"},
		{"field number", `syntax = "proto3"; message M { string a = 0; }`, "out of range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProtoSchema(tc.def)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseProtoSchema = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateProtoMessage(t *testing.T) {
	md := mustParseProto(t, `syntax = "proto3"; message M { string name = 1; int32 n = 2; map<string, int32> tally = 3; }`)
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString("fox"))
	m.Set(md.Fields().ByName("n"), protoreflect.ValueOfInt32(3))
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProtoMessage(md, b, pubsubpb.Encoding_BINARY); err != nil {
		t.Errorf("BINARY of a valid message: %v", err)
	}
	if err := validateProtoMessage(md, []byte{0x0a, 0x05, 'a'}, pubsubpb.Encoding_BINARY); err == nil {
		t.Errorf("BINARY of truncated bytes passed")
	}
	if err := validateProtoMessage(md, []byte(`{"name":"fox","n":3,"tally":{"a":1}}`), pubsubpb.Encoding_JSON); err != nil {
		t.Errorf("JSON of a valid message: %v", err)
	}
	if err := validateProtoMessage(md, []byte(`{"name":"fox","extra":1}`), pubsubpb.Encoding_JSON); err == nil {
		t.Errorf("JSON with an unknown field passed")
	}
	if err := validateProtoMessage(md, []byte(`{"n":"many"}`), pubsubpb.Encoding_JSON); err == nil {
		t.Errorf("JSON with a wrongly typed field passed")
	}
	if err := validateProtoMessage(md, b, pubsubpb.Encoding_ENCODING_UNSPECIFIED); err == nil {
		t.Errorf("an unspecified encoding passed")
	}

	req := mustParseProto(t, `syntax = "proto2"; message R { required string id = 1; optional int32 n = 2; }`)
	r := dynamicpb.NewMessage(req)
	r.Set(req.Fields().ByName("n"), protoreflect.ValueOfInt32(1))
	b, err = proto.MarshalOptions{AllowPartial: true}.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProtoMessage(req, b, pubsubpb.Encoding_BINARY); err == nil {
		t.Errorf("BINARY without a required field passed")
	}
	if err := validateProtoMessage(req, []byte(`{"n":1}`), pubsubpb.Encoding_JSON); err == nil {
		t.Errorf("JSON without a required field passed")
	}
}
