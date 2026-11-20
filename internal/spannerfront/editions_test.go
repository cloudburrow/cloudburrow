package spannerfront_test

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func autoscaling(lo, hi, hp, st int32) *instancepb.AutoscalingConfig {
	return &instancepb.AutoscalingConfig{
		AutoscalingLimits: &instancepb.AutoscalingConfig_AutoscalingLimits{
			MinLimit: &instancepb.AutoscalingConfig_AutoscalingLimits_MinNodes{MinNodes: lo},
			MaxLimit: &instancepb.AutoscalingConfig_AutoscalingLimits_MaxNodes{MaxNodes: hi}},
		AutoscalingTargets: &instancepb.AutoscalingConfig_AutoscalingTargets{HighPriorityCpuUtilizationPercent: hp, StorageUtilizationPercent: st}}
}

// The edition, default backup schedule type and autoscaling configuration
// are kept, defaulted and checked as Google documents them (#1949).
func TestEditionsAndAutoscaling(t *testing.T) {
	_, c := serve(t)
	ctx := context.Background()
	create := func(id string, in *instancepb.Instance) (*instancepb.Instance, error) {
		in.Config = "projects/p/instanceConfigs/regional-us-central1"
		in.DisplayName = "instance " + id
		op, err := c.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/p", InstanceId: id, Instance: in})
		if err != nil {
			return nil, err
		}
		got := &instancepb.Instance{}
		if err := op.GetResponse().UnmarshalTo(got); err != nil {
			t.Fatal(err)
		}
		return got, nil
	}
	got, err := create("std", &instancepb.Instance{NodeCount: 1})
	if err != nil || got.GetEdition() != instancepb.Instance_STANDARD || got.GetDefaultBackupScheduleType() != instancepb.Instance_AUTOMATIC {
		t.Fatalf("a regional instance with no edition = %v, %v; want STANDARD, AUTOMATIC", got, err)
	}
	if _, err := create("std2", &instancepb.Instance{Edition: instancepb.Instance_STANDARD, AutoscalingConfig: autoscaling(1, 5, 65, 90)}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("autoscaling a Standard instance = %v, want FailedPrecondition", err)
	}
	if _, err := c.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/p", InstanceId: "multi",
		Instance: &instancepb.Instance{Config: "projects/p/instanceConfigs/nam3", Edition: instancepb.Instance_ENTERPRISE, NodeCount: 1}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a multi-region Enterprise instance = %v, want FailedPrecondition", err)
	}
	for _, bad := range []*instancepb.AutoscalingConfig{
		autoscaling(1, 11, 65, 90), autoscaling(0, 2, 65, 90), autoscaling(3, 2, 65, 90),
		autoscaling(1, 5, 95, 90), autoscaling(1, 5, 65, 100), autoscaling(1, 5, 0, 90), {},
	} {
		if _, err := create("bad", &instancepb.Instance{Edition: instancepb.Instance_ENTERPRISE, AutoscalingConfig: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("autoscaling %v = %v, want InvalidArgument", bad, err)
		}
	}
	got, err = create("auto", &instancepb.Instance{Edition: instancepb.Instance_ENTERPRISE, DefaultBackupScheduleType: instancepb.Instance_NONE,
		AutoscalingConfig: autoscaling(2, 8, 65, 90)})
	if err != nil || got.GetNodeCount() != 2 || got.GetProcessingUnits() != 2000 || got.GetAutoscalingConfig().GetAutoscalingLimits().GetMaxNodes() != 8 ||
		got.GetDefaultBackupScheduleType() != instancepb.Instance_NONE {
		t.Fatalf("an autoscaled instance = %v, %v; want 2 nodes, its minimum", got, err)
	}
	name := "projects/p/instances/auto"
	upd := func(in *instancepb.Instance, paths ...string) (*instancepb.Instance, error) {
		in.Name = name
		if _, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: in, FieldMask: &fieldmaskpb.FieldMask{Paths: paths}}); err != nil {
			return nil, err
		}
		return c.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: name})
	}
	if _, err := upd(&instancepb.Instance{NodeCount: 5}, "node_count"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("node_count while autoscaled = %v, want InvalidArgument", err)
	}
	if _, err := upd(&instancepb.Instance{Edition: instancepb.Instance_STANDARD}, "edition"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("downgrading an autoscaled instance = %v, want FailedPrecondition", err)
	}
	if got, err := upd(&instancepb.Instance{AutoscalingConfig: autoscaling(3, 9, 50, 80)}, "autoscaling_config"); err != nil || got.GetNodeCount() != 3 {
		t.Errorf("new limits = %v, %v; want 3 nodes", got, err)
	}
	// Terraform names the configuration's fields.
	if got, err := upd(&instancepb.Instance{AutoscalingConfig: autoscaling(1, 7, 10, 10)}, "autoscaling_config.autoscaling_limits.max_nodes"); err != nil ||
		got.GetAutoscalingConfig().GetAutoscalingLimits().GetMaxNodes() != 7 || got.GetAutoscalingConfig().GetAutoscalingLimits().GetMinNodes() != 3 ||
		got.GetAutoscalingConfig().GetAutoscalingTargets().GetHighPriorityCpuUtilizationPercent() != 50 {
		t.Errorf("a new maximum alone = %v, %v; want 3 to 7 nodes, the targets kept", got, err)
	}
	if got, err := upd(&instancepb.Instance{NodeCount: 4}, "autoscaling_config", "node_count"); err != nil || got.GetNodeCount() != 4 || got.GetAutoscalingConfig() != nil {
		t.Errorf("back to manual scaling = %v, %v; want 4 nodes", got, err)
	}
	if got, err := upd(&instancepb.Instance{Edition: instancepb.Instance_STANDARD}, "edition"); err != nil || got.GetEdition() != instancepb.Instance_STANDARD {
		t.Errorf("the downgrade once manual = %v, %v", got, err)
	}
	if got, err := upd(&instancepb.Instance{Edition: instancepb.Instance_ENTERPRISE_PLUS, DefaultBackupScheduleType: instancepb.Instance_AUTOMATIC},
		"edition", "default_backup_schedule_type"); err != nil || got.GetEdition() != instancepb.Instance_ENTERPRISE_PLUS || got.GetDefaultBackupScheduleType() != instancepb.Instance_AUTOMATIC {
		t.Errorf("the upgrade = %v, %v", got, err)
	}
}

// An incremental backup schedule needs an Enterprise edition, and a
// backup at most every 4 hours.
func TestIncrementalSchedulesNeedEnterprise(t *testing.T) {
	_, c := serve(t)
	ctx := context.Background()
	dc := databasepb.NewDatabaseAdminClient(lastConn)
	inc := func(cron string) *databasepb.BackupSchedule {
		return &databasepb.BackupSchedule{
			Spec:              &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: cron}}},
			RetentionDuration: durationpb.New(48 * time.Hour),
			BackupTypeSpec:    &databasepb.BackupSchedule_IncrementalBackupSpec{IncrementalBackupSpec: &databasepb.IncrementalBackupSpec{}}}
	}
	db := "projects/p/instances/i/databases/d"
	if _, err := dc.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: db, BackupScheduleId: "inc", BackupSchedule: inc("0 */4 * * *")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an incremental schedule on a legacy (Standard) instance = %v, want FailedPrecondition", err)
	}
	if _, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: "projects/p/instances/i", Edition: instancepb.Instance_ENTERPRISE},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"edition"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: db, BackupScheduleId: "inc", BackupSchedule: inc("0 */2 * * *")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an incremental schedule every 2 hours = %v, want InvalidArgument", err)
	}
	if _, err := dc.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: db, BackupScheduleId: "inc", BackupSchedule: inc("0 */4 * * *")}); err != nil {
		t.Fatalf("an incremental schedule on an Enterprise instance: %v", err)
	}
	if _, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: "projects/p/instances/i", Edition: instancepb.Instance_STANDARD},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"edition"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a downgrade with an incremental schedule = %v, want FailedPrecondition", err)
	}
}
