// Package datastoreversions keeps the versions of a Datastore emulator's
// entities, so the Datastore call front can serve reads at an earlier
// read_time, and exports and clones at a snapshot time (#1967).
//
// Firestore in Datastore mode serves Lookup, RunQuery and
// RunAggregationQuery at a past read_options.read_time, and a read-only
// transaction at a read_time, within the database's version window: an
// hour, or seven days at whole minutes with point-in-time recovery
// (https://docs.cloud.google.com/firestore/native/docs/pitr). The Datastore
// emulator keeps no earlier versions (measured, #1942), so the front keeps
// them itself.
//
// The log. Every Commit the front passes to the emulator is recorded,
// after the emulator has applied it, as one version per mutated key: the
// commit's time, the emulator's version of the entity, and the entity as
// the mutation left it (nil for a delete). The first time a key is
// recorded its state before the commit (its base) is recorded too, read
// from the emulator before the commit (an insert, or an incomplete key,
// had none). So a key's state at time T is its last version at or before
// T, or its base; and a key the log does not hold has not changed since
// the log began, and is the emulator's entity now. Pruning folds the
// versions older than the database's retention into the base, and drops a
// key whose versions are all folded: its base is then the emulator's
// entity.
//
// A read at T reads the emulator's entities now, then waits for every
// commit that had begun by then to be recorded, and puts the log's state
// at T over what it read: a commit the read saw is recorded by then, and
// one it did not see changed nothing it read. Commits never wait for
// reads, so a transaction holding the emulator's locks is never held up
// behind one.
//
// The log is kept in the front's state directory (persist.go), bounded by
// the retention and by a size, beyond which the oldest versions are folded
// early and the database's earliest version time moves up. It lives
// exactly as long as the emulator's entities: a generation marker, an
// entity in the emulator's own marker project, names the log the entities
// go with, and a log whose marker the emulator does not hold (its entities
// were lost or reset) is discarded, the versions starting again from then.
package datastoreversions

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// DefaultMaxBytes bounds the log's entities, in memory and on disk.
const DefaultMaxBytes = 256 << 20

// waitCommits bounds how long a read waits for the commits that had begun
// before it to be recorded.
const waitCommits = 30 * time.Second

// version is one committed state of a key.
type version struct {
	at  time.Time
	ver int64
	// entity is the key's entity after the commit; nil for a delete.
	entity *pb.Entity
}

// history is a key's versions, oldest first, and its state before them.
type history struct {
	base     *pb.Entity
	baseVer  int64
	versions []version
}

// at is the key's entity at t, its version, and whether it existed.
func (h *history) at(t time.Time) (*pb.Entity, int64, time.Time) {
	for i := len(h.versions) - 1; i >= 0; i-- {
		if !h.versions[i].at.After(t) {
			return h.versions[i].entity, h.versions[i].ver, h.versions[i].at
		}
	}
	return h.base, h.baseVer, time.Time{}
}

func (h *history) size() int {
	n := proto.Size(h.base) + 64
	for _, v := range h.versions {
		n += proto.Size(v.entity) + 48
	}
	return n
}

// dbLog is one database's histories, by key.
type dbLog struct {
	keys map[string]*history
	// pk is each key's *pb.Key, by the same string.
	pk map[string]*pb.Key
}

// Log is the version log of a Datastore emulator's entities.
type Log struct {
	now  func() time.Time
	logf func(string, ...any)

	// retention is a database's version retention, which pruning keeps.
	retention func(db string) time.Duration
	maxBytes  int

	ready     chan struct{}
	readyOnce sync.Once

	mu    sync.Mutex
	dbs   map[string]*dbLog
	bytes int
	// since is when the log began; sinceDB and sinceProject when it
	// began again for a database or a project (a database deleted, a
	// project purged, the oldest versions folded early).
	since        time.Time
	sinceDB      map[string]time.Time
	sinceProject map[string]time.Time
	// gen is the generation marker the log goes with.
	gen string
	// store persists the log; nil keeps it in memory alone.
	store *store

	// The commits begun and not yet recorded, by sequence number.
	cmu     sync.Mutex
	cond    *sync.Cond
	nextSeq uint64
	active  map[uint64]bool

	txns   *txns
	snaps  *snapshots
	marker markerCheck
}

// New is a log in memory, keeping every database's versions for the
// retention it says; it is ready once Open has run.
func New(retention func(db string) time.Duration, logf func(string, ...any)) *Log {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	l := &Log{now: time.Now, logf: logf, retention: retention, maxBytes: DefaultMaxBytes,
		ready: make(chan struct{}), dbs: map[string]*dbLog{},
		sinceDB: map[string]time.Time{}, sinceProject: map[string]time.Time{},
		active: map[uint64]bool{}}
	l.cond = sync.NewCond(&l.cmu)
	l.txns = newTxns()
	l.snaps = newSnapshots()
	return l
}

// SetMaxBytes bounds the log's entities (tests).
func (l *Log) SetMaxBytes(n int) { l.maxBytes = n }

// SetClock sets the log's clock (tests).
func (l *Log) SetClock(now func() time.Time) { l.now = now }

// markReady lets the log's calls through.
func (l *Log) markReady() { l.readyOnce.Do(func() { close(l.ready) }) }

// waitReady waits until the log is open.
func (l *Log) waitReady(ctx context.Context) error {
	select {
	case <-l.ready:
		return nil
	default:
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	select {
	case <-l.ready:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-t.C:
		return status.Error(codes.Unavailable, "the Datastore front's version log is not open yet: the emulator has not answered; retry")
	}
}

// DBName is a database's name in the log, "projects/p/databases/d", with
// "(default)" for the default database.
func DBName(project, database string) string {
	if database == "" {
		database = "(default)"
	}
	return "projects/" + project + "/databases/" + database
}

// emulatorDB is the database ID a call names a database by: "" for the
// default one.
func emulatorDB(database string) string {
	if database == "(default)" {
		return ""
	}
	return database
}

// keyID is a key's identity within its database: its namespace and path.
func keyID(k *pb.Key) string {
	c := &pb.Key{PartitionId: &pb.PartitionId{NamespaceId: k.GetPartitionId().GetNamespaceId()}, Path: k.GetPath()}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return string(b)
}

// complete reports whether every element of k's path has an ID or a name.
func complete(k *pb.Key) bool {
	p := k.GetPath()
	if len(p) == 0 {
		return false
	}
	for _, e := range p {
		if e.GetId() == 0 && e.GetName() == "" {
			return false
		}
	}
	return true
}

// Since is the earliest time the log holds the versions of the database
// from.
func (l *Log) Since(project, database string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sinceLocked(project, database)
}

func (l *Log) sinceLocked(project, database string) time.Time {
	s := l.since
	if t := l.sinceDB[DBName(project, database)]; t.After(s) {
		s = t
	}
	if t := l.sinceProject[project]; t.After(s) {
		s = t
	}
	return s
}

// record is one mutated key of a commit, as recorded.
type record struct {
	db      string
	key     *pb.Key
	at      time.Time
	ver     int64
	entity  *pb.Entity
	hasBase bool
	base    *pb.Entity
	baseVer int64
}

// apply puts r in the log; l.mu is held. It reports whether r was new.
func (l *Log) apply(r record) {
	d := l.dbs[r.db]
	if d == nil {
		d = &dbLog{keys: map[string]*history{}, pk: map[string]*pb.Key{}}
		l.dbs[r.db] = d
	}
	id := keyID(r.key)
	h := d.keys[id]
	if h == nil {
		h = &history{}
		if r.hasBase {
			h.base, h.baseVer = r.base, r.baseVer
		}
		d.keys[id] = h
		d.pk[id] = r.key
	} else {
		l.bytes -= h.size()
		// A base read before an earlier version than the one kept: two
		// commits of a key raced, and the earlier one's base is the
		// key's.
		if r.hasBase && len(h.versions) > 0 && r.baseVer < h.versions[0].ver && r.baseVer < h.baseVer {
			h.base, h.baseVer = r.base, r.baseVer
		}
	}
	v := version{at: r.at, ver: r.ver, entity: r.entity}
	// Versions are ordered by the emulator's version; a commit recorded
	// late is put in its place, and its time is no later than the
	// versions after it.
	i := sort.Search(len(h.versions), func(i int) bool { return h.versions[i].ver > v.ver && v.ver != 0 })
	if v.ver == 0 {
		i = len(h.versions)
	}
	if i > 0 && h.versions[i-1].at.After(v.at) {
		v.at = h.versions[i-1].at
	}
	if i < len(h.versions) && h.versions[i].at.Before(v.at) {
		v.at = h.versions[i].at
	}
	h.versions = append(h.versions, version{})
	copy(h.versions[i+1:], h.versions[i:])
	h.versions[i] = v
	l.bytes += h.size()
}

// stateAt is the database's entity of key k at t from the log, and
// whether the log holds k; l.mu is held.
func (l *Log) stateAt(db string, k *pb.Key, t time.Time) (*pb.Entity, int64, time.Time, bool) {
	d := l.dbs[db]
	if d == nil {
		return nil, 0, time.Time{}, false
	}
	h := d.keys[keyID(k)]
	if h == nil {
		return nil, 0, time.Time{}, false
	}
	e, v, at := h.at(t)
	return e, v, at, true
}

// begin registers a commit about to reach the emulator.
func (l *Log) begin() uint64 {
	l.cmu.Lock()
	defer l.cmu.Unlock()
	l.nextSeq++
	l.active[l.nextSeq] = true
	return l.nextSeq
}

// end registers a commit as recorded (or failed).
func (l *Log) end(seq uint64) {
	l.cmu.Lock()
	delete(l.active, seq)
	l.cmu.Unlock()
	l.cond.Broadcast()
}

// begun is the sequence number of the last commit begun.
func (l *Log) begun() uint64 {
	l.cmu.Lock()
	defer l.cmu.Unlock()
	return l.nextSeq
}

// settle waits until every commit up to seq has been recorded.
func (l *Log) settle(ctx context.Context, seq uint64) error {
	deadline := time.Now().Add(waitCommits)
	stop := context.AfterFunc(ctx, l.cond.Broadcast)
	defer stop()
	timer := time.AfterFunc(waitCommits, l.cond.Broadcast)
	defer timer.Stop()
	l.cmu.Lock()
	defer l.cmu.Unlock()
	for {
		pending := false
		for s := range l.active {
			if s <= seq {
				pending = true
				break
			}
		}
		if !pending {
			return nil
		}
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if time.Now().After(deadline) {
			return status.Error(codes.Unavailable, "a commit the read waits for has not finished; retry")
		}
		l.cond.Wait()
	}
}

// Forget drops every version of the project's databases (a purge): the
// versions begin again now.
func (l *Log) Forget(project string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := "projects/" + project + "/databases/"
	for db, d := range l.dbs {
		if len(db) > len(prefix) && db[:len(prefix)] == prefix {
			l.dropLocked(db, d)
		}
	}
	l.sinceProject[project] = l.now()
	l.persistSince()
}

// Drop drops every version of a database (deleted, or cleared): the
// versions begin again now.
func (l *Log) Drop(project, database string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	db := DBName(project, emulatorDB(database))
	if d := l.dbs[db]; d != nil {
		l.dropLocked(db, d)
	}
	l.sinceDB[db] = l.now()
	l.persistSince()
}

func (l *Log) dropLocked(db string, d *dbLog) {
	for _, h := range d.keys {
		l.bytes -= h.size()
	}
	delete(l.dbs, db)
	if l.store != nil {
		l.store.appendRecord(dropRecord(db))
	}
}

// Prune folds the versions older than each database's retention, and the
// oldest beyond the size bound.
func (l *Log) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for db, d := range l.dbs {
		l.foldLocked(d, now.Add(-l.retention(db)))
		if len(d.keys) == 0 {
			delete(l.dbs, db)
		}
	}
	if l.bytes > l.maxBytes {
		l.shrinkLocked()
	}
	if l.store != nil {
		l.store.maybeCompact(l)
	}
}

// foldLocked folds d's versions at or before cutoff into their bases.
func (l *Log) foldLocked(d *dbLog, cutoff time.Time) {
	for id, h := range d.keys {
		n := 0
		for n < len(h.versions) && !h.versions[n].at.After(cutoff) {
			n++
		}
		if n == 0 {
			continue
		}
		l.bytes -= h.size()
		last := h.versions[n-1]
		h.base, h.baseVer = last.entity, last.ver
		h.versions = append([]version(nil), h.versions[n:]...)
		if len(h.versions) == 0 {
			delete(d.keys, id)
			delete(d.pk, id)
			continue
		}
		l.bytes += h.size()
	}
}

// shrinkLocked folds the oldest versions of every database until the log
// is under three quarters of its bound, moving the earliest version time
// of the databases it folds up to the cutoff.
func (l *Log) shrinkLocked() {
	var times []time.Time
	for _, d := range l.dbs {
		for _, h := range d.keys {
			for _, v := range h.versions {
				times = append(times, v.at)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	l.logf("datastore versions: the log reached %d bytes, beyond its bound of %d; the oldest versions are folded", l.bytes, l.maxBytes)
	target := l.maxBytes * 3 / 4
	// Fold by the oldest tenth each time, until small enough.
	for i := len(times) / 10; l.bytes > target && len(times) > 0; i += max(1, len(times)/10) {
		cutoff := times[min(i, len(times)-1)]
		for db, d := range l.dbs {
			before := l.bytes
			l.foldLocked(d, cutoff)
			if l.bytes != before {
				if s := l.sinceDB[db]; cutoff.After(s) {
					l.sinceDB[db] = cutoff.Add(time.Microsecond)
				}
			}
			if len(d.keys) == 0 {
				delete(l.dbs, db)
			}
		}
		if i >= len(times)-1 {
			break
		}
	}
	l.persistSince()
}

// Bytes is the size of the log's entities.
func (l *Log) Bytes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes
}

// persistSince writes the log's since times; l.mu is held.
func (l *Log) persistSince() {
	if l.store != nil {
		l.store.appendRecord(sinceRecord(l))
	}
}

// reset discards every version, the log beginning again now under the
// generation gen; l.mu is held.
func (l *Log) resetLocked(gen string) {
	l.dbs = map[string]*dbLog{}
	l.bytes = 0
	l.since = l.now()
	l.sinceDB = map[string]time.Time{}
	l.sinceProject = map[string]time.Time{}
	l.gen = gen
	if l.store != nil {
		if err := l.store.rewrite(l); err != nil {
			l.logf("datastore versions: rewrite the log: %v", err)
		}
	}
}

func (l *Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("datastore versions: %d databases, %d bytes, since %s", len(l.dbs), l.bytes, l.since.Format(time.RFC3339Nano))
}
