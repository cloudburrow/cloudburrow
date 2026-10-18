package firestoreadmin_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
)

// pipelineFixture is an Admin API in front of a stand-in emulator, with an
// Enterprise database "ent" holding a few documents, and the official
// client's pipelines pointed at the Admin API.
func pipelineFixture(t *testing.T) (ent, std *firestore.Client) {
	t.Helper()
	ctx := context.Background()
	_, docsAddr, _, _ := fstest.Serve(t)
	a := firestoreadmin.New(docsAddr)
	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.GRPCServer().Serve(al) }()
	t.Cleanup(a.GRPCServer().Stop)
	if _, err := a.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "entdb",
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}}); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(docsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	put := func(path string, fields map[string]*firestorepb.Value) {
		_, err := firestorepb.NewFirestoreClient(conn).Commit(ctx, &firestorepb.CommitRequest{Writes: []*firestorepb.Write{{
			Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: "projects/p/databases/entdb/documents/" + path, Fields: fields}}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *firestorepb.Value {
		return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: s}}
	}
	num := func(n int64) *firestorepb.Value {
		return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: n}}
	}
	arr := func(vs ...*firestorepb.Value) *firestorepb.Value {
		return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{ArrayValue: &firestorepb.ArrayValue{Values: vs}}}
	}
	put("books/a", map[string]*firestorepb.Value{"title": str("Alpha"), "genre": str("sf"), "rating": num(5), "tags": arr(str("x"), str("y"))})
	put("books/b", map[string]*firestorepb.Value{"title": str("Beta"), "genre": str("sf"), "rating": num(3), "tags": arr(str("y"))})
	put("books/c", map[string]*firestorepb.Value{"title": str("Gamma"), "genre": str("poetry"), "rating": num(4)})
	put("shelves/s1/books/d", map[string]*firestorepb.Value{"title": str("Delta"), "genre": str("poetry"), "rating": num(1)})

	t.Setenv("FIRESTORE_EMULATOR_HOST", al.Addr().String())
	ent, err = firestore.NewClientWithDatabase(ctx, "p", "entdb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ent.Close() })
	std, err = firestore.NewClient(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = std.Close() })
	return ent, std
}

func titles(t *testing.T, rs []*firestore.PipelineResult) string {
	t.Helper()
	var out []string
	for _, r := range rs {
		d := r.Data()
		out = append(out, d["title"].(string))
	}
	return strings.Join(out, ",")
}

func TestPipelineStages(t *testing.T) {
	ctx := context.Background()
	c, std := pipelineFixture(t)

	got, err := c.Pipeline().Collection("books").
		Where(firestore.GreaterThan(firestore.FieldOf("rating"), 3)).
		Sort(firestore.Orders(firestore.Descending(firestore.FieldOf("rating")))).
		Execute(ctx).Results().GetAll()
	if err != nil {
		t.Fatalf("where and sort: %v", err)
	}
	if s := titles(t, got); s != "Alpha,Gamma" {
		t.Errorf("where rating > 3, by rating descending = %s, want Alpha,Gamma", s)
	}
	if got[0].Ref() == nil || got[0].Ref().ID != "a" {
		t.Errorf("result ref = %v, want books/a", got[0].Ref())
	}

	got, err = c.Pipeline().CollectionGroup("books").
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("rating")))).
		Offset(1).Limit(2).
		Execute(ctx).Results().GetAll()
	if err != nil {
		t.Fatalf("collection group: %v", err)
	}
	if s := titles(t, got); s != "Beta,Gamma" {
		t.Errorf("collection group by rating, offset 1, limit 2 = %s, want Beta,Gamma", s)
	}

	got, err = c.Pipeline().Collection("books").
		Aggregate(firestore.Accumulators(firestore.CountAll().As("n"), firestore.Average("rating").As("avg")),
			firestore.WithAggregateGroups("genre")).
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("genre")))).
		Execute(ctx).Results().GetAll()
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("aggregate by genre = %d rows, want 2", len(got))
	}
	if d := got[1].Data(); d["genre"] != "sf" || d["n"] != int64(2) || d["avg"] != 4.0 {
		t.Errorf("aggregate sf = %v, want n 2, avg 4", d)
	}

	got, err = c.Pipeline().Collection("books").
		Select(firestore.Fields("title", firestore.ToUpper(firestore.FieldOf("title")).As("loud"))).
		Where(firestore.Equal(firestore.FieldOf("loud"), "BETA")).
		Execute(ctx).Results().GetAll()
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 || got[0].Data()["loud"] != "BETA" || len(got[0].Data()) != 2 {
		t.Errorf("select = %v", got)
	}

	got, err = c.Pipeline().Collection("books").
		UnnestWithAlias("tags", "tag").
		Where(firestore.Equal(firestore.FieldOf("tag"), "y")).
		Execute(ctx).Results().GetAll()
	if err != nil {
		t.Fatalf("unnest: %v", err)
	}
	if s := titles(t, got); s != "Alpha,Beta" {
		t.Errorf("unnest tags where tag == y = %s, want Alpha,Beta", s)
	}

	// Explain stats, in analyze mode, arrive with the results.
	snap := c.Pipeline().Collection("books").Limit(1).Execute(ctx, firestore.WithExplainMode(firestore.ExplainModeAnalyze))
	if _, err := snap.Results().GetAll(); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	text, err := snap.ExplainStats().Text()
	if err != nil || !strings.Contains(text, "results returned: 1") {
		t.Errorf("explain stats = %q, %v", text, err)
	}

	// A Standard database refuses pipelines, as Google does.
	_, err = std.Pipeline().Collection("books").Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "requires the Database Edition to be `enterprise`") {
		t.Errorf("pipeline on (default) = %v, want INVALID_ARGUMENT", err)
	}
	// The subcollection stage stands in a subquery alone.
	_, err = c.Pipeline().Collection("books").RawStage("subcollection", []any{"x"}).Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "within a subquery") {
		t.Errorf("subcollection stage = %v, want INVALID_ARGUMENT: within a subquery", err)
	}
	// Full-text search stays refused, saying why.
	_, err = c.Pipeline().Collection("books").Search(firestore.WithSearchQuery("alpha")).Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "full-text search") {
		t.Errorf("full-text search = %v, want UNIMPLEMENTED", err)
	}
}
