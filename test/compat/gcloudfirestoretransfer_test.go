//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestGcloudFirestoreTransfer (#1500), in the emulators shard: gcloud
// firestore, configured by `cloudburrow gcloud-setup` alone, exports
// `(default)` to the instance's Cloud Storage, bulk deletes a collection
// group, imports the export back, lists, describes and deletes the backup a
// schedule took, restores it into a new database, and clones `(default)`;
// the official clients read each result back.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/ExportDocuments, google.firestore.admin.v1.FirestoreAdmin/ImportDocuments, google.firestore.admin.v1.FirestoreAdmin/BulkDeleteDocuments
// covers: google.firestore.admin.v1.FirestoreAdmin/ListBackups, google.firestore.admin.v1.FirestoreAdmin/GetBackup, google.firestore.admin.v1.FirestoreAdmin/DeleteBackup, google.firestore.admin.v1.FirestoreAdmin/RestoreDatabase, google.firestore.admin.v1.FirestoreAdmin/CloneDatabase
func TestGcloudFirestoreTransfer(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)    // asks for CLOUDBURROW_TEST_FIRESTORE
	sc := firestoreStorage(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE_STORAGE
	fs := firestoreClient(t, h)
	gc := gcloudAdminSession(t, h.Project(), "firestore")
	ctx := h.Context()
	project := "projects/" + h.Project()
	db := project + "/databases/(default)"
	bh := bucket(t, h, sc)
	seedFirestore(t, fs)
	all := "cities/la,cities/sf,cities/sf/landmarks/bridge,users/ada"

	prefix := "gs://" + bh.BucketName() + "/gcloud"
	gc("firestore", "export", prefix)
	gc("firestore", "bulk-delete", "--collection-ids", "cities")
	if got := strings.Join(docPaths(t, fs), ","); got != "cities/sf/landmarks/bridge,users/ada" {
		t.Errorf("after gcloud's bulk delete of cities: %s", got)
	}
	gc("firestore", "import", prefix)
	if got := strings.Join(docPaths(t, fs), ","); got != all {
		t.Errorf("after gcloud's import: %s, want %s", got, all)
	}

	s, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: s.Name}) })
	list, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: project + "/locations/-"})
	if err != nil || len(list.GetBackups()) != 1 {
		t.Fatalf("backups = %v, %v, want one", list, err)
	}
	b := list.Backups[0]
	parts := strings.Split(b.Name, "/")
	loc, bid := parts[3], parts[5]
	if got := gc("firestore", "backups", "list", "--format=value(name)"); !strings.Contains(got, b.Name) {
		t.Errorf("backups list does not show %s:\n%s", b.Name, got)
	}
	if got := gc("firestore", "backups", "describe", "--location", loc, "--backup", bid, "--format=value(state)"); strings.TrimSpace(got) != "READY" {
		t.Errorf("backups describe state = %q, want READY", got)
	}
	gc("firestore", "databases", "restore", "--source-backup", b.Name, "--destination-database", "gcloud-restored")
	restored, err := firestore.NewClientWithDatabase(ctx, h.Project(), "gcloud-restored")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if got := strings.Join(docPaths(t, restored), ","); got != all {
		t.Errorf("gcloud's restored database: %s, want %s", got, all)
	}
	gc("firestore", "databases", "clone", "--source-database", db,
		"--snapshot-time", time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339), "--destination-database", "gcloud-cloned")
	cloned, err := firestore.NewClientWithDatabase(ctx, h.Project(), "gcloud-cloned")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cloned.Close() })
	if got := strings.Join(docPaths(t, cloned), ","); got != all {
		t.Errorf("gcloud's cloned database: %s, want %s", got, all)
	}
	gc("firestore", "backups", "delete", "--location", loc, "--backup", bid)
	if l, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: project + "/locations/-"}); err != nil || len(l.GetBackups()) != 0 {
		t.Errorf("backups after gcloud's delete = %v, %v", l, err)
	}
}
