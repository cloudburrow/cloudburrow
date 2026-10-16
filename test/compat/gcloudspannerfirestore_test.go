//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gcloudAdminSession is a gcloud configured by `cloudburrow gcloud-setup` alone,
// with egress to Google blocked; it fails the test unless the configuration
// has the named override.
func gcloudAdminSession(t *testing.T, project, override string) func(args ...string) string {
	t.Helper()
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), override+" = http://") {
		t.Fatalf("the configuration has no %s override:\n%s", override, conf)
	}
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", project, "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
}

// TestGcloudSpannerInstances (#1461), in the emulators shard: gcloud
// spanner, configured by `cloudburrow gcloud-setup` alone (its spanner
// override, Spanner's admin REST port), lists and describes an instance the
// official client made, and updates its description and node count, which
// the call front answers (the emulator refuses instance updates); the
// official gRPC client reads the update back.
// covers: google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance, google.spanner.admin.instance.v1.InstanceAdmin/GetInstance, google.spanner.admin.instance.v1.InstanceAdmin/ListInstances
func TestGcloudSpannerInstances(t *testing.T) {
	f := newSpannerAdmin(t, "gc-spanner", "gcdb") // skips without CLOUDBURROW_TEST_SPANNER
	New(t).Endpoint(EnvSpannerREST)
	gc := gcloudAdminSession(t, f.project, "spanner")
	id := strings.TrimPrefix(f.instance, "projects/"+f.project+"/instances/")

	if got := gc("spanner", "instances", "list", "--format=value(name)"); !strings.Contains(got, id) {
		t.Errorf("instances list does not show %s:\n%s", id, got)
	}
	gc("spanner", "instances", "update", id, "--description", "updated by gcloud", "--nodes", "2")
	in, err := f.inst.GetInstance(f.ctx, &instancepb.GetInstanceRequest{Name: f.instance})
	if err != nil || in.GetDisplayName() != "updated by gcloud" || in.GetNodeCount() != 2 {
		t.Fatalf("the instance gcloud updated = %v, %v", in, err)
	}
	if got := gc("spanner", "instances", "describe", id, "--format=value(displayName,nodeCount)"); !strings.Contains(got, "updated by gcloud") || !strings.Contains(got, "2") {
		t.Errorf("instances describe = %q", got)
	}
}

// TestGcloudFirestoreBackupSchedules (#1461), in the emulators shard:
// gcloud firestore, configured by `cloudburrow gcloud-setup` alone (its
// firestore override, the Firestore Admin API the front in the Firestore
// pod serves), lists databases, and creates, lists, updates and deletes a
// weekly backup schedule on a database the official admin client made,
// which reads each change back.
// covers: google.firestore.admin.v1.FirestoreAdmin/ListDatabases, google.firestore.admin.v1.FirestoreAdmin/CreateBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/ListBackupSchedules, google.firestore.admin.v1.FirestoreAdmin/UpdateBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/DeleteBackupSchedule
func TestGcloudFirestoreBackupSchedules(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE
	gc := gcloudAdminSession(t, h.Project(), "firestore")
	ctx := h.Context()
	const dbID = "gcloud-schedules"
	dbName := "projects/" + h.Project() + "/databases/" + dbID
	if _, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: dbName}); status.Code(err) == codes.NotFound {
		op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + h.Project(), DatabaseId: dbID,
			Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("CreateDatabase: %v", err)
		}
	}
	t.Cleanup(func() {
		if op, err := c.DeleteDatabase(h.Context(), &adminpb.DeleteDatabaseRequest{Name: dbName}); err == nil {
			_, _ = op.Wait(h.Context())
		}
	})

	if got := gc("firestore", "databases", "list", "--format=value(name)"); !strings.Contains(got, dbName) {
		t.Errorf("databases list does not show %s:\n%s", dbName, got)
	}
	gc("firestore", "backups", "schedules", "create", "--database", dbID, "--recurrence", "weekly", "--day-of-week", "MON", "--retention", "14w")
	l, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: dbName})
	if err != nil || len(l.GetBackupSchedules()) != 1 {
		t.Fatalf("the schedules after gcloud's create = %v, %v", l, err)
	}
	s := l.BackupSchedules[0]
	if s.GetWeeklyRecurrence().GetDay().String() != "MONDAY" || s.GetRetention().AsDuration() != 14*7*24*time.Hour {
		t.Errorf("the schedule gcloud created = %v", s)
	}
	sid := s.GetName()[strings.LastIndex(s.GetName(), "/")+1:]
	if got := gc("firestore", "backups", "schedules", "list", "--database", dbID, "--format=value(name)"); !strings.Contains(got, sid) {
		t.Errorf("schedules list does not show %s:\n%s", sid, got)
	}
	gc("firestore", "backups", "schedules", "update", "--database", dbID, "--backup-schedule", sid, "--retention", "7d")
	g, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: s.GetName()})
	if err != nil || g.GetRetention().AsDuration() != 7*24*time.Hour {
		t.Errorf("the schedule gcloud updated = %v, %v", g, err)
	}
	gc("firestore", "backups", "schedules", "delete", "--database", dbID, "--backup-schedule", sid)
	if _, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: s.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackupSchedule after gcloud's delete: %v, want NotFound", err)
	}
}
