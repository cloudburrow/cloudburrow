//go:build compat

package compat

import (
	"errors"
	"io"
	"os"
	"testing"

	sp "cloud.google.com/go/spanner/apiv1"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/option"
)

// TestSpannerQueryModes (#1859, #2017, #2029) is what Spanner Studio's Run
// and Explain rest on, over gRPC (ExecuteSql and ExecuteStreamingSql) and
// REST. PLAN returns the query's plan (plan.go) and no rows; PROFILE the
// rows, the plan with each operator's execution statistics, and the query
// statistics; WITH_PLAN_AND_STATS the rows, the plan without operator
// statistics, and the query statistics. WITH_STATS, which Google documents
// as the results with "the overall (but not operator-level) execution
// statistics", returns the rows and the statistics the emulator keeps
// (rows_returned, elapsed_time), with no plan. NORMAL returns no
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
		wantRows := 3
		if m == spannerpb.ExecuteSqlRequest_PLAN {
			wantRows = 0
		}
		rs, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: m})
		if err != nil {
			t.Fatalf("ExecuteSql %s: %v", m, err)
		}
		if len(rs.GetRows()) != wantRows {
			t.Errorf("ExecuteSql %s returned %d rows, want %d", m, len(rs.GetRows()), wantRows)
		}
		checkPlan(t, "ExecuteSql "+m.String(), m, rs.GetStats())
		stream, err := c.ExecuteStreamingSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: m})
		if err != nil {
			t.Fatalf("ExecuteStreamingSql %s: %v", m, err)
		}
		var stats *spannerpb.ResultSetStats
		values := 0
		for {
			prs, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("ExecuteStreamingSql %s Recv: %v", m, err)
			}
			values += len(prs.GetValues())
			if prs.GetStats() != nil {
				stats = prs.GetStats()
			}
		}
		if values != 2*wantRows {
			t.Errorf("ExecuteStreamingSql %s returned %d values, want %d", m, values, 2*wantRows)
		}
		checkPlan(t, "ExecuteStreamingSql "+m.String(), m, stats)
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
	rs, err = rc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: spannerpb.ExecuteSqlRequest_PLAN})
	if err != nil {
		t.Fatalf("REST ExecuteSql PLAN: %v", err)
	}
	checkPlan(t, "REST ExecuteSql PLAN", spannerpb.ExecuteSqlRequest_PLAN, rs.GetStats())
	rs, err = rc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: query, QueryMode: spannerpb.ExecuteSqlRequest_PROFILE})
	if err != nil || len(rs.GetRows()) != 3 {
		t.Fatalf("REST ExecuteSql PROFILE = %v, %v; want 3 rows", rs, err)
	}
	checkPlan(t, "REST ExecuteSql PROFILE", spannerpb.ExecuteSqlRequest_PROFILE, rs.GetStats())
	// a set operation is planned over REST too (#2029)
	rs, err = rc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s, Sql: "SELECT Id FROM Items INTERSECT DISTINCT SELECT Id FROM Items", QueryMode: spannerpb.ExecuteSqlRequest_PLAN})
	if err != nil {
		t.Errorf("REST ExecuteSql PLAN of an INTERSECT: %v", err)
	} else {
		union := false
		for _, n := range rs.GetStats().GetQueryPlan().GetPlanNodes() {
			union = union || n.GetDisplayName() == "Union All"
		}
		if !union {
			t.Errorf("REST ExecuteSql PLAN of an INTERSECT: no Union All in %v", rs.GetStats().GetQueryPlan())
		}
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

// checkPlan wants a mode's plan and statistics: PLAN a plan without
// statistics, PROFILE a plan whose operators carry their statistics and the
// query statistics for three rows, WITH_PLAN_AND_STATS the plan without
// operator statistics and the query statistics.
func checkPlan(t *testing.T, what string, m spannerpb.ExecuteSqlRequest_QueryMode, stats *spannerpb.ResultSetStats) {
	t.Helper()
	nodes := stats.GetQueryPlan().GetPlanNodes()
	if len(nodes) == 0 || nodes[0].GetKind() != spannerpb.PlanNode_RELATIONAL {
		t.Errorf("%s returned no plan: %v", what, stats)
		return
	}
	scan := false
	for _, n := range nodes {
		if n.GetDisplayName() == "Scan" && n.GetMetadata().GetFields()["scan_target"].GetStringValue() == "Items" {
			scan = true
		}
		if n.GetKind() == spannerpb.PlanNode_RELATIONAL && (n.GetExecutionStats() != nil) != (m == spannerpb.ExecuteSqlRequest_PROFILE) {
			t.Errorf("%s: node %d (%s) execution statistics %v", what, n.GetIndex(), n.GetDisplayName(), n.GetExecutionStats())
		}
	}
	if !scan {
		t.Errorf("%s: no Scan of Items in the plan %v", what, nodes)
	}
	if m == spannerpb.ExecuteSqlRequest_PLAN {
		if stats.GetQueryStats() != nil {
			t.Errorf("%s returned query statistics %v", what, stats.GetQueryStats())
		}
		return
	}
	f := stats.GetQueryStats().GetFields()
	if f["rows_returned"].GetStringValue() != "3" || f["elapsed_time"] == nil || f["cpu_time"] == nil || f["rows_scanned"].GetStringValue() != "3" {
		t.Errorf("%s query_stats = %v, want 3 rows returned and scanned, elapsed_time and cpu_time", what, f)
	}
}
