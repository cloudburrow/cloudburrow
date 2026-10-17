//go:build compat

package compat

import (
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Exports, imports, bulk deletes, backups, restores and clones (#1500),
// which the Firestore pod's call front serves from the emulator's own
// documents API, writing exports to the instance's Cloud Storage.

// envFirestoreStorage is the instance's Cloud Storage, which Firestore's
// exports go to (#1500): the emulators shard exports it under this name, so
// the Cloud Storage suite stays in the storage shard.
const envFirestoreStorage = "CLOUDBURROW_TEST_FIRESTORE_STORAGE"

// firestoreStorage is an official Cloud Storage client for Firestore's
// exports.
func firestoreStorage(t *testing.T, h *Harness) *storage.Client {
	t.Helper()
	endpoint := h.Endpoint(envFirestoreStorage)
	t.Setenv("STORAGE_EMULATOR_HOST", endpoint)
	c, err := storage.NewClient(h.Context(), option.WithoutAuthentication(), option.WithEndpoint(endpoint+"/storage/v1/"))
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// seedFirestore writes documents in two collections and a subcollection.
func seedFirestore(t *testing.T, fs *firestore.Client) {
	t.Helper()
	ctx := t.Context()
	for path, v := range map[string]int{"cities/sf": 1, "cities/la": 2, "cities/sf/landmarks/bridge": 3, "users/ada": 4} {
		if _, err := fs.Doc(path).Set(ctx, map[string]any{"v": v, "path": path}); err != nil {
			t.Fatalf("Set %s: %v", path, err)
		}
	}
}

// docPaths is every document of the client's database, by path.
func docPaths(t *testing.T, fs *firestore.Client) []string {
	t.Helper()
	ctx := t.Context()
	var out []string
	var walk func(cols *firestore.CollectionIterator)
	walk = func(cols *firestore.CollectionIterator) {
		for {
			col, err := cols.Next()
			if err == iterator.Done {
				return
			}
			if err != nil {
				t.Fatalf("collections: %v", err)
			}
			refs, err := col.DocumentRefs(ctx).GetAll()
			if err != nil {
				t.Fatalf("documents of %s: %v", col.Path, err)
			}
			for _, r := range refs {
				if s, err := r.Get(ctx); err == nil && s.Exists() {
					out = append(out, strings.SplitN(r.Path, "/documents/", 2)[1])
				}
				walk(r.Collections(ctx))
			}
		}
	}
	walk(fs.Collections(ctx))
	sort.Strings(out)
	return out
}

// TestFirestoreExportImport exports `(default)` to a bucket of the
// instance's Cloud Storage in the managed export layout, bulk deletes one
// collection group and then every document, and imports the export back,
// one collection group and then whole, through the official clients.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/ExportDocuments, google.firestore.admin.v1.FirestoreAdmin/ImportDocuments, google.firestore.admin.v1.FirestoreAdmin/BulkDeleteDocuments
func TestFirestoreExportImport(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	sc := firestoreStorage(t, h)
	fs := firestoreClient(t, h)
	ctx := h.Context()
	db := "projects/" + h.Project() + "/databases/(default)"
	bh := bucket(t, h, sc)
	seedFirestore(t, fs)
	all := []string{"cities/la", "cities/sf", "cities/sf/landmarks/bridge", "users/ada"}

	prefix := "gs://" + bh.BucketName() + "/nightly"
	op, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: db, OutputUriPrefix: prefix})
	if err != nil {
		t.Fatalf("ExportDocuments: %v", err)
	}
	resp, err := op.Wait(ctx)
	if err != nil || resp.GetOutputUriPrefix() != prefix {
		t.Fatalf("export = %v, %v, want output_uri_prefix %s", resp, err, prefix)
	}
	if md, _ := op.Metadata(); md.GetOperationState() != adminpb.OperationState_SUCCESSFUL || md.GetProgressDocuments().GetCompletedWork() != 4 {
		t.Errorf("export metadata = %v, want SUCCESSFUL with 4 documents", md)
	}
	var objects []string
	it := bh.Objects(ctx, &storage.Query{Prefix: "nightly/"})
	for {
		o, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("listing the export: %v", err)
		}
		objects = append(objects, o.Name)
	}
	sort.Strings(objects)
	want := []string{"nightly/all_namespaces/all_kinds/all_kinds.export_metadata", "nightly/all_namespaces/all_kinds/output-0", "nightly/nightly.overall_export_metadata"}
	if strings.Join(objects, ",") != strings.Join(want, ",") {
		t.Errorf("export objects = %v, want %v", objects, want)
	}

	bd, err := c.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: db, CollectionIds: []string{"cities"}})
	if err != nil {
		t.Fatalf("BulkDeleteDocuments: %v", err)
	}
	if _, err := bd.Wait(ctx); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if got := docPaths(t, fs); strings.Join(got, ",") != "cities/sf/landmarks/bridge,users/ada" {
		t.Errorf("after deleting the cities collection group: %v", got)
	}
	if bd, err = c.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: db}); err == nil {
		_, err = bd.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("bulk delete everything: %v", err)
	}
	if got := docPaths(t, fs); len(got) != 0 {
		t.Errorf("after deleting everything: %v", got)
	}

	imp, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: db, InputUriPrefix: prefix, CollectionIds: []string{"users"}})
	if err == nil {
		err = imp.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("import users: %v", err)
	}
	if got := docPaths(t, fs); strings.Join(got, ",") != "users/ada" {
		t.Errorf("after importing users: %v", got)
	}
	if imp, err = c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: db, InputUriPrefix: prefix}); err == nil {
		err = imp.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := docPaths(t, fs); strings.Join(got, ",") != strings.Join(all, ",") {
		t.Errorf("after importing everything: %v, want %v", got, all)
	}
	s, err := fs.Doc("cities/sf/landmarks/bridge").Get(ctx)
	if err != nil || s.Data()["v"] != int64(3) {
		t.Errorf("imported bridge = %v, %v", s.Data(), err)
	}

	if _, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: db, InputUriPrefix: "gs://" + bh.BucketName() + "/none"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("import of no export: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: db, OutputUriPrefix: "gs://" + h.Project() + "-no-such-bucket"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("export to a missing bucket: %v, want FAILED_PRECONDITION", err)
	}
}

// TestFirestoreBackupRestoreClone: a daily backup schedule takes its first
// backup of `(default)`, which is listed, got, restored into a new
// database and deleted; the database is cloned at the current minute, and
// a clone from an hour back is refused UNIMPLEMENTED (the emulator keeps no
// earlier versions).
//
// covers: google.firestore.admin.v1.FirestoreAdmin/ListBackups, google.firestore.admin.v1.FirestoreAdmin/GetBackup, google.firestore.admin.v1.FirestoreAdmin/DeleteBackup, google.firestore.admin.v1.FirestoreAdmin/RestoreDatabase, google.firestore.admin.v1.FirestoreAdmin/CloneDatabase
func TestFirestoreBackupRestoreClone(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	fs := firestoreClient(t, h)
	ctx := h.Context()
	project := "projects/" + h.Project()
	db := project + "/databases/(default)"
	seedFirestore(t, fs)

	s, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: s.Name}) })
	list, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: project + "/locations/-"})
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list.Backups) != 1 || list.Backups[0].State != adminpb.Backup_READY || list.Backups[0].Database != db ||
		list.Backups[0].GetStats().GetDocumentCount() != 4 {
		t.Fatalf("backups = %v, want one READY backup of %s with 4 documents", list.Backups, db)
	}
	b, err := c.GetBackup(ctx, &adminpb.GetBackupRequest{Name: list.Backups[0].Name})
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}

	// A write after the backup is not in the restore.
	if _, err := fs.Doc("cities/ny").Set(ctx, map[string]any{"v": 5}); err != nil {
		t.Fatal(err)
	}
	rop, err := c.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: project, DatabaseId: "restored", Backup: b.Name})
	if err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	rdb, err := rop.Wait(ctx)
	if err != nil || rdb.GetName() != project+"/databases/restored" {
		t.Fatalf("restored = %v, %v", rdb, err)
	}
	restored, err := firestore.NewClientWithDatabase(ctx, h.Project(), "restored")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if got := docPaths(t, restored); strings.Join(got, ",") != "cities/la,cities/sf,cities/sf/landmarks/bridge,users/ada" {
		t.Errorf("restored documents = %v", got)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	cop, err := c.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: project, DatabaseId: "cloned",
		PitrSnapshot: &adminpb.PitrSnapshot{Database: db, SnapshotTime: timestamppb.New(now)}})
	if err != nil {
		t.Fatalf("CloneDatabase: %v", err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cloned, err := firestore.NewClientWithDatabase(ctx, h.Project(), "cloned")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cloned.Close() })
	if got := docPaths(t, cloned); len(got) != 5 {
		t.Errorf("cloned documents = %v, want 5", got)
	}
	_, err = c.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: project, DatabaseId: "older",
		PitrSnapshot: &adminpb.PitrSnapshot{Database: db, SnapshotTime: timestamppb.New(now.Add(-time.Hour))}})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("clone an hour back: %v, want UNIMPLEMENTED", err)
	}

	if err := c.DeleteBackup(ctx, &adminpb.DeleteBackupRequest{Name: b.Name}); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if _, err := c.GetBackup(ctx, &adminpb.GetBackupRequest{Name: b.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackup after delete: %v, want NOT_FOUND", err)
	}
}
