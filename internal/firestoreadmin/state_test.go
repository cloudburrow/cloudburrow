package firestoreadmin_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// keptAdmin serves an Admin API for the emulator at docs that keeps its
// state in dir; stop ends it, as a restart of the front's container does.
func keptAdmin(t *testing.T, docs, dir string) (c *admin.FirestoreAdminClient, a *firestoreadmin.Admin, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a = firestoreadmin.New(docs)
	if err := a.KeepState(ctx, dir, quietLogf(t)); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.GRPCServer().Serve(l) }()
	c, err = admin.NewFirestoreAdminClient(context.Background(), option.WithEndpoint(l.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	stop = func() { _ = c.Close(); a.GRPCServer().Stop(); cancel() }
	t.Cleanup(stop)
	return c, a, stop
}

// TestAdminKeepsItsStateAcrossARestart (#1514): a database, an index, a
// backup schedule and the backup it took, made through one Admin API, are
// read back through a new one on the same state directory, the emulator
// still running: the backup restores, with its documents. A reset clears
// them.
func TestAdminKeepsItsStateAcrossARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fd, docsAddr, _, _ := fstest.Serve(t)
	dir := t.TempDir()
	c, _, stop := keptAdmin(t, docsAddr, dir)
	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "named",
		Database: &adminpb.Database{LocationId: "us-east1", Type: adminpb.Database_FIRESTORE_NATIVE}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	iop, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: db.Name + "/collectionGroups/cities",
		Index: &adminpb.Index{QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{
			{FieldPath: "country", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "pop", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_DESCENDING}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	src := "projects/p/databases/(default)"
	fd.Seed(src)
	sched, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: src, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	stop()

	c2, _, _ := keptAdmin(t, docsAddr, dir)
	if got, err := c2.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db.Name}); err != nil || got.Uid != db.Uid {
		t.Errorf("GetDatabase after the restart = %v, %v; want uid %s", got, err, db.Uid)
	}
	if got, err := c2.GetIndex(ctx, &adminpb.GetIndexRequest{Name: idx.Name}); err != nil || len(got.Fields) != 3 {
		t.Errorf("GetIndex after the restart = %v, %v", got, err)
	}
	if got, err := c2.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: sched.Name}); err != nil || got.GetRetention().AsDuration() != 24*time.Hour {
		t.Errorf("GetBackupSchedule after the restart = %v, %v", got, err)
	}
	lb, err := c2.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/p/locations/-"})
	if err != nil || len(lb.Backups) != 1 || lb.Backups[0].Stats.GetDocumentCount() != 5 {
		t.Fatalf("ListBackups after the restart = %v, %v; want the schedule's backup of 5 documents", lb, err)
	}
	rop, err := c2.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: "projects/p", DatabaseId: "restored", Backup: lb.Backups[0].Name})
	if err != nil {
		t.Fatalf("RestoreDatabase after the restart: %v", err)
	}
	if got, err := rop.Wait(ctx); err != nil || len(fd.Names(got.GetName())) != 5 {
		t.Fatalf("restored = %v, %v", got, err)
	}
}

// TestAdminResetClearsItsState (#1514): the reset path forgets the
// databases and backups, and a new API on the directory starts without
// them.
func TestAdminResetClearsItsState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fd, docsAddr, _, _ := fstest.Serve(t)
	dir := t.TempDir()
	c, a, stop := keptAdmin(t, docsAddr, dir)
	src := "projects/p/databases/(default)"
	fd.Seed(src)
	if _, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: src, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, frontstate.ResetPath, nil)
	if !a.Claims(req) {
		t.Fatal("the API does not claim the reset path")
	}
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	stop()
	c2, _, _ := keptAdmin(t, docsAddr, dir)
	lb, err := c2.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/p/locations/-"})
	if err != nil || len(lb.Backups) != 0 {
		t.Errorf("ListBackups after the reset = %v, %v; want none", lb, err)
	}
	ls, err := c2.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: src})
	if err != nil || len(ls.BackupSchedules) != 0 {
		t.Errorf("ListBackupSchedules after the reset = %v, %v; want none", ls, err)
	}
	if _, err := c2.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/p/databases/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDatabase of an unknown database = %v", err)
	}
}

// quietLogf is t.Logf until the test's cleanups start: the state's saver
// writes once more when its context ends, and may run after the test.
func quietLogf(t *testing.T) func(string, ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() { mu.Lock(); done = true; mu.Unlock() })
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}
