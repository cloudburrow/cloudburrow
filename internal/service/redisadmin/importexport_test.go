package redisadmin

import (
	"context"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newIO(t *testing.T) (*Server, *fakeIO, fakeObjects, *map[string]string) {
	t.Helper()
	s := newTestServer(nil)
	io := &fakeIO{files: map[string][]byte{}, data: "REDIS0011 one"}
	set := &map[string]string{}
	p := shellProv{fakeProv: &fakeProv{running: map[string]Spec{}}, io: io}
	s.EnableCreate(context.Background(), p, func(addr, pw string) DataPlane { return fakeData{set: set, io: io} }, store.NewMemory(), nil)
	objs := fakeObjects{}
	s.UseStorage(objs)
	op, err := s.CreateInstance(context.Background(), &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cache",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	return s, io, objs, set
}

func TestExportWritesASnapshot(t *testing.T) {
	s, io, objs, _ := newIO(t)
	ctx := context.Background()
	name := parent + "/instances/cache"
	op, err := s.ExportInstance(ctx, &redispb.ExportInstanceRequest{Name: name,
		OutputConfig: &redispb.OutputConfig{Destination: &redispb.OutputConfig_GcsDestination{GcsDestination: &redispb.GcsDestination{Uri: "gs://b/dump.rdb"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := string(objs["b/dump.rdb"]); got != "REDIS0011 one" || io.saves != 1 {
		t.Errorf("exported %q after %d saves", got, io.saves)
	}
	for _, bad := range []string{"", "gs://b", "gs:///o", "s3://b/o"} {
		_, err := s.ExportInstance(ctx, &redispb.ExportInstanceRequest{Name: name,
			OutputConfig: &redispb.OutputConfig{Destination: &redispb.OutputConfig_GcsDestination{GcsDestination: &redispb.GcsDestination{Uri: bad}}}})
		if code(err) != codes.InvalidArgument {
			t.Errorf("export to %q: %v", bad, err)
		}
	}
}

func importReq(name, uri string) *redispb.ImportInstanceRequest {
	return &redispb.ImportInstanceRequest{Name: name,
		InputConfig: &redispb.InputConfig{Source: &redispb.InputConfig_GcsSource{GcsSource: &redispb.GcsSource{Uri: uri}}}}
}

func TestImportReplacesTheData(t *testing.T) {
	s, io, objs, _ := newIO(t)
	ctx := context.Background()
	name := parent + "/instances/cache"
	if _, err := s.ImportInstance(ctx, importReq(name, "gs://b/missing.rdb")); code(err) != codes.NotFound {
		t.Errorf("import of a missing object: %v", err)
	}
	objs["b/in.rdb"] = []byte("REDIS0011 two")
	op, err := s.ImportInstance(ctx, importReq(name, "gs://b/in.rdb"))
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if io.data != "REDIS0011 two" || io.loaded != "127.0.0.1:6380" {
		t.Errorf("data %q, loaded from %q", io.data, io.loaded)
	}
	ran := map[string]bool{}
	for _, a := range io.argv {
		ran[a[0]] = true
	}
	for _, want := range []string{"valkey-check-rdb", "valkey-server", "valkey-cli", "rm"} {
		if !ran[want] {
			t.Errorf("import did not run %s: %v", want, io.argv)
		}
	}
	inst, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if inst.GetState() != redispb.Instance_READY {
		t.Errorf("state after import %v", inst.GetState())
	}

	io.badRDB = true
	objs["b/bad.rdb"] = []byte("not an rdb")
	op, err = s.ImportInstance(ctx, importReq(name, "gs://b/bad.rdb"))
	if err != nil {
		t.Fatal(err)
	}
	if op = wait(t, s, op); codes.Code(op.GetError().GetCode()) != codes.InvalidArgument {
		t.Errorf("import of a bad file: %v", op.GetError())
	}
	if io.data != "REDIS0011 two" {
		t.Errorf("a refused import changed the data to %q", io.data)
	}
}

func TestImportExportNeedStorage(t *testing.T) {
	s, _, _ := newCreating(t, nil)
	ctx := context.Background()
	if _, err := s.ImportInstance(ctx, importReq(s.inst.Name(), "gs://b/o")); code(err) != codes.FailedPrecondition {
		t.Errorf("import into the server up runs: %v", err)
	}
	op, _ := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cache",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	wait(t, s, op)
	if _, err := s.ImportInstance(ctx, importReq(parent+"/instances/cache", "gs://b/o")); code(err) != codes.FailedPrecondition {
		t.Errorf("import without storage: %v", err)
	}
}

func TestRDBPersistence(t *testing.T) {
	s, io, _, set := newIO(t)
	ctx := context.Background()
	name := parent + "/instances/cache"
	start := time.Date(2033, 1, 1, 6, 45, 0, 0, time.UTC)
	op, err := s.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"persistence_config"}},
		Instance: &redispb.Instance{Name: name, PersistenceConfig: &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB,
			RdbSnapshotPeriod: redispb.PersistenceConfig_SIX_HOURS, RdbSnapshotStartTime: timestamppb.New(start)}}})
	if err != nil {
		t.Fatal(err)
	}
	inst := &redispb.Instance{}
	if err := wait(t, s, op).GetResponse().UnmarshalTo(inst); err != nil {
		t.Fatal(err)
	}
	pc := inst.GetPersistenceConfig()
	if pc.GetPersistenceMode() != redispb.PersistenceConfig_RDB || pc.GetRdbSnapshotPeriod() != redispb.PersistenceConfig_SIX_HOURS ||
		!pc.GetRdbNextSnapshotTime().AsTime().Equal(start) {
		t.Errorf("persistence_config %v", pc)
	}
	if (*set)["save"] != "21600 1" || (*set)["appendonly"] != "no" || io.saves != 1 {
		t.Errorf("server settings %v after %d saves", *set, io.saves)
	}
	s.created.mu.Lock()
	spec := s.created.byID[name].spec()
	s.created.mu.Unlock()
	if spec.RDBSeconds != 21600 {
		t.Errorf("next start's RDB seconds %d", spec.RDBSeconds)
	}

	// The next snapshot is aligned to the start time.
	pr := &persistence{Mode: "RDB", Period: "SIX_HOURS", Start: start}
	if got := pr.render(start.Add(7 * time.Hour)).GetRdbNextSnapshotTime().AsTime(); !got.Equal(start.Add(12 * time.Hour)) {
		t.Errorf("next snapshot %v", got)
	}

	s.PersistentMode(true)
	op, err = s.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"persistenceConfig"}},
		Instance: &redispb.Instance{Name: name, PersistenceConfig: &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_DISABLED}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, op)
	if (*set)["save"] != "" || (*set)["appendonly"] != "yes" {
		t.Errorf("server settings after DISABLED %v", *set)
	}
}

func TestOperationFilter(t *testing.T) {
	for _, bad := range []string{"name = x", "done = maybe", "done"} {
		if _, err := operationFilter(bad); code(err) != codes.InvalidArgument {
			t.Errorf("filter %q: %v", bad, err)
		}
	}
	s, _, _, _ := newIO(t)
	o := NewOperationsServer(s)
	resp, err := o.ListOperations(context.Background(), &longrunningpb.ListOperationsRequest{Name: parent, Filter: `metadata.verb != "create"`})
	if err != nil || len(resp.GetOperations()) != 0 {
		t.Errorf("verb != create = %v, %v", resp, err)
	}
	resp, err = o.ListOperations(context.Background(), &longrunningpb.ListOperationsRequest{Name: parent, Filter: `done = true AND metadata.verb = "create"`})
	if err != nil || len(resp.GetOperations()) != 1 {
		t.Fatalf("done create = %v, %v", resp, err)
	}
	name := resp.GetOperations()[0].GetName()
	if _, err := o.DeleteOperation(context.Background(), &longrunningpb.DeleteOperationRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: name}); code(err) != codes.NotFound {
		t.Errorf("after delete: %v", err)
	}
}
