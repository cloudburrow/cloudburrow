package main

import (
	"context"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
)

// aggregatingDatastore answers RunAggregationQuery with a fixed value and
// keeps the request; RunQuery is unimplemented, so the test fails if
// Aggregate lists entities instead.
type aggregatingDatastore struct {
	datastorepb.UnimplementedDatastoreServer
	mu  sync.Mutex
	got *datastorepb.RunAggregationQueryRequest
}

func (f *aggregatingDatastore) RunAggregationQuery(_ context.Context, req *datastorepb.RunAggregationQueryRequest) (*datastorepb.RunAggregationQueryResponse, error) {
	f.mu.Lock()
	f.got = req
	f.mu.Unlock()
	alias := req.GetAggregationQuery().GetAggregations()[0].GetAlias()
	return &datastorepb.RunAggregationQueryResponse{Batch: &datastorepb.AggregationResultBatch{
		AggregationResults: []*datastorepb.AggregationResult{{AggregateProperties: map[string]*datastorepb.Value{
			alias: {ValueType: &datastorepb.Value_DoubleValue{DoubleValue: 2.5}}}}},
	}}, nil
}

// TestDatastoreAggregateRunsAnAggregationQuery (#1405).
//
// Aggregate in the Datastore query builder sends one RunAggregationQuery
// over the filtered kind, in the page's namespace, without the listing's
// Limit, and shows Datastore's value as the listing's one row.
func TestDatastoreAggregateRunsAnAggregationQuery(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &aggregatingDatastore{}
	srv := grpc.NewServer()
	datastorepb.RegisterDatastoreServer(srv, fake)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	p := datastoreProvider{endpoint: l.Addr().String()}
	out, err := p.Build(context.Background(), "demo", []string{"Task"},
		map[string]string{"property": "done", "op": "=", "value": "true", "limit": "5", "aggregate": "AVG", "aggregateProperty": "hours"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "AVG(hours)" || out.Items[0].Fields["Value"] != "2.5" {
		t.Fatalf("Aggregate listed %+v, want one row AVG(hours) = 2.5", out.Items)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	aq := fake.got.GetAggregationQuery()
	if aggs := aq.GetAggregations(); len(aggs) != 1 || aggs[0].GetAvg().GetProperty().GetName() != "hours" {
		t.Errorf("the aggregations sent are %v, want AVG(hours)", aggs)
	}
	q := aq.GetNestedQuery()
	if q.GetFilter().GetPropertyFilter().GetProperty().GetName() != "done" || q.GetLimit() != nil {
		t.Errorf("the nested query is %v, want the done filter and no limit", q)
	}
	if q.GetKind()[0].GetName() != "Task" {
		t.Errorf("the aggregation runs over %v, want Task", q.GetKind())
	}

	if _, err := p.Build(context.Background(), "demo", []string{"Task"}, map[string]string{"aggregate": "SUM"}); err == nil {
		t.Error("SUM with no property was sent")
	}
}
