package datastoreversions

// Snapshots. A query at an earlier time is answered by the emulator's own
// query engine (filters, orders, cursors, projections, GQL, aggregations
// and the front's split queries alike) over a snapshot: every entity of
// the database at that time, written into a database of the emulator's
// own, which the query is then run in. A snapshot is kept while queries
// at its time go on (the pages of one query, a read-only transaction's
// reads), and deleted when it has not been read for snapshotIdle, or to
// make room; the log records each it makes, so a restarted front deletes
// the ones it left.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
)

// snapshotPrefix begins a snapshot's database ID: an underscore, which no
// database ID of Google's has, keeps it apart from every database a call
// can name.
const snapshotPrefix = "cloudburrow_pitr_"

const (
	snapshotIdle = 2 * time.Minute
	maxSnapshots = 4
	// commitBytes bounds a snapshot's commits under the 10 MiB a commit
	// may carry.
	commitBytes = 4 << 20
)

type snapKey struct {
	db string
	at time.Time
}

type snapshot struct {
	project, id string
	used        time.Time
	done        chan struct{}
	err         error
}

type snapshots struct {
	client pb.DatastoreClient
	mu     sync.Mutex
	m      map[snapKey]*snapshot
	n      int
}

func newSnapshots() *snapshots { return &snapshots{m: map[snapKey]*snapshot{}} }

func (s *snapshots) open() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][2]string
	for _, sn := range s.m {
		out = append(out, [2]string{sn.project, sn.id})
	}
	return out
}

// snapshot is the ID of the database holding the project's database at t,
// made if need be.
func (l *Log) snapshot(ctx context.Context, project, database string, t time.Time) (string, error) {
	s := l.snaps
	k := snapKey{DBName(project, database), t}
	s.mu.Lock()
	sn := s.m[k]
	if sn == nil {
		s.n++
		sn = &snapshot{project: project, id: fmt.Sprintf("%s%d_%d", snapshotPrefix, time.Now().UnixNano(), s.n), done: make(chan struct{})}
		s.m[k] = sn
		sn.used = time.Now()
		s.mu.Unlock()
		l.sweepSnapshots(ctx, false)
		l.noteSnapshot("snap", project, sn.id)
		sn.err = l.materialize(context.WithoutCancel(ctx), project, database, sn.id, t)
		close(sn.done)
		if sn.err != nil {
			s.mu.Lock()
			delete(s.m, k)
			s.mu.Unlock()
			s.drop(context.WithoutCancel(ctx), project, sn.id)
			l.noteSnapshot("snapdone", project, sn.id)
		}
	} else {
		sn.used = time.Now()
		s.mu.Unlock()
	}
	select {
	case <-sn.done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return sn.id, sn.err
}

// sweepSnapshots deletes the snapshots not read for snapshotIdle, and the
// least recently read beyond maxSnapshots; all of them with all.
func (l *Log) sweepSnapshots(ctx context.Context, all bool) {
	s := l.snaps
	s.mu.Lock()
	var gone []*snapshot
	for {
		var oldest snapKey
		var o *snapshot
		for k, sn := range s.m {
			select {
			case <-sn.done:
			default:
				continue // being made
			}
			if o == nil || sn.used.Before(o.used) {
				oldest, o = k, sn
			}
		}
		if o == nil || (!all && len(s.m) < maxSnapshots && time.Since(o.used) < snapshotIdle) {
			break
		}
		delete(s.m, oldest)
		gone = append(gone, o)
	}
	s.mu.Unlock()
	for _, sn := range gone {
		s.drop(context.WithoutCancel(ctx), sn.project, sn.id)
		l.noteSnapshot("snapdone", sn.project, sn.id)
	}
}

// namespaces is every namespace of a database with an entity.
func namespaces(ctx context.Context, c pb.DatastoreClient, project, database string) ([]string, error) {
	keys, err := queryAll(ctx, c, project, database, "", "__namespace__", true)
	if err != nil {
		return nil, err
	}
	out := []string{""}
	for _, e := range keys {
		if p := e.GetKey().GetPath(); len(p) > 0 && p[len(p)-1].GetName() != "" {
			out = append(out, p[len(p)-1].GetName())
		}
	}
	return out, nil
}

// queryAll is every entity of kind (all when "") in a namespace.
func queryAll(ctx context.Context, c pb.DatastoreClient, project, database, ns, kind string, keysOnly bool) ([]*pb.Entity, error) {
	ctx = routed(ctx, project, database)
	var out []*pb.Entity
	var cursor []byte
	for {
		q := &pb.Query{StartCursor: cursor}
		if keysOnly {
			q.Projection = []*pb.Projection{{Property: &pb.PropertyReference{Name: "__key__"}}}
		}
		if kind != "" {
			q.Kind = []*pb.KindExpression{{Name: kind}}
		}
		resp, err := c.RunQuery(ctx, &pb.RunQueryRequest{ProjectId: project, DatabaseId: database,
			PartitionId: &pb.PartitionId{ProjectId: project, DatabaseId: database, NamespaceId: ns},
			QueryType:   &pb.RunQueryRequest_Query{Query: q}})
		if err != nil {
			return nil, err
		}
		b := resp.GetBatch()
		for _, r := range b.GetEntityResults() {
			out = append(out, r.GetEntity())
		}
		if b.GetMoreResults() != pb.QueryResultBatch_NOT_FINISHED || len(b.GetEntityResults()) == 0 {
			return out, nil
		}
		cursor = b.GetEndCursor()
	}
}

// readAll is every entity of a database, the emulator's own left out.
func readAll(ctx context.Context, c pb.DatastoreClient, project, database string) ([]*pb.Entity, error) {
	nss, err := namespaces(ctx, c, project, database)
	if err != nil {
		return nil, fmt.Errorf("list the namespaces: %w", err)
	}
	var out []*pb.Entity
	for _, ns := range nss {
		es, err := queryAll(ctx, c, project, database, ns, "", false)
		if err != nil {
			return nil, fmt.Errorf("read namespace %q: %w", ns, err)
		}
		for _, e := range es {
			if !strings.HasPrefix(kindOf(e.GetKey()), "__") {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// commitAll applies muts in commits of at most 500 mutations and
// commitBytes.
func commitAll(ctx context.Context, c pb.DatastoreClient, project, database string, muts []*pb.Mutation) error {
	ctx = routed(ctx, project, database)
	for len(muts) > 0 {
		n, size := 0, 0
		for n < len(muts) && n < 500 && (n == 0 || size+proto.Size(muts[n]) <= commitBytes) {
			size += proto.Size(muts[n])
			n++
		}
		if _, err := c.Commit(ctx, &pb.CommitRequest{ProjectId: project, DatabaseId: database,
			Mode: pb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts[:n]}); err != nil {
			return err
		}
		muts = muts[n:]
	}
	return nil
}

// materialize writes the database's entities at t into the snapshot.
func (l *Log) materialize(ctx context.Context, project, database, snap string, t time.Time) error {
	c := l.snaps.client
	es, err := l.At(ctx, project, database, t, EntityFilter{}, func() ([]*pb.Entity, error) {
		return readAll(ctx, c, project, database)
	})
	if err != nil {
		return err
	}
	muts := make([]*pb.Mutation, 0, len(es))
	for _, e := range es {
		e = proto.Clone(e).(*pb.Entity)
		retarget(e, project, database, snap)
		muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Upsert{Upsert: e}})
	}
	if err := commitAll(ctx, c, project, snap, muts); err != nil {
		return fmt.Errorf("write the snapshot at %s: %w", t.Format(time.RFC3339Nano), err)
	}
	return nil
}

// drop deletes every entity of a snapshot.
func (s *snapshots) drop(ctx context.Context, project, snap string) {
	if s.client == nil {
		return
	}
	es, err := readAll(ctx, s.client, project, snap)
	if err != nil {
		return
	}
	muts := make([]*pb.Mutation, 0, len(es))
	for _, e := range es {
		muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Delete{Delete: e.GetKey()}})
	}
	_ = commitAll(ctx, s.client, project, snap, muts)
}
