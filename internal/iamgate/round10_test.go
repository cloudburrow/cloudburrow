package iamgate

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	dspb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	// The DatabaseAdmin service's descriptor, which the gate decodes by.
	_ "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/protobuf/proto"
)

func perms(cs []Check) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Permission+"@"+c.Resource)
	}
	return strings.Join(out, ",")
}

func TestDatastoreRules(t *testing.T) {
	const db = "projects/p/databases/(default)"
	key := func(kind string) *dspb.Key {
		return &dspb.Key{Path: []*dspb.Key_PathElement{{Kind: kind, IdType: &dspb.Key_PathElement_Name{Name: "x"}}}}
	}
	query := func(kind string, keysOnly bool) *dspb.RunQueryRequest {
		q := &dspb.Query{}
		if kind != "" {
			q.Kind = []*dspb.KindExpression{{Name: kind}}
		}
		if keysOnly {
			q.Projection = []*dspb.Projection{{Property: &dspb.PropertyReference{Name: "__key__"}}}
		}
		return &dspb.RunQueryRequest{ProjectId: "p", QueryType: &dspb.RunQueryRequest_Query{Query: q}}
	}
	gql := func(s string) *dspb.RunQueryRequest {
		return &dspb.RunQueryRequest{ProjectId: "p", DatabaseId: "named", QueryType: &dspb.RunQueryRequest_GqlQuery{GqlQuery: &dspb.GqlQuery{QueryString: s}}}
	}
	ent := &dspb.Entity{Key: key("K")}
	for _, c := range []struct {
		method string
		req    proto.Message
		want   string
	}{
		{"Lookup", &dspb.LookupRequest{ProjectId: "p", Keys: []*dspb.Key{key("K")}}, "datastore.entities.get@" + db},
		{"Lookup", &dspb.LookupRequest{ProjectId: "p", Keys: []*dspb.Key{key("__Stat_Total__")}}, "datastore.entities.get@" + db + ",datastore.statistics.get@" + db},
		{"RunQuery", query("K", false), "datastore.entities.get@" + db + ",datastore.entities.list@" + db},
		{"RunQuery", query("K", true), "datastore.entities.list@" + db},
		{"RunQuery", query("__namespace__", true), "datastore.namespaces.get@" + db + ",datastore.namespaces.list@" + db},
		{"RunQuery", query("__Stat_Kind__", false), "datastore.statistics.get@" + db + ",datastore.statistics.list@" + db},
		{"RunQuery", query("", false), "datastore.entities.get@" + db + ",datastore.entities.list@" + db + ",datastore.statistics.get@" + db + ",datastore.statistics.list@" + db},
		{"RunQuery", gql("SELECT __key__ FROM K"), "datastore.entities.list@projects/p/databases/named"},
		{"RunQuery", gql("select * from `K` where a = 1"), "datastore.entities.get@projects/p/databases/named,datastore.entities.list@projects/p/databases/named"},
		{"RunAggregationQuery", &dspb.RunAggregationQueryRequest{ProjectId: "p", QueryType: &dspb.RunAggregationQueryRequest_AggregationQuery{
			AggregationQuery: &dspb.AggregationQuery{QueryType: &dspb.AggregationQuery_NestedQuery{NestedQuery: &dspb.Query{Kind: []*dspb.KindExpression{{Name: "K"}}}}}}},
			"datastore.entities.get@" + db + ",datastore.entities.list@" + db},
		{"Commit", &dspb.CommitRequest{ProjectId: "p"}, "datastore.databases.get@" + db},
		{"Commit", &dspb.CommitRequest{ProjectId: "p", Mutations: []*dspb.Mutation{{Operation: &dspb.Mutation_Insert{Insert: ent}}}}, "datastore.entities.create@" + db},
		{"Commit", &dspb.CommitRequest{ProjectId: "p", Mutations: []*dspb.Mutation{{Operation: &dspb.Mutation_Upsert{Upsert: ent}},
			{Operation: &dspb.Mutation_Delete{Delete: key("K")}}}}, "datastore.entities.create@" + db + ",datastore.entities.update@" + db + ",datastore.entities.delete@" + db},
		{"Commit", &dspb.CommitRequest{ProjectId: "p", Mutations: []*dspb.Mutation{{Operation: &dspb.Mutation_Update{Update: ent}}}}, "datastore.entities.update@" + db},
		{"AllocateIds", &dspb.AllocateIdsRequest{ProjectId: "p"}, "datastore.entities.allocateIds@" + db},
		{"ReserveIds", &dspb.ReserveIdsRequest{ProjectId: "p"}, "datastore.entities.allocateIds@" + db},
		{"BeginTransaction", &dspb.BeginTransactionRequest{ProjectId: "p"}, "datastore.databases.get@" + db},
	} {
		if got := perms(checks(t, Datastore, dsData+c.method, c.req)); got != c.want {
			t.Errorf("%s %v = %s, want %s", c.method, c.req, got, c.want)
		}
	}
	got := perms(checks(t, Datastore, dsAdmin+"ExportEntities", &dsadminpb.ExportEntitiesRequest{ProjectId: "p"}))
	if got != "datastore.databases.export@projects/p/databases/"+Routed {
		t.Errorf("ExportEntities = %s", got)
	}
	got = perms(checks(t, Datastore, fsOps+"GetOperation", &longrunningpb.GetOperationRequest{Name: "projects/p/operations/o"}))
	if got != "datastore.operations.get@projects/p/databases/"+Routed {
		t.Errorf("GetOperation = %s", got)
	}
}

func TestFirestorePipeline(t *testing.T) {
	const db = "projects/p/databases/(default)"
	stage := func(name string, args ...*firestorepb.Value) *firestorepb.Pipeline_Stage {
		return &firestorepb.Pipeline_Stage{Name: name, Args: args}
	}
	sub := &firestorepb.Value{ValueType: &firestorepb.Value_PipelineValue{PipelineValue: &firestorepb.Pipeline{
		Stages: []*firestorepb.Pipeline_Stage{stage("documents")}}}}
	req := &firestorepb.ExecutePipelineRequest{Database: db, PipelineType: &firestorepb.ExecutePipelineRequest_StructuredPipeline{
		StructuredPipeline: &firestorepb.StructuredPipeline{Pipeline: &firestorepb.Pipeline{Stages: []*firestorepb.Pipeline_Stage{
			stage("collection"), stage("where", sub), stage("delete")}}}}}
	if got, want := perms(checks(t, Firestore, fsData+"ExecutePipeline", req)),
		"datastore.entities.get@"+db+",datastore.entities.list@"+db+",datastore.entities.delete@"+db; got != want {
		t.Errorf("ExecutePipeline = %s, want %s", got, want)
	}
}

func TestOperationRules(t *testing.T) {
	const db = "projects/p/instances/i/databases/d"
	for _, c := range []struct {
		svc  *Service
		full string
		req  proto.Message
		want string
	}{
		{Spanner, spOps + "GetOperation", &longrunningpb.GetOperationRequest{Name: db + "/operations/o"}, "spanner.databaseOperations.get@" + db},
		{Spanner, spOps + "ListOperations", &longrunningpb.ListOperationsRequest{Name: db + "/operations"}, "spanner.databaseOperations.list@" + db},
		{Spanner, spOps + "DeleteOperation", &longrunningpb.DeleteOperationRequest{Name: db + "/operations/o"}, "spanner.databaseOperations.cancel@" + db},
		{Spanner, spOps + "CancelOperation", &longrunningpb.CancelOperationRequest{Name: "projects/p/instances/i/operations/o"}, "spanner.instanceOperations.cancel@projects/p/instances/i"},
		{Spanner, spOps + "DeleteOperation", &longrunningpb.DeleteOperationRequest{Name: "projects/p/instances/i/operations/o"}, "spanner.instanceOperations.delete@projects/p/instances/i"},
		{Spanner, spOps + "GetOperation", &longrunningpb.GetOperationRequest{Name: "projects/p/instances/i/backups/b/operations/o"}, "spanner.backupOperations.get@projects/p/instances/i/backups/b"},
		{Bigtable, btOps + "GetOperation", &longrunningpb.GetOperationRequest{Name: "operations/projects/p/instances/i/tables/t/operations/x"}, "bigtable.tables.get@projects/p/instances/i/tables/t"},
		{Bigtable, btOps + "CancelOperation", &longrunningpb.CancelOperationRequest{Name: "operations/projects/p/instances/i/operations/x"}, "bigtable.instances.update@projects/p/instances/i"},
		{Bigtable, btData + "ReadChangeStream", &btpb.ReadChangeStreamRequest{TableName: "projects/p/instances/i/tables/t"}, "bigtable.tables.readRows@projects/p/instances/i/tables/t"},
		{Bigtable, btTables + "CreateSchemaBundle", &adminpb.CreateSchemaBundleRequest{Parent: "projects/p/instances/i/tables/t"}, "bigtable.schemaBundles.create@projects/p/instances/i/tables/t"},
		{Bigtable, btTables + "GetSnapshot", &adminpb.GetSnapshotRequest{Name: "projects/p/instances/i/clusters/c/snapshots/s"}, "bigtable.backups.get@projects/p/instances/i/clusters/c"},
	} {
		if got := perms(checks(t, c.svc, c.full, c.req)); got != c.want {
			t.Errorf("%s %v = %s, want %s", c.full, c.req, got, c.want)
		}
	}
}

// TestWrapStream: a client stream's later messages are each checked; a
// refused one never reaches the emulator, which is ended, and the call's
// status is the refusal.
func TestWrapStream(t *testing.T) {
	c, _ := fakeHost(t, "datastore.entities.update")
	g := &Gate{Client: c, Services: []*Service{Firestore}}
	const db = "projects/p/databases/(default)"
	upd := &firestorepb.WriteRequest{Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Update{
		Update: &firestorepb.Document{Name: db + "/documents/c/x"}}, CurrentDocument: &firestorepb.Precondition{
		ConditionType: &firestorepb.Precondition_Exists{Exists: true}}}}}
	del := &firestorepb.WriteRequest{Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Delete{Delete: db + "/documents/c/x"}}}}
	first := grpcBody(&firestorepb.WriteRequest{Database: db})
	var seen []byte
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("answer"))
		seen, _ = io.ReadAll(r.Body)
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
	}))
	call := func(body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, fsData+"Write", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/grpc")
		r.Header.Set("Authorization", "Bearer granted")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ok := append(append([]byte{}, first...), grpcBody(upd)...)
	if w := call(ok); !bytes.Equal(seen, ok) || w.Header().Get(http.TrailerPrefix+"Grpc-Status") != "0" {
		t.Errorf("granted stream: emulator saw %d of %d bytes, trailers %v", len(seen), len(ok), w.Header())
	}
	body := append(append([]byte{}, ok...), grpcBody(del)...)
	w := call(body)
	if !bytes.Equal(seen, ok) {
		t.Errorf("refused delete reached the emulator: saw %d bytes, want %d", len(seen), len(ok))
	}
	if w.Header().Get(http.TrailerPrefix+"Grpc-Status") != "7" || !strings.Contains(w.Header().Get(http.TrailerPrefix+"Grpc-Message"), "insufficient") {
		t.Errorf("refused stream trailers = %v", w.Header())
	}
}

func TestRoutedDatabase(t *testing.T) {
	if got := routedDatabase(withRouting(context.Background(), "project_id=p&database_id=named")); got != "named" {
		t.Errorf("routed = %q", got)
	}
	if got := routedDatabase(context.Background()); got != "(default)" {
		t.Errorf("unrouted = %q", got)
	}
}

// TestWrapTestIamPermissions: a caller's TestIamPermissions is answered
// with what the evaluator says it holds; the owner's goes to the front.
func TestWrapTestIamPermissions(t *testing.T) {
	c, _ := fakeHost(t, "spanner.databases.select")
	g := &Gate{Client: c, Services: []*Service{Spanner}}
	passed := false
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { passed = true }))
	body := grpcBody(&iampb.TestIamPermissionsRequest{Resource: "projects/p/instances/i/databases/d",
		Permissions: []string{"spanner.databases.select", "spanner.databases.write"}})
	call := func(bearer string) *httptest.ResponseRecorder {
		passed = false
		r := httptest.NewRequest(http.MethodPost, spDBAdmin+"TestIamPermissions", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/grpc")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call("granted")
	b := w.Body.Bytes()
	var got iampb.TestIamPermissionsResponse
	if passed || len(b) < 5 || proto.Unmarshal(b[5:], &got) != nil || strings.Join(got.GetPermissions(), ",") != "spanner.databases.select" ||
		w.Header().Get("Grpc-Status") != "0" {
		t.Errorf("granted: passed %v, answer %v, header %v", passed, &got, w.Header())
	}
	if call(""); !passed {
		t.Error("the owner's TestIamPermissions did not reach the front")
	}
}
