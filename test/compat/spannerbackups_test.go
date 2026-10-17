//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// spannerRows reads "SELECT Id, Name FROM Items ORDER BY Id" and the child
// table's rows from db, as "id=name" and "id/n=note" strings.
func spannerRows(t *testing.T, ctx context.Context, addr, db string) []string {
	t.Helper()
	c, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient(%s): %v", db, err)
	}
	defer c.Close()
	var out []string
	for _, q := range []string{"SELECT CAST(Id AS STRING), Name FROM Items ORDER BY Id",
		"SELECT CONCAT(CAST(Id AS STRING), '/', CAST(N AS STRING)), Note FROM Notes ORDER BY Id, N",
		"SELECT CONCAT(CAST(Id AS STRING), '*2=', CAST(Twice AS STRING)), '' FROM Items ORDER BY Id"} {
		it := c.Single().Query(ctx, spanner.Statement{SQL: q})
		err := it.Do(func(r *spanner.Row) error {
			var k string
			var v spanner.NullString
			if err := r.Columns(&k, &v); err != nil {
				return err
			}
			out = append(out, k+"="+v.StringVal)
			return nil
		})
		if err != nil {
			t.Fatalf("%s on %s: %v", q, db, err)
		}
	}
	return out
}

// TestSpannerBackupAndRestore (#1498): a backup holds the database's
// schema and rows as they were at its version time. The official client
// creates one and waits for it, gets it, finds it in ListBackups (filtered
// by database), extends its expiry, copies it, and finds both operations
// in ListBackupOperations; a row written after the backup is not in the
// database restored from the copy, which has the rows, the interleaved
// child's, its generated column recomputed, and its restore information;
// the restore is in ListDatabaseOperations. A backup's IAM policy is
// stored, and DeleteBackup removes it. An expiry under 6 hours is
// INVALID_ARGUMENT.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateBackup, google.spanner.admin.database.v1.DatabaseAdmin/GetBackup, google.spanner.admin.database.v1.DatabaseAdmin/ListBackups, google.spanner.admin.database.v1.DatabaseAdmin/UpdateBackup, google.spanner.admin.database.v1.DatabaseAdmin/CopyBackup, google.spanner.admin.database.v1.DatabaseAdmin/ListBackupOperations, google.spanner.admin.database.v1.DatabaseAdmin/RestoreDatabase, google.spanner.admin.database.v1.DatabaseAdmin/ListDatabaseOperations, google.spanner.admin.database.v1.DatabaseAdmin/DeleteBackup
func TestSpannerBackupAndRestore(t *testing.T) {
	f := newSpannerAdmin(t, "cb-backup", "backupdb")
	ctx := f.ctx

	op, err := f.db.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: f.database, Statements: []string{
		`ALTER TABLE Items ADD COLUMN Twice INT64 AS (Id * 2) STORED`,
		`CREATE TABLE Notes (Id INT64 NOT NULL, N INT64 NOT NULL, Note STRING(MAX)) PRIMARY KEY (Id, N), INTERLEAVE IN PARENT Items ON DELETE CASCADE`,
	}})
	if err != nil {
		t.Fatalf("UpdateDatabaseDdl: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("UpdateDatabaseDdl: %v", err)
	}
	dc, err := spanner.NewClient(ctx, f.database, spannerOpts(f.addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	defer dc.Close()
	if _, err := dc.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Items", []string{"Id", "Name"}, []any{1, "one"}),
		spanner.Insert("Items", []string{"Id", "Name"}, []any{2, "two"}),
		spanner.Insert("Notes", []string{"Id", "N", "Note"}, []any{1, 1, "first note"}),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := spannerRows(t, ctx, f.addr, f.database)

	backup := f.instance + "/backups/b1"
	expire := timestamppb.New(time.Now().Add(48 * time.Hour))
	if _, err := f.db.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: f.instance, BackupId: "short",
		Backup: &databasepb.Backup{Database: f.database, ExpireTime: timestamppb.New(time.Now().Add(time.Hour))}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateBackup expiring in an hour = %v, want InvalidArgument", err)
	}
	bop, err := f.db.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: f.instance, BackupId: "b1",
		Backup: &databasepb.Backup{Database: f.database, ExpireTime: expire}})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	b, err := bop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateBackup's operation: %v", err)
	}
	if b.GetName() != backup || b.GetState() != databasepb.Backup_READY || b.GetDatabase() != f.database || b.GetSizeBytes() <= 0 || b.GetVersionTime() == nil {
		t.Fatalf("the backup = %v", b)
	}
	// Written after the backup: no restore of it has this row.
	if _, err := dc.Apply(ctx, []*spanner.Mutation{spanner.Insert("Items", []string{"Id", "Name"}, []any{3, "three"})}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := f.db.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: f.instance, BackupId: "b1",
		Backup: &databasepb.Backup{Database: f.database, ExpireTime: expire}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateBackup of an existing id = %v, want AlreadyExists", err)
	}

	got, err := f.db.GetBackup(ctx, &databasepb.GetBackupRequest{Name: backup})
	if err != nil || got.GetExpireTime().AsTime() != expire.AsTime() {
		t.Fatalf("GetBackup = %v, %v", got, err)
	}
	listed := func(filter string) []string {
		var out []string
		it := f.db.ListBackups(ctx, &databasepb.ListBackupsRequest{Parent: f.instance, Filter: filter})
		for {
			b, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("ListBackups(%q): %v", filter, err)
			}
			out = append(out, b.GetName())
		}
	}
	if l := listed(fmt.Sprintf(`database:%s`, "backupdb")); len(l) != 1 || l[0] != backup {
		t.Errorf("ListBackups by database = %v, want [%s]", l, backup)
	}
	if l := listed(`state:CREATING`); len(l) != 0 {
		t.Errorf("ListBackups of creating backups = %v, want none", l)
	}

	later := timestamppb.New(time.Now().Add(72 * time.Hour))
	ub, err := f.db.UpdateBackup(ctx, &databasepb.UpdateBackupRequest{Backup: &databasepb.Backup{Name: backup, ExpireTime: later},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
	if err != nil || ub.GetExpireTime().AsTime() != later.AsTime() {
		t.Fatalf("UpdateBackup = %v, %v", ub, err)
	}

	cop, err := f.db.CopyBackup(ctx, &databasepb.CopyBackupRequest{Parent: f.instance, BackupId: "b1-copy", SourceBackup: backup, ExpireTime: expire})
	if err != nil {
		t.Fatalf("CopyBackup: %v", err)
	}
	cb, err := cop.Wait(ctx)
	if err != nil || cb.GetVersionTime().AsTime() != b.GetVersionTime().AsTime() {
		t.Fatalf("CopyBackup's operation = %v, %v", cb, err)
	}

	types := map[string]bool{}
	oit := f.db.ListBackupOperations(ctx, &databasepb.ListBackupOperationsRequest{Parent: f.instance})
	for {
		o, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListBackupOperations: %v", err)
		}
		types[o.GetMetadata().GetTypeUrl()] = true
	}
	for _, ty := range []string{"CreateBackupMetadata", "CopyBackupMetadata"} {
		if !types["type.googleapis.com/google.spanner.admin.database.v1."+ty] {
			t.Errorf("ListBackupOperations lists %v, want a %s", types, ty)
		}
	}
	fit := f.db.ListBackupOperations(ctx, &databasepb.ListBackupOperationsRequest{Parent: f.instance,
		Filter: `(metadata.@type:type.googleapis.com/google.spanner.admin.database.v1.CopyBackupMetadata) AND (metadata.source_backup:b1)`})
	if o, err := fit.Next(); err != nil || !strings.HasPrefix(o.GetName(), f.instance+"/backups/b1-copy/operations/") {
		t.Errorf("ListBackupOperations filtered to copies = %v, %v", o, err)
	}

	rop, err := f.db.RestoreDatabase(ctx, &databasepb.RestoreDatabaseRequest{Parent: f.instance, DatabaseId: "restored",
		Source: &databasepb.RestoreDatabaseRequest_Backup{Backup: f.instance + "/backups/b1-copy"}})
	if err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	rdb, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("RestoreDatabase's operation: %v", err)
	}
	restored := f.instance + "/databases/restored"
	if rdb.GetName() != restored || rdb.GetRestoreInfo().GetBackupInfo().GetSourceDatabase() != f.database {
		t.Errorf("the restored database = %v", rdb)
	}
	if rows := spannerRows(t, ctx, f.addr, restored); strings.Join(rows, ";") != strings.Join(want, ";") {
		t.Errorf("the restored rows = %v, want %v (the rows at the backup's version time)", rows, want)
	}
	if gd, err := f.db.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: restored}); err != nil || gd.GetRestoreInfo().GetSourceType() != databasepb.RestoreSourceType_BACKUP {
		t.Errorf("GetDatabase of the restored database = %v, %v", gd, err)
	}
	found := false
	dit := f.db.ListDatabaseOperations(ctx, &databasepb.ListDatabaseOperationsRequest{Parent: f.instance,
		Filter: `(metadata.@type:type.googleapis.com/google.spanner.admin.database.v1.RestoreDatabaseMetadata)`})
	for {
		o, err := dit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListDatabaseOperations: %v", err)
		}
		found = found || strings.HasPrefix(o.GetName(), restored+"/operations/")
	}
	if !found {
		t.Error("ListDatabaseOperations does not list the restore")
	}

	pol, err := f.db.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: backup, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/spanner.backupAdmin", Members: []string{"user:a@example.com"}}}}})
	if err != nil || len(pol.GetBindings()) != 1 {
		t.Errorf("SetIamPolicy on the backup = %v, %v", pol, err)
	}
	for _, name := range []string{backup, f.instance + "/backups/b1-copy"} {
		if err := f.db.DeleteBackup(ctx, &databasepb.DeleteBackupRequest{Name: name}); err != nil {
			t.Fatalf("DeleteBackup(%s): %v", name, err)
		}
	}
	if _, err := f.db.GetBackup(ctx, &databasepb.GetBackupRequest{Name: backup}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackup after DeleteBackup = %v, want NotFound", err)
	}
	if err := f.db.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: restored}); err != nil {
		t.Errorf("DropDatabase of the restored database: %v", err)
	}
}

// TestSpannerBackupSchedules (#1498): a schedule of full daily backups is
// created, got, listed, changed (its cron and retention) and deleted; an
// incremental schedule is refused UNIMPLEMENTED (the emulator has no
// Enterprise edition), a missing retention INVALID_ARGUMENT.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/GetBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/ListBackupSchedules, google.spanner.admin.database.v1.DatabaseAdmin/UpdateBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/DeleteBackupSchedule
func TestSpannerBackupSchedules(t *testing.T) {
	f := newSpannerAdmin(t, "cb-sched", "scheddb")
	ctx := f.ctx
	name := f.database + "/backupSchedules/daily"
	sched := func(text string, keep time.Duration) *databasepb.BackupSchedule {
		return &databasepb.BackupSchedule{
			Spec:              &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: text}}},
			RetentionDuration: durationpb.New(keep),
			BackupTypeSpec:    &databasepb.BackupSchedule_FullBackupSpec{FullBackupSpec: &databasepb.FullBackupSpec{}},
		}
	}
	if _, err := f.db.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: f.database, BackupScheduleId: "nokeep",
		BackupSchedule: &databasepb.BackupSchedule{Spec: sched("0 2 * * *", 0).Spec}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a schedule without retention = %v, want InvalidArgument", err)
	}
	inc := sched("0 2 * * *", 24*time.Hour)
	inc.BackupTypeSpec = &databasepb.BackupSchedule_IncrementalBackupSpec{IncrementalBackupSpec: &databasepb.IncrementalBackupSpec{}}
	if _, err := f.db.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: f.database, BackupScheduleId: "inc", BackupSchedule: inc}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an incremental schedule = %v, want Unimplemented", err)
	}
	created, err := f.db.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: f.database, BackupScheduleId: "daily",
		BackupSchedule: sched("0 2 * * *", 7*24*time.Hour)})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	if created.GetName() != name || created.GetSpec().GetCronSpec().GetTimeZone() != "UTC" || created.GetUpdateTime() == nil {
		t.Errorf("the created schedule = %v", created)
	}
	if g, err := f.db.GetBackupSchedule(ctx, &databasepb.GetBackupScheduleRequest{Name: name}); err != nil || g.GetSpec().GetCronSpec().GetText() != "0 2 * * *" {
		t.Errorf("GetBackupSchedule = %v, %v", g, err)
	}
	it := f.db.ListBackupSchedules(ctx, &databasepb.ListBackupSchedulesRequest{Parent: f.database})
	if s, err := it.Next(); err != nil || s.GetName() != name {
		t.Errorf("ListBackupSchedules = %v, %v", s, err)
	}
	next := sched("30 3 * * *", 14*24*time.Hour)
	next.Name = name
	u, err := f.db.UpdateBackupSchedule(ctx, &databasepb.UpdateBackupScheduleRequest{BackupSchedule: next,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec", "retention_duration"}}})
	if err != nil || u.GetSpec().GetCronSpec().GetText() != "30 3 * * *" || u.GetRetentionDuration().AsDuration() != 14*24*time.Hour {
		t.Errorf("UpdateBackupSchedule = %v, %v", u, err)
	}
	if err := f.db.DeleteBackupSchedule(ctx, &databasepb.DeleteBackupScheduleRequest{Name: name}); err != nil {
		t.Fatalf("DeleteBackupSchedule: %v", err)
	}
	if _, err := f.db.GetBackupSchedule(ctx, &databasepb.GetBackupScheduleRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackupSchedule after DeleteBackupSchedule = %v, want NotFound", err)
	}
}

// TestSpannerInstanceConfigs (#1498): the project lists Google's managed
// configurations (regional-us-central1, nam3) beside the emulator's; a
// custom configuration is made from regional-us-central1 with a read-only
// replica added, renamed, and found in ListInstanceConfigOperations; an
// instance created in it keeps DeleteInstanceConfig FAILED_PRECONDITION
// until MoveInstance moves it to regional-us-east1; then the configuration
// is deleted. A custom replica that is not read-only is INVALID_ARGUMENT.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/ListInstanceConfigOperations, google.spanner.admin.instance.v1.InstanceAdmin/MoveInstance, google.spanner.admin.instance.v1.InstanceAdmin/ListInstanceConfigs, google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig
func TestSpannerInstanceConfigs(t *testing.T) {
	f := newSpannerAdmin(t, "cb-configs", "configsdb")
	ctx := f.ctx
	project := "projects/" + f.project
	names := map[string]bool{}
	it := f.inst.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: project})
	for {
		c, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstanceConfigs: %v", err)
		}
		names[c.GetName()] = true
	}
	for _, id := range []string{"emulator-config", "regional-us-central1", "nam3"} {
		if !names[project+"/instanceConfigs/"+id] {
			t.Errorf("ListInstanceConfigs does not list %s", id)
		}
	}
	base, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: project + "/instanceConfigs/regional-us-central1"})
	if err != nil || base.GetConfigType() != instancepb.InstanceConfig_GOOGLE_MANAGED || len(base.GetReplicas()) != 3 {
		t.Fatalf("GetInstanceConfig(regional-us-central1) = %v, %v", base, err)
	}

	custom := project + "/instanceConfigs/custom-cb-test"
	replicas := append(append([]*instancepb.ReplicaInfo{}, base.GetReplicas()...),
		&instancepb.ReplicaInfo{Location: "us-east1", Type: instancepb.ReplicaInfo_READ_WRITE})
	if _, err := f.inst.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{Parent: project, InstanceConfigId: "custom-cb-bad",
		InstanceConfig: &instancepb.InstanceConfig{Name: project + "/instanceConfigs/custom-cb-bad", DisplayName: "bad", BaseConfig: base.GetName(), Replicas: replicas}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a custom configuration adding a read-write replica = %v, want InvalidArgument", err)
	}
	replicas[len(replicas)-1].Type = instancepb.ReplicaInfo_READ_ONLY
	op, err := f.inst.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{Parent: project, InstanceConfigId: "custom-cb-test",
		InstanceConfig: &instancepb.InstanceConfig{Name: custom, DisplayName: "CB custom", BaseConfig: base.GetName(), Replicas: replicas}})
	if err != nil {
		t.Fatalf("CreateInstanceConfig: %v", err)
	}
	c, err := op.Wait(ctx)
	if err != nil || c.GetConfigType() != instancepb.InstanceConfig_USER_MANAGED || len(c.GetReplicas()) != 4 {
		t.Fatalf("CreateInstanceConfig's operation = %v, %v", c, err)
	}
	uop, err := f.inst.UpdateInstanceConfig(ctx, &instancepb.UpdateInstanceConfigRequest{
		InstanceConfig: &instancepb.InstanceConfig{Name: custom, DisplayName: "CB renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateInstanceConfig: %v", err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetDisplayName() != "CB renamed" {
		t.Fatalf("UpdateInstanceConfig's operation = %v, %v", u, err)
	}
	if g, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: custom}); err != nil || g.GetDisplayName() != "CB renamed" {
		t.Errorf("GetInstanceConfig(custom) = %v, %v", g, err)
	}
	ops := 0
	oit := f.inst.ListInstanceConfigOperations(ctx, &instancepb.ListInstanceConfigOperationsRequest{Parent: project})
	for {
		o, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstanceConfigOperations: %v", err)
		}
		if strings.HasPrefix(o.GetName(), custom+"/operations/") {
			ops++
		}
	}
	if ops != 2 {
		t.Errorf("ListInstanceConfigOperations lists %d operations of %s, want 2 (create, update)", ops, custom)
	}

	inCustom := project + "/instances/cb-in-custom"
	iop, err := f.inst.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: project, InstanceId: "cb-in-custom",
		Instance: &instancepb.Instance{Config: custom, DisplayName: "in custom", NodeCount: 1}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateInstance in the custom configuration: %v", err)
	}
	if err == nil {
		if _, err := iop.Wait(ctx); err != nil {
			t.Fatalf("CreateInstance in the custom configuration: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = f.inst.DeleteInstance(context.WithoutCancel(ctx), &instancepb.DeleteInstanceRequest{Name: inCustom})
	})
	if err := f.inst.DeleteInstanceConfig(ctx, &instancepb.DeleteInstanceConfigRequest{Name: custom}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstanceConfig of a configuration in use = %v, want FailedPrecondition", err)
	}
	east := project + "/instanceConfigs/regional-us-east1"
	mop, err := f.inst.MoveInstance(ctx, &instancepb.MoveInstanceRequest{Name: inCustom, TargetConfig: east})
	if err != nil {
		t.Fatalf("MoveInstance: %v", err)
	}
	if _, err := mop.Wait(ctx); err != nil {
		t.Fatalf("MoveInstance's operation: %v", err)
	}
	if in, err := f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: inCustom}); err != nil || in.GetConfig() != east {
		t.Errorf("GetInstance after MoveInstance = %v, %v, want config %s", in, err, east)
	}
	if err := f.inst.DeleteInstanceConfig(ctx, &instancepb.DeleteInstanceConfigRequest{Name: custom}); err != nil {
		t.Fatalf("DeleteInstanceConfig: %v", err)
	}
	if _, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: custom}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstanceConfig after DeleteInstanceConfig = %v, want NotFound", err)
	}
}

// TestSpannerUpdateInstancePartition (#1498): a partition's display name
// and node count are changed, and GetInstancePartition returns them;
// autoscaling is refused UNIMPLEMENTED.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstancePartition
func TestSpannerUpdateInstancePartition(t *testing.T) {
	f := newSpannerAdmin(t, "cb-upart", "upartdb")
	ctx := f.ctx
	name := f.instance + "/instancePartitions/p1"
	cop, err := f.inst.CreateInstancePartition(ctx, &instancepb.CreateInstancePartitionRequest{Parent: f.instance, InstancePartitionId: "p1",
		InstancePartition: &instancepb.InstancePartition{Name: name, Config: f.config(), DisplayName: "partition one",
			ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: 1}}})
	if err != nil {
		t.Fatalf("CreateInstancePartition: %v", err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatalf("CreateInstancePartition: %v", err)
	}
	t.Cleanup(func() {
		_ = f.inst.DeleteInstancePartition(context.WithoutCancel(ctx), &instancepb.DeleteInstancePartitionRequest{Name: name})
	})
	op, err := f.inst.UpdateInstancePartition(ctx, &instancepb.UpdateInstancePartitionRequest{
		InstancePartition: &instancepb.InstancePartition{Name: name, DisplayName: "partition renamed",
			ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: 2}},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "node_count"}}})
	if err != nil {
		t.Fatalf("UpdateInstancePartition: %v", err)
	}
	if p, err := op.Wait(ctx); err != nil || p.GetDisplayName() != "partition renamed" || p.GetNodeCount() != 2 {
		t.Fatalf("UpdateInstancePartition's operation = %v, %v", p, err)
	}
	if p, err := f.inst.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: name}); err != nil || p.GetDisplayName() != "partition renamed" || p.GetNodeCount() != 2 {
		t.Errorf("GetInstancePartition after the update = %v, %v", p, err)
	}
	_, err = f.inst.UpdateInstancePartition(ctx, &instancepb.UpdateInstancePartitionRequest{
		InstancePartition: &instancepb.InstancePartition{Name: name, AutoscalingConfig: &instancepb.AutoscalingConfig{}},
		FieldMask:         &fieldmaskpb.FieldMask{Paths: []string{"autoscaling_config"}}})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("autoscaling a partition = %v, want Unimplemented", err)
	}
}

// TestSpannerDatabaseRoles (#1498): CREATE ROLE and a GRANT, which the
// emulator accepts and forgets, are kept: GetDatabaseDdl returns them,
// ListDatabaseRoles lists the role with Google's system roles, and the
// role's IAM policy is stored; DROP ROLE removes it.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/ListDatabaseRoles
func TestSpannerDatabaseRoles(t *testing.T) {
	f := newSpannerAdmin(t, "cb-roles", "rolesdb")
	ctx := f.ctx
	ddl := func(stmts ...string) {
		t.Helper()
		op, err := f.db.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: f.database, Statements: stmts})
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("UpdateDatabaseDdl(%v): %v", stmts, err)
		}
	}
	roles := func() map[string]bool {
		out := map[string]bool{}
		it := f.db.ListDatabaseRoles(ctx, &databasepb.ListDatabaseRolesRequest{Parent: f.database})
		for {
			r, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("ListDatabaseRoles: %v", err)
			}
			out[strings.TrimPrefix(r.GetName(), f.database+"/databaseRoles/")] = true
		}
	}
	ddl("CREATE ROLE analyst", "GRANT SELECT ON TABLE Items TO ROLE analyst")
	if r := roles(); !r["analyst"] || !r["public"] || !r["spanner_info_reader"] {
		t.Errorf("ListDatabaseRoles = %v, want analyst and the system roles", r)
	}
	got, err := f.db.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: f.database})
	if err != nil || !strings.Contains(strings.Join(got.GetStatements(), "\n"), "CREATE ROLE analyst") {
		t.Errorf("GetDatabaseDdl = %v, %v, want the CREATE ROLE", got.GetStatements(), err)
	}
	role := f.database + "/databaseRoles/analyst"
	if _, err := f.db.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: role, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/spanner.databaseRoleUser", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Errorf("SetIamPolicy on the role: %v", err)
	}
	ddl("REVOKE SELECT ON TABLE Items FROM ROLE analyst", "DROP ROLE analyst")
	if r := roles(); r["analyst"] {
		t.Errorf("ListDatabaseRoles after DROP ROLE = %v", r)
	}
}
