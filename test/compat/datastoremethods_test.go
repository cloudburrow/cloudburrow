//go:build compat

package compat

import (
	"errors"
	"testing"

	"cloud.google.com/go/datastore"
	"google.golang.org/grpc/codes"
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

// TestDatastoreAggregationIsRefused records that the emulator does not
// serve RunAggregationQuery, which a count through the client sends.
//
// covers: google.datastore.v1.Datastore/RunAggregationQuery (unimplemented)
func TestDatastoreAggregationIsRefused(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	aq := datastore.NewQuery("Counted").NewAggregationQuery().WithCount("n")
	_, err := c.RunAggregationQuery(h.Context(), aq)
	wantRefused(t, "RunAggregationQuery", err, codes.Unimplemented,
		"Method google.datastore.v1.Datastore/RunAggregationQuery is unimplemented")
}
