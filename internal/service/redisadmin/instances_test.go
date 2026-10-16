package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// fakeProv records what it was asked to run; fail fails every Provision.
type fakeProv struct {
	mu      sync.Mutex
	running map[string]Spec
	fail    error
}

func (f *fakeProv) Provision(_ context.Context, spec Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.running[spec.Server] = spec
	return "127.0.0.1:40001", nil
}

func (f *fakeProv) Remove(_ context.Context, server string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, server)
	return nil
}

// fakeData records the CONFIG SETs; refuse fails them.
type fakeData struct {
	set    *map[string]string
	refuse error
	// io, when set, is the server's data for Save and Load.
	io *fakeIO
}

// fakeIO is a server's data and file system, for import and export.
type fakeIO struct {
	mu     sync.Mutex
	data   string            // the dataset, as the RDB file Save writes
	files  map[string][]byte // the container's files
	saves  int
	loaded string // the host:port Load replicated
	argv   [][]string
	badRDB bool // check-rdb refuses the file
}

func (d fakeData) Save(context.Context) (string, error) {
	d.io.mu.Lock()
	defer d.io.mu.Unlock()
	d.io.saves++
	d.io.files["/data/dump.rdb"] = []byte(d.io.data)
	return "/data/dump.rdb", nil
}

func (d fakeData) Load(_ context.Context, host string, port int) error {
	d.io.mu.Lock()
	defer d.io.mu.Unlock()
	d.io.loaded = fmt.Sprintf("%s:%d", host, port)
	d.io.data = string(d.io.files["/tmp/cloudburrow-import.rdb"])
	return nil
}

// Exec plays the programs import and export run in the container.
func (f *fakeIO) Exec(_ context.Context, _ string, stdin []byte, argv ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, argv)
	switch {
	case argv[0] == "cat":
		return f.files[argv[1]], nil
	case argv[0] == "sh":
		f.files[strings.TrimPrefix(argv[2], "cat > ")] = stdin
	case strings.HasSuffix(argv[0], "-check-rdb") && f.badRDB:
		return []byte("--- RDB ERROR DETECTED ---\n[offset 0] Wrong signature trying to load DB from file"), errors.New("exit status 1")
	case strings.HasSuffix(argv[0], "-cli") && argv[len(argv)-1] == "ping":
		return []byte("PONG\n"), nil
	}
	return nil, nil
}

// fakeObjects is a Cloud Storage.
type fakeObjects map[string][]byte

func (o fakeObjects) Read(_ context.Context, bucket, object string) ([]byte, error) {
	b, ok := o[bucket+"/"+object]
	if !ok {
		return nil, apierror.NotFound("gs://%s/%s not found", bucket, object)
	}
	return b, nil
}

func (o fakeObjects) Write(_ context.Context, bucket, object string, data []byte) error {
	o[bucket+"/"+object] = data
	return nil
}

// shellProv is a fakeProv whose servers run programs on io.
type shellProv struct {
	*fakeProv
	io *fakeIO
}

func (p shellProv) Exec(ctx context.Context, server string, stdin []byte, argv ...string) ([]byte, error) {
	return p.io.Exec(ctx, server, stdin, argv...)
}

func (d fakeData) Info(context.Context) (ServerInfo, error) { return ServerInfo{Version: "7.2.4"}, nil }
func (d fakeData) ConfigSet(_ context.Context, s map[string]string) error {
	if d.refuse != nil {
		return d.refuse
	}
	*d.set = s
	return nil
}

func newCreating(t *testing.T, db store.Store) (*Server, *fakeProv, *map[string]string) {
	t.Helper()
	s := newTestServer(nil)
	p := &fakeProv{running: map[string]Spec{}}
	set := &map[string]string{}
	s.EnableCreate(context.Background(), p, func(addr, pw string) DataPlane { return fakeData{set: set} }, db, nil)
	return s, p, set
}

// wait polls the operation until it is done.
func wait(t *testing.T, s *Server, op *longrunningpb.Operation) *longrunningpb.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !op.GetDone() {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s not done", op.GetName())
		}
		time.Sleep(5 * time.Millisecond)
		var err error
		if op, err = NewOperationsServer(s).GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil {
			t.Fatal(err)
		}
	}
	return op
}

const parent = "projects/my-proj/locations/us-central1"

func TestCreateUpdateDelete(t *testing.T) {
	s, p, set := newCreating(t, store.NewMemory())
	ctx := context.Background()
	op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cache",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 2, AuthEnabled: true, DisplayName: "Cache",
			RedisConfigs: map[string]string{"maxmemory-policy": "allkeys-lru"}}})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetDone() || op.GetMetadata() == nil {
		t.Errorf("create operation = %v; want pending, with metadata", op)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatalf("create failed: %v", op.GetError())
	}
	name := parent + "/instances/cache"
	inst, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if inst.GetState() != redispb.Instance_READY || inst.GetHost() != "127.0.0.1" || inst.GetPort() != 40001 ||
		inst.GetMemorySizeGb() != 2 || !inst.GetAuthEnabled() || inst.GetRedisVersion() != "REDIS_7_2" {
		t.Errorf("GetInstance = %v", inst)
	}
	spec := p.running[ServerName(name)]
	auth, err := s.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil || auth.GetAuthString() == "" || auth.GetAuthString() != spec.Password {
		t.Errorf("auth string = %v, %v; server's is %q", auth, err, spec.Password)
	}
	if spec.MaxMemory != 2<<30 || spec.Configs["maxmemory-policy"] != "allkeys-lru" {
		t.Errorf("spec = %+v", spec)
	}
	l, _ := s.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/my-proj/locations/-"})
	if len(l.GetInstances()) != 2 {
		t.Errorf("ListInstances = %d, want the server `up` runs and cache", len(l.GetInstances()))
	}
	if _, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cache",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}}); code(err) != codes.AlreadyExists {
		t.Errorf("second create: %v", err)
	}

	op, err = s.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"memory_size_gb", "labels", "redis_configs"}},
		Instance: &redispb.Instance{Name: name, MemorySizeGb: 3, Labels: map[string]string{"env": "dev"},
			RedisConfigs: map[string]string{"notify-keyspace-events": "Ex"}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, op)
	inst, _ = s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if inst.GetMemorySizeGb() != 3 || inst.GetLabels()["env"] != "dev" || inst.GetDisplayName() != "Cache" ||
		inst.GetRedisConfigs()["notify-keyspace-events"] != "Ex" {
		t.Errorf("after update = %v", inst)
	}
	if (*set)["maxmemory"] != "3221225472" || (*set)["maxmemory-policy"] != "volatile-lru" || (*set)["notify-keyspace-events"] != "Ex" {
		t.Errorf("CONFIG SET = %v; want the new size, the new config and the removed one at its default", *set)
	}

	op, err = s.DeleteInstance(ctx, &redispb.DeleteInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, op)
	if _, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); code(err) != codes.NotFound {
		t.Errorf("after delete: %v", err)
	}
	if len(p.running) != 0 {
		t.Errorf("servers after delete = %v", p.running)
	}
}

func TestCreateRefusals(t *testing.T) {
	s, _, _ := newCreating(t, nil)
	ctx := context.Background()
	basic := func(f func(*redispb.Instance)) *redispb.Instance {
		i := &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}
		if f != nil {
			f(i)
		}
		return i
	}
	for _, tc := range []struct {
		desc string
		req  *redispb.CreateInstanceRequest
		want codes.Code
	}{
		{"bad id", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "Bad_ID", Instance: basic(nil)}, codes.InvalidArgument},
		{"no tier", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: &redispb.Instance{MemorySizeGb: 1}}, codes.InvalidArgument},
		{"no size", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: basic(func(i *redispb.Instance) { i.MemorySizeGb = 0 })}, codes.InvalidArgument},
		{"standard", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: basic(func(i *redispb.Instance) { i.Tier = redispb.Instance_STANDARD_HA })}, codes.Unimplemented},
		{"version", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: basic(func(i *redispb.Instance) { i.RedisVersion = "REDIS_6_X" })}, codes.Unimplemented},
		{"persistence period", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: basic(func(i *redispb.Instance) {
			i.PersistenceConfig = &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB, RdbSnapshotPeriod: 99}
		})}, codes.InvalidArgument},
		{"config", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: basic(func(i *redispb.Instance) { i.RedisConfigs = map[string]string{"save": "60 1"} })}, codes.InvalidArgument},
		{"policy", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "a", Instance: basic(func(i *redispb.Instance) { i.RedisConfigs = map[string]string{"maxmemory-policy": "lots"} })}, codes.InvalidArgument},
		{"the server up runs", &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "memorystore", Instance: basic(nil)}, codes.AlreadyExists},
		{"every region", &redispb.CreateInstanceRequest{Parent: "projects/my-proj/locations/-", InstanceId: "a", Instance: basic(nil)}, codes.InvalidArgument},
	} {
		if _, err := s.CreateInstance(ctx, tc.req); code(err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.desc, err, tc.want)
		}
	}
	def := s.inst.Name()
	if _, err := s.DeleteInstance(ctx, &redispb.DeleteInstanceRequest{Name: def}); code(err) != codes.FailedPrecondition {
		t.Errorf("delete the server up runs: %v", err)
	}
	if _, err := s.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		Instance: &redispb.Instance{Name: def}}); code(err) != codes.FailedPrecondition {
		t.Errorf("update the server up runs: %v", err)
	}
	if _, err := s.FailoverInstance(ctx, &redispb.FailoverInstanceRequest{Name: def}); code(err) != codes.Unimplemented {
		t.Errorf("failover: %v", err)
	}
}

func TestUpdateRefusals(t *testing.T) {
	s, _, _ := newCreating(t, nil)
	ctx := context.Background()
	op, _ := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "u",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	wait(t, s, op)
	name := parent + "/instances/u"
	for _, tc := range []struct {
		paths []string
		inst  *redispb.Instance
		want  codes.Code
	}{
		{nil, &redispb.Instance{Name: name}, codes.InvalidArgument},
		{[]string{"tier"}, &redispb.Instance{Name: name}, codes.InvalidArgument},
		{[]string{"replica_count"}, &redispb.Instance{Name: name}, codes.Unimplemented},
		{[]string{"memory_size_gb"}, &redispb.Instance{Name: name, MemorySizeGb: 0}, codes.InvalidArgument},
		{[]string{"redis_configs"}, &redispb.Instance{Name: name, RedisConfigs: map[string]string{"maxmemory-gb": "2"}}, codes.InvalidArgument},
	} {
		if _, err := s.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: tc.paths}, Instance: tc.inst}); code(err) != tc.want {
			t.Errorf("update %v: %v, want %s", tc.paths, err, tc.want)
		}
	}
}

func TestFailedCreateLeavesNoInstance(t *testing.T) {
	s, p, _ := newCreating(t, store.NewMemory())
	p.fail = errors.New("ImagePullBackOff")
	op, err := s.CreateInstance(context.Background(), &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "f",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() == nil {
		t.Fatal("create succeeded")
	}
	if _, err := s.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: parent + "/instances/f"}); code(err) != codes.NotFound {
		t.Errorf("after a failed create: %v", err)
	}
}

func TestRestoreRunsKeptInstancesAgain(t *testing.T) {
	db := store.NewMemory()
	s, _, _ := newCreating(t, db)
	op, _ := s.CreateInstance(context.Background(), &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "kept",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true}})
	wait(t, s, op)
	before, _ := s.GetInstanceAuthString(context.Background(), &redispb.GetInstanceAuthStringRequest{Name: parent + "/instances/kept"})

	again, p, _ := newCreating(t, db)
	if err := again.Restore(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		inst, err := again.GetInstance(context.Background(), &redispb.GetInstanceRequest{Name: parent + "/instances/kept"})
		if err != nil {
			t.Fatal(err)
		}
		if inst.GetState() == redispb.Instance_READY {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restored instance = %v", inst)
		}
		time.Sleep(5 * time.Millisecond)
	}
	after, _ := again.GetInstanceAuthString(context.Background(), &redispb.GetInstanceAuthStringRequest{Name: parent + "/instances/kept"})
	if after.GetAuthString() != before.GetAuthString() || p.running[ServerName(parent+"/instances/kept")].Password != before.GetAuthString() {
		t.Error("the restored server has another AUTH string")
	}
}

func TestServerName(t *testing.T) {
	a, b := ServerName(parent+"/instances/x"), ServerName("projects/other/locations/us-central1/instances/x")
	if a == b || len(ServerName(parent+"/instances/"+"a234567890123456789012345678901234567890")) > 63 {
		t.Errorf("ServerName: %s, %s", a, b)
	}
}
