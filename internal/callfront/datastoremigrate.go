package callfront

import (
	"context"
	"fmt"
	"sort"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/queryfront"
)

// The upgrade of named databases' entities to their own emulator projects
// (#1885). Before it, an entity of named database d of project p was kept
// in the emulator's project p, its key naming d; since, the front keeps it
// in emulator project queryfront.EmulatorProject(p, d), and a call to d no
// longer sees the entities kept the old way. So the front moves them once,
// when it starts: for each project a marker names (the databases calls
// named, datastoredbs.go, recorded since v0.2.0), every entity of the
// project's emulator project whose key names a named database (the
// emulator's queries of p answer every database of it, which is what
// #1885 fixed) is written to its database's emulator project, its keys,
// those of its key-valued properties too, rewritten as the front rewrites
// a call's, and then deleted from p. The move is recorded, when it is
// done, as an entity of MarkerProject, with the data it is about: on the
// persistent volume when Datastore is persistent, gone in a full reset. A
// move cut short is begun again: its writes are upserts, its deletes of
// what is gone succeed, so it is idempotent. Until it is done the front
// holds calls naming a named database (queryfront.Datastore.HoldUntil),
// which would otherwise miss the entities not yet moved.

// migrationKind and migrationName are the record of the move.
const (
	migrationKind = "Migration"
	migrationName = "1885-named-databases-in-own-projects"
)

// migrationBatch is the most mutations one commit of the move carries
// (Google's limit is 500).
const migrationBatch = 250

func migrationKey() *pb.Key {
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: MarkerProject},
		Path: []*pb.Key_PathElement{{Kind: migrationKind, IdType: &pb.Key_PathElement_Name{Name: migrationName}}}}
}

// rawClient is the emulator's client with no rewriting: the move reads and
// writes the emulator's own projects.
func (d *datastoreDatabases) rawClient() (pb.DatastoreClient, error) {
	if _, err := d.client(); err != nil {
		return nil, err
	}
	return pb.NewDatastoreClient(d.conn), nil
}

// MigrateDatabases moves the entities named databases kept the old way
// (above), retrying until the emulator answers or ctx ends, and closes
// done when it has (or when ctx ends).
func (d *datastoreDatabases) MigrateDatabases(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	wait := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		moved, err := d.migrateOnce(ctx)
		if err == nil {
			if moved >= 0 {
				d.logf("call front: moved %d entities of named Datastore databases to their own emulator projects (#1885)", moved)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		// The emulator starts with the front; until it answers the
		// move waits quietly.
		if attempt > 20 || status.Code(err) != codes.Unavailable {
			d.logf("call front: move named Datastore databases' entities (#1885), attempt %d: %v", attempt, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 10*time.Second {
			wait *= 2
		}
	}
}

// migrateOnce makes the move, if it is not recorded, and returns the
// number of entities moved, or -1 if it was recorded already.
func (d *datastoreDatabases) migrateOnce(ctx context.Context) (int, error) {
	c, err := d.rawClient()
	if err != nil {
		return 0, err
	}
	mctx := routed(ctx, MarkerProject, "")
	got, err := c.Lookup(mctx, &pb.LookupRequest{ProjectId: MarkerProject, Keys: []*pb.Key{migrationKey()}})
	if err != nil {
		return 0, fmt.Errorf("read the record of the move: %w", err)
	}
	if len(got.GetFound()) > 0 {
		return -1, nil
	}
	projects, err := markedProjects(ctx, c)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, p := range projects {
		n, err := migrateProject(ctx, c, p)
		moved += n
		if err != nil {
			return moved, fmt.Errorf("project %s: %w", p, err)
		}
		if n > 0 {
			d.logf("call front: moved %d entities of project %s's named Datastore databases to their own emulator projects (#1885)", n, p)
		}
	}
	rec := &pb.Entity{Key: migrationKey(), Properties: map[string]*pb.Value{
		"moved": {ValueType: &pb.Value_IntegerValue{IntegerValue: int64(moved)}},
		"at":    {ValueType: &pb.Value_StringValue{StringValue: time.Now().UTC().Format(time.RFC3339)}},
	}}
	if _, err := c.Commit(mctx, &pb.CommitRequest{ProjectId: MarkerProject, Mode: pb.CommitRequest_NON_TRANSACTIONAL,
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: rec}}}}); err != nil {
		return moved, fmt.Errorf("record the move: %w", err)
	}
	return moved, nil
}

// markedProjects are the projects the markers name, sorted.
func markedProjects(ctx context.Context, c pb.DatastoreClient) ([]string, error) {
	set := map[string]bool{}
	var cursor []byte
	for {
		resp, err := c.RunQuery(routed(ctx, MarkerProject, ""), &pb.RunQueryRequest{ProjectId: MarkerProject,
			QueryType: &pb.RunQueryRequest_Query{Query: &pb.Query{Kind: []*pb.KindExpression{{Name: markerKind}}, StartCursor: cursor}}})
		if err != nil {
			return nil, fmt.Errorf("read the markers: %w", err)
		}
		b := resp.GetBatch()
		for _, r := range b.GetEntityResults() {
			if p := r.GetEntity().GetProperties()["project"].GetStringValue(); p != "" {
				set[p] = true
			}
		}
		if b.GetMoreResults() != pb.QueryResultBatch_NOT_FINISHED || len(b.GetEntityResults()) == 0 {
			break
		}
		cursor = b.GetEndCursor()
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// queryAllRaw is every result of q in the emulator's project p, namespace
// ns, its default database.
func queryAllRaw(ctx context.Context, c pb.DatastoreClient, p, ns string, q *pb.Query) ([]*pb.Entity, error) {
	var out []*pb.Entity
	for {
		resp, err := c.RunQuery(routed(ctx, p, ""), &pb.RunQueryRequest{ProjectId: p,
			PartitionId: &pb.PartitionId{ProjectId: p, NamespaceId: ns},
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
		q = proto.Clone(q).(*pb.Query)
		q.StartCursor = b.GetEndCursor()
	}
}

// migrateProject moves project p's entities of named databases, and
// returns how many it moved.
func migrateProject(ctx context.Context, c pb.DatastoreClient, p string) (int, error) {
	nss, err := queryAllRaw(ctx, c, p, "", &pb.Query{Kind: []*pb.KindExpression{{Name: "__namespace__"}}})
	if err != nil {
		return 0, fmt.Errorf("list the namespaces: %w", err)
	}
	namespaces := []string{""}
	for _, e := range nss {
		if path := e.GetKey().GetPath(); len(path) > 0 && path[0].GetName() != "" {
			namespaces = append(namespaces, path[0].GetName())
		}
	}
	moved := 0
	for _, ns := range namespaces {
		// A kindless query: every kind of the namespace, of every
		// database the emulator keeps in p.
		all, err := queryAllRaw(ctx, c, p, ns, &pb.Query{})
		if err != nil {
			return moved, fmt.Errorf("read namespace %q: %w", ns, err)
		}
		byDB := map[string][]*pb.Entity{}
		for _, e := range all {
			if db := e.GetKey().GetPartitionId().GetDatabaseId(); queryfront.NamedDatabase(db) {
				byDB[db] = append(byDB[db], e)
			}
		}
		dbs := make([]string, 0, len(byDB))
		for db := range byDB {
			dbs = append(dbs, db)
		}
		sort.Strings(dbs)
		for _, db := range dbs {
			if err := moveEntities(ctx, c, p, db, byDB[db]); err != nil {
				return moved, fmt.Errorf("database %s, namespace %q: %w", db, ns, err)
			}
			moved += len(byDB[db])
		}
	}
	return moved, nil
}

// moveEntities writes es, entities of named database db of project p kept
// in p, to db's emulator project, then deletes them from p.
func moveEntities(ctx context.Context, c pb.DatastoreClient, p, db string, es []*pb.Entity) error {
	ep := queryfront.EmulatorProject(p, db)
	var keys []*pb.Key
	for start := 0; start < len(es); start += migrationBatch {
		end := min(start+migrationBatch, len(es))
		var muts []*pb.Mutation
		var reserve []*pb.Key
		for _, e := range es[start:end] {
			keys = append(keys, e.GetKey())
			out := queryfront.EntityToEmulator(e, p)
			muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Upsert{Upsert: out}})
			if path := out.GetKey().GetPath(); len(path) > 0 && path[len(path)-1].GetId() != 0 {
				reserve = append(reserve, out.GetKey())
			}
		}
		if _, err := c.Commit(routed(ctx, ep, ""), &pb.CommitRequest{ProjectId: ep,
			Mode: pb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts}); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		// The IDs the entities were given stay theirs: the emulator
		// allocates none of them again in the new project.
		if len(reserve) > 0 {
			if _, err := c.ReserveIds(routed(ctx, ep, ""), &pb.ReserveIdsRequest{ProjectId: ep, Keys: reserve}); err != nil &&
				status.Code(err) != codes.Unimplemented {
				return fmt.Errorf("reserve their IDs: %w", err)
			}
		}
	}
	for start := 0; start < len(keys); start += migrationBatch {
		end := min(start+migrationBatch, len(keys))
		var muts []*pb.Mutation
		for _, k := range keys[start:end] {
			muts = append(muts, &pb.Mutation{Operation: &pb.Mutation_Delete{Delete: k}})
		}
		if _, err := c.Commit(routed(ctx, p, db), &pb.CommitRequest{ProjectId: p, DatabaseId: db,
			Mode: pb.CommitRequest_NON_TRANSACTIONAL, Mutations: muts}); err != nil {
			return fmt.Errorf("delete the old copies: %w", err)
		}
	}
	return nil
}
