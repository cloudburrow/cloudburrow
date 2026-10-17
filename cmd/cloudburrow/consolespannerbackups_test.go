package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// fakeSpannerAdmin answers the database and instance admin calls the
// #1498 console surfaces make, recording each request by method.
type fakeSpannerAdmin struct {
	databasepb.UnimplementedDatabaseAdminServer

	mu  sync.Mutex
	got map[string]proto.Message
}

// fakeSpannerInstAdmin is its instance admin half, recording into it.
type fakeSpannerInstAdmin struct {
	instancepb.UnimplementedInstanceAdminServer
	*fakeSpannerAdmin
}

func (f *fakeSpannerAdmin) record(method string, m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got[method] = m
}

func (f *fakeSpannerAdmin) req(method string) proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[method]
}

func spannerDoneOp(name string, resp proto.Message) *longrunningpb.Operation {
	a, _ := anypb.New(resp)
	return &longrunningpb.Operation{Name: name, Done: true, Result: &longrunningpb.Operation_Response{Response: a}}
}

const fakeInst = "projects/proj/instances/inst"

var fakeBackup = &databasepb.Backup{Name: fakeInst + "/backups/b1", Database: fakeInst + "/databases/db", State: databasepb.Backup_READY,
	SizeBytes: 2048, ExpireTime: timestamppb.New(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)),
	VersionTime: timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))}

func (f *fakeSpannerAdmin) ListDatabases(context.Context, *databasepb.ListDatabasesRequest) (*databasepb.ListDatabasesResponse, error) {
	return &databasepb.ListDatabasesResponse{Databases: []*databasepb.Database{{Name: fakeInst + "/databases/db"}}}, nil
}

func (f *fakeSpannerAdmin) GetDatabase(_ context.Context, r *databasepb.GetDatabaseRequest) (*databasepb.Database, error) {
	return &databasepb.Database{Name: r.GetName()}, nil
}

func (f *fakeSpannerAdmin) ListBackups(_ context.Context, r *databasepb.ListBackupsRequest) (*databasepb.ListBackupsResponse, error) {
	f.record("ListBackups", r)
	return &databasepb.ListBackupsResponse{Backups: []*databasepb.Backup{fakeBackup}}, nil
}

func (f *fakeSpannerAdmin) GetBackup(_ context.Context, r *databasepb.GetBackupRequest) (*databasepb.Backup, error) {
	f.record("GetBackup", r)
	return fakeBackup, nil
}

func (f *fakeSpannerAdmin) CreateBackup(_ context.Context, r *databasepb.CreateBackupRequest) (*longrunningpb.Operation, error) {
	f.record("CreateBackup", r)
	return spannerDoneOp(fakeInst+"/backups/"+r.GetBackupId()+"/operations/o1", r.GetBackup()), nil
}

func (f *fakeSpannerAdmin) CopyBackup(_ context.Context, r *databasepb.CopyBackupRequest) (*longrunningpb.Operation, error) {
	f.record("CopyBackup", r)
	return spannerDoneOp(fakeInst+"/backups/"+r.GetBackupId()+"/operations/o2", fakeBackup), nil
}

func (f *fakeSpannerAdmin) UpdateBackup(_ context.Context, r *databasepb.UpdateBackupRequest) (*databasepb.Backup, error) {
	f.record("UpdateBackup", r)
	return r.GetBackup(), nil
}

func (f *fakeSpannerAdmin) DeleteBackup(_ context.Context, r *databasepb.DeleteBackupRequest) (*emptypb.Empty, error) {
	f.record("DeleteBackup", r)
	return &emptypb.Empty{}, nil
}

func (f *fakeSpannerAdmin) RestoreDatabase(_ context.Context, r *databasepb.RestoreDatabaseRequest) (*longrunningpb.Operation, error) {
	f.record("RestoreDatabase", r)
	return spannerDoneOp(fakeInst+"/databases/"+r.GetDatabaseId()+"/operations/o3", &databasepb.Database{Name: fakeInst + "/databases/" + r.GetDatabaseId()}), nil
}

func (f *fakeSpannerAdmin) ListBackupOperations(context.Context, *databasepb.ListBackupOperationsRequest) (*databasepb.ListBackupOperationsResponse, error) {
	meta, _ := anypb.New(&databasepb.CreateBackupMetadata{Name: fakeBackup.Name})
	return &databasepb.ListBackupOperationsResponse{Operations: []*longrunningpb.Operation{{Name: fakeBackup.Name + "/operations/o1", Done: true, Metadata: meta}}}, nil
}

func (f *fakeSpannerAdmin) ListDatabaseOperations(context.Context, *databasepb.ListDatabaseOperationsRequest) (*databasepb.ListDatabaseOperationsResponse, error) {
	meta, _ := anypb.New(&databasepb.RestoreDatabaseMetadata{Name: fakeInst + "/databases/restored"})
	return &databasepb.ListDatabaseOperationsResponse{Operations: []*longrunningpb.Operation{{Name: fakeInst + "/databases/restored/operations/o3", Done: true, Metadata: meta}}}, nil
}

func (f *fakeSpannerAdmin) ListBackupSchedules(context.Context, *databasepb.ListBackupSchedulesRequest) (*databasepb.ListBackupSchedulesResponse, error) {
	return &databasepb.ListBackupSchedulesResponse{BackupSchedules: []*databasepb.BackupSchedule{{Name: fakeInst + "/databases/db/backupSchedules/daily",
		Spec: &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: "0 2 * * *"}}}}}}, nil
}

func (f *fakeSpannerAdmin) CreateBackupSchedule(_ context.Context, r *databasepb.CreateBackupScheduleRequest) (*databasepb.BackupSchedule, error) {
	f.record("CreateBackupSchedule", r)
	return r.GetBackupSchedule(), nil
}

func (f *fakeSpannerAdmin) UpdateBackupSchedule(_ context.Context, r *databasepb.UpdateBackupScheduleRequest) (*databasepb.BackupSchedule, error) {
	f.record("UpdateBackupSchedule", r)
	return r.GetBackupSchedule(), nil
}

func (f *fakeSpannerAdmin) DeleteBackupSchedule(_ context.Context, r *databasepb.DeleteBackupScheduleRequest) (*emptypb.Empty, error) {
	f.record("DeleteBackupSchedule", r)
	return &emptypb.Empty{}, nil
}

func (f *fakeSpannerAdmin) ListDatabaseRoles(context.Context, *databasepb.ListDatabaseRolesRequest) (*databasepb.ListDatabaseRolesResponse, error) {
	return &databasepb.ListDatabaseRolesResponse{DatabaseRoles: []*databasepb.DatabaseRole{{Name: fakeInst + "/databases/db/databaseRoles/public"}}}, nil
}

func (f fakeSpannerInstAdmin) GetInstance(context.Context, *instancepb.GetInstanceRequest) (*instancepb.Instance, error) {
	return &instancepb.Instance{Name: fakeInst, Config: "projects/proj/instanceConfigs/regional-us-central1"}, nil
}

var fakeConfigs = []*instancepb.InstanceConfig{
	{Name: "projects/proj/instanceConfigs/custom-mine", ConfigType: instancepb.InstanceConfig_USER_MANAGED, DisplayName: "Mine"},
	{Name: "projects/proj/instanceConfigs/regional-us-central1", ConfigType: instancepb.InstanceConfig_GOOGLE_MANAGED,
		Replicas: []*instancepb.ReplicaInfo{{Location: "us-central1", Type: instancepb.ReplicaInfo_READ_WRITE, DefaultLeaderLocation: true}}},
	{Name: "projects/proj/instanceConfigs/nam3", ConfigType: instancepb.InstanceConfig_GOOGLE_MANAGED},
}

func (f fakeSpannerInstAdmin) ListInstanceConfigs(context.Context, *instancepb.ListInstanceConfigsRequest) (*instancepb.ListInstanceConfigsResponse, error) {
	return &instancepb.ListInstanceConfigsResponse{InstanceConfigs: fakeConfigs}, nil
}

func (f fakeSpannerInstAdmin) GetInstanceConfig(_ context.Context, r *instancepb.GetInstanceConfigRequest) (*instancepb.InstanceConfig, error) {
	for _, c := range fakeConfigs {
		if c.Name == r.GetName() {
			return c, nil
		}
	}
	return &instancepb.InstanceConfig{Name: r.GetName()}, nil
}

func (f fakeSpannerInstAdmin) CreateInstanceConfig(_ context.Context, r *instancepb.CreateInstanceConfigRequest) (*longrunningpb.Operation, error) {
	f.record("CreateInstanceConfig", r)
	return spannerDoneOp("projects/proj/instanceConfigs/"+r.GetInstanceConfigId()+"/operations/o4", r.GetInstanceConfig()), nil
}

func (f fakeSpannerInstAdmin) UpdateInstanceConfig(_ context.Context, r *instancepb.UpdateInstanceConfigRequest) (*longrunningpb.Operation, error) {
	f.record("UpdateInstanceConfig", r)
	return spannerDoneOp(r.GetInstanceConfig().GetName()+"/operations/o5", r.GetInstanceConfig()), nil
}

func (f fakeSpannerInstAdmin) DeleteInstanceConfig(_ context.Context, r *instancepb.DeleteInstanceConfigRequest) (*emptypb.Empty, error) {
	f.record("DeleteInstanceConfig", r)
	return &emptypb.Empty{}, nil
}

func (f fakeSpannerInstAdmin) ListInstanceConfigOperations(context.Context, *instancepb.ListInstanceConfigOperationsRequest) (*instancepb.ListInstanceConfigOperationsResponse, error) {
	return &instancepb.ListInstanceConfigOperationsResponse{Operations: []*longrunningpb.Operation{{Name: "projects/proj/instanceConfigs/custom-mine/operations/o4", Done: true}}}, nil
}

func (f fakeSpannerInstAdmin) MoveInstance(_ context.Context, r *instancepb.MoveInstanceRequest) (*longrunningpb.Operation, error) {
	f.record("MoveInstance", r)
	return spannerDoneOp(r.GetName()+"/operations/o6", &instancepb.MoveInstanceResponse{}), nil
}

func (f fakeSpannerInstAdmin) GetInstancePartition(_ context.Context, r *instancepb.GetInstancePartitionRequest) (*instancepb.InstancePartition, error) {
	return &instancepb.InstancePartition{Name: r.GetName(), DisplayName: "part one",
		ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: 1}}, nil
}

func (f fakeSpannerInstAdmin) UpdateInstancePartition(_ context.Context, r *instancepb.UpdateInstancePartitionRequest) (*longrunningpb.Operation, error) {
	f.record("UpdateInstancePartition", r)
	return spannerDoneOp(r.GetInstancePartition().GetName()+"/operations/o7", r.GetInstancePartition()), nil
}

func startFakeSpannerAdmin(t *testing.T) (*fakeSpannerAdmin, spannerProvider) {
	t.Helper()
	f := &fakeSpannerAdmin{got: map[string]proto.Message{}}
	g := grpc.NewServer()
	databasepb.RegisterDatabaseAdminServer(g, f)
	instancepb.RegisterInstanceAdminServer(g, fakeSpannerInstAdmin{fakeSpannerAdmin: f})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return f, spannerProvider{endpoint: ln.Addr().String()}
}

func spannerSection(d console.Detail, id string) *console.Section {
	for i := range d.Sections {
		if d.Sections[i].ID == id {
			return &d.Sections[i]
		}
	}
	return nil
}

// TestConsoleSpannerBackupsAndSchedules (#1498): an instance's page has
// Backups, Backup operations, Database operations and Configuration
// operations tabs, a backup row opening the backup's page; Create backup
// offers the instance's databases and Move instance the other
// configurations; the backup's page Updates expiry, Copies, Restores and
// Deletes it; a database's page lists its backup schedules and roles and
// Creates, Edits and Deletes a schedule; a Google-managed configuration's
// page Creates a custom configuration based on it, a custom one's Renames
// and Deletes it; the instance page's actions fit one row (#1498); and a
// partition's page Edits it. Each action sends the official client's
// request, and a malformed value is refused before any call.
func TestConsoleSpannerBackupsAndSchedules(t *testing.T) {
	f, p := startFakeSpannerAdmin(t)
	ctx := context.Background()

	d, err := p.Detail(ctx, "proj", []string{"inst"})
	if err != nil {
		t.Fatal(err)
	}
	backups := spannerSection(d, "backups")
	if backups == nil || backups.Listing.Unavailable != "" || len(backups.Listing.Items) != 1 {
		t.Fatalf("the Backups tab = %+v", backups)
	}
	row := backups.Listing.Items[0]
	if row.Name != "b1" || row.Status != "READY" || row.Fields["Database"] != "db" || row.Fields["Size"] != "2.0 KiB" ||
		strings.Join(row.Opens, "/") != "inst/Backups/b1" {
		t.Errorf("the backup's row = %+v", row)
	}
	for id, want := range map[string]string{"backup-operations": "CreateBackupMetadata", "database-operations": "RestoreDatabaseMetadata"} {
		s := spannerSection(d, id)
		if s == nil || len(s.Listing.Items) != 1 || s.Listing.Items[0].Fields["Type"] != want || s.Listing.Items[0].Fields["Done"] != "true" {
			t.Errorf("the %s tab = %+v, want one done %s", id, s, want)
		}
	}
	if s := spannerSection(d, "config-operations"); s == nil || len(s.Listing.Items) != 1 || s.Listing.Items[0].Fields["Configuration"] != "custom-mine" {
		t.Errorf("the configuration operations tab = %+v", s)
	}
	if s := spannerSection(d, "instance-configs"); s == nil || len(s.Listing.Items) != 3 || strings.Join(s.Listing.Items[0].Opens, "/") != "inst/instanceConfigs/custom-mine" {
		t.Errorf("the Instance configurations tab = %+v, want each row opening its configuration", s)
	}

	acts := p.DetailActions(ctx, "proj", []string{"inst"})
	// The page's buttons must fit one row at the browser tests' width: a
	// sixth (Create custom configuration) wrapped it, which pushed the
	// partitions tab's row under the "applied" snackbar, so a click on the
	// partition's link landed on the snackbar (TestSpannerSchemaAndPartitionsThroughThePage).
	if len(acts) > 5 {
		t.Errorf("an instance's page offers %d actions %v, want at most 5 so they fit one row", len(acts), actionIDs(acts))
	}
	var create, move console.Action
	for _, a := range acts {
		switch a.ID {
		case "createbackup":
			create = a
		case "moveinstance":
			move = a
		}
	}
	if create.Fields[0].Type != "select" || strings.Join(create.Fields[0].Options, ",") != "db" {
		t.Errorf("Create backup's database field = %+v, want a choice of the instance's databases", create.Fields[0])
	}
	if strings.Join(move.Fields[0].Options, ",") != "custom-mine,nam3" || move.Confirm == "" {
		t.Errorf("Move instance = %+v, want the other configurations and a confirmation", move)
	}

	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		if err := p.ActAt(ctx, "proj", path, action, values); err != nil {
			t.Fatalf("%s at %v: %v", action, path, err)
		}
	}
	act([]string{"inst"}, "createbackup", map[string]string{"database": "db", "backup": "b2", "expireDays": "7"})
	if r := f.req("CreateBackup").(*databasepb.CreateBackupRequest); r.GetParent() != fakeInst || r.GetBackupId() != "b2" ||
		r.GetBackup().GetDatabase() != fakeInst+"/databases/db" || time.Until(r.GetBackup().GetExpireTime().AsTime()) < 6*24*time.Hour {
		t.Errorf("CreateBackup = %v", r)
	}
	if err := p.ActAt(ctx, "proj", []string{"inst"}, "createbackup", map[string]string{"database": "db", "backup": "b3", "expireDays": "400"}); err == nil {
		t.Error("Create backup expiring in 400 days was not refused")
	}

	bd, err := p.Detail(ctx, "proj", []string{"inst", spannerBackups, "b1"})
	if err != nil || bd.Unavailable != "" || bd.Summary[1].Value != "db" {
		t.Fatalf("the backup's page = %+v, %v", bd, err)
	}
	if got := strings.Join(actionIDs(p.DetailActions(ctx, "proj", []string{"inst", spannerBackups, "b1"})), ","); got != "updatebackup,copybackup,restorebackup,deletebackup" {
		t.Errorf("the backup's actions = %s", got)
	}
	at := []string{"inst", spannerBackups, "b1"}
	act(at, "updatebackup", map[string]string{"expireDays": "14"})
	if r := f.req("UpdateBackup").(*databasepb.UpdateBackupRequest); r.GetBackup().GetName() != fakeInst+"/backups/b1" || r.GetUpdateMask().GetPaths()[0] != "expire_time" {
		t.Errorf("UpdateBackup = %v", r)
	}
	act(at, "copybackup", map[string]string{"backup": "b1-copy", "expireDays": "3"})
	if r := f.req("CopyBackup").(*databasepb.CopyBackupRequest); r.GetSourceBackup() != fakeInst+"/backups/b1" || r.GetBackupId() != "b1-copy" || r.GetParent() != fakeInst {
		t.Errorf("CopyBackup = %v", r)
	}
	act(at, "restorebackup", map[string]string{"database": "restored"})
	if r := f.req("RestoreDatabase").(*databasepb.RestoreDatabaseRequest); r.GetDatabaseId() != "restored" || r.GetBackup() != fakeInst+"/backups/b1" {
		t.Errorf("RestoreDatabase = %v", r)
	}
	act(at, "deletebackup", nil)
	if r := f.req("DeleteBackup").(*databasepb.DeleteBackupRequest); r.GetName() != fakeInst+"/backups/b1" {
		t.Errorf("DeleteBackup = %v", r)
	}

	// A database's page: its schedules and roles.
	dd, err := p.Detail(ctx, "proj", []string{"inst", "db"})
	if err != nil {
		t.Fatal(err)
	}
	sched := spannerSection(dd, "backup-schedules")
	if sched == nil || len(sched.Listing.Items) != 1 || sched.Listing.Items[0].Fields["Cron"] != "0 2 * * *" ||
		strings.Join(sched.Listing.Items[0].ActsOn, "/") != "inst/db/backupSchedules/daily" {
		t.Errorf("the Backup schedules tab = %+v", sched)
	}
	if roles := spannerSection(dd, "database-roles"); roles == nil || len(roles.Listing.Items) != 1 || roles.Listing.Items[0].Name != "public" {
		t.Errorf("the Database roles tab = %+v", roles)
	}
	act([]string{"inst", "db"}, "createschedule", map[string]string{"schedule": "nightly", "cron": "0 3 * * *", "retentionDays": "2"})
	if r := f.req("CreateBackupSchedule").(*databasepb.CreateBackupScheduleRequest); r.GetParent() != fakeInst+"/databases/db" ||
		r.GetBackupScheduleId() != "nightly" || r.GetBackupSchedule().GetSpec().GetCronSpec().GetText() != "0 3 * * *" ||
		r.GetBackupSchedule().GetRetentionDuration().AsDuration() != 48*time.Hour || r.GetBackupSchedule().GetFullBackupSpec() == nil {
		t.Errorf("CreateBackupSchedule = %v", r)
	}
	if err := p.ActAt(ctx, "proj", []string{"inst", "db"}, "createschedule", map[string]string{"schedule": "bad", "cron": "daily", "retentionDays": "2"}); err == nil {
		t.Error("a cron schedule that is not five fields was not refused")
	}
	sat := []string{"inst", "db", "backupSchedules", "daily"}
	act(sat, "editschedule", map[string]string{"cron": "0 4 * * *", "retentionDays": "7"})
	if r := f.req("UpdateBackupSchedule").(*databasepb.UpdateBackupScheduleRequest); r.GetBackupSchedule().GetName() != fakeInst+"/databases/db/backupSchedules/daily" ||
		r.GetBackupSchedule().GetSpec().GetCronSpec().GetText() != "0 4 * * *" || len(r.GetUpdateMask().GetPaths()) != 2 {
		t.Errorf("UpdateBackupSchedule = %v", r)
	}
	act(sat, "deleteschedule", nil)
	if r := f.req("DeleteBackupSchedule").(*databasepb.DeleteBackupScheduleRequest); r.GetName() != fakeInst+"/databases/db/backupSchedules/daily" {
		t.Errorf("DeleteBackupSchedule = %v", r)
	}

	// Configurations.
	base := []string{"inst", spannerConfigs, "regional-us-central1"}
	act(base, "createconfig", map[string]string{"config": "custom-two"})
	if r := f.req("CreateInstanceConfig").(*instancepb.CreateInstanceConfigRequest); r.GetInstanceConfigId() != "custom-two" ||
		r.GetInstanceConfig().GetBaseConfig() != "projects/proj/instanceConfigs/regional-us-central1" || len(r.GetInstanceConfig().GetReplicas()) != 1 ||
		r.GetInstanceConfig().GetDisplayName() != "custom-two" {
		t.Errorf("CreateInstanceConfig = %v, want the base's replicas", r)
	}
	if err := p.ActAt(ctx, "proj", base, "createconfig", map[string]string{"config": "mine"}); err == nil {
		t.Error("a custom configuration ID without custom- was not refused")
	}
	cd, err := p.Detail(ctx, "proj", []string{"inst", spannerConfigs, "regional-us-central1"})
	if err != nil || cd.Unavailable != "" || len(spannerSection(cd, "replicas").Listing.Items) != 1 {
		t.Errorf("a configuration's page = %+v, %v", cd, err)
	}
	if got := strings.Join(actionIDs(p.DetailActions(ctx, "proj", base)), ","); got != "createconfig" {
		t.Errorf("a Google-managed configuration offers %s, want Create custom configuration alone", got)
	}
	cat := []string{"inst", spannerConfigs, "custom-mine"}
	if got := strings.Join(actionIDs(p.DetailActions(ctx, "proj", cat)), ","); got != "renameconfig,deleteconfig" {
		t.Errorf("a custom configuration's actions = %s", got)
	}
	act(cat, "renameconfig", map[string]string{"displayName": "Renamed"})
	if r := f.req("UpdateInstanceConfig").(*instancepb.UpdateInstanceConfigRequest); r.GetInstanceConfig().GetDisplayName() != "Renamed" ||
		r.GetUpdateMask().GetPaths()[0] != "display_name" {
		t.Errorf("UpdateInstanceConfig = %v", r)
	}
	act(cat, "deleteconfig", nil)
	if r := f.req("DeleteInstanceConfig").(*instancepb.DeleteInstanceConfigRequest); r.GetName() != "projects/proj/instanceConfigs/custom-mine" {
		t.Errorf("DeleteInstanceConfig = %v", r)
	}
	act([]string{"inst"}, "moveinstance", map[string]string{"targetConfig": "nam3"})
	if r := f.req("MoveInstance").(*instancepb.MoveInstanceRequest); r.GetName() != fakeInst || r.GetTargetConfig() != "projects/proj/instanceConfigs/nam3" {
		t.Errorf("MoveInstance = %v", r)
	}

	// A partition's Edit.
	pat := []string{"inst", spannerPartitions, "p1"}
	if got := strings.Join(actionIDs(p.DetailActions(ctx, "proj", pat)), ","); got != "editpartition,deletepartition" {
		t.Errorf("a partition's actions = %s", got)
	}
	act(pat, "editpartition", map[string]string{"displayName": "part two", "nodeCount": "2"})
	if r := f.req("UpdateInstancePartition").(*instancepb.UpdateInstancePartitionRequest); r.GetInstancePartition().GetDisplayName() != "part two" ||
		r.GetInstancePartition().GetNodeCount() != 2 || len(r.GetFieldMask().GetPaths()) != 2 {
		t.Errorf("UpdateInstancePartition = %v", r)
	}

	// Each action refuses a path it does not belong to before any call.
	for _, c := range []struct {
		action string
		path   []string
	}{
		{"createbackup", []string{"inst", "db"}},
		{"deletebackup", []string{"inst", "db", "Table"}},
		{"restorebackup", []string{"inst"}},
		{"createschedule", []string{"inst", spannerBackups}},
		{"editschedule", []string{"inst", "db"}},
		{"renameconfig", []string{"inst", spannerBackups, "b1"}},
		{"moveinstance", []string{"inst", "db"}},
		{"createconfig", []string{"inst"}},
		{"createconfig", []string{"inst", spannerConfigs, "custom-mine"}},
		{"editpartition", []string{"inst", spannerBackups, "b1"}},
	} {
		if handled, err := p.spannerBackupAct(ctx, "proj", c.path, c.action, nil); !handled || err == nil {
			t.Errorf("%s at %v = (%v, %v), want it refused", c.action, c.path, handled, err)
		}
	}
}
