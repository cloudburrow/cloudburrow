package callfront

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/queryfront"
)

// layoutEmulator keeps entities as the Datastore emulator does: by
// project, namespace, database and path, and answers a query of a project
// and namespace with the entities of every database of it (#1885).
type layoutEmulator struct {
	pb.UnimplementedDatastoreServer
	mu       sync.Mutex
	entities map[string]*pb.Entity
	reserved []string
}

func layoutID(k *pb.Key) string {
	p := k.GetPartitionId()
	var parts []string
	for _, e := range k.GetPath() {
		parts = append(parts, e.GetKind()+":"+e.GetName()+":"+itoa(e.GetId()))
	}
	return p.GetProjectId() + "|" + p.GetNamespaceId() + "|" + p.GetDatabaseId() + "|" + strings.Join(parts, "/")
}

func itoa(n int64) string {
	if n == 0 {
		return ""
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// key of the request's project and database, as the emulator reads it.
func (f *layoutEmulator) own(k *pb.Key, project, db string) *pb.Key {
	k = proto.Clone(k).(*pb.Key)
	if k.PartitionId == nil {
		k.PartitionId = &pb.PartitionId{}
	}
	if k.PartitionId.ProjectId == "" {
		k.PartitionId.ProjectId = project
	}
	if k.PartitionId.DatabaseId == "" {
		k.PartitionId.DatabaseId = db
	}
	return k
}

func (f *layoutEmulator) Lookup(_ context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &pb.LookupResponse{}
	for _, k := range req.GetKeys() {
		if e, ok := f.entities[layoutID(f.own(k, req.ProjectId, req.DatabaseId))]; ok {
			out.Found = append(out.Found, &pb.EntityResult{Entity: e})
		} else {
			out.Missing = append(out.Missing, &pb.EntityResult{Entity: &pb.Entity{Key: k}})
		}
	}
	return out, nil
}

func (f *layoutEmulator) Commit(_ context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range req.GetMutations() {
		if e := m.GetUpsert(); e != nil {
			e = proto.Clone(e).(*pb.Entity)
			e.Key = f.own(e.Key, req.ProjectId, req.DatabaseId)
			f.entities[layoutID(e.Key)] = e
		}
		if k := m.GetDelete(); k != nil {
			delete(f.entities, layoutID(f.own(k, req.ProjectId, req.DatabaseId)))
		}
	}
	return &pb.CommitResponse{}, nil
}

func (f *layoutEmulator) ReserveIds(_ context.Context, req *pb.ReserveIdsRequest) (*pb.ReserveIdsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range req.GetKeys() {
		f.reserved = append(f.reserved, layoutID(f.own(k, req.ProjectId, req.DatabaseId)))
	}
	return &pb.ReserveIdsResponse{}, nil
}

func (f *layoutEmulator) RunQuery(_ context.Context, req *pb.RunQueryRequest) (*pb.RunQueryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := req.GetQuery()
	ns := req.GetPartitionId().GetNamespaceId()
	batch := &pb.QueryResultBatch{MoreResults: pb.QueryResultBatch_NO_MORE_RESULTS}
	var ids []string
	for id := range f.entities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(q.GetKind()) == 1 && q.GetKind()[0].GetName() == "__namespace__" {
		seen := map[string]bool{}
		for _, id := range ids {
			k := f.entities[id].GetKey().GetPartitionId()
			if k.GetProjectId() == req.ProjectId && k.GetNamespaceId() != "" && !seen[k.GetNamespaceId()] {
				seen[k.GetNamespaceId()] = true
				batch.EntityResults = append(batch.EntityResults, &pb.EntityResult{Entity: &pb.Entity{Key: &pb.Key{
					PartitionId: &pb.PartitionId{ProjectId: req.ProjectId},
					Path:        []*pb.Key_PathElement{{Kind: "__namespace__", IdType: &pb.Key_PathElement_Name{Name: k.GetNamespaceId()}}}}}})
			}
		}
		return &pb.RunQueryResponse{Batch: batch}, nil
	}
	for _, id := range ids {
		e := f.entities[id]
		k := e.GetKey()
		if k.GetPartitionId().GetProjectId() != req.ProjectId || k.GetPartitionId().GetNamespaceId() != ns {
			continue
		}
		if len(q.GetKind()) == 1 && k.GetPath()[len(k.GetPath())-1].GetKind() != q.GetKind()[0].GetName() {
			continue
		}
		batch.EntityResults = append(batch.EntityResults, &pb.EntityResult{Entity: e})
	}
	return &pb.RunQueryResponse{Batch: batch}, nil
}

func keyOf(project, db, ns, kind, name string, id int64) *pb.Key {
	pe := &pb.Key_PathElement{Kind: kind}
	if name != "" {
		pe.IdType = &pb.Key_PathElement_Name{Name: name}
	} else {
		pe.IdType = &pb.Key_PathElement_Id{Id: id}
	}
	return &pb.Key{PartitionId: &pb.PartitionId{ProjectId: project, DatabaseId: db, NamespaceId: ns}, Path: []*pb.Key_PathElement{pe}}
}

// TestNamedDatabasesKeptTheOldWayAreMovedOnce (#1885): entities a named
// database kept in its project's emulator project, before each named
// database had an emulator project of its own, are moved to it when the
// front starts, their key-valued properties rewritten too; the default
// database's stay; the move is recorded and not made again.
func TestNamedDatabasesKeptTheOldWayAreMovedOnce(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	emu := &layoutEmulator{entities: map[string]*pb.Entity{}}
	srv := grpc.NewServer()
	pb.RegisterDatastoreServer(srv, emu)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	dbs := newDatastoreDatabases(l.Addr().String(), t.Logf)
	t.Cleanup(func() {
		if dbs.conn != nil {
			_ = dbs.conn.Close()
		}
	})
	ctx := context.Background()
	// The old layout: the marker, and entities of named database "orders"
	// of project "p" kept in p, one in a namespace, one referring to
	// another; and one of (default).
	if err := dbs.mark(ctx, "p", "orders"); err != nil {
		t.Fatal(err)
	}
	raw, err := dbs.rawClient()
	if err != nil {
		t.Fatal(err)
	}
	ref := keyOf("p", "orders", "", "Customer", "ada", 0)
	old := []*pb.Entity{
		{Key: ref, Properties: map[string]*pb.Value{"n": {ValueType: &pb.Value_IntegerValue{IntegerValue: 1}}}},
		{Key: keyOf("p", "orders", "", "Order", "", 42), Properties: map[string]*pb.Value{
			"customer": {ValueType: &pb.Value_KeyValue{KeyValue: ref}}}},
		{Key: keyOf("p", "orders", "tenant", "Order", "t1", 0)},
		{Key: keyOf("p", "", "", "Order", "default", 0)},
	}
	for _, e := range old {
		db := e.GetKey().GetPartitionId().GetDatabaseId()
		if _, err := raw.Commit(ctx, &pb.CommitRequest{ProjectId: "p", DatabaseId: db,
			Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: e}}}}); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	go dbs.MigrateDatabases(ctx, done)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the move did not end")
	}

	ep := queryfront.EmulatorProject("p", "orders")
	var got []string
	for id := range emu.entities {
		if !strings.HasPrefix(id, MarkerProject+"|") {
			got = append(got, id)
		}
	}
	sort.Strings(got)
	want := []string{
		ep + "||" + "|Customer:ada:",
		ep + "||" + "|Order::42",
		ep + "|tenant|" + "|Order:t1:",
		"p|||Order:default:",
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entities after the move:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	order := emu.entities[ep+"|||Order::42"]
	if k := order.GetProperties()["customer"].GetKeyValue().GetPartitionId(); k.GetProjectId() != ep || k.GetDatabaseId() != "" {
		t.Errorf("the moved key-valued property names %v, want the emulator project %s", k, ep)
	}
	if len(emu.reserved) != 1 || emu.reserved[0] != ep+"|||Order::42" {
		t.Errorf("reserved IDs = %v, want the moved numeric ID", emu.reserved)
	}
	if _, ok := emu.entities[layoutID(migrationKey())]; !ok {
		t.Error("the move is not recorded")
	}

	// Recorded, the move is not made again: an entity kept the old way
	// now (which no front writes) stays where it is.
	stray := &pb.Entity{Key: keyOf("p", "orders", "", "Order", "stray", 0)}
	if _, err := raw.Commit(ctx, &pb.CommitRequest{ProjectId: "p", DatabaseId: "orders",
		Mutations: []*pb.Mutation{{Operation: &pb.Mutation_Upsert{Upsert: stray}}}}); err != nil {
		t.Fatal(err)
	}
	if n, err := dbs.migrateOnce(ctx); err != nil || n != -1 {
		t.Errorf("a second move = %d, %v; want -1 (recorded), nil", n, err)
	}
	if _, ok := emu.entities["p||orders|Order:stray:"]; !ok {
		t.Error("the recorded move ran again")
	}
}
