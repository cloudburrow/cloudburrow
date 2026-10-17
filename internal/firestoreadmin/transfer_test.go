package firestoreadmin_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
)

func transferAdmin(t *testing.T) (*admin.FirestoreAdminClient, *fstest.Docs, *fstest.GCS) {
	t.Helper()
	ctx := context.Background()
	fd, docsAddr, gcs, gcsURL := fstest.Serve(t)
	a := firestoreadmin.New(docsAddr)
	a.SetStorage(gcsURL)
	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.GRPCServer().Serve(al) }()
	t.Cleanup(a.GRPCServer().Stop)
	c, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(al.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fd, gcs
}

func TestExportImportBulkDelete(t *testing.T) {
	ctx := context.Background()
	c, fd, gcs := transferAdmin(t)
	src := "projects/p/databases/(default)"
	fd.Seed(src)

	exp, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: src, OutputUriPrefix: "gs://exports/full"})
	if err != nil {
		t.Fatalf("ExportDocuments: %v", err)
	}
	resp, err := exp.Wait(ctx)
	if err != nil || resp.OutputUriPrefix != "gs://exports/full" {
		t.Fatalf("export = %v, %v", resp, err)
	}
	md, _ := exp.Metadata()
	if md.GetProgressDocuments().GetCompletedWork() != 5 || md.GetOperationState() != adminpb.OperationState_SUCCESSFUL {
		t.Errorf("export metadata = %v, want 5 documents, SUCCESSFUL", md)
	}
	for _, want := range []string{"full/full.overall_export_metadata", "full/all_namespaces/all_kinds/output-0",
		"full/all_namespaces/all_kinds/all_kinds.export_metadata"} {
		if _, ok := gcs.Objects[want]; !ok {
			t.Errorf("export wrote no %s", want)
		}
	}

	// A bulk delete of one collection group, then of everything.
	bd, err := c.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: src, CollectionIds: []string{"cities"}})
	if err != nil {
		t.Fatalf("BulkDeleteDocuments: %v", err)
	}
	if _, err := bd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fd.Names(src), ","); got != "cities/sf/landmarks/bridge,orphans/missing/kids/k1,users/ada" {
		t.Errorf("after deleting cities: %s", got)
	}
	if bd, err = c.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: src}); err == nil {
		_, err = bd.Wait(ctx)
	}
	if err != nil || len(fd.Names(src)) != 0 {
		t.Fatalf("bulk delete all: %v, left %v", err, fd.Names(src))
	}

	// The export imports back, whole or by collection group.
	imp, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: src, InputUriPrefix: "gs://exports/full", CollectionIds: []string{"users"}})
	if err == nil {
		err = imp.Wait(ctx)
	}
	if err != nil || strings.Join(fd.Names(src), ",") != "users/ada" {
		t.Fatalf("import users: %v, have %v", err, fd.Names(src))
	}
	if imp, err = c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: src, InputUriPrefix: "gs://exports/full"}); err == nil {
		err = imp.Wait(ctx)
	}
	if err != nil || len(fd.Names(src)) != 5 {
		t.Fatalf("import all: %v, have %v", err, fd.Names(src))
	}

	if _, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: src, InputUriPrefix: "gs://exports/nothing"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("import of no export: %v, want InvalidArgument", err)
	}
	if _, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: src, OutputUriPrefix: "gs://missing-bucket"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("export to a missing bucket: %v, want FailedPrecondition", err)
	}
	if _, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: src, OutputUriPrefix: "s3://x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("export to s3: %v, want InvalidArgument", err)
	}
}

func TestBackupRestoreClone(t *testing.T) {
	ctx := context.Background()
	c, fd, _ := transferAdmin(t)
	src := "projects/p/databases/(default)"
	fd.Seed(src)
	s, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: src, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	_ = s
	lb, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/p/locations/-"})
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	backups := lb.Backups
	if len(backups) != 1 || backups[0].State != adminpb.Backup_READY || backups[0].Stats.DocumentCount != 5 || backups[0].Database != src {
		t.Fatalf("backups = %v, want one READY backup of 5 documents", backups)
	}
	b, err := c.GetBackup(ctx, &adminpb.GetBackupRequest{Name: backups[0].Name})
	if err != nil || b.Name != backups[0].Name {
		t.Fatalf("GetBackup = %v, %v", b, err)
	}

	rop, err := c.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: "projects/p", DatabaseId: "restored", Backup: b.Name})
	if err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	db, err := rop.Wait(ctx)
	if err != nil || db.Name != "projects/p/databases/restored" || len(fd.Names(db.Name)) != 5 {
		t.Fatalf("restored = %v, %v, documents %v", db, err, fd.Names("projects/p/databases/restored"))
	}
	if _, err := c.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: "projects/p", DatabaseId: "restored", Backup: b.Name}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("restore over an existing database: %v, want AlreadyExists", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	cop, err := c.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: "projects/p", DatabaseId: "cloned",
		PitrSnapshot: &adminpb.PitrSnapshot{Database: src, SnapshotTime: timestamppb.New(now)}})
	if err != nil {
		t.Fatalf("CloneDatabase: %v", err)
	}
	if db, err := cop.Wait(ctx); err != nil || len(fd.Names(db.GetName())) != 5 {
		t.Fatalf("cloned = %v, %v", db, err)
	}
	if _, err := c.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: "projects/p", DatabaseId: "older",
		PitrSnapshot: &adminpb.PitrSnapshot{Database: src, SnapshotTime: timestamppb.New(now.Add(-time.Hour))}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("clone an hour back: %v, want Unimplemented", err)
	}

	if err := c.DeleteBackup(ctx, &adminpb.DeleteBackupRequest{Name: b.Name}); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if _, err := c.GetBackup(ctx, &adminpb.GetBackupRequest{Name: b.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted backup: %v, want NotFound", err)
	}
}
