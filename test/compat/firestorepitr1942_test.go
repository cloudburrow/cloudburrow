//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Point-in-time recovery (#1942): gcloud creates a database with PITR
// enabled; the official Go client reads its documents at earlier read
// times, a whole minute and a microsecond one, and is refused a read from
// before the database's earliest version time; the Admin client exports at
// a snapshot time, imported back into another database, gcloud clones at
// that minute, and both hold the documents as they were at it. With PITR
// disabled through gcloud, an export at a snapshot time is refused.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/UpdateDatabase, google.firestore.admin.v1.FirestoreAdmin/GetDatabase, google.firestore.admin.v1.FirestoreAdmin/ExportDocuments, google.firestore.admin.v1.FirestoreAdmin/CloneDatabase
func TestFirestorePointInTimeRecovery(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)    // asks for CLOUDBURROW_TEST_FIRESTORE
	sc := firestoreStorage(t, h) // asks for CLOUDBURROW_TEST_FIRESTORE_STORAGE
	firestoreClient(t, h)        // sets FIRESTORE_EMULATOR_HOST
	gc := gcloudAdminSession(t, h.Project(), "firestore")
	ctx := minuteContext(t) // waits for a whole minute (#1942)
	project := "projects/" + h.Project()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	id := "pitr-" + suffix
	dbName := project + "/databases/" + id
	bh := bucket(t, h, sc)

	gc("firestore", "databases", "create", "--database", id, "--location", "nam5", "--type", "firestore-native", "--enable-pitr")
	t.Cleanup(func() { _, _ = c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: dbName}) })
	db, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: dbName})
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if db.PointInTimeRecoveryEnablement != adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED ||
		db.GetVersionRetentionPeriod().AsDuration() != 7*24*time.Hour ||
		db.GetEarliestVersionTime().AsTime().Before(db.GetCreateTime().AsTime()) {
		t.Fatalf("database = PITR %v, retention %v, earliest %v (created %v); want enabled, 7 days, not before its creation",
			db.PointInTimeRecoveryEnablement, db.GetVersionRetentionPeriod().AsDuration(), db.GetEarliestVersionTime().AsTime(), db.GetCreateTime().AsTime())
	}

	fs, err := firestore.NewClientWithDatabase(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	sf := fs.Doc("cities/sf")
	set := func(ref *firestore.DocumentRef, v int) {
		t.Helper()
		if _, err := ref.Set(ctx, map[string]any{"v": v}); err != nil {
			t.Fatalf("Set %s: %v", ref.Path, err)
		}
	}
	valueAt := func(ref *firestore.DocumentRef, at time.Time) (int64, bool) {
		t.Helper()
		snap, err := ref.WithReadOptions(firestore.ReadTime(at)).Get(ctx)
		if status.Code(err) == codes.NotFound {
			return 0, false
		}
		if err != nil {
			t.Fatalf("Get %s at %v: %v", ref.Path, at, err)
		}
		v, _ := snap.DataAt("v")
		n, _ := v.(int64)
		return n, true
	}
	queryAt := func(at time.Time) string {
		t.Helper()
		q := fs.Collection("cities").Query
		docs, err := q.WithReadOptions(firestore.ReadTime(at)).Documents(ctx).GetAll()
		if err != nil {
			t.Fatalf("query at %v: %v", at, err)
		}
		var out []string
		for _, d := range docs {
			out = append(out, fmt.Sprintf("%s=%v", d.Ref.ID, d.Data()["v"]))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	set(sf, 1)
	time.Sleep(1100 * time.Millisecond)
	t1 := time.Now()
	time.Sleep(1100 * time.Millisecond)
	set(sf, 2)
	set(fs.Doc("cities/la"), 10)
	if v, ok := valueAt(sf, t1); !ok || v != 1 {
		t.Errorf("cities/sf at %v = %d (exists %v), want 1", t1, v, ok)
	}
	if got := queryAt(t1); got != "sf=1" {
		t.Errorf("query at %v = %q, want sf=1", t1, got)
	}
	if got := queryAt(time.Now()); got != "la=10,sf=2" {
		t.Errorf("query now = %q, want la=10,sf=2", got)
	}
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	if _, err := sf.WithReadOptions(firestore.ReadTime(old)).Get(ctx); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Get two hours back, before the database's earliest version time: %v, want FailedPrecondition", err)
	}

	// A whole minute with sf=3 and la=10, then changed after it.
	set(sf, 3)
	// The next whole minute, which every write after this follows.
	snapshot := nextWholeMinute()
	set(sf, 4)
	if _, err := fs.Doc("cities/la").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if v, ok := valueAt(sf, snapshot); !ok || v != 3 {
		t.Fatalf("cities/sf at the whole minute %v = %d (exists %v), want 3", snapshot, v, ok)
	}

	prefix := "gs://" + bh.BucketName() + "/pitr-" + suffix
	op, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: dbName, OutputUriPrefix: prefix, SnapshotTime: timestamppb.New(snapshot)})
	if err != nil {
		t.Fatalf("ExportDocuments at %v: %v", snapshot, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("export: %v", err)
	}
	if md, err := op.Metadata(); err != nil || !md.GetSnapshotTime().AsTime().Equal(snapshot) {
		t.Errorf("export metadata snapshot_time = %v, %v; want %v", md.GetSnapshotTime(), err, snapshot)
	}
	impID := "pitr-imp-" + suffix
	gc("firestore", "databases", "create", "--database", impID, "--location", "nam5", "--type", "firestore-native")
	t.Cleanup(func() {
		_, _ = c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: project + "/databases/" + impID})
	})
	iop, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: project + "/databases/" + impID, InputUriPrefix: prefix})
	if err != nil {
		t.Fatalf("ImportDocuments: %v", err)
	}
	if err := iop.Wait(ctx); err != nil {
		t.Fatalf("import: %v", err)
	}
	contents := func(id string) string {
		t.Helper()
		cl, err := firestore.NewClientWithDatabase(ctx, h.Project(), id)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cl.Close() }()
		docs, err := cl.Collection("cities").Documents(ctx).GetAll()
		if err != nil {
			t.Fatalf("documents of %s: %v", id, err)
		}
		var out []string
		for _, d := range docs {
			out = append(out, fmt.Sprintf("%s=%v", d.Ref.ID, d.Data()["v"]))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got := contents(impID); got != "la=10,sf=3" {
		t.Errorf("the export at %v, imported: %q, want la=10,sf=3", snapshot, got)
	}

	cloneID := "pitr-clone-" + suffix
	gc("firestore", "databases", "clone", "--source-database", dbName,
		"--snapshot-time", snapshot.UTC().Format(time.RFC3339), "--destination-database", cloneID)
	t.Cleanup(func() {
		_, _ = c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: project + "/databases/" + cloneID})
	})
	if got := contents(cloneID); got != "la=10,sf=3" {
		t.Errorf("gcloud's clone at %v: %q, want la=10,sf=3", snapshot, got)
	}
	if got := contents(id); got != "sf=4" {
		t.Errorf("the source now: %q, want sf=4", got)
	}

	gc("firestore", "databases", "update", "--database", id, "--no-enable-pitr")
	db, err = c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: dbName})
	if err != nil || db.PointInTimeRecoveryEnablement != adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED || db.GetVersionRetentionPeriod().AsDuration() != time.Hour {
		t.Errorf("after --no-enable-pitr: %v, %v; want disabled, an hour's retention", db, err)
	}
	if _, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: dbName, OutputUriPrefix: prefix + "-off", SnapshotTime: timestamppb.New(snapshot)}); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(status.Convert(err).Message(), "Point-in-Time Recovery must be enabled") {
		t.Errorf("export at a snapshot time without PITR: %v, want FailedPrecondition", err)
	}
	if _, err := sf.WithReadOptions(firestore.ReadTime(snapshot.Add(1500 * time.Millisecond))).Get(ctx); err != nil {
		t.Errorf("Get within the hour, at a microsecond time, without PITR: %v", err)
	}
}

// minuteContext is a test's context with room for nextWholeMinute's wait
// beside its calls: the harness's own allows 60 seconds.
func minuteContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// nextWholeMinute waits for the next whole minute to pass, by a second,
// and is that minute: a clone or export at it holds every write made
// before the call, and none made after it returns.
func nextWholeMinute() time.Time {
	m := time.Now().Truncate(time.Minute).Add(time.Minute)
	time.Sleep(time.Until(m.Add(time.Second)))
	return m
}

// TestDatastoreReadAtAnEarlierTimeIsRefused (#1942): the Datastore
// emulator keeps no earlier versions of its entities, so the official Go
// client's read at an earlier read time is refused UNIMPLEMENTED rather
// than answered with the entities as they are now; a read now is served.
//
// covers: google.datastore.v1.Datastore/Lookup
func TestDatastoreReadAtAnEarlierTimeIsRefused(t *testing.T) {
	h := New(t)
	ds := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	key := datastore.NameKey("PITR", "e1", nil)
	type ent struct{ V int }
	if _, err := ds.Put(ctx, key, &ent{V: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var got ent
	if err := ds.Get(ctx, key, &got); err != nil || got.V != 1 {
		t.Fatalf("Get now = %v, %v", got, err)
	}
	tx, err := ds.NewTransaction(ctx, datastore.ReadOnly, datastore.WithReadTime(time.Now().Add(-time.Minute)))
	if err == nil {
		err = tx.Get(key, &got)
		_ = tx.Rollback()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("a read-only transaction a minute back: %v, want Unimplemented", err)
	}
}
