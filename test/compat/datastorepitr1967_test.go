//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type pitrEntity struct {
	V int64
}

// TestDatastorePointInTimeReads (#1967): the Datastore front keeps the
// versions of the entities, so the official Go client reads a Datastore
// mode database (with point-in-time recovery) as it was at an earlier
// read_time: an overwritten entity as it was, a deleted one still there,
// one written since not yet; queries, with the !=, IN and OR filters the
// front splits, GQL and COUNT/SUM aggregations, at that time; a read-only
// transaction WithReadTime; a read before the database's earliest version
// time refused FAILED_PRECONDITION; an export at a snapshot time, imported
// into another database, and a clone of that minute, holding the entities
// as they were then. The default database's entities are versioned too.
//
// covers: google.datastore.v1.Datastore/Lookup, google.datastore.v1.Datastore/RunQuery, google.datastore.v1.Datastore/RunAggregationQuery, google.datastore.v1.Datastore/BeginTransaction
func TestDatastorePointInTimeReads(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvDatastore)
	fa := firestoreAdmin(t, h)   // asks for CLOUDBURROW_TEST_FIRESTORE
	sc := firestoreStorage(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE_STORAGE
	t.Setenv("DATASTORE_EMULATOR_HOST", ep)
	ctx := minuteContext(t)
	project := h.Project()
	id := uniqueDB("dspitr")
	dbName := "projects/" + project + "/databases/" + id
	if _, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE,
		PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED}); err != nil {
		t.Fatalf("CreateDatabase %s: %v", id, err)
	}
	c, err := datastore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	const kind = "Pitr1967"
	k := func(name string) *datastore.Key {
		kk := datastore.NameKey(kind, name, nil)
		return kk
	}
	put := func(name string, v int64) {
		t.Helper()
		if _, err := c.Put(ctx, k(name), &pitrEntity{V: v}); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	valueAt := func(name string, at time.Time) string {
		t.Helper()
		var e pitrEntity
		err := c.WithReadOptions(datastore.ReadTime(at)).Get(ctx, k(name), &e)
		if err == datastore.ErrNoSuchEntity {
			return "none"
		}
		if err != nil {
			t.Fatalf("Get %s at %v: %v", name, at, err)
		}
		return fmt.Sprint(e.V)
	}
	names := func(q *datastore.Query, cl *datastore.Client) string {
		t.Helper()
		var es []pitrEntity
		keys, err := cl.GetAll(ctx, q, &es)
		if err != nil {
			t.Fatalf("query %v: %v", q, err)
		}
		var out []string
		for i, kk := range keys {
			out = append(out, fmt.Sprintf("%s=%d", kk.Name, es[i].V))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	put("a", 1)
	put("b", 1)
	time.Sleep(1100 * time.Millisecond)
	t1 := time.Now()
	time.Sleep(1100 * time.Millisecond)
	put("a", 2)
	if err := c.Delete(ctx, k("b")); err != nil {
		t.Fatal(err)
	}
	put("c", 3)

	for name, want := range map[string]string{"a": "1", "b": "1", "c": "none"} {
		if got := valueAt(name, t1); got != want {
			t.Errorf("%s at t1 = %s, want %s", name, got, want)
		}
	}
	for name, want := range map[string]string{"a": "2", "b": "none", "c": "3"} {
		var e pitrEntity
		err := c.Get(ctx, k(name), &e)
		got := fmt.Sprint(e.V)
		if err == datastore.ErrNoSuchEntity {
			got = "none"
		}
		if got != want {
			t.Errorf("%s now = %s (%v), want %s", name, got, err, want)
		}
	}
	at := c.WithReadOptions(datastore.ReadTime(t1))
	for _, tc := range []struct {
		what string
		q    *datastore.Query
		want string
	}{
		{"every entity", datastore.NewQuery(kind), "a=1,b=1"},
		{"V = 1", datastore.NewQuery(kind).FilterField("V", "=", 1), "a=1,b=1"},
		{"V != 2", datastore.NewQuery(kind).FilterField("V", "!=", 2), "a=1,b=1"},
		{"V in [1, 3]", datastore.NewQuery(kind).FilterField("V", "in", []any{1, 3}), "a=1,b=1"},
		{"V = 3 or V = 1", datastore.NewQuery(kind).FilterEntity(datastore.OrFilter{Filters: []datastore.EntityFilter{
			datastore.PropertyFilter{FieldName: "V", Operator: "=", Value: 3}, datastore.PropertyFilter{FieldName: "V", Operator: "=", Value: 1}}}), "a=1,b=1"},
		{"ordered, limit 1", datastore.NewQuery(kind).Order("-__key__").Limit(1), "b=1"},
	} {
		if got := names(tc.q, at); got != tc.want {
			t.Errorf("query %s at t1 = %q, want %q", tc.what, got, tc.want)
		}
	}
	if got := names(datastore.NewQuery(kind), c); got != "a=2,c=3" {
		t.Errorf("query now = %q, want a=2,c=3", got)
	}
	agg, err := at.RunAggregationQuery(ctx, datastore.NewQuery(kind).NewAggregationQuery().WithCount("n").WithSum("V", "s"))
	if err != nil {
		t.Fatalf("aggregation at t1: %v", err)
	}
	if n, s := fmt.Sprint(agg["n"]), fmt.Sprint(agg["s"]); !strings.HasSuffix(n, "2") || !strings.HasSuffix(s, "2") {
		t.Errorf("COUNT, SUM(V) at t1 = %v, %v; want 2, 2", agg["n"], agg["s"])
	}

	// GQL, through the API itself.
	conn, err := grpc.NewClient(ep, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := datastorepb.NewDatastoreClient(conn)
	rctx := metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", "project_id="+project+"&database_id="+id)
	resp, err := raw.RunQuery(rctx, &datastorepb.RunQueryRequest{ProjectId: project, DatabaseId: id,
		ReadOptions: &datastorepb.ReadOptions{ConsistencyType: &datastorepb.ReadOptions_ReadTime{ReadTime: timestamppb.New(t1)}},
		QueryType: &datastorepb.RunQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{
			QueryString: "SELECT * FROM " + kind + " WHERE V != 2", AllowLiterals: true}}})
	if err != nil {
		t.Fatalf("GQL at t1: %v", err)
	}
	if n := len(resp.GetBatch().GetEntityResults()); n != 2 || !resp.GetBatch().GetReadTime().AsTime().Equal(t1) {
		t.Errorf("GQL at t1 = %d entities, read time %v; want 2 at %v", n, resp.GetBatch().GetReadTime().AsTime(), t1)
	}

	// A read-only transaction at t1.
	tx, err := c.NewTransaction(ctx, datastore.ReadOnly, datastore.WithReadTime(t1))
	if err != nil {
		t.Fatalf("NewTransaction WithReadTime: %v", err)
	}
	var e pitrEntity
	if err := tx.Get(k("a"), &e); err != nil || e.V != 1 {
		t.Errorf("a in the transaction at t1 = %d, %v; want 1", e.V, err)
	}
	if got := names(datastore.NewQuery(kind).Transaction(tx), c); got != "a=1,b=1" {
		t.Errorf("query in the transaction at t1 = %q, want a=1,b=1", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback: %v", err)
	}

	// Before the database's earliest version time.
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	if err := c.WithReadOptions(datastore.ReadTime(old)).Get(ctx, k("a"), &e); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Get two hours back: %v, want FailedPrecondition", err)
	}

	// An export at a whole minute, and a clone of it.
	put("d", 4)
	snapshot := nextWholeMinute()
	put("a", 5)
	if err := c.Delete(ctx, k("d")); err != nil {
		t.Fatal(err)
	}
	bh := bucket(t, h, sc)
	prefix := "gs://" + bh.BucketName() + "/dspitr-" + id
	op, err := fa.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: dbName, OutputUriPrefix: prefix, SnapshotTime: timestamppb.New(snapshot)})
	if err != nil {
		t.Fatalf("ExportDocuments at %v: %v", snapshot, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("export: %v", err)
	}
	if md, err := op.Metadata(); err != nil || !md.GetSnapshotTime().AsTime().Equal(snapshot) {
		t.Errorf("export metadata snapshot_time = %v, %v; want %v", md.GetSnapshotTime(), err, snapshot)
	}
	contents := func(db string) string {
		t.Helper()
		cl, err := datastore.NewClientWithDatabase(ctx, project, db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cl.Close() }()
		return names(datastore.NewQuery(kind), cl)
	}
	imp := uniqueDB("dspitr-imp")
	if _, err := createDB(t, h, imp, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatalf("CreateDatabase %s: %v", imp, err)
	}
	iop, err := fa.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: "projects/" + project + "/databases/" + imp, InputUriPrefix: prefix})
	if err != nil {
		t.Fatalf("ImportDocuments: %v", err)
	}
	if err := iop.Wait(ctx); err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := contents(imp); got != "a=2,c=3,d=4" {
		t.Errorf("the export at %v, imported: %q, want a=2,c=3,d=4", snapshot, got)
	}
	clone := uniqueDB("dspitr-clone")
	cop, err := fa.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: "projects/" + project, DatabaseId: clone,
		PitrSnapshot: &adminpb.PitrSnapshot{Database: dbName, SnapshotTime: timestamppb.New(snapshot)}})
	if err != nil {
		t.Fatalf("CloneDatabase at %v: %v", snapshot, err)
	}
	cloned, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := fa.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: cloned.GetName()}); err == nil {
			_, _ = dop.Wait(ctx)
		}
	})
	if got := contents(clone); got != "a=2,c=3,d=4" {
		t.Errorf("the clone at %v: %q, want a=2,c=3,d=4", snapshot, got)
	}
	if got := contents(id); got != "a=5,c=3" {
		t.Errorf("the source now: %q, want a=5,c=3", got)
	}

	// The default database's entities are versioned too.
	dc := datastoreClient(t, h, project)
	dk := datastore.NameKey(kind, fmt.Sprintf("default-%d", time.Now().UnixNano()), nil)
	if _, err := dc.Put(ctx, dk, &pitrEntity{V: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	td := time.Now()
	time.Sleep(1100 * time.Millisecond)
	if _, err := dc.Put(ctx, dk, &pitrEntity{V: 2}); err != nil {
		t.Fatal(err)
	}
	if err := dc.WithReadOptions(datastore.ReadTime(td)).Get(ctx, dk, &e); err != nil || e.V != 1 {
		t.Errorf("the default database's entity at an earlier time = %d, %v; want 1", e.V, err)
	}
}
