//go:build compat

package compat

import (
	"errors"
	"fmt"
	"testing"

	"cloud.google.com/go/firestore"
	fsapi "cloud.google.com/go/firestore/apiv1"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The Firestore RPCs TestFirestoreDocumentCRUD does not reach (#1197). The
// veneer client builds no CreateDocument, GetDocument, UpdateDocument,
// DeleteDocument or Write call, so those go through the generated client
// of the same module, cloud.google.com/go/firestore/apiv1.

func firestoreClient(t *testing.T, h *Harness) *firestore.Client {
	t.Helper()
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	c, err := firestore.NewClient(h.Context(), h.Project())
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func firestoreGAPIC(t *testing.T, h *Harness) *fsapi.Client {
	t.Helper()
	c, err := fsapi.NewClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvFirestore)),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("firestore apiv1.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func firestoreDocs(h *Harness) string {
	return fmt.Sprintf("projects/%s/databases/(default)/documents", h.Project())
}

// TestFirestoreDocumentRPCs creates, reads, updates and deletes one
// document with the single-document RPCs, re-reading after each write.
//
// covers: google.firestore.v1.Firestore/CreateDocument, google.firestore.v1.Firestore/GetDocument, google.firestore.v1.Firestore/UpdateDocument, google.firestore.v1.Firestore/DeleteDocument
func TestFirestoreDocumentRPCs(t *testing.T) {
	h := New(t)
	c := firestoreGAPIC(t, h)
	ctx := h.Context()
	parent := firestoreDocs(h)
	name := parent + "/gadgets/one"

	created, err := c.CreateDocument(ctx, &firestorepb.CreateDocumentRequest{
		Parent: parent, CollectionId: "gadgets", DocumentId: "one",
		Document: &firestorepb.Document{Fields: map[string]*firestorepb.Value{
			"name": {ValueType: &firestorepb.Value_StringValue{StringValue: "first"}},
			"size": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 3}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if created.GetName() != name {
		t.Errorf("CreateDocument name = %q, want %q", created.GetName(), name)
	}
	// Creating it again is ALREADY_EXISTS, as in Google Cloud.
	_, err = c.CreateDocument(ctx, &firestorepb.CreateDocumentRequest{
		Parent: parent, CollectionId: "gadgets", DocumentId: "one", Document: &firestorepb.Document{},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("second CreateDocument = %v, want ALREADY_EXISTS", err)
	}

	got, err := c.GetDocument(ctx, &firestorepb.GetDocumentRequest{Name: name})
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if v := got.GetFields()["name"].GetStringValue(); v != "first" {
		t.Errorf("GetDocument name = %q, want first", v)
	}

	// A masked update changes size and leaves name alone.
	if _, err := c.UpdateDocument(ctx, &firestorepb.UpdateDocumentRequest{
		Document: &firestorepb.Document{Name: name, Fields: map[string]*firestorepb.Value{
			"size": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 42}},
		}},
		UpdateMask: &firestorepb.DocumentMask{FieldPaths: []string{"size"}},
	}); err != nil {
		t.Fatalf("UpdateDocument: %v", err)
	}
	got, err = c.GetDocument(ctx, &firestorepb.GetDocumentRequest{Name: name})
	if err != nil {
		t.Fatalf("GetDocument after update: %v", err)
	}
	if s, n := got.GetFields()["size"].GetIntegerValue(), got.GetFields()["name"].GetStringValue(); s != 42 || n != "first" {
		t.Errorf("after the masked update size=%d name=%q, want 42 and first", s, n)
	}

	if err := c.DeleteDocument(ctx, &firestorepb.DeleteDocumentRequest{Name: name}); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if _, err := c.GetDocument(ctx, &firestorepb.GetDocumentRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDocument after delete = %v, want NOT_FOUND", err)
	}
}

// TestFirestoreWriteStream writes a document over the streaming Write RPC:
// the first request opens the stream and returns its token, the second
// carries the write with that token. Like Google Cloud, the emulator needs
// the google-cloud-resource-prefix header on the stream, which the veneer
// client sends on every call: without it the handshake fails with UNKNOWN
// and an empty message.
//
// covers: google.firestore.v1.Firestore/Write
func TestFirestoreWriteStream(t *testing.T) {
	h := New(t)
	c := firestoreGAPIC(t, h)
	ctx := h.Context()
	db := fmt.Sprintf("projects/%s/databases/(default)", h.Project())
	name := firestoreDocs(h) + "/streamed/one"

	ctx = metadata.AppendToOutgoingContext(ctx, "google-cloud-resource-prefix", db)
	st, err := c.Write(ctx)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := st.Send(&firestorepb.WriteRequest{Database: db}); err != nil {
		t.Fatalf("Write handshake send: %v", err)
	}
	hello, err := st.Recv()
	if err != nil {
		t.Fatalf("Write handshake: %v", err)
	}
	if hello.GetStreamId() == "" || len(hello.GetStreamToken()) == 0 {
		t.Fatalf("handshake = %v, want a stream ID and token", hello)
	}
	if err := st.Send(&firestorepb.WriteRequest{
		StreamToken: hello.GetStreamToken(),
		Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{
			Name:   name,
			Fields: map[string]*firestorepb.Value{"n": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 7}}},
		}}}},
	}); err != nil {
		t.Fatalf("Write send: %v", err)
	}
	resp, err := st.Recv()
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(resp.GetWriteResults()) != 1 {
		t.Errorf("Write returned %d results, want 1", len(resp.GetWriteResults()))
	}
	_ = st.CloseSend()

	got, err := c.GetDocument(ctx, &firestorepb.GetDocumentRequest{Name: name})
	if err != nil {
		t.Fatalf("GetDocument of the streamed write: %v", err)
	}
	if n := got.GetFields()["n"].GetIntegerValue(); n != 7 {
		t.Errorf("n = %d, want 7", n)
	}
}

// TestFirestoreRollback begins a transaction, rolls it back, and shows the
// transaction can then no longer commit. The veneer's RunTransaction
// ignores its Rollback's result, so the generated client calls it.
//
// covers: google.firestore.v1.Firestore/Rollback
func TestFirestoreRollback(t *testing.T) {
	h := New(t)
	c := firestoreGAPIC(t, h)
	ctx := h.Context()
	db := fmt.Sprintf("projects/%s/databases/(default)", h.Project())

	tx, err := c.BeginTransaction(ctx, &firestorepb.BeginTransactionRequest{Database: db})
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	if err := c.Rollback(ctx, &firestorepb.RollbackRequest{Database: db, Transaction: tx.GetTransaction()}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_, err = c.Commit(ctx, &firestorepb.CommitRequest{
		Database: db, Transaction: tx.GetTransaction(),
		Writes: []*firestorepb.Write{{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{
			Name: firestoreDocs(h) + "/rolled/back",
		}}}},
	})
	if err == nil {
		t.Errorf("Commit of a rolled-back transaction succeeded")
	}
	if _, err := c.GetDocument(ctx, &firestorepb.GetDocumentRequest{Name: firestoreDocs(h) + "/rolled/back"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDocument of the rolled-back write = %v, want NOT_FOUND", err)
	}
}

// TestFirestoreBulkWriter writes through BulkWriter, which sends BatchWrite,
// and reads every document back.
//
// covers: google.firestore.v1.Firestore/BatchWrite
func TestFirestoreBulkWriter(t *testing.T) {
	h := New(t)
	c := firestoreClient(t, h)
	ctx := h.Context()

	coll := c.Collection("bulk")
	bw := c.BulkWriter(ctx)
	var jobs []*firestore.BulkWriterJob
	for i := 0; i < 5; i++ {
		j, err := bw.Set(coll.Doc(fmt.Sprintf("d%d", i)), map[string]any{"i": i})
		if err != nil {
			t.Fatalf("BulkWriter.Set %d: %v", i, err)
		}
		jobs = append(jobs, j)
	}
	bw.End()
	for i, j := range jobs {
		if _, err := j.Results(); err != nil {
			t.Fatalf("BulkWriter job %d: %v", i, err)
		}
	}
	docs, err := coll.Documents(ctx).GetAll()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(docs) != 5 {
		t.Errorf("read back %d documents, want 5", len(docs))
	}
}

// TestFirestoreSnapshotListener listens to a document, which opens the
// Listen stream, and sees first its absence and then a write.
//
// covers: google.firestore.v1.Firestore/Listen
func TestFirestoreSnapshotListener(t *testing.T) {
	h := New(t)
	c := firestoreClient(t, h)
	ctx := h.Context()
	doc := c.Collection("watched").Doc("one")

	it := doc.Snapshots(ctx)
	defer it.Stop()
	snap, err := it.Next()
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if snap.Exists() {
		t.Fatalf("first snapshot exists, want the document absent")
	}
	if _, err := doc.Set(ctx, map[string]any{"v": "seen"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	snap, err = it.Next()
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if !snap.Exists() || snap.Data()["v"] != "seen" {
		t.Errorf("second snapshot = %v, want v=seen", snap.Data())
	}
}

// TestFirestoreCountAggregation counts a filtered query with
// RunAggregationQuery.
//
// covers: google.firestore.v1.Firestore/RunAggregationQuery
func TestFirestoreCountAggregation(t *testing.T) {
	h := New(t)
	c := firestoreClient(t, h)
	ctx := h.Context()
	coll := c.Collection("counted")
	for i := 0; i < 6; i++ {
		if _, err := coll.Doc(fmt.Sprintf("d%d", i)).Set(ctx, map[string]any{"i": i}); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	q := coll.Where("i", ">=", 2)
	res, err := q.NewAggregationQuery().WithCount("n").WithSum("i", "total").Get(ctx)
	if err != nil {
		t.Fatalf("aggregation: %v", err)
	}
	if n := fmt.Sprint(valueOf(res["n"])); n != "4" {
		t.Errorf("count = %s, want 4", n)
	}
	if s := fmt.Sprint(valueOf(res["total"])); s != "14" {
		t.Errorf("sum = %s, want 14", s)
	}
}

// valueOf unwraps the *firestorepb.Value an aggregation result holds.
func valueOf(v any) any {
	if pv, ok := v.(*firestorepb.Value); ok {
		switch x := pv.GetValueType().(type) {
		case *firestorepb.Value_IntegerValue:
			return x.IntegerValue
		case *firestorepb.Value_DoubleValue:
			return x.DoubleValue
		}
	}
	return v
}

// TestFirestorePartitionedQueries splits a collection group into
// partitions with the client's GetPartitionedQueries, and reads every
// document exactly once across them. The emulator does not serve
// PartitionQuery; the call front answers it from the emulator's RunQuery
// (#1405), with cursors at document references that split the group
// evenly. A paged PartitionQuery returns the same cursors.
//
// covers: google.firestore.v1.Firestore/PartitionQuery
func TestFirestorePartitionedQueries(t *testing.T) {
	h := New(t)
	c := firestoreClient(t, h)
	ctx := h.Context()
	want := map[string]bool{}
	for _, parent := range []string{"a", "b", "c"} {
		for i := 0; i < 7; i++ {
			ref := c.Collection("owners").Doc(parent).Collection("parted").Doc(fmt.Sprintf("d%02d", i))
			if _, err := ref.Set(ctx, map[string]any{"i": i}); err != nil {
				t.Fatalf("Set %s: %v", ref.Path, err)
			}
			want[ref.Path] = true
		}
	}
	queries, err := c.CollectionGroup("parted").GetPartitionedQueries(ctx, 4)
	if err != nil {
		t.Fatalf("GetPartitionedQueries: %v", err)
	}
	if len(queries) < 2 {
		t.Fatalf("GetPartitionedQueries(4) = %d partitions, want the group split", len(queries))
	}
	seen := map[string]int{}
	for i, q := range queries {
		docs, err := q.Documents(ctx).GetAll()
		if err != nil {
			t.Fatalf("partition %d: %v", i, err)
		}
		if len(docs) == 0 {
			t.Errorf("partition %d is empty", i)
		}
		for _, d := range docs {
			seen[d.Ref.Path]++
		}
	}
	for p := range want {
		if seen[p] != 1 {
			t.Errorf("%s read %d times across partitions, want once", p, seen[p])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("read %d documents, want %d", len(seen), len(want))
	}

	// Paged by the generated client: one cursor a page, the same cursors.
	g := firestoreGAPIC(t, h)
	it := g.PartitionQuery(ctx, &firestorepb.PartitionQueryRequest{
		Parent:         firestoreDocs(h),
		PartitionCount: 3,
		PageSize:       1,
		QueryType: &firestorepb.PartitionQueryRequest_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
			From:    []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: "parted", AllDescendants: true}},
			OrderBy: []*firestorepb.StructuredQuery_Order{{Field: &firestorepb.StructuredQuery_FieldReference{FieldPath: "__name__"}, Direction: firestorepb.StructuredQuery_ASCENDING}},
		}},
	})
	var cursors []string
	for {
		cur, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("PartitionQuery: %v", err)
		}
		cursors = append(cursors, cur.GetValues()[0].GetReferenceValue())
	}
	if len(cursors) != 3 {
		t.Errorf("PartitionQuery(3) paged one a page = %v, want 3 cursors", cursors)
	}
}
