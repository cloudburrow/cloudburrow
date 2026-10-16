//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestDatastoreGQLAggregation (#1461): a GQL aggregation query, in both of
// Google's forms (AGGREGATE ... OVER (SELECT ...) and SELECT COUNT(*) ...
// FROM), through the official Datastore v1 stubs. The emulator serves no
// RunAggregationQuery; the call front has the emulator parse the nested
// GQL and aggregates over it, as it does a structured aggregation (#1405).
// explain_options stay UNIMPLEMENTED: the emulator plans no query.
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
	_, err = raw.RunAggregationQuery(ctx, &datastorepb.RunAggregationQueryRequest{ProjectId: h.Project(), ExplainOptions: &datastorepb.ExplainOptions{},
		QueryType: &datastorepb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{QueryString: "SELECT COUNT(*) FROM GqlAgg", AllowLiterals: true}}})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("explain_options: %v, want UNIMPLEMENTED", err)
	}
}
