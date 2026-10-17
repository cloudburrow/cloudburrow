package queryfront

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dspb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

func TestPlanSummary(t *testing.T) {
	q := &dspb.Query{Kind: []*dspb.KindExpression{{Name: "Task"}},
		Filter: &dspb.Filter{FilterType: &dspb.Filter_CompositeFilter{CompositeFilter: &dspb.CompositeFilter{Op: dspb.CompositeFilter_AND,
			Filters: []*dspb.Filter{
				{FilterType: &dspb.Filter_PropertyFilter{PropertyFilter: &dspb.PropertyFilter{Property: &dspb.PropertyReference{Name: "priority"}, Op: dspb.PropertyFilter_GREATER_THAN}}},
				{FilterType: &dspb.Filter_PropertyFilter{PropertyFilter: &dspb.PropertyFilter{Property: &dspb.PropertyReference{Name: "done"}, Op: dspb.PropertyFilter_EQUAL}}},
			}}}},
		Order: []*dspb.PropertyOrder{{Property: &dspb.PropertyReference{Name: "priority"}, Direction: dspb.PropertyOrder_DESCENDING}}}
	got := planSummary(q).GetIndexesUsed()[0].AsMap()
	if got["query_scope"] != "Collection" || got["properties"] != "(done ASC, priority DESC, __name__ DESC)" {
		t.Errorf("plan = %v", got)
	}
	if got := planSummary(&dspb.Query{}).GetIndexesUsed()[0].AsMap(); got["query_scope"] != "Collection group" || got["properties"] != "(__name__ ASC)" {
		t.Errorf("kindless plan = %v", got)
	}
}

func TestExplainRunQuery(t *testing.T) {
	d, f := newDatastore(t, ent(nil), ent(nil))
	ctx := context.Background()
	q := &dspb.Query{Kind: []*dspb.KindExpression{{Name: "k"}}}
	resp, err := d.RunQuery(ctx, &dspb.RunQueryRequest{ExplainOptions: &dspb.ExplainOptions{}, QueryType: &dspb.RunQueryRequest_Query{Query: q}})
	if err != nil || resp.GetExplainMetrics().GetPlanSummary() == nil || resp.GetExplainMetrics().GetExecutionStats() != nil ||
		len(resp.GetBatch().GetEntityResults()) != 0 || f.calls != 0 {
		t.Fatalf("plan only = %v, %v (%d emulator calls)", resp, err, f.calls)
	}
	resp, err = d.RunQuery(ctx, &dspb.RunQueryRequest{ExplainOptions: &dspb.ExplainOptions{Analyze: true}, QueryType: &dspb.RunQueryRequest_Query{Query: q}})
	st := resp.GetExplainMetrics().GetExecutionStats()
	if err != nil || st.GetResultsReturned() != 2 || st.GetReadOperations() != 2 || st.GetDebugStats().AsMap()["documents_scanned"] != "2" {
		t.Fatalf("analyze = %v, %v", resp, err)
	}
	agg, err := d.RunAggregationQuery(ctx, &dspb.RunAggregationQueryRequest{ExplainOptions: &dspb.ExplainOptions{Analyze: true},
		QueryType: &dspb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &dspb.GqlQuery{QueryString: "SELECT COUNT(*) FROM k"}}})
	if err != nil || agg.GetExplainMetrics().GetExecutionStats().GetResultsReturned() != 1 || len(agg.GetBatch().GetAggregationResults()) != 1 {
		t.Fatalf("aggregation analyze = %v, %v", agg, err)
	}
}

func TestAsksExplain(t *testing.T) {
	grpcBody := func(m proto.Message) []byte {
		b, _ := proto.Marshal(m)
		h := make([]byte, 5)
		binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
		return append(h, b...)
	}
	with := &dspb.RunQueryRequest{ExplainOptions: &dspb.ExplainOptions{}}
	for name, c := range map[string]struct {
		r    *http.Request
		want bool
	}{
		"grpc explain": {httptest.NewRequest("POST", datastoreRunQuery, bytes.NewReader(grpcBody(with))), true},
		"grpc plain":   {httptest.NewRequest("POST", datastoreRunQuery, bytes.NewReader(grpcBody(&dspb.RunQueryRequest{ProjectId: "p"}))), false},
		"rest explain": {httptest.NewRequest("POST", "/v1/projects/p:runQuery", strings.NewReader(`{"explainOptions":{"analyze":true}}`)), true},
		"rest plain":   {httptest.NewRequest("POST", "/v1/projects/p:runQuery", strings.NewReader(`{"query":{}}`)), false},
	} {
		if strings.HasPrefix(name, "grpc") {
			c.r.Header.Set("Content-Type", "application/grpc")
		}
		before, _ := io.ReadAll(c.r.Body)
		c.r.Body = io.NopCloser(bytes.NewReader(before))
		d := &Datastore{}
		if got := d.Claims(c.r); got != c.want {
			t.Errorf("%s: claimed %v, want %v", name, got, c.want)
		}
		after, _ := io.ReadAll(c.r.Body)
		if !bytes.Equal(before, after) {
			t.Errorf("%s: the body was not put back", name)
		}
	}
}
