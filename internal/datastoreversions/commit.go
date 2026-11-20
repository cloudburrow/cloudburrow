package datastoreversions

import (
	"context"
	"strings"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// maxLookup is the most keys one Lookup is sent.
const maxLookup = 1000

// recorded reports whether the log keeps the versions of the project's
// database: not the front's own records, nor a snapshot.
func (l *Log) recorded(project, database string) bool {
	return project != "" && project != l.marker.project && !isSnapshot(database)
}

// Client is c with every commit it makes recorded in the log. c must be
// the emulator's client with each database named as the caller names it
// (queryfront.DatabasesClient).
func (l *Log) Client(c pb.DatastoreClient) pb.DatastoreClient {
	if v, ok := c.(versionedClient); ok {
		c = v.DatastoreClient
	}
	return versionedClient{DatastoreClient: c, l: l}
}

type versionedClient struct {
	pb.DatastoreClient
	l *Log
}

// mutationKey is the key a mutation writes.
func mutationKey(m *pb.Mutation) (*pb.Key, *pb.Entity) {
	switch op := m.GetOperation().(type) {
	case *pb.Mutation_Insert:
		return op.Insert.GetKey(), op.Insert
	case *pb.Mutation_Update:
		return op.Update.GetKey(), op.Update
	case *pb.Mutation_Upsert:
		return op.Upsert.GetKey(), op.Upsert
	case *pb.Mutation_Delete:
		return op.Delete, nil
	}
	return nil, nil
}

func (v versionedClient) Commit(ctx context.Context, req *pb.CommitRequest, opts ...grpc.CallOption) (*pb.CommitResponse, error) {
	l := v.l
	if !l.recorded(req.GetProjectId(), req.GetDatabaseId()) || len(req.GetMutations()) == 0 {
		return v.DatastoreClient.Commit(ctx, req, opts...)
	}
	if err := l.waitReady(ctx); err != nil {
		return nil, err
	}
	db := DBName(req.GetProjectId(), emulatorDB(req.GetDatabaseId()))
	seq := l.begin()
	defer l.end(seq)

	// The bases of the keys the log does not hold yet, read before the
	// commit: an insert, or a key the emulator completes, had none.
	type base struct {
		e   *pb.Entity
		ver int64
	}
	bases := map[string]base{}
	asked := map[string]bool{}
	var ask []*pb.Key
	l.mu.Lock()
	for _, m := range req.GetMutations() {
		k, _ := mutationKey(m)
		if k == nil {
			continue
		}
		if !complete(k) || m.GetInsert() != nil {
			// No base: the key had no entity.
			continue
		}
		if _, _, _, held := l.stateAt(db, k, l.now()); !held {
			ask = append(ask, k)
			asked[keyID(k)] = true
		}
	}
	l.mu.Unlock()
	if len(ask) > 0 {
		found, err := v.lookupNow(ctx, req.GetProjectId(), req.GetDatabaseId(), ask)
		if err != nil {
			return nil, err
		}
		for id, r := range found {
			bases[id] = base{r.GetEntity(), r.GetVersion()}
		}
	}

	resp, err := v.DatastoreClient.Commit(ctx, req, opts...)
	if err != nil {
		return resp, err
	}
	at := l.now()
	if ct := resp.GetCommitTime(); ct != nil && ct.CheckValid() == nil && !ct.AsTime().After(at) {
		at = ct.AsTime()
	}
	results := resp.GetMutationResults()
	var recs []record
	var reread []*pb.Key
	for i, m := range req.GetMutations() {
		k, e := mutationKey(m)
		if k == nil {
			continue
		}
		var res *pb.MutationResult
		if i < len(results) {
			res = results[i]
		}
		if res.GetConflictDetected() {
			continue
		}
		fresh := !complete(k) || m.GetInsert() != nil
		if res.GetKey() != nil && !complete(k) {
			k = res.GetKey()
		}
		k = proto.Clone(k).(*pb.Key)
		// A key the log did not hold has its base: the entity read
		// before, or none.
		r := record{db: db, key: k, at: at, ver: res.GetVersion(), hasBase: asked[keyID(k)] || fresh}
		if b, ok := bases[keyID(k)]; ok {
			r.base, r.baseVer = b.e, b.ver
		}
		if e != nil {
			e = proto.Clone(e).(*pb.Entity)
			e.Key = k
			r.entity = e
			// A masked or transformed write's entity is the emulator's.
			if m.GetPropertyMask() != nil || len(m.GetPropertyTransforms()) > 0 {
				reread = append(reread, k)
			}
		}
		recs = append(recs, r)
	}
	if len(reread) > 0 {
		if found, err := v.lookupNow(ctx, req.GetProjectId(), req.GetDatabaseId(), reread); err == nil {
			for i := range recs {
				if r, ok := found[keyID(recs[i].key)]; ok && recs[i].entity != nil {
					recs[i].entity = r.GetEntity()
				}
			}
		}
	}
	l.mu.Lock()
	for _, r := range recs {
		l.apply(r)
		if l.store != nil {
			l.store.appendRecord(versionRecord(r))
		}
	}
	if l.bytes > l.maxBytes {
		l.shrinkLocked()
		if l.store != nil {
			l.store.maybeCompact(l)
		}
	}
	l.mu.Unlock()
	return resp, nil
}

// lookupNow is the emulator's entities of keys now, found ones by keyID.
func (v versionedClient) lookupNow(ctx context.Context, project, database string, keys []*pb.Key) (map[string]*pb.EntityResult, error) {
	return lookupNow(ctx, v.DatastoreClient, project, database, keys)
}

func lookupNow(ctx context.Context, c pb.DatastoreClient, project, database string, keys []*pb.Key) (map[string]*pb.EntityResult, error) {
	out := map[string]*pb.EntityResult{}
	for len(keys) > 0 {
		n := min(len(keys), maxLookup)
		batch := keys[:n]
		keys = keys[n:]
		for len(batch) > 0 {
			resp, err := c.Lookup(ctx, &pb.LookupRequest{ProjectId: project, DatabaseId: database, Keys: batch})
			if err != nil {
				return nil, err
			}
			for _, f := range resp.GetFound() {
				out[keyID(f.GetEntity().GetKey())] = f
			}
			batch = resp.GetDeferred()
		}
	}
	return out, nil
}

// isSnapshot reports whether a database is one of the log's snapshots.
func isSnapshot(database string) bool { return strings.HasPrefix(database, snapshotPrefix) }
