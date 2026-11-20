package firestoreadmin

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestReadTimeWindow (#1942): an hour back at any microsecond, older reads
// at whole minutes within seven days with PITR, never before PITR was
// enabled less an hour, nor before the emulator's last import.
func TestReadTimeWindow(t *testing.T) {
	a := New("")
	now := time.Date(2026, 11, 19, 12, 30, 15, 500000, time.UTC)
	a.now = func() time.Time { return now }
	def := "projects/p/databases/(default)"
	code := func(at time.Time) codes.Code { return status.Code(a.checkReadTime(def, at)) }

	if c := code(now.Add(-59 * time.Minute)); c != codes.OK {
		t.Errorf("59 minutes back: %v, want OK", c)
	}
	if c := code(now.Add(-61 * time.Minute).Truncate(time.Minute)); c != codes.FailedPrecondition {
		t.Errorf("61 minutes back without PITR: %v, want FailedPrecondition", c)
	}
	if c := code(now.Add(time.Hour)); c != codes.OK {
		t.Errorf("future: %v, want OK (the emulator refuses it)", c)
	}
	// Finer than a microsecond: refused on every read, ListCollectionIds
	// (which the emulator would answer) included.
	if c := code(now.Add(-time.Minute + 123)); c != codes.InvalidArgument {
		t.Errorf("a minute back with nanoseconds: %v, want InvalidArgument", c)
	}
	lc := &firestorepb.ListCollectionIdsRequest{Parent: def + "/documents",
		ConsistencySelector: &firestorepb.ListCollectionIdsRequest_ReadTime{ReadTime: timestamppb.New(now.Add(-time.Second + 1))}}
	if err := a.checkRequestReadTime(lc); status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "timestamp cannot have more than microseconds precision" {
		t.Errorf("ListCollectionIds with nanoseconds: %v, want InvalidArgument", err)
	}

	ctx := context.Background()
	if _, err := a.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
		Database:   &adminpb.Database{Name: def, PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"point_in_time_recovery_enablement"}}}); err != nil {
		t.Fatalf("UpdateDatabase: %v", err)
	}
	// Enabled just now: an hour before it, no further.
	if c := code(now.Add(-61 * time.Minute).Truncate(time.Minute)); c != codes.FailedPrecondition {
		t.Errorf("61 minutes back, PITR just enabled: %v, want FailedPrecondition", c)
	}
	// Three days on, three days back at a whole minute.
	now = now.Add(72 * time.Hour)
	back := now.Add(-71 * time.Hour).Truncate(time.Minute)
	if c := code(back); c != codes.OK {
		t.Errorf("71 hours back at a whole minute: %v, want OK", c)
	}
	if c := code(back.Add(time.Second)); c != codes.FailedPrecondition {
		t.Errorf("71 hours back, not a whole minute: %v, want FailedPrecondition", c)
	}
	a.mu.Lock()
	db := a.describe(a.databases[def])
	a.mu.Unlock()
	if db.GetVersionRetentionPeriod().AsDuration() != week || !db.GetEarliestVersionTime().AsTime().Equal(now.Add(-72*time.Hour-time.Hour)) {
		t.Errorf("retention %v, earliest %v; want 7 days, an hour before PITR was enabled", db.GetVersionRetentionPeriod().AsDuration(), db.GetEarliestVersionTime().AsTime())
	}
	// Ten days on, seven days back.
	now = now.Add(7 * 24 * time.Hour)
	if c := code(now.Add(-7*24*time.Hour + time.Minute).Truncate(time.Minute)); c != codes.OK {
		t.Errorf("just under 7 days back: %v, want OK", c)
	}
	if c := code(now.Add(-7*24*time.Hour - time.Minute).Truncate(time.Minute)); c != codes.FailedPrecondition {
		t.Errorf("over 7 days back: %v, want FailedPrecondition", c)
	}
	// The emulator restarted, its documents imported: nothing before.
	a.sawImport(now.Add(-time.Minute))
	if c := code(now.Add(-2 * time.Minute)); c != codes.FailedPrecondition {
		t.Errorf("before the import: %v, want FailedPrecondition", c)
	}

	// Exports need PITR; snapshots are whole minutes.
	if err := a.checkSnapshotTime(def, "snapshot_time", timestamppb.New(now.Truncate(time.Minute).Add(time.Second)), true); status.Code(err) != codes.InvalidArgument {
		t.Errorf("export at a non-minute: %v, want InvalidArgument", err)
	}
	if _, err := a.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
		Database:   &adminpb.Database{Name: def, PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"point_in_time_recovery_enablement"}}}); err != nil {
		t.Fatalf("UpdateDatabase: %v", err)
	}
	if err := a.checkSnapshotTime(def, "snapshot_time", timestamppb.New(now.Truncate(time.Minute)), true); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("export without PITR: %v, want FailedPrecondition", err)
	}
	if err := a.checkSnapshotTime(def, "pitr_snapshot.snapshot_time", timestamppb.New(now.Truncate(time.Minute)), false); err != nil {
		t.Errorf("clone without PITR, this minute: %v", err)
	}
}

// TestReadTimeGuardClaimsOldReads: the guard claims a gRPC or REST read
// outside the window, and leaves the rest to the emulator.
func TestReadTimeGuardClaimsOldReads(t *testing.T) {
	a := New("")
	now := time.Now().UTC()
	a.now = func() time.Time { return now }
	g := a.EditionGuard()
	name := "projects/p/databases/(default)/documents/c/d"
	grpcReq := func(at time.Time) bool {
		req := &firestorepb.GetDocumentRequest{Name: name, ConsistencySelector: &firestorepb.GetDocumentRequest_ReadTime{ReadTime: timestamppb.New(at)}}
		r := httptest.NewRequest("POST", firestoreService+"GetDocument", bytes.NewReader(grpcFrame(req)))
		r.Header.Set("Content-Type", "application/grpc")
		return g.Claims(r)
	}
	if grpcReq(now.Add(-time.Minute)) {
		t.Error("a read a minute back was claimed")
	}
	if !grpcReq(now.Add(-2 * time.Hour)) {
		t.Error("a read two hours back was not claimed")
	}
	rest := httptest.NewRequest("GET", "/v1/"+name+"?readTime="+now.Add(-2*time.Hour).Format(time.RFC3339Nano), nil)
	if !g.Claims(rest) {
		t.Error("a REST read two hours back was not claimed")
	}
	body := `{"structuredQuery":{},"newTransaction":{"readOnly":{"readTime":"` + now.Add(-3*time.Hour).Format(time.RFC3339) + `"}}}`
	q := httptest.NewRequest("POST", "/v1/projects/p/databases/(default)/documents:runQuery", bytes.NewReader([]byte(body)))
	if !g.Claims(q) {
		t.Error("a REST query in a read-only transaction three hours back was not claimed")
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, q)
	if w.Code != 400 {
		t.Errorf("refusal = HTTP %d %s, want 400 FAILED_PRECONDITION", w.Code, w.Body)
	}
}
