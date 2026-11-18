package firestoreadmin

// Firestore with MongoDB compatibility (#1738): sessions and multi-document
// transactions, as Google documents them ("Behavior differences",
// Transactions; "Quotas and limits", Reads, writes, and transactions; the
// supported features page's Session commands):
//
//   - a transaction is started by a driver's session (startTransaction,
//     autocommit: false, on its first command), and ended by
//     commitTransaction or abortTransaction; endSessions ends its sessions'
//     transactions;
//   - "transactions use optimistic concurrency controls with snapshot
//     isolation": every read of a transaction sees the database as it was
//     when the transaction started, with the transaction's own writes over
//     it, and its writes are kept until it commits. Its commit is one
//     Firestore Commit of all its writes, each with a precondition that
//     the document is as the transaction's snapshot read it (its update
//     time, or that it did not exist), so the Firestore API sees the
//     transaction atomically or not at all, and a write another client
//     committed since the snapshot fails the commit with WriteConflict
//     (112) and the TransientTransactionError label, which makes the
//     driver's WithTransaction run the transaction again;
//   - "Time limit for a transaction: 270 seconds, with a 60-second idle
//     expiration time": a transaction past either is aborted, and its next
//     command is NoSuchTransaction (251) with the TransientTransactionError
//     label;
//   - an error inside a transaction aborts it, as MongoDB's does;
//   - read and write concerns follow MongoDB's rules for a transaction: a
//     read concern on its first command only, a write concern on its
//     commit or abort only.
//
// The snapshot is a Firestore read time: the reads are RunQuery and
// GetDocument at the transaction's start time.

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// Google's documented limits of a transaction; variables for the tests.
var (
	txnLifetime = 270 * time.Second
	txnIdle     = 60 * time.Second
)

// labeled is a MongoDB error with error labels.
type labeled struct {
	err    *mql.Error
	labels []string
}

func (l *labeled) Error() string { return l.err.Error() }
func (l *labeled) Unwrap() error { return l.err }

func transient(e *mql.Error) error {
	return &labeled{err: e, labels: []string{"TransientTransactionError"}}
}

func noSuchTxn(n int64) error {
	return transient(mql.Errorf(251, "NoSuchTransaction", "Transaction with { txnNumber: %d } has been aborted.", n))
}

// txnState is where a session's newest transaction is.
type txnState int

const (
	txnNone txnState = iota
	txnOpen
	txnCommitted
	txnAborted
)

// mongoSession is a logical session's transaction state.
type mongoSession struct {
	mu     sync.Mutex
	db     string // the database the session's connection authenticated to
	number int64
	state  txnState
	tx     *mongoTxn
}

// pendingWrite is a write a transaction keeps until it commits: a
// document's new fields, or its deletion (doc nil).
type pendingWrite struct {
	name string
	doc  *firestorepb.Document
}

// mongoTxn is an open transaction.
type mongoTxn struct {
	number   int64
	started  time.Time
	lastUsed time.Time
	readTime *timestamppb.Timestamp
	// seen is each document's update time at the snapshot, nil for one
	// that did not exist; the precondition of its write.
	seen    map[string]*timestamppb.Timestamp
	order   []string
	pending map[string]*pendingWrite
}

func newTxn(number int64, now time.Time) *mongoTxn {
	return &mongoTxn{number: number, started: now, lastUsed: now, readTime: timestamppb.New(now.Truncate(time.Microsecond)),
		seen: map[string]*timestamppb.Timestamp{}, pending: map[string]*pendingWrite{}}
}

// expired is whether tx is past Google's limits at now.
func (tx *mongoTxn) expired(now time.Time) bool {
	return now.Sub(tx.started) > txnLifetime || now.Sub(tx.lastUsed) > txnIdle
}

// sessionKey is a command's lsid.id, as text.
func sessionKey(cmd bson.D) (string, bool) {
	lsid, ok := mql.Lookup(cmd, "lsid").(bson.D)
	if !ok {
		return "", false
	}
	switch id := mql.Lookup(lsid, "id").(type) {
	case bson.Binary:
		return hex.EncodeToString(id.Data), true
	case string:
		return id, true
	}
	return "", false
}

func (s *MongoServer) session(mc *mongoConn, key string) *mongoSession {
	s.smu.Lock()
	defer s.smu.Unlock()
	k := mc.db.name + "|" + key
	ss, ok := s.sessions[k]
	if !ok {
		ss = &mongoSession{db: mc.db.name}
		s.sessions[k] = ss
	}
	return ss
}

// endSessions ends cmd's sessions, aborting their open transactions.
func (s *MongoServer) endSessions(mc *mongoConn, cmd bson.D) {
	ids, _ := cmd[0].Value.(bson.A)
	if mc.db == nil {
		return
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	for _, v := range ids {
		d, ok := v.(bson.D)
		if !ok {
			continue
		}
		key, ok := sessionKey(bson.D{{Key: "lsid", Value: d}})
		if ok {
			delete(s.sessions, mc.db.name+"|"+key)
		}
	}
}

// txnFor is the transaction cmd runs in, nil for none: it starts one for
// startTransaction, and refuses a command of a transaction that is not
// open, or the options a transaction does not take. The session is locked
// until done is called.
func (s *MongoServer) txnFor(ctx context.Context, mc *mongoConn, lname string, cmd bson.D) (tx *mongoTxn, done func(failed bool), err error) {
	nop := func(bool) {}
	af, hasAuto := mql.LookupE(cmd, "autocommit")
	if !hasAuto {
		return nil, nop, nil
	}
	if b, ok := af.(bool); !ok || b {
		return nil, nop, mql.Errorf(72, "InvalidOptions", "Specifying autocommit=true is not allowed.")
	}
	key, ok := sessionKey(cmd)
	if !ok {
		return nil, nop, mql.Errorf(50768, "Location50768", "Transaction must be run in a session.")
	}
	num, ok := mql.Lookup(cmd, "txnNumber").(int64)
	if !ok {
		return nil, nop, mql.Errorf(50768, "Location50768", "txnNumber may only be provided for multi-document transactions and retryable write commands.")
	}
	ss := s.session(mc, key)
	ss.mu.Lock()
	unlock := func(failed bool) {
		if failed && ss.state == txnOpen && ss.number == num {
			ss.state, ss.tx = txnAborted, nil
		}
		ss.mu.Unlock()
	}
	fail := func(e error) (*mongoTxn, func(bool), error) {
		ss.mu.Unlock()
		return nil, nop, e
	}
	now := time.Now()
	if ss.state == txnOpen && ss.tx != nil && ss.tx.expired(now) {
		ss.state, ss.tx = txnAborted, nil
	}
	if mql.Truthy(mql.Lookup(cmd, "startTransaction")) {
		if lname == "committransaction" || lname == "aborttransaction" {
			return fail(mql.Errorf(72, "InvalidOptions", "Cannot start a transaction with %s.", cmd[0].Key))
		}
		if num < ss.number || (num == ss.number && ss.state != txnNone) {
			return fail(mql.Errorf(225, "TransactionTooOld", "Cannot start transaction %d on session because a newer transaction %d has already started.", num, ss.number))
		}
		if rc, ok := mql.Lookup(cmd, "readConcern").(bson.D); ok {
			if lv, ok := mql.LookupE(rc, "level"); ok && lv != "snapshot" && lv != "majority" && lv != "local" {
				return fail(mql.Errorf(72, "InvalidOptions", "The readConcern level must be either 'local' (default), 'majority' or 'snapshot' in order to run in a transaction."))
			}
		}
		ss.number, ss.state, ss.tx = num, txnOpen, newTxn(num, now)
	} else {
		if num != ss.number {
			if num > ss.number {
				ss.number, ss.state, ss.tx = num, txnNone, nil
			}
			return fail(noSuchTxn(num))
		}
		switch ss.state {
		case txnCommitted:
			if lname == "committransaction" {
				// A commit retried after it succeeded succeeds again.
				ss.mu.Unlock()
				return nil, nop, errCommitted
			}
			if lname == "aborttransaction" {
				return fail(mql.Errorf(256, "TransactionCommitted", "Transaction %d has been committed.", num))
			}
			return fail(mql.Errorf(256, "TransactionCommitted", "Transaction %d has been committed.", num))
		case txnOpen:
		default:
			return fail(noSuchTxn(num))
		}
		if _, ok := mql.LookupE(cmd, "readConcern"); ok {
			return fail(mql.Errorf(72, "InvalidOptions", "Only the first command in a transaction may specify a readConcern"))
		}
	}
	if lname != "committransaction" && lname != "aborttransaction" {
		if _, ok := mql.LookupE(cmd, "writeConcern"); ok {
			unlock(true)
			return nil, nop, mql.Errorf(72, "InvalidOptions", "Cannot set write concern after starting a transaction.")
		}
		if notInTxn[lname] {
			unlock(true)
			return nil, nop, mql.Errorf(263, "OperationNotSupportedInTransaction", "Cannot run '%s' in a multi-document transaction.", cmd[0].Key)
		}
	}
	ss.tx.lastUsed = now
	tx = ss.tx
	switch lname {
	case "committransaction":
		err := s.commitTxn(ctx, mc, tx)
		if err == nil {
			ss.state, ss.tx = txnCommitted, nil
		} else {
			ss.state, ss.tx = txnAborted, nil
		}
		ss.mu.Unlock()
		if err != nil {
			return nil, nop, err
		}
		return nil, nop, errCommitted
	case "aborttransaction":
		ss.state, ss.tx = txnAborted, nil
		ss.mu.Unlock()
		return nil, nop, errAborted
	}
	return tx, unlock, nil
}

// errCommitted and errAborted end commitTransaction and abortTransaction,
// which txnFor answers itself.
var (
	errCommitted = errors.New("committed")
	errAborted   = errors.New("aborted")
)

// notInTxn are the commands a transaction refuses, as MongoDB's does.
var notInTxn = map[string]bool{"count": true, "listcollections": true, "listindexes": true, "createindexes": true,
	"dropindexes": true, "drop": true, "listdatabases": true, "explain": true, "killcursors": false}

// commitTxn commits tx's writes in one Firestore Commit, each conditioned
// on the document being as tx's snapshot read it.
func (s *MongoServer) commitTxn(ctx context.Context, mc *mongoConn, tx *mongoTxn) error {
	if len(tx.order) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var writes []*firestorepb.Write
	for _, name := range tx.order {
		p := tx.pending[name]
		pre := &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_Exists{Exists: false}}
		if t := tx.seen[name]; t != nil {
			pre = &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_UpdateTime{UpdateTime: t}}
		}
		w := &firestorepb.Write{CurrentDocument: pre}
		if p.doc == nil {
			w.Operation = &firestorepb.Write_Delete{Delete: name}
		} else {
			w.Operation = &firestorepb.Write_Update{Update: p.doc}
		}
		writes = append(writes, w)
	}
	c, err := s.docs()
	if err != nil {
		return fsErr(err)
	}
	_, err = c.Commit(asOwner(ctx), &firestorepb.CommitRequest{Database: mc.db.name, Writes: writes})
	switch status.Code(err) {
	case codes.OK:
		return nil
	case codes.FailedPrecondition, codes.Aborted, codes.AlreadyExists, codes.NotFound:
		return transient(mql.Errorf(112, "WriteConflict", "Write conflict during plan execution and yielding is disabled. :: Please retry your operation or multi-document transaction."))
	case codes.InvalidArgument:
		st, _ := status.FromError(err)
		return mql.BadValue("%s", st.Message())
	}
	return fsErr(err)
}

// snapshotDoc is the update time of name at tx's snapshot, nil when it did
// not exist then.
func (s *MongoServer) snapshotDoc(ctx context.Context, tx *mongoTxn, name string) (*timestamppb.Timestamp, error) {
	if t, ok := tx.seen[name]; ok {
		return t, nil
	}
	c, err := s.docs()
	if err != nil {
		return nil, fsErr(err)
	}
	d, err := c.GetDocument(asOwner(ctx), &firestorepb.GetDocumentRequest{Name: name,
		ConsistencySelector: &firestorepb.GetDocumentRequest_ReadTime{ReadTime: tx.readTime}})
	switch status.Code(err) {
	case codes.OK:
		tx.seen[name] = d.GetUpdateTime()
		return d.GetUpdateTime(), nil
	case codes.NotFound:
		tx.seen[name] = nil
		return nil, nil
	}
	return nil, fsErr(err)
}

// txnExists is whether name exists in tx: its own writes over its snapshot.
func (s *MongoServer) txnExists(ctx context.Context, tx *mongoTxn, name string) (bool, error) {
	if p, ok := tx.pending[name]; ok {
		return p.doc != nil, nil
	}
	t, err := s.snapshotDoc(ctx, tx, name)
	return t != nil, err
}

// txnPut keeps a write of tx: doc's new fields, or name's deletion.
func (s *MongoServer) txnPut(ctx context.Context, tx *mongoTxn, name string, doc *firestorepb.Document) error {
	if _, err := s.snapshotDoc(ctx, tx, name); err != nil {
		return err
	}
	if _, ok := tx.pending[name]; !ok {
		tx.order = append(tx.order, name)
	}
	tx.pending[name] = &pendingWrite{name: name, doc: doc}
	return nil
}

// txnLoad is every document of a collection in tx: its snapshot, with tx's
// writes over it, in document name order.
func (s *MongoServer) txnLoad(ctx context.Context, mc *mongoConn, tx *mongoTxn, coll string) ([]bson.D, []*firestorepb.Document, error) {
	c, err := s.docs()
	if err != nil {
		return nil, nil, fsErr(err)
	}
	stream, err := c.RunQuery(asOwner(ctx), &firestorepb.RunQueryRequest{Parent: mc.docsParent(),
		QueryType: &firestorepb.RunQueryRequest_StructuredQuery{StructuredQuery: &firestorepb.StructuredQuery{
			From: []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: coll}}}},
		ConsistencySelector: &firestorepb.RunQueryRequest_ReadTime{ReadTime: tx.readTime}})
	if err != nil {
		return nil, nil, fsErr(err)
	}
	byName := map[string]*firestorepb.Document{}
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fsErr(err)
		}
		if d := r.GetDocument(); d != nil {
			byName[d.GetName()] = d
			if _, ok := tx.seen[d.GetName()]; !ok {
				tx.seen[d.GetName()] = d.GetUpdateTime()
			}
		}
	}
	prefix := mc.docsParent() + "/" + coll + "/"
	for name, p := range tx.pending {
		if !strings.HasPrefix(name, prefix) || strings.Contains(strings.TrimPrefix(name, prefix), "/") {
			continue
		}
		if p.doc == nil {
			delete(byName, name)
		} else {
			byName[name] = p.doc
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]bson.D, 0, len(names))
	raw := make([]*firestorepb.Document, 0, len(names))
	for _, n := range names {
		out = append(out, mql.FromFirestoreDoc(byName[n]))
		raw = append(raw, byName[n])
	}
	return out, raw, nil
}
