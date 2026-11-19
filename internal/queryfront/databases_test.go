package queryfront

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func TestEmulatorProjectIsToldBack(t *testing.T) {
	for _, c := range [][2]string{{"demo", "db-2"}, {"a-b-c", "x-1-y"}, {"example.com:p", "abcd"}} {
		ep := EmulatorProject(c[0], c[1])
		if len(ep) <= 30 {
			t.Errorf("EmulatorProject(%q, %q) = %q: a project of Google's could have it", c[0], c[1], ep)
		}
		p, d, ok := FromEmulatorProject(ep)
		if !ok || p != c[0] || d != c[1] {
			t.Errorf("FromEmulatorProject(%q) = %q, %q, %v; want %q, %q", ep, p, d, ok, c[0], c[1])
		}
	}
	for _, ep := range []string{"demo", "cloudburrow-database-", "cloudburrow-database-9-ab-c", "cloudburrow-database-x-ab-c"} {
		if _, _, ok := FromEmulatorProject(ep); ok {
			t.Errorf("FromEmulatorProject(%q) is a database's", ep)
		}
	}
}

func key(project, database, ns, kind, name string) *pb.Key {
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: project, DatabaseId: database, NamespaceId: ns},
		Path: []*pb.Key_PathElement{{Kind: kind, IdType: &pb.Key_PathElement_Name{Name: name}}}}
}

func TestMapRequestKeepsANamedDatabaseInItsProject(t *testing.T) {
	ref := key("demo", "", "", "Other", "o") // a key value naming the default database
	req := &pb.CommitRequest{ProjectId: "demo", DatabaseId: "db-a", Mutations: []*pb.Mutation{
		{Operation: &pb.Mutation_Upsert{Upsert: &pb.Entity{Key: key("demo", "db-a", "ns", "Task", "x"),
			Properties: map[string]*pb.Value{"ref": {ValueType: &pb.Value_KeyValue{KeyValue: ref}}}}}},
		{Operation: &pb.Mutation_Delete{Delete: key("demo", "", "", "Task", "y")}},
	}}
	out, ok := mapRequest(req)
	if !ok {
		t.Fatal("a named database's call was not mapped")
	}
	ep := EmulatorProject("demo", "db-a")
	if out.GetProjectId() != ep || out.GetDatabaseId() != "" {
		t.Fatalf("request = %s/%q, want %s/\"\"", out.GetProjectId(), out.GetDatabaseId(), ep)
	}
	up := out.GetMutations()[0].GetUpsert()
	if p := up.GetKey().GetPartitionId(); p.GetProjectId() != ep || p.GetDatabaseId() != "" || p.GetNamespaceId() != "ns" {
		t.Errorf("entity key partition = %v", p)
	}
	if p := out.GetMutations()[1].GetDelete().GetPartitionId(); p.GetProjectId() != ep {
		t.Errorf("a call key with no database = %v, want the call's database", p)
	}
	if p := up.GetProperties()["ref"].GetKeyValue().GetPartitionId(); p.GetProjectId() != "demo" || p.GetDatabaseId() != "" {
		t.Errorf("a key value naming the default database = %v, want it unchanged", p)
	}
	if req.GetProjectId() != "demo" || req.GetMutations()[0].GetUpsert().GetKey().GetPartitionId().GetDatabaseId() != "db-a" {
		t.Error("the caller's request was changed")
	}

	resp := &pb.CommitResponse{MutationResults: []*pb.MutationResult{{Key: key(ep, "", "ns", "Task", "x")}}}
	mapResponse(resp)
	if p := resp.GetMutationResults()[0].GetKey().GetPartitionId(); p.GetProjectId() != "demo" || p.GetDatabaseId() != "db-a" || p.GetNamespaceId() != "ns" {
		t.Errorf("answer key partition = %v, want demo/db-a/ns", p)
	}

	def := &pb.LookupRequest{ProjectId: "demo", Keys: []*pb.Key{key("demo", "", "", "Task", "x")}}
	if out, ok := mapRequest(def); ok || !proto.Equal(out, def) {
		t.Error("a default database's call was mapped")
	}
}

type recordingClient struct {
	pb.DatastoreClient
	got *pb.RunQueryRequest
	md  metadata.MD
}

func (r *recordingClient) RunQuery(ctx context.Context, in *pb.RunQueryRequest, _ ...grpc.CallOption) (*pb.RunQueryResponse, error) {
	r.got = in
	r.md, _ = metadata.FromOutgoingContext(ctx)
	return &pb.RunQueryResponse{Batch: &pb.QueryResultBatch{EntityResults: []*pb.EntityResult{
		{Entity: &pb.Entity{Key: key(in.GetProjectId(), "", "", "Task", "x")}}}}}, nil
}

func TestDatabasesClientRoutesAQueryToItsDatabase(t *testing.T) {
	rec := &recordingClient{}
	c := DatabasesClient(rec)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-goog-request-params", "project_id=demo&database_id=db-b")
	resp, err := c.RunQuery(ctx, &pb.RunQueryRequest{ProjectId: "demo", DatabaseId: "db-b",
		PartitionId: &pb.PartitionId{ProjectId: "demo", DatabaseId: "db-b"},
		QueryType:   &pb.RunQueryRequest_Query{Query: &pb.Query{Kind: []*pb.KindExpression{{Name: "Task"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	ep := EmulatorProject("demo", "db-b")
	if rec.got.GetProjectId() != ep || rec.got.GetPartitionId().GetProjectId() != ep || rec.got.GetPartitionId().GetDatabaseId() != "" {
		t.Errorf("the emulator was asked %v", rec.got)
	}
	if h := rec.md.Get("x-goog-request-params"); len(h) != 1 || h[0] != "project_id="+ep {
		t.Errorf("routing header = %q", h)
	}
	if p := resp.GetBatch().GetEntityResults()[0].GetEntity().GetKey().GetPartitionId(); p.GetProjectId() != "demo" || p.GetDatabaseId() != "db-b" {
		t.Errorf("answer key = %v", p)
	}
}

func TestNamesDatabase(t *testing.T) {
	cases := []struct {
		name, path, ctype, header, body, want string
	}{
		{"rest json", "/v1/projects/demo:runQuery", "application/json", "", `{"databaseId":"db-a"}`, "projects/demo/databases/db-a"},
		{"rest default", "/v1/projects/demo:runQuery", "application/json", "", `{"databaseId":"(default)"}`, ""},
		{"rest none", "/v1/projects/demo:lookup", "application/json", "", `{}`, ""},
		{"rest admin", "/v1/projects/demo:export", "application/json", "", `{"databaseId":"db-a"}`, ""},
		{"grpc header", "/google.datastore.v1.Datastore/Lookup", "application/grpc", "project_id=demo&database_id=db-a", "", "projects/demo/databases/db-a"},
		{"grpc default", "/google.datastore.v1.Datastore/Lookup", "application/grpc", "project_id=demo", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		r.Header.Set("Content-Type", c.ctype)
		if c.header != "" {
			r.Header.Set("X-Goog-Request-Params", c.header)
		}
		if got := namesDatabase(r); got != c.want {
			t.Errorf("%s: namesDatabase = %q, want %q", c.name, got, c.want)
		}
	}
	// A gRPC call naming its database in its body alone.
	msg, _ := proto.Marshal(&pb.LookupRequest{ProjectId: "demo", DatabaseId: "db-c"})
	frame := append([]byte{0, 0, 0, 0, byte(len(msg))}, msg...)
	r := httptest.NewRequest(http.MethodPost, "/google.datastore.v1.Datastore/Lookup", strings.NewReader(string(frame)))
	r.Header.Set("Content-Type", "application/grpc")
	if got := namesDatabase(r); got != "projects/demo/databases/db-c" {
		t.Errorf("grpc body: namesDatabase = %q", got)
	}
}
