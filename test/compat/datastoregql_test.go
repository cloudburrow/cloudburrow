//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestDatastoreGQLAggregation (#1461): a GQL aggregation query, in both of
// Google's forms (AGGREGATE ... OVER (SELECT ...) and SELECT COUNT(*) ...
// FROM), through the official Datastore v1 stubs. The emulator serves no
// RunAggregationQuery; the call front has the emulator parse the nested
// GQL and aggregates over it, as it does a structured aggregation (#1405).
// With explain_options it answers Google's ExplainMetrics (#1500,
// TestDatastoreExplain).
//
// covers: google.datastore.v1.Datastore/RunAggregationQuery
func TestDatastoreGQLAggregation(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	conn, err := grpc.NewClient(h.Endpoint(EnvDatastore), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := datastorepb.NewDatastoreClient(conn)

	type row struct{ I int64 }
	var keys []*datastore.Key
	var rows []row
	for i := int64(1); i <= 4; i++ {
		keys = append(keys, datastore.IDKey("GqlAgg", i, nil))
		rows = append(rows, row{I: i})
	}
	if _, err := c.PutMulti(ctx, keys, rows); err != nil {
		t.Fatalf("PutMulti: %v", err)
	}
	run := func(q string) map[string]*datastorepb.Value {
		t.Helper()
		resp, err := raw.RunAggregationQuery(ctx, &datastorepb.RunAggregationQueryRequest{ProjectId: h.Project(),
			QueryType: &datastorepb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{QueryString: q, AllowLiterals: true}}})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if resp.GetQuery() == nil {
			t.Errorf("%s: no parsed query in the response", q)
		}
		res := resp.GetBatch().GetAggregationResults()
		if len(res) != 1 {
			t.Fatalf("%s: %d results, want 1", q, len(res))
		}
		return res[0].GetAggregateProperties()
	}
	got := run("AGGREGATE COUNT(*) AS n, SUM(I), AVG(I) AS a OVER (SELECT * FROM GqlAgg WHERE I >= 2)")
	if got["n"].GetIntegerValue() != 3 || got["property_2"].GetIntegerValue() != 9 || got["a"].GetDoubleValue() != 3 {
		t.Errorf("AGGREGATE ... OVER = %v", got)
	}
	got = run("SELECT COUNT_UP_TO(2) AS c FROM GqlAgg")
	if got["c"].GetIntegerValue() != 2 {
		t.Errorf("SELECT COUNT_UP_TO(2) = %v", got)
	}
	resp, err := raw.RunAggregationQuery(ctx, &datastorepb.RunAggregationQueryRequest{ProjectId: h.Project(), ExplainOptions: &datastorepb.ExplainOptions{Analyze: true},
		QueryType: &datastorepb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{QueryString: "SELECT COUNT(*) FROM GqlAgg", AllowLiterals: true}}})
	if err != nil || resp.GetExplainMetrics().GetExecutionStats().GetResultsReturned() != 1 ||
		resp.GetBatch().GetAggregationResults()[0].GetAggregateProperties()["property_1"].GetIntegerValue() != 4 {
		t.Errorf("GQL aggregation with explain_options = %v, %v", resp, err)
	}
}

// TestDatastoreExplain (#1500): a query and an aggregation with explain
// options, through the official Datastore client's RunWithOptions and
// RunAggregationQueryWithOptions. Planned alone, they return the plan
// summary (the index Google's planner would use, in Google's shape) and
// no results; analyzed, the results and the execution stats too.
//
// covers: google.datastore.v1.Datastore/RunQuery
func TestDatastoreExplain(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	type task struct {
		Done     bool
		Priority int64
	}
	keys := []*datastore.Key{datastore.NameKey("ExplainTask", "a", nil), datastore.NameKey("ExplainTask", "b", nil), datastore.NameKey("ExplainTask", "c", nil)}
	if _, err := c.PutMulti(ctx, keys, []task{{true, 1}, {false, 2}, {false, 3}}); err != nil {
		t.Fatalf("PutMulti: %v", err)
	}
	q := datastore.NewQuery("ExplainTask").FilterField("Done", "=", false).Order("-Priority")

	it := c.RunWithOptions(ctx, q, datastore.ExplainOptions{})
	var tk task
	if _, err := it.Next(&tk); err != iterator.Done {
		t.Errorf("planned query: %v, want no results", err)
	}
	m := it.ExplainMetrics
	if m == nil || m.PlanSummary == nil || len(m.PlanSummary.IndexesUsed) != 1 || m.ExecutionStats != nil {
		t.Fatalf("planned metrics = %+v, want a plan summary alone", m)
	}
	if idx := *m.PlanSummary.IndexesUsed[0]; idx["query_scope"] != "Collection" || idx["properties"] != "(Done ASC, Priority DESC, __name__ DESC)" {
		t.Errorf("index used = %v", idx)
	}

	it = c.RunWithOptions(ctx, q, datastore.ExplainOptions{Analyze: true})
	n := 0
	for {
		_, err := it.Next(&tk)
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("analyzed query: %v", err)
		}
		n++
	}
	st := it.ExplainMetrics.ExecutionStats
	if n != 2 || st == nil || st.ResultsReturned != 2 || st.ReadOperations != 2 || st.ExecutionDuration == nil || st.DebugStats == nil {
		t.Fatalf("analyzed: %d results, stats %+v", n, st)
	}

	ar, err := c.RunAggregationQueryWithOptions(ctx, q.NewAggregationQuery().WithCount("n"), datastore.ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatalf("RunAggregationQueryWithOptions: %v", err)
	}
	if v, ok := ar.Result["n"].(*datastorepb.Value); !ok || v.GetIntegerValue() != 2 || ar.ExplainMetrics.ExecutionStats.ResultsReturned != 1 {
		t.Errorf("aggregation with explain = %v, %+v", ar.Result, ar.ExplainMetrics)
	}
}
