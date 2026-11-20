//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	firestorerest "google.golang.org/api/firestore/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Point-in-time reads through every read method (#1942): Google's
// use-pitr page says "PITR reads are supported in all Firestore read
// methods" (get, list, batchGet, listCollectionIds, listDocuments,
// runQuery, runAggregationQuery, partitionQuery) and, in the server
// libraries, through a read-only transaction at a read time
// (https://docs.cloud.google.com/firestore/native/docs/use-pitr, read
// 2026-11-20). On a database the Admin client creates with PITR, the
// official Go client reads at an earlier microsecond time with GetAll
// (BatchGetDocuments), DocumentRefs (ListDocuments) and a read-only
// transaction (TransactionReadTime: BeginTransaction, then a get, a query,
// a document listing and a COUNT/SUM aggregation in it); the generated
// client's RunAggregationQuery, ListCollectionIds and PartitionQuery take
// read_time directly; the REST API's get, list, batchGet, runQuery,
// runAggregationQuery, listCollectionIds, partitionQuery and a read-only
// beginTransaction (google.golang.org/api/firestore/v1, and plain JSON for
// the streamed answers) do the same. Each answers the documents as they
// were, and a read from before the database's earliest version time is
// refused on every one.
//
// covers: google.firestore.v1.Firestore/BatchGetDocuments, google.firestore.v1.Firestore/ListDocuments, google.firestore.v1.Firestore/BeginTransaction, google.firestore.v1.Firestore/RunAggregationQuery, google.firestore.v1.Firestore/ListCollectionIds, google.firestore.v1.Firestore/PartitionQuery
func TestFirestorePointInTimeReadsThroughEveryReadMethod(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE
	firestoreClient(t, h)     // sets FIRESTORE_EMULATOR_HOST
	g := firestoreGAPIC(t, h)
	ctx := h.Context()
	project := "projects/" + h.Project()
	id := fmt.Sprintf("pitr-reads-%d", time.Now().UnixNano()%1e9)
	dbName := project + "/databases/" + id
	docs := dbName + "/documents"

	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: project, DatabaseId: id, Database: &adminpb.Database{
		LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE,
		PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase with PITR: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: dbName}) })

	fs, err := firestore.NewClientWithDatabase(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	set := func(path string, v int) {
		t.Helper()
		if _, err := fs.Doc(path).Set(ctx, map[string]any{"v": v}); err != nil {
			t.Fatalf("Set %s: %v", path, err)
		}
	}
	// At t1: cities/sf (v 1) with its landmarks/bridge; nothing else.
	set("cities/sf", 1)
	set("cities/sf/landmarks/bridge", 5)
	time.Sleep(1100 * time.Millisecond)
	t1 := time.Now().Truncate(time.Microsecond)
	time.Sleep(1100 * time.Millisecond)
	// Since: sf is 2, la, ny and towns/x are new, the landmark is gone.
	set("cities/sf", 2)
	set("cities/la", 10)
	set("cities/ny", 20)
	set("towns/x", 7)
	if _, err := fs.Doc("cities/sf/landmarks/bridge").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-2 * time.Hour).Truncate(time.Minute) // before the database was created
	ts1, tsBefore := timestamppb.New(t1), timestamppb.New(before)

	// BatchGetDocuments through Client.WithReadOptions, which sets the
	// client's own read time: a client of its own for each.
	clientAt := func(at time.Time) *firestore.Client {
		t.Helper()
		cl, err := firestore.NewClientWithDatabase(ctx, h.Project(), id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.Close() })
		return cl.WithReadOptions(firestore.ReadTime(at))
	}
	snaps, err := clientAt(t1).GetAll(ctx, []*firestore.DocumentRef{fs.Doc("cities/sf"), fs.Doc("cities/la")})
	if err != nil || len(snaps) != 2 || !snaps[0].Exists() || snaps[0].Data()["v"] != int64(1) || snaps[1].Exists() {
		t.Errorf("GetAll at t1 = %v, %v; want sf v 1 and la missing", snaps, err)
	}
	if _, err := clientAt(before).GetAll(ctx, []*firestore.DocumentRef{fs.Doc("cities/sf")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetAll before the earliest version time: %v, want FailedPrecondition", err)
	}

	// ListDocuments through CollectionRef.WithReadOptions.
	refIDs := func(it *firestore.DocumentRefIterator) string {
		t.Helper()
		refs, err := it.GetAll()
		if err != nil {
			t.Fatalf("DocumentRefs: %v", err)
		}
		var out []string
		for _, r := range refs {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got := refIDs(fs.Collection("cities").WithReadOptions(firestore.ReadTime(t1)).DocumentRefs(ctx)); got != "sf" {
		t.Errorf("DocumentRefs at t1 = %q, want sf", got)
	}
	if got := refIDs(fs.Collection("cities").DocumentRefs(ctx)); got != "la,ny,sf" {
		t.Errorf("DocumentRefs now = %q, want la,ny,sf", got)
	}

	// A read-only transaction at t1: BeginTransaction with read_time,
	// then a get, a query, a listing and an aggregation in it.
	err = fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(fs.Doc("cities/sf"))
		if err != nil {
			return fmt.Errorf("get: %w", err)
		}
		if snap.Data()["v"] != int64(1) {
			t.Errorf("in the transaction at t1, cities/sf v = %v, want 1", snap.Data()["v"])
		}
		qd, err := tx.Documents(fs.Collection("cities")).GetAll()
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		if len(qd) != 1 || qd[0].Ref.ID != "sf" {
			t.Errorf("in the transaction at t1, the query answers %d documents, want sf alone", len(qd))
		}
		refs, err := tx.DocumentRefs(fs.Collection("cities")).GetAll()
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		if len(refs) != 1 {
			t.Errorf("in the transaction at t1, DocumentRefs = %d, want 1", len(refs))
		}
		res, err := fs.Collection("cities").NewAggregationQuery().WithCount("n").WithSum("v", "s").Transaction(tx).Get(ctx)
		if err != nil {
			return fmt.Errorf("aggregation: %w", err)
		}
		if n, s := aggInt(res["n"]), aggInt(res["s"]); n != 1 || s != 1 {
			t.Errorf("in the transaction at t1, COUNT, SUM(v) = %d, %d; want 1, 1", n, s)
		}
		return nil
	}, firestore.TransactionReadTime(t1))
	if err != nil {
		t.Errorf("read-only transaction at t1: %v", err)
	}
	if _, err := g.BeginTransaction(ctx, &firestorepb.BeginTransactionRequest{Database: dbName, Options: &firestorepb.TransactionOptions{
		Mode: &firestorepb.TransactionOptions_ReadOnly_{ReadOnly: &firestorepb.TransactionOptions_ReadOnly{
			ConsistencySelector: &firestorepb.TransactionOptions_ReadOnly_ReadTime{ReadTime: tsBefore}}}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("BeginTransaction read-only before the earliest version time: %v, want FailedPrecondition", err)
	}

	// The generated client: RunAggregationQuery, ListCollectionIds and
	// PartitionQuery with read_time. Google's read_time "must be a
	// microsecond precision timestamp within the past one hour"
	// (google.firestore.v1 reference), and time.Now has nanoseconds on
	// Linux (CI's runners) though microseconds on macOS, so "now" is
	// truncated as a client must.
	now := func() *timestamppb.Timestamp { return timestamppb.New(time.Now().Truncate(time.Microsecond)) }
	count := func(rt *timestamppb.Timestamp) (int64, error) {
		stream, err := g.RunAggregationQuery(ctx, &firestorepb.RunAggregationQueryRequest{Parent: docs,
			QueryType: &firestorepb.RunAggregationQueryRequest_StructuredAggregationQuery{StructuredAggregationQuery: &firestorepb.StructuredAggregationQuery{
				QueryType: &firestorepb.StructuredAggregationQuery_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
					From: []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: "cities"}}}},
				Aggregations: []*firestorepb.StructuredAggregationQuery_Aggregation{{Alias: "n",
					Operator: &firestorepb.StructuredAggregationQuery_Aggregation_Count_{Count: &firestorepb.StructuredAggregationQuery_Aggregation_Count{}}}}}},
			ConsistencySelector: &firestorepb.RunAggregationQueryRequest_ReadTime{ReadTime: rt}})
		if err != nil {
			return 0, err
		}
		var n int64 = -1
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return n, nil
			}
			if err != nil {
				return 0, err
			}
			if r := resp.GetResult(); r != nil {
				n = r.GetAggregateFields()["n"].GetIntegerValue()
			}
		}
	}
	if n, err := count(ts1); err != nil || n != 1 {
		t.Errorf("RunAggregationQuery COUNT at t1 = %d, %v; want 1", n, err)
	}
	if n, err := count(now()); err != nil || n != 3 {
		t.Errorf("RunAggregationQuery COUNT now = %d, %v; want 3", n, err)
	}
	if _, err := count(tsBefore); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("RunAggregationQuery before the earliest version time: %v, want FailedPrecondition", err)
	}
	// ListCollectionIds is a metadata call, which the emulator answers
	// its owner alone: the bearer token the official clients send it
	// (cloud.google.com/go/firestore's emulator credentials).
	owner := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer owner")
	collections := func(parent string, rt *timestamppb.Timestamp) (string, error) {
		it := g.ListCollectionIds(owner, &firestorepb.ListCollectionIdsRequest{Parent: parent,
			ConsistencySelector: &firestorepb.ListCollectionIdsRequest_ReadTime{ReadTime: rt}})
		var out []string
		for {
			s, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return "", err
			}
			out = append(out, s)
		}
		sort.Strings(out)
		return strings.Join(out, ","), nil
	}
	if got, err := collections(docs, ts1); err != nil || got != "cities" {
		t.Errorf("ListCollectionIds of the root at t1 = %q, %v; want cities", got, err)
	}
	if got, err := collections(docs, now()); err != nil || got != "cities,towns" {
		t.Errorf("ListCollectionIds of the root now = %q, %v; want cities,towns", got, err)
	}
	if got, err := collections(docs+"/cities/sf", ts1); err != nil || got != "landmarks" {
		t.Errorf("ListCollectionIds of cities/sf at t1 = %q, %v; want landmarks", got, err)
	}
	if got, err := collections(docs+"/cities/sf", now()); err != nil || got != "" {
		t.Errorf("ListCollectionIds of cities/sf now = %q, %v; want none", got, err)
	}
	if _, err := collections(docs, tsBefore); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ListCollectionIds before the earliest version time: %v, want FailedPrecondition", err)
	}
	// Finer than a microsecond is refused on every read method, this one
	// too (the emulator would answer it), with the emulator's words.
	nanos := now()
	nanos.Nanos += 1
	if _, err := collections(docs, nanos); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "microseconds precision") {
		t.Errorf("ListCollectionIds at a nanosecond read_time: %v, want InvalidArgument", err)
	}
	if _, err := count(nanos); status.Code(err) != codes.InvalidArgument {
		t.Errorf("RunAggregationQuery at a nanosecond read_time: %v, want InvalidArgument", err)
	}
	partitions := func(rt *timestamppb.Timestamp) (int, error) {
		_, err := g.PartitionQuery(ctx, &firestorepb.PartitionQueryRequest{Parent: docs, PartitionCount: 3,
			QueryType: &firestorepb.PartitionQueryRequest_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
				From:    []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: "cities", AllDescendants: true}},
				OrderBy: []*firestorepb.StructuredQuery_Order{{Field: &firestorepb.StructuredQuery_FieldReference{FieldPath: "__name__"}, Direction: firestorepb.StructuredQuery_ASCENDING}}}},
			ConsistencySelector: &firestorepb.PartitionQueryRequest_ReadTime{ReadTime: rt}}).Next()
		if err == iterator.Done {
			return 0, nil
		}
		return 1, err
	}
	// One city at t1 cannot be split; three now can.
	if n, err := partitions(ts1); err != nil || n != 0 {
		t.Errorf("PartitionQuery at t1 = %d cursors, %v; want none", n, err)
	}
	if n, err := partitions(now()); err != nil || n == 0 {
		t.Errorf("PartitionQuery now = %d cursors, %v; want some", n, err)
	}
	if _, err := partitions(tsBefore); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("PartitionQuery before the earliest version time: %v, want FailedPrecondition", err)
	}

	// REST.
	base := "http://" + h.Endpoint(EnvFirestore)
	rest, err := firestorerest.NewService(ctx, option.WithEndpoint(base+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r1, rBefore := t1.UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano)
	d := rest.Projects.Databases.Documents
	if doc, err := d.Get(docs + "/cities/sf").ReadTime(r1).Context(ctx).Do(); err != nil || doc.Fields["v"].IntegerValue != 1 {
		t.Errorf("REST get at t1 = %v, %v; want v 1", doc, err)
	}
	if _, err := d.Get(docs + "/cities/sf").ReadTime(rBefore).Context(ctx).Do(); restStatus(err) != http.StatusBadRequest {
		t.Errorf("REST get before the earliest version time: %v, want 400 FAILED_PRECONDITION", err)
	}
	if l, err := d.List(docs, "cities").ReadTime(r1).Context(ctx).Do(); err != nil || len(l.Documents) != 1 {
		t.Errorf("REST list at t1 = %v, %v; want one document", l, err)
	}
	lc := d.ListCollectionIds(docs, &firestorerest.ListCollectionIdsRequest{ReadTime: r1})
	lc.Header().Set("Authorization", "Bearer owner")
	if ids, err := lc.Context(ctx).Do(); err != nil || strings.Join(ids.CollectionIds, ",") != "cities" {
		t.Errorf("REST listCollectionIds at t1 = %v, %v; want cities", ids, err)
	}
	if _, err := d.ListCollectionIds(docs, &firestorerest.ListCollectionIdsRequest{ReadTime: rBefore}).Context(ctx).Do(); restStatus(err) != http.StatusBadRequest {
		t.Errorf("REST listCollectionIds before the earliest version time: %v, want 400", err)
	}
	pq, err := d.PartitionQuery(docs, &firestorerest.PartitionQueryRequest{ReadTime: r1, PartitionCount: 3, StructuredQuery: &firestorerest.StructuredQuery{
		From:    []*firestorerest.CollectionSelector{{CollectionId: "cities", AllDescendants: true}},
		OrderBy: []*firestorerest.Order{{Field: &firestorerest.FieldReference{FieldPath: "__name__"}, Direction: "ASCENDING"}}}}).Context(ctx).Do()
	if err != nil || len(pq.Partitions) != 0 {
		t.Errorf("REST partitionQuery at t1 = %v, %v; want no cursors", pq, err)
	}
	tx, err := d.BeginTransaction(dbName, &firestorerest.BeginTransactionRequest{Options: &firestorerest.TransactionOptions{
		ReadOnly: &firestorerest.ReadOnly{ReadTime: r1}}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("REST beginTransaction read-only at t1: %v", err)
	}
	if doc, err := d.Get(docs + "/cities/sf").Transaction(tx.Transaction).Context(ctx).Do(); err != nil || doc.Fields["v"].IntegerValue != 1 {
		t.Errorf("REST get in the read-only transaction at t1 = %v, %v; want v 1", doc, err)
	}
	if _, err := d.BeginTransaction(dbName, &firestorerest.BeginTransactionRequest{Options: &firestorerest.TransactionOptions{
		ReadOnly: &firestorerest.ReadOnly{ReadTime: rBefore}}}).Context(ctx).Do(); restStatus(err) != http.StatusBadRequest {
		t.Errorf("REST beginTransaction before the earliest version time: %v, want 400", err)
	}

	// The streamed answers (a JSON array), posted as Google's REST
	// reference gives them.
	post := func(method, body string) (int, []map[string]any) {
		t.Helper()
		resp, err := http.Post(base+"/v1/"+docs+":"+method, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out []map[string]any
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("%s answered %s: %v", method, b, err)
			}
		} else if !strings.Contains(string(b), "FAILED_PRECONDITION") {
			t.Errorf("%s answered %d %s; want FAILED_PRECONDITION", method, resp.StatusCode, b)
		}
		return resp.StatusCode, out
	}
	query := `"structuredQuery":{"from":[{"collectionId":"cities"}]}`
	if code, out := post("runQuery", `{`+query+`,"readTime":"`+r1+`"}`); code != http.StatusOK || len(out) != 1 || out[0]["document"] == nil {
		t.Errorf("REST runQuery at t1 = %d %v; want sf alone", code, out)
	}
	if code, _ := post("runQuery", `{`+query+`,"readTime":"`+rBefore+`"}`); code != http.StatusBadRequest {
		t.Errorf("REST runQuery before the earliest version time = %d, want 400", code)
	}
	agg := `{"structuredAggregationQuery":{` + query + `,"aggregations":[{"alias":"n","count":{}}]},"readTime":"%s"}`
	if code, out := post("runAggregationQuery", fmt.Sprintf(agg, r1)); code != http.StatusOK || len(out) != 1 ||
		fmt.Sprint(out[0]["result"]) != "map[aggregateFields:map[n:map[integerValue:1]]]" {
		t.Errorf("REST runAggregationQuery at t1 = %d %v; want n 1", code, out)
	}
	if code, _ := post("runAggregationQuery", fmt.Sprintf(agg, rBefore)); code != http.StatusBadRequest {
		t.Errorf("REST runAggregationQuery before the earliest version time = %d, want 400", code)
	}
	bg := `{"documents":["` + docs + `/cities/sf","` + docs + `/cities/la"],"readTime":"%s"}`
	if code, out := post("batchGet", fmt.Sprintf(bg, r1)); code != http.StatusOK || len(out) != 2 {
		t.Errorf("REST batchGet at t1 = %d %v; want two answers", code, out)
	} else {
		found, missing := 0, 0
		for _, o := range out {
			if o["found"] != nil {
				found++
			}
			if o["missing"] != nil {
				missing++
			}
		}
		if found != 1 || missing != 1 {
			t.Errorf("REST batchGet at t1 = %v; want sf found and la missing", out)
		}
	}
	if code, _ := post("batchGet", fmt.Sprintf(bg, rBefore)); code != http.StatusBadRequest {
		t.Errorf("REST batchGet before the earliest version time = %d, want 400", code)
	}
}

// aggInt is an aggregation result's integer: COUNT is an int64, and a SUM
// of integers is one too.
func aggInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case *firestorepb.Value:
		return x.GetIntegerValue()
	}
	return -1
}

// restStatus is the HTTP status of a REST client's error, 0 for none.
func restStatus(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	if err == nil {
		return 0
	}
	return -1
}
