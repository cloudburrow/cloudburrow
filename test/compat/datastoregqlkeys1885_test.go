//go:build compat

package compat

import (
	"bytes"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestDatastoreNamedDatabaseKeysInGQLCursorsAndErrors (#1885): the front
// keeps a named database in an emulator project of its own, which no
// caller sees. A GQL key literal naming the project
// (KEY(PROJECT('p'), NAMESPACE(...), kind, id), Google's grammar, and a
// DATABASE of the call's database) finds the named database's entity, by
// __key__ and in a key-valued property; a key value bound to a GQL query
// does too; the results' keys and key-valued properties name the project
// and database; a cursor the front answers names neither the emulator
// project nor carries it, and resumes; and an error names the project, not
// the emulator project.
//
// covers: google.datastore.v1.Datastore/RunQuery, google.datastore.v1.Datastore/RunAggregationQuery
func TestDatastoreNamedDatabaseKeysInGQLCursorsAndErrors(t *testing.T) {
	h := New(t)
	dsAddr := h.Endpoint(EnvDatastore)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	project := h.Project()
	db := uniqueDB("gqk")
	if _, err := createDB(t, h, db, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatalf("CreateDatabase %s: %v", db, err)
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	c, err := datastore.NewClientWithDatabase(ctx, project, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	kind := uniqueDB("Gk1885")
	type ent struct {
		V   string
		Ref *datastore.Key
	}
	ada := datastore.NameKey(kind+"C", "ada", nil)
	t1 := datastore.NameKey(kind, "t1", nil)
	t2 := datastore.NameKey(kind, "t2", nil)
	t2.Namespace = "tenant"
	if _, err := c.PutMulti(ctx, []*datastore.Key{ada, t1, datastore.NameKey(kind, "t3", nil), t2},
		[]*ent{{V: "ada"}, {V: "one", Ref: ada}, {V: "three"}, {V: "two"}}); err != nil {
		t.Fatal(err)
	}

	conn, err := grpc.NewClient(dsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := datastorepb.NewDatastoreClient(conn)
	run := func(ns string, g *datastorepb.GqlQuery) (*datastorepb.RunQueryResponse, error) {
		g.AllowLiterals = true
		return raw.RunQuery(inDatabase(ctx, project, db), &datastorepb.RunQueryRequest{ProjectId: project, DatabaseId: db,
			PartitionId: &datastorepb.PartitionId{ProjectId: project, DatabaseId: db, NamespaceId: ns},
			QueryType:   &datastorepb.RunQueryRequest_GqlQuery{GqlQuery: g}})
	}
	names := func(resp *datastorepb.RunQueryResponse) string {
		var out []string
		for _, r := range resp.GetBatch().GetEntityResults() {
			k := r.GetEntity().GetKey()
			if p := k.GetPartitionId(); p.GetProjectId() != project || p.GetDatabaseId() != db {
				t.Errorf("a result's key names %v, want %s/%s", p, project, db)
			}
			if ref := r.GetEntity().GetProperties()["Ref"].GetKeyValue(); ref != nil {
				if p := ref.GetPartitionId(); p.GetProjectId() != project || p.GetDatabaseId() != db {
					t.Errorf("a result's key-valued property names %v, want %s/%s", p, project, db)
				}
			}
			out = append(out, k.GetPath()[len(k.GetPath())-1].GetName())
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct{ ns, q, want string }{
		{"", "SELECT * FROM `" + kind + "` WHERE __key__ = KEY(PROJECT('" + project + "'), `" + kind + "`, 't1')", "t1"},
		{"", "SELECT * FROM `" + kind + "` WHERE Ref = KEY(PROJECT('" + project + "'), `" + kind + "C`, 'ada')", "t1"},
		{"", "SELECT * FROM `" + kind + "` WHERE Ref = KEY(`" + kind + "C`, 'ada')", "t1"},
		{"", "SELECT * FROM `" + kind + "` WHERE __key__ = KEY(PROJECT('" + project + "'), DATABASE('" + db + "'), `" + kind + "`, 't1')", "t1"},
		{"tenant", "SELECT * FROM `" + kind + "` WHERE __key__ = KEY(PROJECT('" + project + "'), NAMESPACE('tenant'), `" + kind + "`, 't2')", "t2"},
	} {
		resp, err := run(c.ns, &datastorepb.GqlQuery{QueryString: c.q})
		if err != nil {
			t.Errorf("GQL %q: %v", c.q, err)
			continue
		}
		if got := names(resp); got != c.want {
			t.Errorf("GQL %q = %q, want %q", c.q, got, c.want)
		}
	}
	// A bound key value.
	resp, err := run("", &datastorepb.GqlQuery{QueryString: "SELECT * FROM `" + kind + "` WHERE __key__ = @k",
		NamedBindings: map[string]*datastorepb.GqlQueryParameter{"k": {ParameterType: &datastorepb.GqlQueryParameter_Value{Value: &datastorepb.Value{
			ValueType: &datastorepb.Value_KeyValue{KeyValue: &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: project, DatabaseId: db},
				Path: []*datastorepb.Key_PathElement{{Kind: kind, IdType: &datastorepb.Key_PathElement_Name{Name: "t1"}}}}}}}}}})
	if err != nil {
		t.Fatalf("GQL with a bound key: %v", err)
	}
	if got := names(resp); got != "t1" {
		t.Errorf("GQL with a bound key = %q, want t1", got)
	}

	// A cursor: the emulator project is not in it, and it resumes.
	resp, err = run("", &datastorepb.GqlQuery{QueryString: "SELECT * FROM `" + kind + "` ORDER BY __key__ LIMIT 1"})
	if err != nil {
		t.Fatal(err)
	}
	cur := resp.GetBatch().GetEndCursor()
	if len(cur) == 0 || bytes.Contains(cur, []byte("cloudburrow-database-")) {
		t.Errorf("the end cursor %q carries the emulator project", cur)
	}
	resp, err = run("", &datastorepb.GqlQuery{QueryString: "SELECT * FROM `" + kind + "` ORDER BY __key__ OFFSET @c",
		NamedBindings: map[string]*datastorepb.GqlQueryParameter{"c": {ParameterType: &datastorepb.GqlQueryParameter_Cursor{Cursor: cur}}}})
	if err != nil {
		t.Fatalf("GQL resumed from the cursor: %v", err)
	}
	if got := names(resp); got != "t3" {
		t.Errorf("GQL resumed from the cursor = %q, want t3", got)
	}

	// An error names the project alone.
	_, err = run("", &datastorepb.GqlQuery{QueryString: "SELECT * FROM `" + kind + "` WHERE __key__ = KEY(PROJECT('" + project + "'), `" + kind + "`)"})
	if err == nil {
		t.Fatal("a key literal with no ID succeeded")
	}
	if msg := status.Convert(err).Message(); strings.Contains(msg, "cloudburrow-database-") {
		t.Errorf("the error names the emulator project: %s", msg)
	}
}
