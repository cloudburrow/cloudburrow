//go:build compat

package compat

import (
	"errors"
	"io"
	"testing"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
)

// TestSpannerProfileModeReturnsQueryStats is what Spanner Studio's Run and
// Explain rest on (#1859): a query in PROFILE mode returns its rows and the
// query statistics the emulator documents (rows_returned, elapsed_time),
// and neither PLAN nor PROFILE returns a query plan with an operator in it
// ("The SQL query modes PLAN and PROFILE do not return any query plans",
// the emulator's README), which is why the console says the plan is not
// available locally rather than drawing one.
func TestSpannerProfileModeReturnsQueryStats(t *testing.T) {
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

	stream, err := c.ExecuteStreamingSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s,
		Sql: "SELECT Id, Name FROM Items WHERE Id >= 0", QueryMode: spannerpb.ExecuteSqlRequest_PROFILE})
	if err != nil {
		t.Fatalf("ExecuteStreamingSql PROFILE: %v", err)
	}
	var stats *spannerpb.ResultSetStats
	for {
		prs, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ExecuteStreamingSql PROFILE Recv: %v", err)
		}
		if prs.GetStats() != nil {
			stats = prs.GetStats()
		}
	}
	if stats == nil {
		t.Fatal("PROFILE mode returned no ResultSetStats")
	}
	fields := stats.GetQueryStats().GetFields()
	if got := fields["rows_returned"].GetStringValue(); got != "3" {
		t.Errorf("PROFILE query_stats rows_returned = %q, want \"3\" (%v)", got, fields)
	}
	if _, ok := fields["elapsed_time"]; !ok {
		t.Errorf("PROFILE query_stats has no elapsed_time: %v", fields)
	}
	checkNoRealPlan(t, "PROFILE", stats.GetQueryPlan())

	plan, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: s,
		Sql: "SELECT Id, Name FROM Items WHERE Id >= 0", QueryMode: spannerpb.ExecuteSqlRequest_PLAN})
	if err != nil {
		t.Fatalf("ExecuteSql PLAN: %v", err)
	}
	checkNoRealPlan(t, "PLAN", plan.GetStats().GetQueryPlan())
}

// checkNoRealPlan fails if a plan has a node other than the emulator's
// placeholder, which would mean the emulator now returns plans and the
// console should draw them.
func checkNoRealPlan(t *testing.T, mode string, p *spannerpb.QueryPlan) {
	t.Helper()
	for _, n := range p.GetPlanNodes() {
		if n.GetDisplayName() != "No query plan" || n.GetKind() != spannerpb.PlanNode_KIND_UNSPECIFIED {
			t.Errorf("%s mode returned a plan node %q (%s): the emulator returns plans now; draw them in Spanner Studio", mode, n.GetDisplayName(), n.GetKind())
		}
	}
}
