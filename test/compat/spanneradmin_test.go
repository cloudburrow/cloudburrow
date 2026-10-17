//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// spannerAdminFixture holds the official instance and database admin clients
// for one test, with an instance and a database already created.
//
// A call that returns a long-running operation is waited on, so a refusal
// reported by the operation rather than the call is seen too.
type spannerAdminFixture struct {
	ctx               context.Context
	project, instance string // instance is the full resource name
	database          string // the full resource name
	inst              *instance.InstanceAdminClient
	db                *database.DatabaseAdminClient
}

func newSpannerAdmin(t *testing.T, instanceID, dbID string) *spannerAdminFixture {
	t.Helper()
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	db := spannerDatabase(t, ctx, addr, h.Project(), instanceID, dbID,
		`CREATE TABLE Items (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)`)
	ic, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	t.Cleanup(func() { ic.Close() })
	dc, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	t.Cleanup(func() { dc.Close() })
	return &spannerAdminFixture{
		ctx: ctx, project: h.Project(),
		instance: fmt.Sprintf("projects/%s/instances/%s", h.Project(), instanceID),
		database: db, inst: ic, db: dc,
	}
}

func (f *spannerAdminFixture) config() string {
	return fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", f.project)
}

// TestSpannerInstanceAdminListsAndDeletes covers the instance admin reads and
// the instance's own deletion: the emulator's one configuration is listed and
// got by name, the instance is listed under its project, and after
// DeleteInstance GetInstance answers NOT_FOUND and the listing omits it.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/ListInstanceConfigs, google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/ListInstances, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstance
func TestSpannerInstanceAdminListsAndDeletes(t *testing.T) {
	f := newSpannerAdmin(t, "cb-iadm", "iadmdb")
	ctx := f.ctx

	var configs []string
	cit := f.inst.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: "projects/" + f.project})
	for {
		c, err := cit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstanceConfigs: %v", err)
		}
		configs = append(configs, c.GetName())
	}
	found := false
	for _, c := range configs {
		found = found || c == f.config()
	}
	if !found {
		t.Errorf("ListInstanceConfigs returned %v, want it to include %s", configs, f.config())
	}
	cfg, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: f.config()})
	if err != nil {
		t.Fatalf("GetInstanceConfig: %v", err)
	}
	if cfg.GetName() != f.config() {
		t.Errorf("GetInstanceConfig returned %q, want %q", cfg.GetName(), f.config())
	}

	listed := func() map[string]bool {
		out := map[string]bool{}
		it := f.inst.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: "projects/" + f.project})
		for {
			i, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("ListInstances: %v", err)
			}
			out[i.GetName()] = true
		}
	}
	if !listed()[f.instance] {
		t.Fatalf("ListInstances does not list %s", f.instance)
	}

	if err := f.inst.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: f.instance}); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: f.instance}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after DeleteInstance = %v, want NotFound", err)
	}
	if listed()[f.instance] {
		t.Errorf("ListInstances still lists %s after DeleteInstance", f.instance)
	}
}

// TestSpannerInstancePartitionLifecycle creates an instance partition and
// waits for it, gets it, finds it and its create operation in the listings,
// and deletes it, after which GetInstancePartition answers NOT_FOUND.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/GetInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/ListInstancePartitions, google.spanner.admin.instance.v1.InstanceAdmin/ListInstancePartitionOperations, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstancePartition
func TestSpannerInstancePartitionLifecycle(t *testing.T) {
	f := newSpannerAdmin(t, "cb-ipart", "ipartdb")
	ctx := f.ctx
	name := f.instance + "/instancePartitions/p1"

	op, err := f.inst.CreateInstancePartition(ctx, &instancepb.CreateInstancePartitionRequest{
		Parent: f.instance, InstancePartitionId: "p1",
		InstancePartition: &instancepb.InstancePartition{
			Name: name, Config: f.config(), DisplayName: "partition one",
			ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateInstancePartition: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the instance partition: %v", err)
	}

	got, err := f.inst.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstancePartition: %v", err)
	}
	if got.GetName() != name || got.GetDisplayName() != "partition one" {
		t.Errorf("GetInstancePartition returned %q (%q), want %q (\"partition one\")", got.GetName(), got.GetDisplayName(), name)
	}

	var parts []string
	pit := f.inst.ListInstancePartitions(ctx, &instancepb.ListInstancePartitionsRequest{Parent: f.instance})
	for {
		p, err := pit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstancePartitions: %v", err)
		}
		parts = append(parts, p.GetName())
	}
	if len(parts) != 1 || parts[0] != name {
		t.Errorf("ListInstancePartitions returned %v, want [%s]", parts, name)
	}

	var ops []string
	oit := f.inst.ListInstancePartitionOperations(ctx, &instancepb.ListInstancePartitionOperationsRequest{Parent: f.instance})
	for {
		o, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstancePartitionOperations: %v", err)
		}
		ops = append(ops, o.GetName())
	}
	if len(ops) == 0 {
		t.Error("ListInstancePartitionOperations lists no operation after a create")
	}
	for _, o := range ops {
		if !strings.HasPrefix(o, name+"/operations/") {
			t.Errorf("ListInstancePartitionOperations lists %q, which is not an operation of %s", o, name)
		}
	}

	if err := f.inst.DeleteInstancePartition(ctx, &instancepb.DeleteInstancePartitionRequest{Name: name}); err != nil {
		t.Fatalf("DeleteInstancePartition: %v", err)
	}
	if _, err := f.inst.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstancePartition after DeleteInstancePartition = %v, want NotFound", err)
	}
}

// TestSpannerDatabaseAdminSchemaListAndDrop adds a table with
// UpdateDatabaseDdl and reads it back in the DDL, finds the database in
// ListDatabases, and drops it, after which GetDatabase answers NOT_FOUND.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/UpdateDatabaseDdl, google.spanner.admin.database.v1.DatabaseAdmin/ListDatabases, google.spanner.admin.database.v1.DatabaseAdmin/DropDatabase
func TestSpannerDatabaseAdminSchemaListAndDrop(t *testing.T) {
	f := newSpannerAdmin(t, "cb-dadm", "dadmdb")
	ctx := f.ctx

	op, err := f.db.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
		Database:   f.database,
		Statements: []string{"CREATE TABLE Tags (Id INT64 NOT NULL, Tag STRING(64)) PRIMARY KEY (Id)"},
	})
	if err != nil {
		t.Fatalf("UpdateDatabaseDdl: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the DDL: %v", err)
	}
	ddl, err := f.db.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: f.database})
	if err != nil {
		t.Fatalf("GetDatabaseDdl: %v", err)
	}
	if !strings.Contains(strings.Join(ddl.GetStatements(), "\n"), "CREATE TABLE Tags") {
		t.Errorf("after UpdateDatabaseDdl the DDL is %q, with no Tags table", ddl.GetStatements())
	}

	var dbs []string
	it := f.db.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: f.instance})
	for {
		d, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListDatabases: %v", err)
		}
		dbs = append(dbs, d.GetName())
	}
	if len(dbs) != 1 || dbs[0] != f.database {
		t.Errorf("ListDatabases returned %v, want [%s]", dbs, f.database)
	}

	if err := f.db.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: f.database}); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	if _, err := f.db.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: f.database}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDatabase after DropDatabase = %v, want NotFound", err)
	}
}

// TestSpannerAdminRefusals records what Google's emulator answers for the
// admin RPCs it does not implement, as its README documents for backups,
// backup schedules, instance configurations and instance partition
// updates: each is called through the official admin client, with a
// well-formed request on a real instance and database, and must answer
// UNIMPLEMENTED, saying why. The partition update is the emulator's own
// refusal ("... updating instance partitions."); the rest, which the
// emulator answers with an empty message, the call front refuses itself
// since #1461, naming the reason (internal/spannerfront). Instance updates,
// which the emulator refuses too, the front serves (TestSpannerUpdateInstance),
// and so it does IAM policies, stored and not enforced (TestSpannerIAM, #1486).
//
// A method the front starts serving fails this test, which is the signal
// to move it to a test that verifies it.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/MoveInstance (unimplemented)
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstanceConfig (unimplemented)
// covers: google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstanceConfig (unimplemented)
// covers: google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstanceConfig (unimplemented)
// covers: google.spanner.admin.instance.v1.InstanceAdmin/ListInstanceConfigOperations (unimplemented)
// covers: google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstancePartition (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListDatabaseOperations (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListDatabaseRoles (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/AddSplitPoints (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/InternalUpdateGraphOperation (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateBackup (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CopyBackup (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/GetBackup (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/UpdateBackup (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/DeleteBackup (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListBackups (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListBackupOperations (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/RestoreDatabase (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateBackupSchedule (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/GetBackupSchedule (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/UpdateBackupSchedule (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/DeleteBackupSchedule (unimplemented)
// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListBackupSchedules (unimplemented)
func TestSpannerAdminRefusals(t *testing.T) {
	f := newSpannerAdmin(t, "cb-refuse", "refusedb")
	project := "projects/" + f.project
	customConfig := project + "/instanceConfigs/custom-cb"
	partition := f.instance + "/instancePartitions/p1"
	backup := f.instance + "/backups/b1"
	schedule := f.database + "/backupSchedules/s1"
	expire := timestamppb.New(time.Now().Add(48 * time.Hour))
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	fullDaily := &databasepb.BackupSchedule{
		Name: schedule,
		Spec: &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{
			CronSpec: &databasepb.CrontabSpec{Text: "0 2 * * *"}}},
		BackupTypeSpec: &databasepb.BackupSchedule_FullBackupSpec{FullBackupSpec: &databasepb.FullBackupSpec{}},
	}
	ia, da := f.inst, f.db

	calls := map[string]func(context.Context) error{
		"InstanceAdmin/MoveInstance": func(ctx context.Context) error {
			op, err := ia.MoveInstance(ctx, &instancepb.MoveInstanceRequest{Name: f.instance, TargetConfig: f.config()})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"InstanceAdmin/CreateInstanceConfig": func(ctx context.Context) error {
			op, err := ia.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{
				Parent: project, InstanceConfigId: "custom-cb",
				InstanceConfig: &instancepb.InstanceConfig{Name: customConfig, BaseConfig: f.config(), DisplayName: "custom"}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"InstanceAdmin/UpdateInstanceConfig": func(ctx context.Context) error {
			op, err := ia.UpdateInstanceConfig(ctx, &instancepb.UpdateInstanceConfigRequest{
				InstanceConfig: &instancepb.InstanceConfig{Name: f.config(), DisplayName: "renamed"},
				UpdateMask:     mask("display_name")})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"InstanceAdmin/DeleteInstanceConfig": func(ctx context.Context) error {
			return ia.DeleteInstanceConfig(ctx, &instancepb.DeleteInstanceConfigRequest{Name: customConfig})
		},
		"InstanceAdmin/ListInstanceConfigOperations": func(ctx context.Context) error {
			_, err := ia.ListInstanceConfigOperations(ctx, &instancepb.ListInstanceConfigOperationsRequest{Parent: project}).Next()
			return err
		},
		"InstanceAdmin/UpdateInstancePartition": func(ctx context.Context) error {
			// The partition exists, so the refusal is not a NOT_FOUND in
			// disguise.
			cop, err := ia.CreateInstancePartition(ctx, &instancepb.CreateInstancePartitionRequest{
				Parent: f.instance, InstancePartitionId: "p1",
				InstancePartition: &instancepb.InstancePartition{Name: partition, Config: f.config(), DisplayName: "p1",
					ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: 1}}})
			if err != nil {
				return fmt.Errorf("creating the partition to update: %w", err)
			}
			if _, err := cop.Wait(ctx); err != nil {
				return fmt.Errorf("creating the partition to update: %w", err)
			}
			op, err := ia.UpdateInstancePartition(ctx, &instancepb.UpdateInstancePartitionRequest{
				InstancePartition: &instancepb.InstancePartition{Name: partition, DisplayName: "renamed"},
				FieldMask:         mask("display_name")})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"DatabaseAdmin/ListDatabaseOperations": func(ctx context.Context) error {
			_, err := da.ListDatabaseOperations(ctx, &databasepb.ListDatabaseOperationsRequest{Parent: f.instance}).Next()
			return err
		},
		"DatabaseAdmin/ListDatabaseRoles": func(ctx context.Context) error {
			_, err := da.ListDatabaseRoles(ctx, &databasepb.ListDatabaseRolesRequest{Parent: f.database}).Next()
			return err
		},
		"DatabaseAdmin/AddSplitPoints": func(ctx context.Context) error {
			_, err := da.AddSplitPoints(ctx, &databasepb.AddSplitPointsRequest{
				Database: f.database,
				SplitPoints: []*databasepb.SplitPoints{{Table: "Items", Keys: []*databasepb.SplitPoints_Key{{
					KeyParts: &structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue("100")}}}}}}})
			return err
		},
		"DatabaseAdmin/InternalUpdateGraphOperation": func(ctx context.Context) error {
			_, err := da.InternalUpdateGraphOperation(ctx, &databasepb.InternalUpdateGraphOperationRequest{
				Database: f.database, OperationId: "op1"})
			return err
		},
		"DatabaseAdmin/CreateBackup": func(ctx context.Context) error {
			op, err := da.CreateBackup(ctx, &databasepb.CreateBackupRequest{
				Parent: f.instance, BackupId: "b1",
				Backup: &databasepb.Backup{Database: f.database, ExpireTime: expire}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"DatabaseAdmin/CopyBackup": func(ctx context.Context) error {
			op, err := da.CopyBackup(ctx, &databasepb.CopyBackupRequest{
				Parent: f.instance, BackupId: "b2", SourceBackup: backup, ExpireTime: expire})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"DatabaseAdmin/GetBackup": func(ctx context.Context) error {
			_, err := da.GetBackup(ctx, &databasepb.GetBackupRequest{Name: backup})
			return err
		},
		"DatabaseAdmin/UpdateBackup": func(ctx context.Context) error {
			_, err := da.UpdateBackup(ctx, &databasepb.UpdateBackupRequest{
				Backup:     &databasepb.Backup{Name: backup, ExpireTime: expire},
				UpdateMask: mask("expire_time")})
			return err
		},
		"DatabaseAdmin/DeleteBackup": func(ctx context.Context) error {
			return da.DeleteBackup(ctx, &databasepb.DeleteBackupRequest{Name: backup})
		},
		"DatabaseAdmin/ListBackups": func(ctx context.Context) error {
			_, err := da.ListBackups(ctx, &databasepb.ListBackupsRequest{Parent: f.instance}).Next()
			return err
		},
		"DatabaseAdmin/ListBackupOperations": func(ctx context.Context) error {
			_, err := da.ListBackupOperations(ctx, &databasepb.ListBackupOperationsRequest{Parent: f.instance}).Next()
			return err
		},
		"DatabaseAdmin/RestoreDatabase": func(ctx context.Context) error {
			op, err := da.RestoreDatabase(ctx, &databasepb.RestoreDatabaseRequest{
				Parent: f.instance, DatabaseId: "restored",
				Source: &databasepb.RestoreDatabaseRequest_Backup{Backup: backup}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		},
		"DatabaseAdmin/CreateBackupSchedule": func(ctx context.Context) error {
			_, err := da.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{
				Parent: f.database, BackupScheduleId: "s1", BackupSchedule: fullDaily})
			return err
		},
		"DatabaseAdmin/GetBackupSchedule": func(ctx context.Context) error {
			_, err := da.GetBackupSchedule(ctx, &databasepb.GetBackupScheduleRequest{Name: schedule})
			return err
		},
		"DatabaseAdmin/UpdateBackupSchedule": func(ctx context.Context) error {
			_, err := da.UpdateBackupSchedule(ctx, &databasepb.UpdateBackupScheduleRequest{
				BackupSchedule: fullDaily, UpdateMask: mask("spec")})
			return err
		},
		"DatabaseAdmin/DeleteBackupSchedule": func(ctx context.Context) error {
			return da.DeleteBackupSchedule(ctx, &databasepb.DeleteBackupScheduleRequest{Name: schedule})
		},
		"DatabaseAdmin/ListBackupSchedules": func(ctx context.Context) error {
			_, err := da.ListBackupSchedules(ctx, &databasepb.ListBackupSchedulesRequest{Parent: f.database}).Next()
			return err
		},
	}

	names := make([]string, 0, len(calls))
	for n := range calls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		err := calls[n](ctx)
		cancel()
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("%s = %v, want Unimplemented", n, err)
			continue
		}
		if len(status.Convert(err).Message()) < 20 {
			t.Errorf("%s = %v, want a reason", n, err)
		}
		t.Logf("%s: %v", n, err)
	}
	if want := 23; len(calls) != want {
		t.Errorf("%d refusals are called, want %d, one per covers line", len(calls), want)
	}
}

// TestSpannerUpdateInstance (#1461): the call front in the Spanner pod
// answers UpdateInstance, which the emulator refuses, for the display name,
// compute capacity and labels; the official client's operation is done,
// GetInstance and ListInstances return the changes, and what an emulator
// instance cannot carry (autoscaling) is refused UNIMPLEMENTED, naming why.
// A capacity Google refuses (1500 processing units) is INVALID_ARGUMENT.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance
func TestSpannerUpdateInstance(t *testing.T) {
	f := newSpannerAdmin(t, "cb-update", "updatedb")
	ctx := f.ctx
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	op, err := f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance:  &instancepb.Instance{Name: f.instance, DisplayName: "updated by sdk", NodeCount: 3, Labels: map[string]string{"env": "test"}},
		FieldMask: mask("display_name", "node_count", "labels")})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	got, err := op.Wait(ctx)
	if err != nil || got.GetDisplayName() != "updated by sdk" || got.GetNodeCount() != 3 || got.GetProcessingUnits() != 3000 {
		t.Fatalf("the operation's instance = %v, %v", got, err)
	}
	in, err := f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: f.instance})
	if err != nil || in.GetDisplayName() != "updated by sdk" || in.GetNodeCount() != 3 || in.GetLabels()["env"] != "test" || in.GetConfig() != f.config() {
		t.Fatalf("GetInstance after the update = %v, %v", in, err)
	}
	found := false
	it := f.inst.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: "projects/" + f.project})
	for {
		i, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		if i.GetName() == f.instance {
			found = i.GetDisplayName() == "updated by sdk" && i.GetNodeCount() == 3
		}
	}
	if !found {
		t.Error("ListInstances does not show the update")
	}
	op, err = f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{Name: f.instance, ProcessingUnits: 500}, FieldMask: mask("processing_units")})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("UpdateInstance to 500 processing units: %v", err)
	}
	if in, err := f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: f.instance}); err != nil || in.GetProcessingUnits() != 500 || in.GetNodeCount() != 0 {
		t.Errorf("after 500 processing units = %v, %v", in, err)
	}
	if _, err := f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{Name: f.instance, ProcessingUnits: 1500}, FieldMask: mask("processing_units")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("1500 processing units: %v, want InvalidArgument", err)
	}
	_, err = f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{Name: f.instance, AutoscalingConfig: &instancepb.AutoscalingConfig{}}, FieldMask: mask("autoscaling_config")})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "autoscale") {
		t.Errorf("autoscaling: %v, want UNIMPLEMENTED naming why", err)
	}
}

// TestSpannerDatabaseDropProtection (#1461): UpdateDatabase, which the
// emulator refuses, sets enable_drop_protection through the official
// database admin client (the call front answers it); GetDatabase reads it
// back, DropDatabase and DeleteInstance are refused FAILED_PRECONDITION while
// it is on, as Google refuses them, and the database drops once it is off.
// Another field is UNIMPLEMENTED, naming why.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/UpdateDatabase
func TestSpannerDatabaseDropProtection(t *testing.T) {
	f := newSpannerAdmin(t, "cb-protect", "protectdb")
	ctx := f.ctx
	set := func(on bool) {
		t.Helper()
		op, err := f.db.UpdateDatabase(ctx, &databasepb.UpdateDatabaseRequest{
			Database:   &databasepb.Database{Name: f.database, EnableDropProtection: on},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enable_drop_protection"}}})
		if err != nil {
			t.Fatalf("UpdateDatabase(%v): %v", on, err)
		}
		if db, err := op.Wait(ctx); err != nil || db.GetEnableDropProtection() != on {
			t.Fatalf("the operation's database = %v, %v", db, err)
		}
	}
	set(true)
	if db, err := f.db.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: f.database}); err != nil || !db.GetEnableDropProtection() {
		t.Fatalf("GetDatabase = %v, %v", db, err)
	}
	if err := f.db.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: f.database}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DropDatabase of a protected database: %v, want FAILED_PRECONDITION", err)
	}
	if err := f.inst.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: f.instance}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstance holding a protected database: %v, want FAILED_PRECONDITION", err)
	}
	if _, err := f.db.UpdateDatabase(ctx, &databasepb.UpdateDatabaseRequest{Database: &databasepb.Database{Name: f.database},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"encryption_config"}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateDatabase of encryption_config: %v, want UNIMPLEMENTED", err)
	}
	set(false)
	if err := f.db.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: f.database}); err != nil {
		t.Errorf("DropDatabase once unprotected: %v", err)
	}
}
