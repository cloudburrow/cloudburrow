package firestoreadmin

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// TestTransactionLimits (#1738): Google's 270-second lifetime and 60-second
// idle expiry abort a transaction, whose next command is then
// NoSuchTransaction with the TransientTransactionError label; a commit
// retried after it succeeded succeeds again; a write concern inside a
// transaction is refused.
func TestTransactionLimits(t *testing.T) {
	s := (&Admin{}).NewMongoServer("", nil)
	mc := &mongoConn{db: &mongoDB{name: "projects/p/databases/d", id: "d"}}
	lsid := bson.D{{Key: "id", Value: bson.Binary{Subtype: 4, Data: []byte("0123456789abcdef")}}}
	cmd := func(name string, n int64, extra ...bson.E) bson.D {
		return append(bson.D{{Key: name, Value: "c"}, {Key: "lsid", Value: lsid}, {Key: "txnNumber", Value: n}, {Key: "autocommit", Value: false}}, extra...)
	}
	ctx := context.Background()
	code := func(err error) int32 {
		var me *mql.Error
		if errors.As(err, &me) {
			return me.Code
		}
		return 0
	}

	tx, done, err := s.txnFor(ctx, mc, "find", cmd("find", 1, bson.E{Key: "startTransaction", Value: true}))
	if err != nil || tx == nil {
		t.Fatalf("starting a transaction: %v", err)
	}
	done(false)
	tx.lastUsed = time.Now().Add(-txnIdle - time.Second)
	if _, _, err := s.txnFor(ctx, mc, "find", cmd("find", 1)); code(err) != 251 {
		t.Errorf("a command after 60 idle seconds: %v, want NoSuchTransaction (251)", err)
	} else if r := errorReply(err); mql.Lookup(r, "errorLabels") == nil {
		t.Errorf("the reply %v has no TransientTransactionError label", r)
	}

	tx, done, err = s.txnFor(ctx, mc, "find", cmd("find", 2, bson.E{Key: "startTransaction", Value: true}))
	if err != nil {
		t.Fatalf("starting transaction 2: %v", err)
	}
	done(false)
	tx.started = time.Now().Add(-txnLifetime - time.Second)
	if _, _, err := s.txnFor(ctx, mc, "committransaction", cmd("commitTransaction", 2)); code(err) != 251 {
		t.Errorf("a commit after 270 seconds: %v, want NoSuchTransaction (251)", err)
	}

	_, done, _ = s.txnFor(ctx, mc, "find", cmd("find", 3, bson.E{Key: "startTransaction", Value: true}))
	done(false)
	if _, _, err := s.txnFor(ctx, mc, "insert", cmd("insert", 3, bson.E{Key: "writeConcern", Value: bson.D{{Key: "w", Value: 1}}})); code(err) != 72 {
		t.Errorf("a write concern inside a transaction: %v, want InvalidOptions (72)", err)
	}
	_, done, _ = s.txnFor(ctx, mc, "find", cmd("find", 4, bson.E{Key: "startTransaction", Value: true}))
	done(false)
	for i := 0; i < 2; i++ {
		if _, _, err := s.txnFor(ctx, mc, "committransaction", cmd("commitTransaction", 4)); err != errCommitted {
			t.Errorf("commit %d of an empty transaction: %v", i+1, err)
		}
	}
	if _, _, err := s.txnFor(ctx, mc, "aborttransaction", cmd("abortTransaction", 4)); code(err) != 256 {
		t.Errorf("aborting a committed transaction: %v, want TransactionCommitted (256)", err)
	}
}
