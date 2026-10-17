//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGcloudSpannerBackups (#1498), in the emulators shard: gcloud spanner,
// configured by `cloudburrow gcloud-setup` alone (its spanner override, the
// admin REST API the call front answers), creates a backup of a database
// the official client made and waits for it, lists, describes and extends
// it, restores it to a new database, lists the instance configurations
// (Google's regional ones among them), and deletes the backup; the official
// clients read each change back.
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateBackup, google.spanner.admin.database.v1.DatabaseAdmin/ListBackups, google.spanner.admin.database.v1.DatabaseAdmin/GetBackup, google.spanner.admin.database.v1.DatabaseAdmin/UpdateBackup, google.spanner.admin.database.v1.DatabaseAdmin/RestoreDatabase, google.spanner.admin.database.v1.DatabaseAdmin/DeleteBackup, google.spanner.admin.instance.v1.InstanceAdmin/ListInstanceConfigs, google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig
func TestGcloudSpannerBackups(t *testing.T) {
	f := newSpannerAdmin(t, "gc-backups", "gcbackdb") // skips without CLOUDBURROW_TEST_SPANNER
	New(t).Endpoint(EnvSpannerREST)
	gc := gcloudAdminSession(t, f.project, "spanner")
	id := strings.TrimPrefix(f.instance, "projects/"+f.project+"/instances/")
	backup := f.instance + "/backups/gcb"

	gc("spanner", "backups", "create", "gcb", "--instance", id, "--database", "gcbackdb", "--retention-period", "2d")
	b, err := f.db.GetBackup(f.ctx, &databasepb.GetBackupRequest{Name: backup})
	if err != nil || b.GetState() != databasepb.Backup_READY || b.GetDatabase() != f.database {
		t.Fatalf("the backup gcloud created = %v, %v", b, err)
	}
	if got := gc("spanner", "backups", "list", "--instance", id, "--format=value(name)"); !strings.Contains(got, "gcb") {
		t.Errorf("backups list does not show gcb:\n%s", got)
	}
	if got := gc("spanner", "backups", "describe", "gcb", "--instance", id, "--format=value(state)"); !strings.Contains(got, "READY") {
		t.Errorf("backups describe = %q", got)
	}
	gc("spanner", "backups", "update-metadata", "gcb", "--instance", id, "--retention-period", "5d")
	if u, err := f.db.GetBackup(f.ctx, &databasepb.GetBackupRequest{Name: backup}); err != nil || !u.GetExpireTime().AsTime().After(b.GetExpireTime().AsTime().Add(48*time.Hour)) {
		t.Errorf("the backup after update-metadata = %v, %v", u, err)
	}
	gc("spanner", "databases", "restore", "--source-backup", "gcb", "--source-instance", id,
		"--destination-database", "gcrestored", "--destination-instance", id)
	restored := f.instance + "/databases/gcrestored"
	if d, err := f.db.GetDatabase(f.ctx, &databasepb.GetDatabaseRequest{Name: restored}); err != nil || d.GetRestoreInfo().GetBackupInfo().GetBackup() != backup {
		t.Errorf("the database gcloud restored = %v, %v", d, err)
	}
	if got := gc("spanner", "instance-configs", "list", "--format=value(name)"); !strings.Contains(got, "regional-us-central1") || !strings.Contains(got, "emulator-config") {
		t.Errorf("instance-configs list = %q, want Google's regional configurations and the emulator's", got)
	}
	if got := gc("spanner", "instance-configs", "describe", "nam3", "--format=value(configType)"); !strings.Contains(got, "GOOGLE_MANAGED") {
		t.Errorf("instance-configs describe nam3 = %q", got)
	}
	gc("spanner", "backups", "delete", "gcb", "--instance", id)
	if _, err := f.db.GetBackup(f.ctx, &databasepb.GetBackupRequest{Name: backup}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackup after gcloud's delete = %v, want NotFound", err)
	}
	_ = f.db.DropDatabase(f.ctx, &databasepb.DropDatabaseRequest{Database: restored})
}

// TestTerraformSpannerBackupScheduleAndInstanceConfig (#1498): a
// google_spanner_instance_config (custom, from regional-us-central1) and a
// google_spanner_backup_schedule apply through `cloudburrow terraform` with
// egress blocked, plan clean, are read back with the official clients, and
// destroy.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/CreateInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/DeleteInstanceConfig, google.spanner.admin.database.v1.DatabaseAdmin/CreateBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/GetBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/DeleteBackupSchedule
func TestTerraformSpannerBackupScheduleAndInstanceConfig(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvSpanner) == "" || os.Getenv(EnvSpannerREST) == "" {
		t.Skipf("%s and %s must both be set", EnvSpanner, EnvSpannerREST)
	}
	h.Endpoint(EnvSpannerREST)
	f := newSpannerAdmin(t, "tf-sched-fixture", "fixdb") // the clients
	m := newTFModule(t)
	const id, db = "tf-sched", "tfscheddb"
	project := "projects/" + h.Project()
	m.write(fmt.Sprintf(`
resource "google_spanner_instance_config" "c" {
  project      = %[1]q
  name         = "custom-tf-config"
  display_name = "TF custom"
  base_config  = "regional-us-central1"
  replicas {
    location                = "us-central1"
    type                    = "READ_WRITE"
    default_leader_location = true
  }
  replicas {
    location                = "us-east1"
    type                    = "READ_ONLY"
    default_leader_location = false
  }
}
resource "google_spanner_instance" "i" {
  project      = %[1]q
  name         = %[2]q
  config       = "emulator-config"
  display_name = "Schedules"
  num_nodes    = 1
}
resource "google_spanner_database" "d" {
  project             = google_spanner_instance.i.project
  instance            = google_spanner_instance.i.name
  name                = %[3]q
  deletion_protection = false
}
resource "google_spanner_backup_schedule" "s" {
  project            = google_spanner_database.d.project
  instance           = google_spanner_database.d.instance
  database           = google_spanner_database.d.name
  name               = "tf-daily"
  retention_duration = "172800s"
  spec {
    cron_spec {
      text = "0 12 * * *"
    }
  }
  full_backup_spec {}
}
`, h.Project(), id, db))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	c, err := f.inst.GetInstanceConfig(f.ctx, &instancepb.GetInstanceConfigRequest{Name: project + "/instanceConfigs/custom-tf-config"})
	if err != nil || c.GetConfigType() != instancepb.InstanceConfig_USER_MANAGED || len(c.GetReplicas()) < 2 {
		t.Errorf("the configuration Terraform made = %v, %v", c, err)
	}
	sched := project + "/instances/" + id + "/databases/" + db + "/backupSchedules/tf-daily"
	s, err := f.db.GetBackupSchedule(f.ctx, &databasepb.GetBackupScheduleRequest{Name: sched})
	if err != nil || s.GetSpec().GetCronSpec().GetText() != "0 12 * * *" || s.GetRetentionDuration().AsDuration() != 48*time.Hour {
		t.Errorf("the schedule Terraform made = %v, %v", s, err)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := f.db.GetBackupSchedule(f.ctx, &databasepb.GetBackupScheduleRequest{Name: sched}); status.Code(err) != codes.NotFound {
		t.Errorf("the schedule after destroy = %v, want NotFound", err)
	}
	if _, err := f.inst.GetInstanceConfig(f.ctx, &instancepb.GetInstanceConfigRequest{Name: project + "/instanceConfigs/custom-tf-config"}); status.Code(err) != codes.NotFound {
		t.Errorf("the configuration after destroy = %v, want NotFound", err)
	}
}
