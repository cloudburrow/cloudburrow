//go:build compat

package compat

import (
	"errors"
	"fmt"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Datastore RPCs TestDatastoreEntityCRUD does not reach (#1197).

// TestDatastoreAllocateAndReserveIDs allocates IDs for incomplete keys,
// reserves an ID chosen by the caller, and writes under both.
//
// covers: google.datastore.v1.Datastore/AllocateIds, google.datastore.v1.Datastore/ReserveIds
func TestDatastoreAllocateAndReserveIDs(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()

	keys, err := c.AllocateIDs(ctx, []*datastore.Key{
		datastore.IncompleteKey("Ticket", nil), datastore.IncompleteKey("Ticket", nil),
	})
	if err != nil {
		t.Fatalf("AllocateIDs: %v", err)
	}
	if len(keys) != 2 || keys[0].ID == 0 || keys[1].ID == 0 || keys[0].ID == keys[1].ID {
		t.Fatalf("AllocateIDs = %v, want two distinct complete keys", keys)
	}

	reserved := datastore.IDKey("Ticket", 900001, nil)
	if err := c.ReserveIDs(ctx, []*datastore.Key{reserved}); err != nil {
		t.Fatalf("ReserveIDs: %v", err)
	}

	type ticket struct{ N int }
	if _, err := c.PutMulti(ctx, append(keys, reserved), []ticket{{1}, {2}, {3}}); err != nil {
		t.Fatalf("PutMulti under the allocated and reserved keys: %v", err)
	}
	var got ticket
	if err := c.Get(ctx, reserved, &got); err != nil || got.N != 3 {
		t.Errorf("Get(reserved) = %v, %v; want N=3", got, err)
	}
}

// TestDatastoreRollback writes inside a transaction, rolls it back, and
// shows the write never landed and the transaction can no longer commit.
// NewTransaction begins the transaction, so Rollback sends the RPC.
//
// covers: google.datastore.v1.Datastore/Rollback
func TestDatastoreRollback(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	key := datastore.NameKey("Rolled", "back", nil)

	tx, err := c.NewTransaction(ctx)
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}
	type v struct{ N int }
	if _, err := tx.Put(key, &v{N: 1}); err != nil {
		t.Fatalf("tx.Put: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := tx.Commit(); err == nil {
		t.Errorf("Commit after Rollback succeeded")
	}
	var got v
	if err := c.Get(ctx, key, &got); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("Get of the rolled-back write = %v, %v; want ErrNoSuchEntity", got, err)
	}
}

// TestDatastoreAggregation counts, sums and averages through the client's
// aggregation query. The emulator does not serve RunAggregationQuery; the
// call front answers it from the emulator's RunQuery (#1405): integers sum
// as an integer and a double makes the sum a double, a string is skipped,
// an empty set sums to 0 and averages to NULL, an unnamed aggregation is
// property_1, the nested query's limit applies, and an aggregation inside a
// transaction reads the transaction's snapshot.
//
// covers: google.datastore.v1.Datastore/RunAggregationQuery
func TestDatastoreAggregation(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	type row struct {
		I int64
		V float64
		W string
	}
	parent := datastore.NameKey("AggParent", "p", nil)
	var keys []*datastore.Key
	var rows []row
	for i := int64(0); i < 8; i++ {
		keys = append(keys, datastore.IDKey("Agg", i+1, parent))
		rows = append(rows, row{I: i, V: 1.5, W: "str"})
	}
	if _, err := c.PutMulti(ctx, keys, rows); err != nil {
		t.Fatalf("PutMulti: %v", err)
	}
	q := datastore.NewQuery("Agg").Ancestor(parent)
	asInt := func(v any) any {
		if pv, ok := v.(*datastorepb.Value); ok {
			switch x := pv.GetValueType().(type) {
			case *datastorepb.Value_IntegerValue:
				return x.IntegerValue
			case *datastorepb.Value_DoubleValue:
				return x.DoubleValue
			case *datastorepb.Value_NullValue:
				return nil
			}
		}
		return v
	}
	check := func(name string, aq *datastore.AggregationQuery, want map[string]any) {
		t.Helper()
		res, err := c.RunAggregationQuery(ctx, aq)
		if err != nil {
			t.Fatalf("%s: RunAggregationQuery: %v", name, err)
		}
		for k, w := range want {
			if got := asInt(res[k]); fmt.Sprint(got) != fmt.Sprint(w) {
				t.Errorf("%s: %s = %v (%T), want %v", name, k, got, got, w)
			}
		}
	}
	check("count", q.Filter("I >=", 3).NewAggregationQuery().WithCount("count"), map[string]any{"count": int64(5)})
	check("sum and avg", q.NewAggregationQuery().WithSum("I", "i_sum").WithAvg("I", "avg").WithSum("V", "v_sum"),
		map[string]any{"i_sum": int64(28), "avg": 3.5, "v_sum": 12.0})
	check("limit", q.Limit(2).NewAggregationQuery().WithSum("I", "sum").WithAvg("I", "avg"),
		map[string]any{"sum": int64(1), "avg": 0.5})
	check("non-numeric", q.NewAggregationQuery().WithSum("W", "sum").WithAvg("W", "avg"),
		map[string]any{"sum": int64(0), "avg": nil})
	check("default alias", q.NewAggregationQuery().WithSum("I", ""), map[string]any{"property_1": int64(28)})

	if _, err := c.RunAggregationQuery(ctx, q.NewAggregationQuery().WithSum("__key__", "")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SUM(__key__) = %v, want InvalidArgument", err)
	}

	tx, err := c.NewTransaction(ctx)
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := c.RunAggregationQuery(ctx, q.Transaction(tx).NewAggregationQuery().WithCount("n"))
	if err != nil {
		t.Fatalf("aggregation in a transaction: %v", err)
	}
	if n := asInt(res["n"]); fmt.Sprint(n) != "8" {
		t.Errorf("count in a transaction = %v, want 8", n)
	}
}
