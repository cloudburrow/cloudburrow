package spannerfront

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// purgeEmulator keeps instances and databases of two projects.
type purgeEmulator struct {
	instancepb.UnimplementedInstanceAdminServer
	mu    sync.Mutex
	insts map[string]bool
	dbs   map[string]bool
}

func (f *purgeEmulator) ListInstances(_ context.Context, r *instancepb.ListInstancesRequest) (*instancepb.ListInstancesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &instancepb.ListInstancesResponse{}
	for n := range f.insts {
		if strings.HasPrefix(n, r.Parent+"/") {
			resp.Instances = append(resp.Instances, &instancepb.Instance{Name: n})
		}
	}
	return resp, nil
}

func (f *purgeEmulator) DeleteInstance(_ context.Context, r *instancepb.DeleteInstanceRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.insts, r.Name)
	return &emptypb.Empty{}, nil
}

// purgeDatabases is purgeEmulator's database admin.
type purgeDatabases struct {
	databasepb.UnimplementedDatabaseAdminServer
	*purgeEmulator
}

func (f purgeDatabases) ListDatabases(_ context.Context, r *databasepb.ListDatabasesRequest) (*databasepb.ListDatabasesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &databasepb.ListDatabasesResponse{}
	for n := range f.dbs {
		if strings.HasPrefix(n, r.Parent+"/") {
			resp.Databases = append(resp.Databases, &databasepb.Database{Name: n})
		}
	}
	return resp, nil
}

func (f purgeDatabases) DropDatabase(_ context.Context, r *databasepb.DropDatabaseRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dbs, r.Database)
	return &emptypb.Empty{}, nil
}

// TestResetProjectDropsOnlyThatProject (#1540): a project's reset deletes
// its instances and databases in the emulator and its records in the
// front, and leaves another project's alone.
func TestResetProjectDropsOnlyThatProject(t *testing.T) {
	emu := &purgeEmulator{
		insts: map[string]bool{"projects/gone/instances/i": true, "projects/kept/instances/i": true},
		dbs:   map[string]bool{"projects/gone/instances/i/databases/d": true, "projects/kept/instances/i/databases/d": true},
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(gs, emu)
	databasepb.RegisterDatabaseAdminServer(gs, purgeDatabases{purgeEmulator: emu})
	go func() { _ = gs.Serve(l) }()
	t.Cleanup(gs.Stop)
	s, err := New(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	for _, p := range []string{"gone", "kept"} {
		db := "projects/" + p + "/instances/i/databases/d"
		s.protected[db] = true
		s.policies[db] = &iampolicy.Stored{}
		s.roleStmts[db] = []string{"CREATE ROLE r"}
		s.configs["projects/"+p+"/instanceConfigs/custom-c"] = &instancepb.InstanceConfig{}
		s.backups["projects/"+p+"/instances/i/backups/b"] = &backup{pb: &databasepb.Backup{}}
		s.opOrder = append(s.opOrder, "projects/"+p+"/instances/i/operations/o")
	}
	s.mu.Unlock()

	if err := s.ResetProject(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if emu.insts["projects/gone/instances/i"] || emu.dbs["projects/gone/instances/i/databases/d"] {
		t.Errorf("the purged project's instance or database is left: %v %v", emu.insts, emu.dbs)
	}
	if !emu.insts["projects/kept/instances/i"] || !emu.dbs["projects/kept/instances/i/databases/d"] {
		t.Errorf("another project's instance or database went: %v %v", emu.insts, emu.dbs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range []int{len(s.protected), len(s.policies), len(s.roleStmts), len(s.configs), len(s.backups), len(s.opOrder)} {
		if n != 1 {
			t.Fatalf("records left: protected %v, policies %v, roles %v, configs %v, backups %v, ops %v",
				s.protected, s.policies, s.roleStmts, s.configs, s.backups, s.opOrder)
		}
	}
	if !s.protected["projects/kept/instances/i/databases/d"] || s.configs["projects/kept/instanceConfigs/custom-c"] == nil {
		t.Errorf("another project's records went")
	}
}
