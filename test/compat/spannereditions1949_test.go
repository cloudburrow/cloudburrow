//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func spannerAutoscaling(lo, hi, cpu, storage int32) *instancepb.AutoscalingConfig {
	return &instancepb.AutoscalingConfig{
		AutoscalingLimits: &instancepb.AutoscalingConfig_AutoscalingLimits{
			MinLimit: &instancepb.AutoscalingConfig_AutoscalingLimits_MinNodes{MinNodes: lo},
			MaxLimit: &instancepb.AutoscalingConfig_AutoscalingLimits_MaxNodes{MaxNodes: hi}},
		AutoscalingTargets: &instancepb.AutoscalingConfig_AutoscalingTargets{HighPriorityCpuUtilizationPercent: cpu, StorageUtilizationPercent: storage}}
}

// spannerSchedules lists database's backup schedules by ID.
func spannerSchedules(t *testing.T, ctx context.Context, f *spannerAdminFixture, database string) map[string]*databasepb.BackupSchedule {
	t.Helper()
	out := map[string]*databasepb.BackupSchedule{}
	it := f.db.ListBackupSchedules(ctx, &databasepb.ListBackupSchedulesRequest{Parent: database})
	for {
		s, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("ListBackupSchedules(%s): %v", database, err)
		}
		out[s.GetName()[strings.LastIndex(s.GetName(), "/")+1:]] = s
	}
}

func spannerCreateDB(t *testing.T, ctx context.Context, f *spannerAdminFixture, instance, id string) string {
	t.Helper()
	op, err := f.db.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: instance, CreateStatement: "CREATE DATABASE " + id})
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", id, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase %s: %v", id, err)
	}
	return instance + "/databases/" + id
}

// TestSpannerEditionsAutoscalingAndSchedules (#1949), with the official
// instance and database admin clients: an Enterprise instance is created
// with a managed autoscaler configuration, which is validated as Google
// documents it (limits of 1 node or more, the maximum at most 10 times
// the minimum, CPU targets of 10-90%, a storage target of 10-99%) and
// reported with the instance's capacity at its minimum (no load locally);
// the autoscaler is a FAILED_PRECONDITION on the Standard edition, as is a
// multi-region configuration below Enterprise Plus. A downgrade to
// Standard is FAILED_PRECONDITION while the instance autoscales or has an
// incremental backup schedule, and succeeds once it uses neither. A
// regional instance created with no edition is Standard, with default
// backup schedules AUTOMATIC: its new database gets Google's default
// schedule (a full backup every 24 hours, kept 7 days); with NONE a new
// database gets none. An incremental schedule is created on the
// Enterprise instance and refused on the Standard one, as is a search
// index.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstance, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance, google.spanner.admin.instance.v1.InstanceAdmin/GetInstance, google.spanner.admin.database.v1.DatabaseAdmin/CreateDatabase, google.spanner.admin.database.v1.DatabaseAdmin/ListBackupSchedules, google.spanner.admin.database.v1.DatabaseAdmin/CreateBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/GetBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/UpdateDatabaseDdl
func TestSpannerEditionsAutoscalingAndSchedules(t *testing.T) {
	f := newSpannerAdmin(t, "cb-editions-fixture", "edfixdb")
	ctx := f.ctx
	parent := "projects/" + f.project
	regional := parent + "/instanceConfigs/regional-us-central1"
	create := func(id string, in *instancepb.Instance) (*instancepb.Instance, error) {
		in.DisplayName = "Editions " + id
		if in.Config == "" {
			in.Config = regional
		}
		op, err := f.inst.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
		if err != nil {
			return nil, err
		}
		if _, err := op.Wait(ctx); err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			_ = f.inst.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: parent + "/instances/" + id})
		})
		return f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: parent + "/instances/" + id})
	}
	update := func(name string, in *instancepb.Instance, paths ...string) (*instancepb.Instance, error) {
		in.Name = name
		op, err := f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: in, FieldMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return nil, err
		}
		if _, err := op.Wait(ctx); err != nil {
			return nil, err
		}
		return f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: name})
	}

	for _, bad := range []*instancepb.AutoscalingConfig{spannerAutoscaling(1, 11, 65, 90), spannerAutoscaling(0, 3, 65, 90),
		spannerAutoscaling(2, 1, 65, 90), spannerAutoscaling(1, 3, 95, 90), spannerAutoscaling(1, 3, 65, 5)} {
		if _, err := create("cb-ed-bad", &instancepb.Instance{Edition: instancepb.Instance_ENTERPRISE, AutoscalingConfig: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("autoscaling %v = %v, want InvalidArgument", bad, err)
		}
	}
	if _, err := create("cb-ed-std-auto", &instancepb.Instance{Edition: instancepb.Instance_STANDARD, AutoscalingConfig: spannerAutoscaling(1, 3, 65, 95)}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("autoscaling a Standard instance = %v, want FailedPrecondition", err)
	}
	if _, err := create("cb-ed-multi", &instancepb.Instance{Config: parent + "/instanceConfigs/nam3", Edition: instancepb.Instance_STANDARD, NodeCount: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a Standard instance in nam3 = %v, want FailedPrecondition", err)
	}

	ent, err := create("cb-ed-ent", &instancepb.Instance{Edition: instancepb.Instance_ENTERPRISE, AutoscalingConfig: spannerAutoscaling(2, 6, 65, 95),
		DefaultBackupScheduleType: instancepb.Instance_NONE})
	if err != nil {
		t.Fatalf("CreateInstance of an autoscaled Enterprise instance: %v", err)
	}
	if ent.GetEdition() != instancepb.Instance_ENTERPRISE || ent.GetNodeCount() != 2 || ent.GetProcessingUnits() != 2000 ||
		ent.GetAutoscalingConfig().GetAutoscalingLimits().GetMaxNodes() != 6 || ent.GetAutoscalingConfig().GetAutoscalingTargets().GetStorageUtilizationPercent() != 95 ||
		ent.GetDefaultBackupScheduleType() != instancepb.Instance_NONE {
		t.Fatalf("the Enterprise instance = %v, want ENTERPRISE, autoscaled at its 2-node minimum, NONE", ent)
	}
	entDB := spannerCreateDB(t, ctx, f, ent.GetName(), "entdb")
	if got := spannerSchedules(t, ctx, f, entDB); len(got) != 0 {
		t.Errorf("a new database with NONE has schedules %v, want none", got)
	}
	if _, err := update(ent.GetName(), &instancepb.Instance{NodeCount: 4}, "node_count"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("node_count while autoscaled = %v, want InvalidArgument", err)
	}
	if _, err := update(ent.GetName(), &instancepb.Instance{Edition: instancepb.Instance_STANDARD}, "edition"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("downgrading an autoscaled instance = %v, want FailedPrecondition", err)
	}
	if got, err := update(ent.GetName(), &instancepb.Instance{AutoscalingConfig: spannerAutoscaling(3, 9, 50, 80)}, "autoscaling_config"); err != nil || got.GetNodeCount() != 3 {
		t.Errorf("new autoscaling limits = %v, %v, want 3 nodes", got, err)
	}

	inc := &databasepb.BackupSchedule{
		Spec:              &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: "0 */6 * * *"}}},
		RetentionDuration: durationpb.New(48 * time.Hour),
		BackupTypeSpec:    &databasepb.BackupSchedule_IncrementalBackupSpec{IncrementalBackupSpec: &databasepb.IncrementalBackupSpec{}}}
	if _, err := f.db.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: entDB, BackupScheduleId: "inc", BackupSchedule: inc}); err != nil {
		t.Fatalf("CreateBackupSchedule of an incremental schedule on Enterprise: %v", err)
	}
	if s, err := f.db.GetBackupSchedule(ctx, &databasepb.GetBackupScheduleRequest{Name: entDB + "/backupSchedules/inc"}); err != nil || s.GetIncrementalBackupSpec() == nil {
		t.Errorf("the incremental schedule = %v, %v", s, err)
	}
	if got, err := update(ent.GetName(), &instancepb.Instance{NodeCount: 3}, "autoscaling_config", "node_count"); err != nil || got.GetAutoscalingConfig() != nil || got.GetNodeCount() != 3 {
		t.Errorf("back to manual scaling = %v, %v", got, err)
	}
	if _, err := update(ent.GetName(), &instancepb.Instance{Edition: instancepb.Instance_STANDARD}, "edition"); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(status.Convert(err).Message(), "incremental") {
		t.Errorf("downgrading with an incremental schedule = %v, want FailedPrecondition naming it", err)
	}
	if err := f.db.DeleteBackupSchedule(ctx, &databasepb.DeleteBackupScheduleRequest{Name: entDB + "/backupSchedules/inc"}); err != nil {
		t.Fatal(err)
	}
	if got, err := update(ent.GetName(), &instancepb.Instance{Edition: instancepb.Instance_STANDARD}, "edition"); err != nil || got.GetEdition() != instancepb.Instance_STANDARD {
		t.Errorf("the downgrade once the features are gone = %v, %v", got, err)
	}

	std, err := create("cb-ed-std", &instancepb.Instance{NodeCount: 1})
	if err != nil || std.GetEdition() != instancepb.Instance_STANDARD || std.GetDefaultBackupScheduleType() != instancepb.Instance_AUTOMATIC {
		t.Fatalf("a regional instance with no edition = %v, %v; want STANDARD, AUTOMATIC", std, err)
	}
	stdDB := spannerCreateDB(t, ctx, f, std.GetName(), "stddb")
	def := spannerSchedules(t, ctx, f, stdDB)["default-backup-schedule"]
	if def.GetFullBackupSpec() == nil || def.GetRetentionDuration().AsDuration() != 7*24*time.Hour || def.GetSpec().GetCronSpec().GetTimeZone() != "UTC" {
		t.Errorf("the default backup schedule = %v, want full backups daily, kept 7 days", def)
	}
	if _, err := f.db.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: stdDB, BackupScheduleId: "inc", BackupSchedule: inc}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an incremental schedule on Standard = %v, want FailedPrecondition", err)
	}
	if _, err := f.db.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: stdDB, Statements: []string{
		"CREATE TABLE Docs (Id INT64 NOT NULL, Body STRING(MAX), Tokens TOKENLIST AS (TOKENIZE_FULLTEXT(Body)) HIDDEN) PRIMARY KEY (Id)",
		"CREATE SEARCH INDEX DocsIndex ON Docs(Tokens)"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a search index on Standard = %v, want FailedPrecondition", err)
	}
	if got, err := update(std.GetName(), &instancepb.Instance{DefaultBackupScheduleType: instancepb.Instance_NONE}, "default_backup_schedule_type"); err != nil || got.GetDefaultBackupScheduleType() != instancepb.Instance_NONE {
		t.Errorf("default_backup_schedule_type NONE = %v, %v", got, err)
	}
	if got := spannerSchedules(t, ctx, f, spannerCreateDB(t, ctx, f, std.GetName(), "stddb2")); len(got) != 0 {
		t.Errorf("a database created after NONE has %v, want no schedule", got)
	}
}

// TestGcloudSpannerEditions (#1949): gcloud, configured by `cloudburrow
// gcloud-setup` alone, creates an Enterprise instance with --edition, the
// autoscaling flags and --default-backup-schedule-type=AUTOMATIC, which the
// official client reads back; a database it creates gets the default
// schedule, which backup-schedules list shows; backup-schedules create
// makes an incremental schedule; instances update upgrades the edition to
// Enterprise Plus and sets the default schedule type to NONE.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstance, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance, google.spanner.admin.database.v1.DatabaseAdmin/ListBackupSchedules, google.spanner.admin.database.v1.DatabaseAdmin/CreateBackupSchedule
func TestGcloudSpannerEditions(t *testing.T) {
	f := newSpannerAdmin(t, "gc-editions-fixture", "gcedfix") // skips without CLOUDBURROW_TEST_SPANNER
	New(t).Endpoint(EnvSpannerREST)
	gc := gcloudAdminSession(t, f.project, "spanner")
	const id = "gc-editions"
	name := "projects/" + f.project + "/instances/" + id
	gc("spanner", "instances", "create", id, "--config", "regional-us-central1", "--description", "Editions by gcloud",
		"--edition", "ENTERPRISE", "--default-backup-schedule-type", "AUTOMATIC",
		"--autoscaling-min-nodes", "1", "--autoscaling-max-nodes", "4",
		"--autoscaling-high-priority-cpu-target", "60", "--autoscaling-storage-target", "90")
	t.Cleanup(func() { _ = f.inst.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: name}) })
	in, err := f.inst.GetInstance(f.ctx, &instancepb.GetInstanceRequest{Name: name})
	if err != nil || in.GetEdition() != instancepb.Instance_ENTERPRISE || in.GetAutoscalingConfig().GetAutoscalingLimits().GetMaxNodes() != 4 ||
		in.GetAutoscalingConfig().GetAutoscalingTargets().GetHighPriorityCpuUtilizationPercent() != 60 || in.GetNodeCount() != 1 ||
		in.GetDefaultBackupScheduleType() != instancepb.Instance_AUTOMATIC {
		t.Fatalf("the instance gcloud created = %v, %v", in, err)
	}
	if got := gc("spanner", "instances", "describe", id, "--format=value(edition,autoscalingConfig.autoscalingLimits.maxNodes)"); !strings.Contains(got, "ENTERPRISE") || !strings.Contains(got, "4") {
		t.Errorf("instances describe = %q", got)
	}
	gc("spanner", "databases", "create", "gcdb", "--instance", id)
	if got := gc("spanner", "backup-schedules", "list", "--instance", id, "--database", "gcdb", "--format=value(name)"); !strings.Contains(got, "default-backup-schedule") {
		t.Errorf("backup-schedules list = %q, want the default schedule", got)
	}
	gc("spanner", "backup-schedules", "create", "gcinc", "--instance", id, "--database", "gcdb",
		"--cron", "0 */6 * * *", "--retention-duration", "2d", "--backup-type", "incremental-backup")
	s, err := f.db.GetBackupSchedule(f.ctx, &databasepb.GetBackupScheduleRequest{Name: name + "/databases/gcdb/backupSchedules/gcinc"})
	if err != nil || s.GetIncrementalBackupSpec() == nil || s.GetSpec().GetCronSpec().GetText() != "0 */6 * * *" {
		t.Errorf("the incremental schedule gcloud created = %v, %v", s, err)
	}
	gc("spanner", "instances", "update", id, "--edition", "ENTERPRISE_PLUS", "--default-backup-schedule-type", "NONE")
	if in, err := f.inst.GetInstance(f.ctx, &instancepb.GetInstanceRequest{Name: name}); err != nil || in.GetEdition() != instancepb.Instance_ENTERPRISE_PLUS ||
		in.GetDefaultBackupScheduleType() != instancepb.Instance_NONE {
		t.Errorf("the instance gcloud updated = %v, %v", in, err)
	}
	_ = f.db.DropDatabase(f.ctx, &databasepb.DropDatabaseRequest{Database: name + "/databases/gcdb"})
}

// TestTerraformSpannerInstanceEdition (#1949): a google_spanner_instance
// with edition ENTERPRISE, default_backup_schedule_type NONE and an
// autoscaling_config applies through `cloudburrow terraform` with egress
// blocked, plans clean, and is read back with the official client at its
// minimum capacity; changing its limits and upgrading it to
// ENTERPRISE_PLUS applies and plans clean; it destroys.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstance, google.spanner.admin.instance.v1.InstanceAdmin/GetInstance, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstance
func TestTerraformSpannerInstanceEdition(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvSpanner) == "" || os.Getenv(EnvSpannerREST) == "" {
		t.Skipf("%s and %s must both be set", EnvSpanner, EnvSpannerREST)
	}
	h.Endpoint(EnvSpannerREST)
	f := newSpannerAdmin(t, "tf-ed-fixture", "tfedfix")
	m := newTFModule(t)
	const id = "tf-editions"
	name := "projects/" + h.Project() + "/instances/" + id
	module := func(edition string, max int) string {
		return fmt.Sprintf(`
resource "google_spanner_instance" "i" {
  project                      = %q
  name                         = %q
  config                       = "regional-us-central1"
  display_name                 = "TF editions"
  edition                      = %q
  default_backup_schedule_type = "NONE"
  force_destroy                = true
  autoscaling_config {
    autoscaling_limits {
      min_nodes = 1
      max_nodes = %d
    }
    autoscaling_targets {
      high_priority_cpu_utilization_percent = 65
      storage_utilization_percent           = 95
    }
  }
}
`, h.Project(), id, edition, max)
	}
	m.write(module("ENTERPRISE", 3))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	in, err := f.inst.GetInstance(f.ctx, &instancepb.GetInstanceRequest{Name: name})
	if err != nil || in.GetEdition() != instancepb.Instance_ENTERPRISE || in.GetDefaultBackupScheduleType() != instancepb.Instance_NONE ||
		in.GetAutoscalingConfig().GetAutoscalingLimits().GetMaxNodes() != 3 || in.GetNodeCount() != 1 {
		t.Fatalf("the instance Terraform made = %v, %v", in, err)
	}
	m.planClean()
	m.write(module("ENTERPRISE_PLUS", 5))
	m.apply()
	in, err = f.inst.GetInstance(f.ctx, &instancepb.GetInstanceRequest{Name: name})
	if err != nil || in.GetEdition() != instancepb.Instance_ENTERPRISE_PLUS || in.GetAutoscalingConfig().GetAutoscalingLimits().GetMaxNodes() != 5 {
		t.Errorf("the instance Terraform updated = %v, %v", in, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := f.inst.GetInstance(f.ctx, &instancepb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("the instance after destroy = %v, want NotFound", err)
	}
}

// TestSpannerInstancePartitionNeedsEnterprisePlus (#1974): geo-partitioning
// is an Enterprise Plus feature, so CreateInstancePartition on a Standard
// and on an Enterprise instance is FAILED_PRECONDITION naming it, with no
// partition made; once the instance is upgraded to Enterprise Plus the
// partition is created, and a downgrade while it exists is refused.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance, google.spanner.admin.instance.v1.InstanceAdmin/ListInstancePartitions, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstancePartition
func TestSpannerInstancePartitionNeedsEnterprisePlus(t *testing.T) {
	f := newSpannerAdmin(t, "cb-partedition", "parteddb")
	ctx := f.ctx
	name := f.instance + "/instancePartitions/p1"
	create := func() error {
		op, err := f.inst.CreateInstancePartition(ctx, &instancepb.CreateInstancePartitionRequest{Parent: f.instance, InstancePartitionId: "p1",
			InstancePartition: &instancepb.InstancePartition{Name: name, Config: f.config(), DisplayName: "partition one",
				ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: 1}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	edition := func(e instancepb.Instance_Edition) error {
		op, err := f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: f.instance, Edition: e},
			FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"edition"}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	partitions := func() int {
		n := 0
		it := f.inst.ListInstancePartitions(ctx, &instancepb.ListInstancePartitionsRequest{Parent: f.instance})
		for {
			_, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return n
			}
			if err != nil {
				t.Fatalf("ListInstancePartitions: %v", err)
			}
			n++
		}
	}
	for _, e := range []instancepb.Instance_Edition{instancepb.Instance_STANDARD, instancepb.Instance_ENTERPRISE} {
		if err := edition(e); err != nil {
			t.Fatalf("UpdateInstance to %s: %v", e, err)
		}
		err := create()
		want := "CreateInstancePartition: geo-partitioning requires the ENTERPRISE_PLUS edition or higher; the instance is " + e.String() + " edition: upgrade it with UpdateInstance (edition) first."
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
			t.Errorf("CreateInstancePartition on a %s instance = %v; want FAILED_PRECONDITION %q", e, err, want)
		}
		if n := partitions(); n != 0 {
			t.Fatalf("after the refused create on a %s instance ListInstancePartitions lists %d partitions, want none", e, n)
		}
	}
	if err := edition(instancepb.Instance_ENTERPRISE_PLUS); err != nil {
		t.Fatalf("UpdateInstance to ENTERPRISE_PLUS: %v", err)
	}
	if err := create(); err != nil {
		t.Fatalf("CreateInstancePartition on an Enterprise Plus instance: %v", err)
	}
	t.Cleanup(func() {
		_ = f.inst.DeleteInstancePartition(context.WithoutCancel(ctx), &instancepb.DeleteInstancePartitionRequest{Name: name})
	})
	if n := partitions(); n != 1 {
		t.Errorf("ListInstancePartitions lists %d partitions after the create, want 1", n)
	}
	if err := edition(instancepb.Instance_ENTERPRISE); status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "geo-partitioning") {
		t.Errorf("downgrading to ENTERPRISE with a partition = %v; want FAILED_PRECONDITION naming geo-partitioning", err)
	}
}
