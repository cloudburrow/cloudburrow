//go:build compat

package compat

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Named Datastore databases kept apart (#1885): the emulator's queries
// answered every database of a project (its lookups did not), so a query
// in one named database returned another's entities. The Datastore front
// keeps each named database in an emulator project of its own, so
// queries, counts, GQL, metadata queries and transactions see the named
// database alone, as Google's do.

type ds1885 struct {
	V string
	N int
}

// keysOf is "namespace/Kind/name" of each key, sorted.
func keysOf(keys []*datastore.Key) string {
	var out []string
	for _, k := range keys {
		out = append(out, k.Namespace+"/"+k.Kind+"/"+k.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestDatastoreNamedDatabasesKeepTheirEntitiesApart (#1885): two Datastore
// mode databases hold the same kind and keys; the official Datastore
// client's queries (kind, kindless, keys-only, filtered, limited, offset
// and resumed from a cursor, in a namespace too), its counts and sums, its
// transactions and the metadata kinds see each database's own entities
// alone, and GQL through the official v1 stubs likewise; (default)
// sees neither.
//
// covers: google.datastore.v1.Datastore/RunQuery, google.datastore.v1.Datastore/RunAggregationQuery, google.datastore.v1.Datastore/BeginTransaction, google.datastore.v1.Datastore/Commit, google.datastore.v1.Datastore/Lookup
func TestDatastoreNamedDatabasesKeepTheirEntitiesApart(t *testing.T) {
	h := New(t)
	dsAddr := h.Endpoint(EnvDatastore)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	project := h.Project()
	dbA, dbB := uniqueDB("dsa"), uniqueDB("dsb")
	for _, id := range []string{dbA, dbB} {
		if _, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
			t.Fatalf("CreateDatabase %s: %v", id, err)
		}
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	client := func(db string) *datastore.Client {
		c, err := datastore.NewClientWithDatabase(ctx, project, db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	a, b := client(dbA), client(dbB)
	def, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = def.Close() })

	kind := uniqueDB("Task1885")
	other := kind + "Note"
	ns := "tenant"
	keys := func() []*datastore.Key {
		k3 := datastore.NameKey(kind, "x3", nil)
		k3.Namespace = ns
		return []*datastore.Key{datastore.NameKey(kind, "x1", nil), datastore.NameKey(kind, "x2", nil), k3}
	}
	// a holds x1, x2 and tenant/x3 of the kind and a note; b holds x1 alone.
	if _, err := a.PutMulti(ctx, keys(), []*ds1885{{"a1", 1}, {"a2", 2}, {"a3", 3}}); err != nil {
		t.Fatalf("Put in %s: %v", dbA, err)
	}
	if _, err := a.Put(ctx, datastore.NameKey(other, "n1", nil), &ds1885{"note", 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, datastore.NameKey(kind, "x1", nil), &ds1885{"b1", 100}); err != nil {
		t.Fatalf("Put in %s: %v", dbB, err)
	}

	query := func(c *datastore.Client, q *datastore.Query) string {
		t.Helper()
		got, err := c.GetAll(ctx, q.KeysOnly(), nil)
		if err != nil {
			t.Fatalf("GetAll %v: %v", q, err)
		}
		return keysOf(got)
	}
	kindless := func(c *datastore.Client) string {
		t.Helper()
		got, err := c.GetAll(ctx, datastore.NewQuery("").KeysOnly(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var mine []*datastore.Key
		for _, k := range got {
			if k.Kind == kind || k.Kind == other {
				mine = append(mine, k)
			}
		}
		return keysOf(mine)
	}

	for _, c := range []struct {
		db, keys, prefix string
		client           *datastore.Client
	}{{dbA, "/" + kind + "/x1,/" + kind + "/x2", "a", a}, {dbB, "/" + kind + "/x1", "b", b}} {
		if got := query(c.client, datastore.NewQuery(kind)); got != c.keys {
			t.Errorf("%s: kind query = %s, want %s", c.db, got, c.keys)
		}
		var es []ds1885
		if _, err := c.client.GetAll(ctx, datastore.NewQuery(kind), &es); err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if !strings.HasPrefix(e.V, c.prefix) {
				t.Errorf("%s: a query returned another database's %+v", c.db, e)
			}
		}
	}
	if got, w := kindless(a), "/"+kind+"/x1,/"+kind+"/x2,/"+other+"/n1"; got != w {
		t.Errorf("%s: kindless query = %s, want %s", dbA, got, w)
	}
	if got, w := kindless(b), "/"+kind+"/x1"; got != w {
		t.Errorf("%s: kindless query = %s, want %s", dbB, got, w)
	}
	if got := kindless(def); got != "" {
		t.Errorf("(default): kindless query = %s, want none of the named databases' entities", got)
	}
	if got := query(def, datastore.NewQuery(kind)); got != "" {
		t.Errorf("(default): kind query = %s, want none", got)
	}
	if got, w := query(a, datastore.NewQuery(kind).Namespace(ns)), ns+"/"+kind+"/x3"; got != w {
		t.Errorf("%s: namespace query = %s, want %s", dbA, got, w)
	}
	if got := query(b, datastore.NewQuery(kind).Namespace(ns)); got != "" {
		t.Errorf("%s: namespace query = %s, want none", dbB, got)
	}
	// A filter, a limit, an offset and a cursor count the database's own
	// entities alone.
	if got, w := query(b, datastore.NewQuery(kind).FilterField("N", ">=", 1)), "/"+kind+"/x1"; got != w {
		t.Errorf("%s: filtered query = %s, want %s", dbB, got, w)
	}
	if got, w := query(a, datastore.NewQuery(kind).Order("N").Offset(1).Limit(1)), "/"+kind+"/x2"; got != w {
		t.Errorf("%s: offset 1 limit 1 = %s, want %s", dbA, got, w)
	}
	if got := query(b, datastore.NewQuery(kind).Order("N").Offset(1)); got != "" {
		t.Errorf("%s: offset 1 = %s, want none", dbB, got)
	}
	it := a.Run(ctx, datastore.NewQuery(kind).Order("N").Limit(1).KeysOnly())
	first, err := it.Next(nil)
	if err != nil || first.Name != "x1" {
		t.Fatalf("%s: first page = %v, %v", dbA, first, err)
	}
	cur, err := it.Cursor()
	if err != nil {
		t.Fatal(err)
	}
	if got, w := query(a, datastore.NewQuery(kind).Order("N").Start(cur)), "/"+kind+"/x2"; got != w {
		t.Errorf("%s: resumed from the cursor = %s, want %s", dbA, got, w)
	}

	// Counts and sums.
	count := func(c *datastore.Client) (int64, int64) {
		t.Helper()
		res, err := c.RunAggregationQuery(ctx, datastore.NewQuery(kind).NewAggregationQuery().WithCount("n").WithSum("N", "s"))
		if err != nil {
			t.Fatalf("RunAggregationQuery: %v", err)
		}
		n, _ := res["n"].(*datastorepb.Value)
		s, _ := res["s"].(*datastorepb.Value)
		return n.GetIntegerValue(), s.GetIntegerValue()
	}
	if n, s := count(a); n != 2 || s != 3 {
		t.Errorf("%s: COUNT, SUM(N) = %d, %d, want 2, 3", dbA, n, s)
	}
	if n, s := count(b); n != 1 || s != 100 {
		t.Errorf("%s: COUNT, SUM(N) = %d, %d, want 1, 100", dbB, n, s)
	}
	if n, err := def.Count(ctx, datastore.NewQuery(kind)); err != nil || n != 0 {
		t.Errorf("(default): Count = %d, %v, want 0", n, err)
	}

	// The metadata kinds: b has the kind and no note, and no tenant.
	kinds := func(c *datastore.Client) map[string]bool {
		got, err := c.GetAll(ctx, datastore.NewQuery("__kind__").KeysOnly(), nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, k := range got {
			out[k.Name] = true
		}
		return out
	}
	if ka, kb := kinds(a), kinds(b); !ka[kind] || !ka[other] || !kb[kind] || kb[other] {
		t.Errorf("__kind__: %s has %v, %s has %v", dbA, ka, dbB, kb)
	}
	nss, err := b.GetAll(ctx, datastore.NewQuery("__namespace__").KeysOnly(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range nss {
		if k.Name == ns {
			t.Errorf("%s: __namespace__ lists %s's namespace %q", dbB, dbA, ns)
		}
	}

	// A transaction reads and writes its own database.
	if _, err := b.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		var e ds1885
		if err := tx.Get(datastore.NameKey(kind, "x2", nil), &e); !errors.Is(err, datastore.ErrNoSuchEntity) {
			t.Errorf("%s: a transaction reads %s's x2: %+v, %v", dbB, dbA, e, err)
		}
		_, err := tx.Put(datastore.NameKey(kind, "x9", nil), &ds1885{"b9", 9})
		return err
	}); err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}
	if got := query(a, datastore.NewQuery(kind).FilterField("N", "=", 9)); got != "" {
		t.Errorf("%s sees %s's transaction's write: %s", dbA, dbB, got)
	}
	if got, w := query(b, datastore.NewQuery(kind)), "/"+kind+"/x1,/"+kind+"/x9"; got != w {
		t.Errorf("%s after the transaction = %s, want %s", dbB, got, w)
	}

	// GQL, through the official v1 stubs.
	conn, err := grpc.NewClient(dsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := datastorepb.NewDatastoreClient(conn)
	gql := func(db, q string) []*datastorepb.EntityResult {
		t.Helper()
		resp, err := raw.RunQuery(inDatabase(ctx, project, db), &datastorepb.RunQueryRequest{ProjectId: project, DatabaseId: db,
			PartitionId: &datastorepb.PartitionId{ProjectId: project, DatabaseId: db},
			QueryType:   &datastorepb.RunQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{QueryString: q, AllowLiterals: true}}})
		if err != nil {
			t.Fatalf("GQL %q in %s: %v", q, db, err)
		}
		return resp.GetBatch().GetEntityResults()
	}
	if got := gql(dbA, "SELECT * FROM `"+kind+"`"); len(got) != 2 {
		t.Errorf("%s: GQL returned %d entities, want 2", dbA, len(got))
	} else {
		for _, r := range got {
			if p := r.GetEntity().GetKey().GetPartitionId(); p.GetProjectId() != project || p.GetDatabaseId() != dbA {
				t.Errorf("%s: a GQL result's partition = %v", dbA, p)
			}
		}
	}
	if got := gql(dbB, "SELECT * FROM `"+kind+"` WHERE N = 100"); len(got) != 1 {
		t.Errorf("%s: GQL WHERE N = 100 returned %d entities, want 1", dbB, len(got))
	}
	if got := gql(dbB, "SELECT * FROM `"+kind+"` WHERE N = 2"); len(got) != 0 {
		t.Errorf("%s: GQL WHERE N = 2 returned %s's entity", dbB, dbA)
	}
	agg, err := raw.RunAggregationQuery(inDatabase(ctx, project, dbB), &datastorepb.RunAggregationQueryRequest{ProjectId: project, DatabaseId: dbB,
		QueryType: &datastorepb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{
			QueryString: "SELECT COUNT(*) AS n FROM `" + kind + "`", AllowLiterals: true}}})
	if err != nil {
		t.Fatalf("GQL COUNT in %s: %v", dbB, err)
	}
	if n := agg.GetBatch().GetAggregationResults()[0].GetAggregateProperties()["n"].GetIntegerValue(); n != 2 {
		t.Errorf("%s: GQL COUNT(*) = %d, want 2 (x1, x9)", dbB, n)
	}

	// Lookups still keep them apart, and a deleted database's entities go
	// with it alone.
	var e ds1885
	if err := a.Get(ctx, datastore.NameKey(kind, "x1", nil), &e); err != nil || e.V != "a1" {
		t.Errorf("%s: Get x1 = %+v, %v", dbA, e, err)
	}
	if err := b.Get(ctx, datastore.NameKey(kind, "x1", nil), &e); err != nil || e.V != "b1" {
		t.Errorf("%s: Get x1 = %+v, %v", dbB, e, err)
	}
	fa := firestoreAdmin(t, h)
	op, err := fa.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: "projects/" + project + "/databases/" + dbB})
	if err != nil {
		t.Fatalf("DeleteDatabase %s: %v", dbB, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, w := query(a, datastore.NewQuery(kind)), "/"+kind+"/x1,/"+kind+"/x2"; got != w {
		t.Errorf("%s after %s was deleted = %s, want %s", dbA, dbB, got, w)
	}
}
