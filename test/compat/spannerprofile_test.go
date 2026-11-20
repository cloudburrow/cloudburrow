//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	sp "cloud.google.com/go/spanner/apiv1"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSpannerQueryModes (#1859, #2017) is what Spanner Studio's Run and
// Explain rest on. The emulator answers PLAN and PROFILE with a
// placeholder plan of one node, "No query plan" ("The SQL query modes PLAN
// and PROFILE do not return any query plans", its README), and no local
// optimizer makes Spanner's plan, so the front refuses the modes whose
// answer is a plan (PLAN, PROFILE, WITH_PLAN_AND_STATS) UNIMPLEMENTED,
// over gRPC and REST, rather than return a plan that is not Spanner's.
// WITH_STATS, which Google documents as the results with "the overall (but
// not operator-level) execution statistics", returns the rows and the
// statistics the emulator keeps (rows_returned, elapsed_time), with no
// plan, on ExecuteSql and ExecuteStreamingSql. NORMAL returns no
// statistics.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerQueryModes(t *testing.T) {
	ctx, c, db := spannerDataClient(t, "cb-profile", "profiledb")
	sess, err := c.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: db})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s := sess.GetName()
	ins, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{
		Session: s, Seqno: 1,
		Sql:         "INSERT INTO Items (Id, Name) VALUES (1, 'one'), (2, 'two'), (3, 'three')",
		Transaction: &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Begin{Begin: spannerReadWrite()}},
	})
	if err != nil {
		t.Fatalf("ExecuteSql INSERT: %v", err)
	}
	if _, err := c.Commit(ctx, &spannerpb.CommitRequest{Session: s,
		Transaction: &spannerpb.CommitRequest_TransactionId{TransactionId: ins.GetMetadata().GetTransaction().GetId()}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	const query = "SELECT Id, Name FROM Items WHERE Id >= 0"

	for _, m := range []spannerpb.ExecuteSqlRequest_QueryMode{spannerpb.ExecuteSqlRequest_PLAN, spannerpb.ExecuteSqlRequest_PROFILE, spannerpb.ExecuteSqlRequest_WITH_PLAN_AND_STATS} {
		_, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: m})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "query plan") {
			t.Errorf("ExecuteSql %s = %v, want UNIMPLEMENTED naming the query plan", m, err)
		}
		stream, err := c.ExecuteStreamingSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: m})
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("ExecuteStreamingSql %s = %v, want UNIMPLEMENTED", m, err)
		}
	}

	stream, err := c.ExecuteStreamingSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: spannerpb.ExecuteSqlRequest_WITH_STATS})
	if err != nil {
		t.Fatalf("ExecuteStreamingSql WITH_STATS: %v", err)
	}
	var stats *spannerpb.ResultSetStats
	rows := 0
	for {
		prs, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ExecuteStreamingSql WITH_STATS Recv: %v", err)
		}
		rows += len(prs.GetValues())
		if prs.GetStats() != nil {
			stats = prs.GetStats()
		}
	}
	if rows != 6 {
		t.Errorf("ExecuteStreamingSql WITH_STATS returned %d values, want 6 (3 rows of 2)", rows)
	}
	checkStats(t, "ExecuteStreamingSql WITH_STATS", stats)

	rs, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: spannerpb.ExecuteSqlRequest_WITH_STATS})
	if err != nil {
		t.Fatalf("ExecuteSql WITH_STATS: %v", err)
	}
	if len(rs.GetRows()) != 3 {
		t.Errorf("ExecuteSql WITH_STATS returned %d rows, want 3", len(rs.GetRows()))
	}
	checkStats(t, "ExecuteSql WITH_STATS", rs.GetStats())

	rs, err = c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query})
	if err != nil || len(rs.GetRows()) != 3 || rs.GetStats() != nil {
		t.Errorf("ExecuteSql NORMAL = %d rows, stats %v, %v; want 3 rows and no statistics", len(rs.GetRows()), rs.GetStats(), err)
	}

	if os.Getenv(EnvSpannerREST) == "" {
		return
	}
	h := New(t)
	rc, err := sp.NewRESTClient(ctx, option.WithEndpoint(h.Endpoint(EnvSpannerREST)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("spanner.NewRESTClient: %v", err)
	}
	defer rc.Close()
	_, err = rc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: spannerpb.ExecuteSqlRequest_PLAN})
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented || !strings.Contains(err.Error(), "query plan") {
		t.Errorf("REST ExecuteSql PLAN = %v, want 501 naming the query plan", err)
	}
	rs, err = rc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: spannerpb.ExecuteSqlRequest_WITH_STATS})
	if err != nil {
		t.Fatalf("REST ExecuteSql WITH_STATS: %v", err)
	}
	if len(rs.GetRows()) != 3 {
		t.Errorf("REST ExecuteSql WITH_STATS returned %d rows, want 3", len(rs.GetRows()))
	}
	checkStats(t, "REST ExecuteSql WITH_STATS", rs.GetStats())
	rs, err = rc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query})
	if err != nil || len(rs.GetRows()) != 3 {
		t.Errorf("REST ExecuteSql NORMAL = %v, %v; want 3 rows", rs, err)
	}
}

// checkStats wants the emulator's query statistics for three rows, and no
// plan.
func checkStats(t *testing.T, what string, stats *spannerpb.ResultSetStats) {
	t.Helper()
	if stats == nil {
		t.Errorf("%s returned no ResultSetStats", what)
		return
	}
	fields := stats.GetQueryStats().GetFields()
	if got := fields["rows_returned"].GetStringValue(); got != "3" {
		t.Errorf("%s query_stats rows_returned = %q, want \"3\" (%v)", what, got, fields)
	}
	if _, ok := fields["elapsed_time"]; !ok {
		t.Errorf("%s query_stats has no elapsed_time: %v", what, fields)
	}
	if stats.GetQueryPlan() != nil {
		t.Errorf("%s returned a query plan %v; WITH_STATS has none", what, stats.GetQueryPlan())
	}
}
