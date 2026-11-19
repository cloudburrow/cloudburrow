package queryfront

import (
	"bytes"
	"testing"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/encoding/protowire"
)

// emulatorCursor is a cursor shaped as the emulator's (measured): a
// message holding a message holding the reference, whose field 13 is the
// project and whose path names an entity.
func emulatorCursor(project, name string) []byte {
	var path []byte
	path = protowire.AppendTag(path, 2, protowire.BytesType)
	path = protowire.AppendString(path, "Task")
	path = protowire.AppendTag(path, 4, protowire.BytesType)
	path = protowire.AppendString(path, name)
	var ref []byte
	ref = protowire.AppendTag(ref, 13, protowire.BytesType)
	ref = protowire.AppendString(ref, project)
	ref = protowire.AppendTag(ref, 14, protowire.BytesType)
	ref = protowire.AppendBytes(ref, path)
	var mid []byte
	mid = protowire.AppendTag(mid, 2, protowire.BytesType)
	mid = protowire.AppendBytes(mid, ref)
	mid = protowire.AppendTag(mid, 3, protowire.VarintType)
	mid = protowire.AppendVarint(mid, 0)
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	return protowire.AppendBytes(out, mid)
}

func TestCursorsNameTheProjectNotTheEmulatorProject(t *testing.T) {
	ep := EmulatorProject("demo", "db-a")
	// A key named as the project stays a key name.
	in := emulatorCursor(ep, "demo")
	got := rewriteCursor(in, ep, "demo")
	if want := emulatorCursor("demo", "demo"); !bytes.Equal(got, want) {
		t.Errorf("rewriteCursor = %q, want %q", got, want)
	}
	if back := rewriteCursor(got, "demo", ep); !bytes.Equal(back, in) {
		t.Errorf("rewritten back = %q, want %q", back, in)
	}
	if odd := []byte("not a cursor \xff"); !bytes.Equal(rewriteCursor(odd, ep, "demo"), odd) {
		t.Error("a cursor that is no protobuf changed")
	}

	resp := &pb.RunQueryResponse{Batch: &pb.QueryResultBatch{EndCursor: emulatorCursor(ep, "x"),
		EntityResults: []*pb.EntityResult{{Cursor: emulatorCursor(ep, "x")}}}}
	rewriteCursors(resp.ProtoReflect(), ep, "demo")
	if !bytes.Equal(resp.Batch.EndCursor, emulatorCursor("demo", "x")) || !bytes.Equal(resp.Batch.EntityResults[0].Cursor, emulatorCursor("demo", "x")) {
		t.Errorf("the answer's cursors = %q, %q", resp.Batch.EndCursor, resp.Batch.EntityResults[0].Cursor)
	}
	req := &pb.RunQueryRequest{ProjectId: "demo", DatabaseId: "db-a", QueryType: &pb.RunQueryRequest_GqlQuery{GqlQuery: &pb.GqlQuery{
		QueryString: "SELECT * FROM Task OFFSET @c", NamedBindings: map[string]*pb.GqlQueryParameter{
			"c": {ParameterType: &pb.GqlQueryParameter_Cursor{Cursor: emulatorCursor("demo", "x")}}}}}}
	out, _ := mapRequest(req)
	if c := out.GetGqlQuery().GetNamedBindings()["c"].GetCursor(); !bytes.Equal(c, emulatorCursor(ep, "x")) {
		t.Errorf("the call's bound cursor = %q", c)
	}
}
