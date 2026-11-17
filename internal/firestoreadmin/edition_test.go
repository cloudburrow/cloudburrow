package firestoreadmin

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func fsMapOf(k string, v *firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: map[string]*firestorepb.Value{k: v}}}}
}

func fsArrayOf(vs ...*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{ArrayValue: &firestorepb.ArrayValue{Values: vs}}}
}

var (
	fsInt64 = &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 5}}
	fsStr   = &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: "507f1f77bcf86cd799439011"}}
	fsNullV = &firestorepb.Value{ValueType: &firestorepb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}}
	fsBytes = func(b ...byte) *firestorepb.Value {
		return &firestorepb.Value{ValueType: &firestorepb.Value_BytesValue{BytesValue: b}}
	}
	fsEmpty  = &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{}}}
	fsInt32V = fsMapOf("__int__", fsInt64)
)

// TestEnterpriseValueOnStandard (#1703): the values "Supported data types"
// marks Enterprise-only, and an array in an array, are refused as the
// Standard emulator refuses them; malformed ones and the rest pass.
func TestEnterpriseValueOnStandard(t *testing.T) {
	cases := []struct {
		name string
		v    *firestorepb.Value
		want string
	}{
		{"int32", fsInt32V, "Value type INT is not allowed"},
		{"objectId", fsMapOf("__oid__", fsStr), "Value type OBJECT_ID is not allowed"},
		{"decimal128", fsMapOf("__decimal128__", fsStr), "Value type DECIMAL128 is not allowed"},
		{"bson timestamp", fsMapOf("__request_timestamp__", fsEmpty), "Value type REQUEST_TIMESTAMP is not allowed"},
		{"regex", fsMapOf("__regex__", fsEmpty), "Value type REGEX is not allowed"},
		{"min key", fsMapOf("__min__", fsNullV), "Value type MIN_KEY is not allowed"},
		{"max key", fsMapOf("__max__", fsNullV), "Value type MAX_KEY is not allowed"},
		{"bytes with subtype", fsMapOf("__binary__", fsBytes(5, 1)), "Value type BYTES_WITH_SUBTYPE is not allowed"},
		{"in an array", fsArrayOf(fsInt32V), "Value type INT is not allowed"},
		{"in a map", fsMapOf("a", fsInt32V), "Value type INT is not allowed"},
		{"nested array", fsArrayOf(fsArrayOf(fsInt64)), "Nested arrays are not allowed"},
		// Left to the emulator, which refuses each in both editions.
		{"int not an integer", fsMapOf("__int__", fsStr), ""},
		{"int out of range", fsMapOf("__int__", &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 1 << 31}}), ""},
		{"binary of subtype 0", fsMapOf("__binary__", fsBytes(0, 1)), ""},
		{"empty binary", fsMapOf("__binary__", fsBytes()), ""},
		// Standard's own.
		{"int64", fsInt64, ""},
		{"array of maps of arrays", fsArrayOf(fsMapOf("a", fsArrayOf(fsInt64))), ""},
		{"two keys", &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: map[string]*firestorepb.Value{"__int__": fsInt64, "b": fsInt64}}}}, ""},
	}
	for _, c := range cases {
		req := &firestorepb.CommitRequest{Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Update{
			Update: &firestorepb.Document{Name: "projects/p/databases/(default)/documents/c/d", Fields: map[string]*firestorepb.Value{"v": c.v}}}}}}
		err := checkStandardValues(req.ProtoReflect())
		got := ""
		if err != nil {
			got = err.Error()
		}
		if (c.want == "") != (err == nil) || !strings.Contains(got, c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// A transform's operand: arrayUnion of an array or of a 32-bit integer.
	for _, op := range []*firestorepb.Value{fsArrayOf(fsInt64), fsInt32V} {
		req := &firestorepb.CommitRequest{Writes: []*firestorepb.Write{{
			Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: "projects/p/databases/(default)/documents/c/d"}},
			UpdateTransforms: []*firestorepb.DocumentTransform_FieldTransform{{FieldPath: "v",
				TransformType: &firestorepb.DocumentTransform_FieldTransform_AppendMissingElements{AppendMissingElements: &firestorepb.ArrayValue{Values: []*firestorepb.Value{op}}}}}}}}
		if err := checkStandardValues(req.ProtoReflect()); err == nil {
			t.Errorf("arrayUnion(%v) passed", op)
		}
	}
}

func grpcFrame(m proto.Message) []byte {
	b, _ := proto.Marshal(m)
	out := make([]byte, 5, 5+len(b))
	binary.BigEndian.PutUint32(out[1:], uint32(len(b)))
	return append(out, b...)
}

// TestEditionGuardClaims: the guard claims a Standard database's write
// carrying an Enterprise-only value, over gRPC and REST, and nothing on
// an Enterprise database; a call it does not claim keeps its body.
func TestEditionGuardClaims(t *testing.T) {
	a := New("")
	a.mu.Lock()
	a.seed("p")
	a.databases["projects/p/databases/ent"] = &adminpb.Database{Name: "projects/p/databases/ent", DatabaseEdition: adminpb.Database_ENTERPRISE}
	a.mu.Unlock()
	g := a.EditionGuard()
	commit := func(db string) *firestorepb.CommitRequest {
		return &firestorepb.CommitRequest{Database: db, Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Update{
			Update: &firestorepb.Document{Name: db + "/documents/c/d", Fields: map[string]*firestorepb.Value{"v": fsInt32V}}}}}}
	}
	grpcReq := func(db string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/google.firestore.v1.Firestore/Commit", bytes.NewReader(grpcFrame(commit(db))))
		r.Header.Set("Content-Type", "application/grpc")
		return r
	}
	if !g.Claims(grpcReq("projects/p/databases/(default)")) {
		t.Error("a Standard database's gRPC Commit of a 32-bit integer was not claimed")
	}
	r := grpcReq("projects/p/databases/ent")
	if g.Claims(r) {
		t.Error("an Enterprise database's gRPC Commit was claimed")
	}
	if b, _ := io.ReadAll(r.Body); !bytes.Equal(b, grpcFrame(commit("projects/p/databases/ent"))) {
		t.Error("the unclaimed call's body was not put back")
	}

	doc, _ := protojson.Marshal(&firestorepb.Document{Fields: map[string]*firestorepb.Value{"v": fsArrayOf(fsArrayOf(fsInt64))}})
	rest := httptest.NewRequest(http.MethodPatch, "/v1/projects/p/databases/(default)/documents/c/d", bytes.NewReader(doc))
	if !g.Claims(rest) {
		t.Fatal("a Standard database's REST patch of a nested array was not claimed")
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, rest)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Nested arrays are not allowed") || !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
		t.Errorf("REST refusal = %d %s", w.Code, w.Body)
	}
	rest = httptest.NewRequest(http.MethodPatch, "/v1/projects/p/databases/ent/documents/c/d", bytes.NewReader(doc))
	if g.Claims(rest) {
		t.Error("an Enterprise database's REST patch was claimed")
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/projects/p/databases/(default)/documents/c/d", nil)
	if g.Claims(get) {
		t.Error("a GET was claimed")
	}
}
