//go:build compat

package compat

import (
	"context"
	"errors"
	"io"
	"sort"
	"testing"

	sp "cloud.google.com/go/spanner/apiv1"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// spannerDataClient returns the official generated Spanner client and a
// database with a two-column table, Items (Id INT64, Name STRING).
//
// The data-plane RPCs below are the ones the high-level spanner.Client calls
// only on some paths (a pool refill, a retry, a partitioned query), so a test
// through it could pass without sending them. The generated client in the same
// module sends exactly the RPC named.
func spannerDataClient(t *testing.T, instanceID, dbID string) (context.Context, *sp.Client, string) {
	t.Helper()
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	db := spannerDatabase(t, ctx, addr, h.Project(), instanceID, dbID,
		`CREATE TABLE Items (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)`)
	c, err := sp.NewClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner apiv1 NewClient: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return ctx, c, db
}

func spannerReadWrite() *spannerpb.TransactionOptions {
	return &spannerpb.TransactionOptions{Mode: &spannerpb.TransactionOptions_ReadWrite_{
		ReadWrite: &spannerpb.TransactionOptions_ReadWrite{}}}
}

// spannerIDs reads every Id in Items with Read, in key order.
func spannerIDs(t *testing.T, ctx context.Context, c *sp.Client, session string) []string {
	t.Helper()
	rs, err := c.Read(ctx, &spannerpb.ReadRequest{
		Session: session, Table: "Items", Columns: []string{"Id"},
		KeySet: &spannerpb.KeySet{All: true},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var ids []string
	for _, row := range rs.GetRows() {
		// INT64 travels as a decimal string on the wire.
		ids = append(ids, row.GetValues()[0].GetStringValue())
	}
	return ids
}

func spannerInsert(id, name string) *spannerpb.Mutation {
	return &spannerpb.Mutation{Operation: &spannerpb.Mutation_Insert{Insert: &spannerpb.Mutation_Write{
		Table: "Items", Columns: []string{"Id", "Name"},
		Values: []*structpb.ListValue{{Values: []*structpb.Value{
			structpb.NewStringValue(id), structpb.NewStringValue(name)}}},
	}}}
}

// TestSpannerSessionLifecycle drives the session RPCs one by one: two
// sessions from BatchCreateSessions, each returned by GetSession and listed by
// ListSessions, and a deleted one answering NOT_FOUND afterwards.
//
// covers: google.spanner.v1.Spanner/BatchCreateSessions, google.spanner.v1.Spanner/GetSession, google.spanner.v1.Spanner/ListSessions, google.spanner.v1.Spanner/DeleteSession
func TestSpannerSessionLifecycle(t *testing.T) {
	ctx, c, db := spannerDataClient(t, "cb-sess", "sessdb")

	resp, err := c.BatchCreateSessions(ctx, &spannerpb.BatchCreateSessionsRequest{Database: db, SessionCount: 2})
	if err != nil {
		t.Fatalf("BatchCreateSessions: %v", err)
	}
	if n := len(resp.GetSession()); n != 2 {
		t.Fatalf("BatchCreateSessions returned %d sessions, want 2", n)
	}
	created := map[string]bool{}
	for _, s := range resp.GetSession() {
		created[s.GetName()] = true
		got, err := c.GetSession(ctx, &spannerpb.GetSessionRequest{Name: s.GetName()})
		if err != nil {
			t.Fatalf("GetSession %s: %v", s.GetName(), err)
		}
		if got.GetName() != s.GetName() {
			t.Errorf("GetSession returned %q, want %q", got.GetName(), s.GetName())
		}
	}

	listed := map[string]bool{}
	it := c.ListSessions(ctx, &spannerpb.ListSessionsRequest{Database: db})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		listed[s.GetName()] = true
	}
	for name := range created {
		if !listed[name] {
			t.Errorf("ListSessions does not list %s", name)
		}
	}

	victim := resp.GetSession()[0].GetName()
	if err := c.DeleteSession(ctx, &spannerpb.DeleteSessionRequest{Name: victim}); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := c.GetSession(ctx, &spannerpb.GetSessionRequest{Name: victim}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSession after DeleteSession = %v, want NotFound", err)
	}
}

// TestSpannerUnaryQueryDMLAndRollback covers the unary query and read RPCs
// and the two ways a write is discarded or batched.
//
// An ExecuteSql INSERT begins its read-write transaction inline and is
// committed; the unary ExecuteSql and Read both see the row. Two
// ExecuteBatchDml inserts in a second transaction report one row each and are
// then rolled back with Rollback, which must leave Items as it was.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/Read, google.spanner.v1.Spanner/ExecuteBatchDml, google.spanner.v1.Spanner/Rollback
func TestSpannerUnaryQueryDMLAndRollback(t *testing.T) {
	ctx, c, db := spannerDataClient(t, "cb-dml", "dmldb")
	sess, err := c.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: db})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s := sess.GetName()

	rs, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{
		Session: s, Seqno: 1,
		Sql:         "INSERT INTO Items (Id, Name) VALUES (1, 'one')",
		Transaction: &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Begin{Begin: spannerReadWrite()}},
	})
	if err != nil {
		t.Fatalf("ExecuteSql INSERT: %v", err)
	}
	if n := rs.GetStats().GetRowCountExact(); n != 1 {
		t.Errorf("ExecuteSql INSERT changed %d rows, want 1", n)
	}
	txID := rs.GetMetadata().GetTransaction().GetId()
	if len(txID) == 0 {
		t.Fatal("ExecuteSql with an inline begin returned no transaction")
	}
	if _, err := c.Commit(ctx, &spannerpb.CommitRequest{
		Session: s, Transaction: &spannerpb.CommitRequest_TransactionId{TransactionId: txID},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	q, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: "SELECT Name FROM Items WHERE Id = 1"})
	if err != nil {
		t.Fatalf("ExecuteSql SELECT: %v", err)
	}
	if rows := q.GetRows(); len(rows) != 1 || rows[0].GetValues()[0].GetStringValue() != "one" {
		t.Errorf("ExecuteSql SELECT returned %v, want one row \"one\"", rows)
	}
	if got := spannerIDs(t, ctx, c, s); len(got) != 1 || got[0] != "1" {
		t.Errorf("Read returned ids %v, want [1]", got)
	}

	tx, err := c.BeginTransaction(ctx, &spannerpb.BeginTransactionRequest{Session: s, Options: spannerReadWrite()})
	if err != nil {
		t.Fatalf("BeginTransaction: %v", err)
	}
	batch, err := c.ExecuteBatchDml(ctx, &spannerpb.ExecuteBatchDmlRequest{
		Session: s, Seqno: 2,
		Transaction: &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Id{Id: tx.GetId()}},
		Statements: []*spannerpb.ExecuteBatchDmlRequest_Statement{
			{Sql: "INSERT INTO Items (Id, Name) VALUES (2, 'two')"},
			{Sql: "INSERT INTO Items (Id, Name) VALUES (3, 'three')"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteBatchDml: %v", err)
	}
	if code := batch.GetStatus().GetCode(); code != 0 {
		t.Fatalf("ExecuteBatchDml status = %v", batch.GetStatus())
	}
	if n := len(batch.GetResultSets()); n != 2 {
		t.Fatalf("ExecuteBatchDml returned %d result sets, want 2", n)
	}
	for i, r := range batch.GetResultSets() {
		if got := r.GetStats().GetRowCountExact(); got != 1 {
			t.Errorf("statement %d changed %d rows, want 1", i, got)
		}
	}
	if err := c.Rollback(ctx, &spannerpb.RollbackRequest{Session: s, TransactionId: tx.GetId()}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := spannerIDs(t, ctx, c, s); len(got) != 1 || got[0] != "1" {
		t.Errorf("after Rollback Read returned ids %v, want [1]: the rolled-back inserts were kept", got)
	}
}

// TestSpannerPartitionedQueryAndRead asks for partitions of a query and a
// read in one read-only transaction, then runs every query partition with
// its token and checks that together they return each row exactly once.
//
// covers: google.spanner.v1.Spanner/PartitionQuery, google.spanner.v1.Spanner/PartitionRead
func TestSpannerPartitionedQueryAndRead(t *testing.T) {
	ctx, c, db := spannerDataClient(t, "cb-part", "partdb")
	sess, err := c.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: db})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s := sess.GetName()
	if _, err := c.Commit(ctx, &spannerpb.CommitRequest{
		Session:     s,
		Transaction: &spannerpb.CommitRequest_SingleUseTransaction{SingleUseTransaction: spannerReadWrite()},
		Mutations:   []*spannerpb.Mutation{spannerInsert("1", "a"), spannerInsert("2", "b"), spannerInsert("3", "c")},
	}); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	ro, err := c.BeginTransaction(ctx, &spannerpb.BeginTransactionRequest{Session: s, Options: &spannerpb.TransactionOptions{
		Mode: &spannerpb.TransactionOptions_ReadOnly_{ReadOnly: &spannerpb.TransactionOptions_ReadOnly{}}}})
	if err != nil {
		t.Fatalf("BeginTransaction read-only: %v", err)
	}
	sel := &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Id{Id: ro.GetId()}}

	const sql = "SELECT Id FROM Items"
	pq, err := c.PartitionQuery(ctx, &spannerpb.PartitionQueryRequest{Session: s, Sql: sql, Transaction: sel})
	if err != nil {
		t.Fatalf("PartitionQuery: %v", err)
	}
	if len(pq.GetPartitions()) == 0 {
		t.Fatal("PartitionQuery returned no partitions")
	}
	var ids []string
	for _, p := range pq.GetPartitions() {
		stream, err := c.ExecuteStreamingSql(ctx, &spannerpb.ExecuteSqlRequest{
			Session: s, Sql: sql, Transaction: sel, PartitionToken: p.GetPartitionToken(),
		})
		if err != nil {
			t.Fatalf("ExecuteStreamingSql partition: %v", err)
		}
		for {
			prs, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("partition stream: %v", err)
			}
			for _, v := range prs.GetValues() {
				ids = append(ids, v.GetStringValue())
			}
		}
	}
	sort.Strings(ids)
	if len(ids) != 3 || ids[0] != "1" || ids[1] != "2" || ids[2] != "3" {
		t.Errorf("the query partitions returned ids %v, want [1 2 3]", ids)
	}

	pr, err := c.PartitionRead(ctx, &spannerpb.PartitionReadRequest{
		Session: s, Table: "Items", Columns: []string{"Id"},
		KeySet: &spannerpb.KeySet{All: true}, Transaction: sel,
	})
	if err != nil {
		t.Fatalf("PartitionRead: %v", err)
	}
	if len(pr.GetPartitions()) == 0 {
		t.Fatal("PartitionRead returned no partitions")
	}
	for _, p := range pr.GetPartitions() {
		if len(p.GetPartitionToken()) == 0 {
			t.Error("PartitionRead returned a partition with no token")
		}
	}
}

// TestSpannerBatchWrite applies two mutation groups with BatchWrite and reads
// both rows back: each group is answered with an OK status naming its index.
//
// covers: google.spanner.v1.Spanner/BatchWrite
func TestSpannerBatchWrite(t *testing.T) {
	ctx, c, db := spannerDataClient(t, "cb-bw", "bwdb")
	sess, err := c.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: db})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s := sess.GetName()
	stream, err := c.BatchWrite(ctx, &spannerpb.BatchWriteRequest{
		Session: s,
		MutationGroups: []*spannerpb.BatchWriteRequest_MutationGroup{
			{Mutations: []*spannerpb.Mutation{spannerInsert("10", "ten")}},
			{Mutations: []*spannerpb.Mutation{spannerInsert("20", "twenty")}},
		},
	})
	if err != nil {
		t.Fatalf("BatchWrite: %v", err)
	}
	applied := map[int32]bool{}
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("BatchWrite stream: %v", err)
		}
		if r.GetStatus().GetCode() != 0 {
			t.Errorf("groups %v answered %v", r.GetIndexes(), r.GetStatus())
			continue
		}
		for _, i := range r.GetIndexes() {
			applied[i] = true
		}
	}
	if !applied[0] || !applied[1] {
		t.Errorf("BatchWrite applied groups %v, want 0 and 1", applied)
	}
	if got := spannerIDs(t, ctx, c, s); len(got) != 2 || got[0] != "10" || got[1] != "20" {
		t.Errorf("after BatchWrite Read returned ids %v, want [10 20]", got)
	}
}

// TestSpannerFetchCacheUpdateIsRefused records what is answered for
// FetchCacheUpdate, the location-aware routing stream: UNIMPLEMENTED on the
// first receive. The emulator says nothing; the call front names why
// (#1498): the emulator is one server, with nowhere to route a client.
//
// covers: google.spanner.v1.Spanner/FetchCacheUpdate (unimplemented)
func TestSpannerFetchCacheUpdateIsRefused(t *testing.T) {
	ctx, c, db := spannerDataClient(t, "cb-fcu", "fcudb")
	stream, err := c.FetchCacheUpdate(ctx, &spannerpb.FetchCacheUpdateRequest{Database: db})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "routing") {
		t.Fatalf("FetchCacheUpdate = %v, want Unimplemented naming why", err)
	}
	t.Logf("FetchCacheUpdate: %v", err)
}
