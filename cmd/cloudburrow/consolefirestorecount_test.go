package main

import (
	"context"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
)

// countingFirestore answers RunAggregationQuery with a fixed count and
// keeps the request; every other method is unimplemented, so the test fails
// if Count only lists documents instead.
type countingFirestore struct {
	firestorepb.UnimplementedFirestoreServer
	mu  sync.Mutex
	got *firestorepb.RunAggregationQueryRequest
}

func (f *countingFirestore) RunAggregationQuery(req *firestorepb.RunAggregationQueryRequest, s firestorepb.Firestore_RunAggregationQueryServer) error {
	f.mu.Lock()
	f.got = req
	f.mu.Unlock()
	return s.Send(&firestorepb.RunAggregationQueryResponse{Result: &firestorepb.AggregationResult{
		AggregateFields: map[string]*firestorepb.Value{"count": {ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 42}}},
	}})
}

// TestFirestoreCountOnlyRunsAnAggregationQuery (#1198).
//
// Count only in the query builder sends one RunAggregationQuery, a COUNT
// over the filtered collection, as the official client sends it, and shows
// Firestore's count as the listing's one row.
func TestFirestoreCountOnlyRunsAnAggregationQuery(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &countingFirestore{}
	srv := grpc.NewServer()
	firestorepb.RegisterFirestoreServer(srv, fake)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	p := firestoreProvider{endpoint: l.Addr().String()}
	out, err := p.Build(context.Background(), "demo", []string{"users"},
		map[string]string{"field": "age", "op": ">=", "value": "18", "count": "true", "limit": "5", "orderBy": "age"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "count(*)" || out.Items[0].Fields["Count"] != "42" {
		t.Fatalf("Count only listed %+v, want one row count(*) = 42", out.Items)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.got == nil {
		t.Fatal("no RunAggregationQuery reached Firestore")
	}
	sq := fake.got.GetStructuredAggregationQuery()
	if aggs := sq.GetAggregations(); len(aggs) != 1 || aggs[0].GetCount() == nil {
		t.Errorf("the aggregations sent are %v, want one COUNT", aggs)
	}
	q := sq.GetStructuredQuery()
	if f := q.GetWhere().GetFieldFilter(); f.GetField().GetFieldPath() != "age" ||
		f.GetOp() != firestorepb.StructuredQuery_FieldFilter_GREATER_THAN_OR_EQUAL || f.GetValue().GetDoubleValue() != 18 {
		t.Errorf("the count's filter is %v, want age >= 18", q.GetWhere())
	}
	if q.GetLimit() != nil || len(q.GetOrderBy()) != 0 {
		t.Errorf("the count was sent with limit %v and order %v, which Count only does not apply", q.GetLimit(), q.GetOrderBy())
	}
	if len(q.GetFrom()) != 1 || q.GetFrom()[0].GetCollectionId() != "users" {
		t.Errorf("the count runs over %v, want users", q.GetFrom())
	}
}
