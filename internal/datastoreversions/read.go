package datastoreversions

import (
	"context"
	"crypto/rand"
	"net/url"
	"strings"
	"sync"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// txnPrefix begins the transactions the log begins itself: read-only ones
// at a read time, which the emulator never sees.
const txnPrefix = "cloudburrow-pitr:"

// txnLife is how long such a transaction is kept.
const txnLife = time.Hour

type txn struct {
	project, database string
	at, expires       time.Time
}

type txns struct {
	mu sync.Mutex
	m  map[string]txn
}

func newTxns() *txns { return &txns{m: map[string]txn{}} }

// IsTransaction reports whether t is a read-only transaction the log
// began.
func IsTransaction(t []byte) bool { return strings.HasPrefix(string(t), txnPrefix) }

// BeginReadOnly begins a read-only transaction of the database at t.
func (l *Log) BeginReadOnly(project, database string, t time.Time) []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := txnPrefix + string(b)
	l.txns.mu.Lock()
	defer l.txns.mu.Unlock()
	now := time.Now()
	for k, x := range l.txns.m {
		if now.After(x.expires) {
			delete(l.txns.m, k)
		}
	}
	l.txns.m[id] = txn{project: project, database: emulatorDB(database), at: t, expires: now.Add(txnLife)}
	return []byte(id)
}

// transactionTime is the read time of a transaction the log began.
func (l *Log) transactionTime(t []byte) (time.Time, error) {
	l.txns.mu.Lock()
	defer l.txns.mu.Unlock()
	x, ok := l.txns.m[string(t)]
	if !ok {
		return time.Time{}, status.Error(codes.InvalidArgument, "The referenced transaction has expired or is no longer valid.")
	}
	return x.at, nil
}

// EndTransaction forgets a transaction the log began: its commit, which
// may not write, or its rollback.
func (l *Log) EndTransaction(t []byte, mutations int) error {
	if _, err := l.transactionTime(t); err != nil {
		return err
	}
	l.txns.mu.Lock()
	delete(l.txns.m, string(t))
	l.txns.mu.Unlock()
	if mutations > 0 {
		return status.Error(codes.InvalidArgument, "Cannot modify entities in a read-only transaction.")
	}
	return nil
}

// ReadTime is the time a read is at, if it is at an earlier time the log
// serves: its read_time, its new read-only transaction's, or the
// transaction's the log began. A read time in the future is the
// emulator's to refuse.
func (l *Log) ReadTime(ro *pb.ReadOptions) (time.Time, bool, error) {
	if t := ro.GetTransaction(); IsTransaction(t) {
		at, err := l.transactionTime(t)
		return at, err == nil, err
	}
	ts := ro.GetReadTime()
	if ts == nil {
		ts = ro.GetNewTransaction().GetReadOnly().GetReadTime()
	}
	if ts == nil || ts.CheckValid() != nil || ts.AsTime().After(l.now()) {
		return time.Time{}, false, nil
	}
	return ts.AsTime(), true, nil
}

// routed is ctx with the routing header of the database's calls alone.
func routed(ctx context.Context, project, database string) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	params := "project_id=" + url.QueryEscape(project)
	if database != "" {
		params += "&database_id=" + url.QueryEscape(database)
	}
	md.Set("x-goog-request-params", params)
	return metadata.NewOutgoingContext(ctx, md)
}

// check refuses a read when the log no longer goes with the emulator's
// entities; it reads the marker again at most every markerRecheck.
func (l *Log) check(ctx context.Context) error {
	if err := l.waitReady(ctx); err != nil {
		return err
	}
	l.recheck(ctx)
	return nil
}

// LookupAt answers a lookup at t: each key's entity then.
func (l *Log) LookupAt(ctx context.Context, c pb.DatastoreClient, req *pb.LookupRequest, t time.Time) (*pb.LookupResponse, error) {
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	project, database := req.GetProjectId(), emulatorDB(req.GetDatabaseId())
	db := DBName(project, database)
	keys := req.GetKeys()
	cur, err := lookupNow(routed(ctx, project, database), c, project, database, keys)
	if err != nil {
		return nil, err
	}
	if err := l.settle(ctx, l.begun()); err != nil {
		return nil, err
	}
	out := &pb.LookupResponse{ReadTime: timestamppb.New(t)}
	if tx := req.GetReadOptions().GetNewTransaction(); tx != nil {
		out.Transaction = l.BeginReadOnly(project, database, t)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		e, ver, at, held := l.stateAt(db, k, t)
		var res *pb.EntityResult
		switch {
		case held && e != nil:
			e = proto.Clone(e).(*pb.Entity)
			e.Key = proto.Clone(k).(*pb.Key)
			res = &pb.EntityResult{Entity: e, Version: ver}
			if !at.IsZero() {
				res.UpdateTime = timestamppb.New(at)
			}
		case held:
			res = &pb.EntityResult{Entity: &pb.Entity{Key: k}, Version: ver}
		default:
			if f, ok := cur[keyID(k)]; ok {
				res = proto.Clone(f).(*pb.EntityResult)
			} else {
				res = &pb.EntityResult{Entity: &pb.Entity{Key: k}}
			}
		}
		if held && e == nil || !held && cur[keyID(k)] == nil {
			out.Missing = append(out.Missing, res)
		} else {
			out.Found = append(out.Found, res)
		}
	}
	return out, nil
}

// EntityFilter takes some of a database's entities: by kind and namespace,
// all when empty.
type EntityFilter struct {
	Kinds, Namespaces []string
}

func (f EntityFilter) takes(k *pb.Key) bool {
	ok := len(f.Kinds) == 0
	for _, x := range f.Kinds {
		ok = ok || kindOf(k) == x
	}
	if !ok {
		return false
	}
	if len(f.Namespaces) == 0 {
		return true
	}
	for _, ns := range f.Namespaces {
		if ns == k.GetPartitionId().GetNamespaceId() {
			return true
		}
	}
	return false
}

func kindOf(k *pb.Key) string {
	if p := k.GetPath(); len(p) > 0 {
		return p[len(p)-1].GetKind()
	}
	return ""
}

// At is the database's entities the filter takes at t, given read, which
// reads them as they are now (in namespace, then key order).
func (l *Log) At(ctx context.Context, project, database string, t time.Time, f EntityFilter, read func() ([]*pb.Entity, error)) ([]*pb.Entity, error) {
	if err := l.check(ctx); err != nil {
		return nil, err
	}
	cur, err := read()
	if err != nil {
		return nil, err
	}
	if err := l.settle(ctx, l.begun()); err != nil {
		return nil, err
	}
	database = emulatorDB(database)
	db := DBName(project, database)
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.dbs[db]
	if d == nil {
		return cur, nil
	}
	seen := map[string]bool{}
	out := make([]*pb.Entity, 0, len(cur))
	for _, e := range cur {
		id := keyID(e.GetKey())
		seen[id] = true
		h := d.keys[id]
		if h == nil {
			out = append(out, e)
			continue
		}
		if was, _, _ := h.at(t); was != nil {
			was = proto.Clone(was).(*pb.Entity)
			was.Key = e.GetKey()
			out = append(out, was)
		}
	}
	for id, h := range d.keys {
		if seen[id] || !f.takes(d.pk[id]) || strings.HasPrefix(kindOf(d.pk[id]), "__") {
			continue
		}
		if was, _, _ := h.at(t); was != nil {
			was = proto.Clone(was).(*pb.Entity)
			k := proto.Clone(d.pk[id]).(*pb.Key)
			if k.PartitionId == nil {
				k.PartitionId = &pb.PartitionId{}
			}
			k.PartitionId.ProjectId, k.PartitionId.DatabaseId = project, database
			was.Key = k
			out = append(out, was)
		}
	}
	return out, nil
}

// walkPartitions calls f on every partition ID in m.
func walkPartitions(m protoreflect.Message, f func(*pb.PartitionId)) {
	if p, ok := m.Interface().(*pb.PartitionId); ok {
		f(p)
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					walkPartitions(mv.Message(), f)
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				lst := v.List()
				for i := 0; i < lst.Len(); i++ {
					walkPartitions(lst.Get(i).Message(), f)
				}
			}
		case fd.Message() != nil:
			walkPartitions(v.Message(), f)
		}
		return true
	})
}

// retarget names, in every partition of m naming the project's database
// from (or naming no project or database), the database to instead.
func retarget(m proto.Message, project, from, to string) {
	walkPartitions(m.ProtoReflect(), func(p *pb.PartitionId) {
		if (p.ProjectId == "" || p.ProjectId == project) && emulatorDB(p.DatabaseId) == from {
			p.ProjectId, p.DatabaseId = project, to
		}
	})
}

// Query is a query request read at an earlier time: Retarget has it read a
// snapshot of its database at that time instead.
type Query interface {
	proto.Message
	GetProjectId() string
	GetDatabaseId() string
	GetReadOptions() *pb.ReadOptions
}

// Retarget is req, a RunQuery or RunAggregationQuery at t, rewritten to
// read a snapshot of its database at t, and the function that rewrites the
// answer back: its entities' database, its read time, and the read-only
// transaction it began.
func Retarget[Q Query](ctx context.Context, l *Log, req Q, t time.Time) (Q, func(proto.Message), error) {
	var zero Q
	if err := l.check(ctx); err != nil {
		return zero, nil, err
	}
	project, database := req.GetProjectId(), emulatorDB(req.GetDatabaseId())
	snap, err := l.snapshot(ctx, project, database, t)
	if err != nil {
		return zero, nil, err
	}
	newTxn := req.GetReadOptions().GetNewTransaction() != nil
	out := proto.Clone(req).(Q)
	retarget(out, project, database, snap)
	m := out.ProtoReflect()
	fields := m.Descriptor().Fields()
	m.Set(fields.ByName("database_id"), protoreflect.ValueOfString(snap))
	m.Clear(fields.ByName("read_options"))
	restore := func(resp proto.Message) {
		if resp == nil {
			return
		}
		retarget(resp, project, snap, database)
		rm := resp.ProtoReflect()
		rf := rm.Descriptor().Fields()
		if b := rf.ByName("batch"); b != nil && rm.Has(b) {
			bm := rm.Mutable(b).Message()
			bm.Set(bm.Descriptor().Fields().ByName("read_time"), protoreflect.ValueOfMessage(timestamppb.New(t).ProtoReflect()))
		}
		if newTxn {
			rm.Set(rf.ByName("transaction"), protoreflect.ValueOfBytes(l.BeginReadOnly(project, database, t)))
		}
	}
	return out, restore, nil
}
