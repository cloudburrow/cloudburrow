//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Firestore Admin API (#1331), which the Firestore pod's call front
// serves itself (internal/firestoreadmin): the emulator serves the
// documents API alone, and answered every admin call 404.

func firestoreAdmin(t *testing.T, h *Harness) *admin.FirestoreAdminClient {
	t.Helper()
	c, err := admin.NewFirestoreAdminClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvFirestore)),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("admin.NewFirestoreAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestFirestoreAdminDatabases lists `(default)`, creates a named database,
// writes and reads a document in it through the official Firestore client,
// updates it, refuses to delete it while delete protection is on, deletes
// it, and finds its documents gone.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/ListDatabases, google.firestore.admin.v1.FirestoreAdmin/GetDatabase, google.firestore.admin.v1.FirestoreAdmin/CreateDatabase, google.firestore.admin.v1.FirestoreAdmin/UpdateDatabase, google.firestore.admin.v1.FirestoreAdmin/DeleteDatabase
func TestFirestoreAdminDatabases(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project()

	list, err := c.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: parent})
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	if len(list.Databases) == 0 || list.Databases[0].Name != parent+"/databases/(default)" ||
		list.Databases[0].Type != adminpb.Database_FIRESTORE_NATIVE || list.Databases[0].LocationId == "" {
		t.Fatalf("ListDatabases = %v, want (default), FIRESTORE_NATIVE, with a location", list.Databases)
	}

	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: parent, DatabaseId: "orders",
		Database: &adminpb.Database{LocationId: "us-east1", Type: adminpb.Database_FIRESTORE_NATIVE,
			ConcurrencyMode: adminpb.Database_OPTIMISTIC}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase's operation: %v", err)
	}
	name := parent + "/databases/orders"
	if db.Name != name || db.LocationId != "us-east1" || db.ConcurrencyMode != adminpb.Database_OPTIMISTIC ||
		db.Uid == "" || db.CreateTime == nil || db.Etag == "" {
		t.Errorf("the created database = %v", db)
	}
	if _, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: parent, DatabaseId: "orders",
		Database: &adminpb.Database{LocationId: "us-east1", Type: adminpb.Database_FIRESTORE_NATIVE}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("creating it again: %v, want AlreadyExists", err)
	}

	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	fs, err := firestore.NewClientWithDatabase(ctx, h.Project(), "orders")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	doc := fs.Collection("o").Doc("one")
	if _, err := doc.Set(ctx, map[string]any{"total": 3}); err != nil {
		t.Fatalf("a write to the named database: %v", err)
	}
	if _, err := doc.Get(ctx); err != nil {
		t.Fatalf("a read from the named database: %v", err)
	}

	uop, err := c.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
		Database: &adminpb.Database{Name: name, DeleteProtectionState: adminpb.Database_DELETE_PROTECTION_ENABLED,
			PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"delete_protection_state", "point_in_time_recovery_enablement"}}})
	if err != nil {
		t.Fatalf("UpdateDatabase: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("UpdateDatabase's operation: %v", err)
	}
	got, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: name})
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if got.DeleteProtectionState != adminpb.Database_DELETE_PROTECTION_ENABLED || got.VersionRetentionPeriod.AsDuration().Hours() != 168 {
		t.Errorf("after the update = %v, want delete protection and a 7-day retention", got)
	}
	if _, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a protected database: %v, want FailedPrecondition", err)
	}
	uop, err = c.UpdateDatabase(ctx, &adminpb.UpdateDatabaseRequest{
		Database:   &adminpb.Database{Name: name, DeleteProtectionState: adminpb.Database_DELETE_PROTECTION_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"delete_protection_state"}}})
	if err == nil {
		_, err = uop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("lifting delete protection: %v", err)
	}
	dop, err := c.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteDatabase: %v", err)
	}
	if gone, err := dop.Wait(ctx); err != nil || gone.DeleteTime == nil {
		t.Fatalf("DeleteDatabase's operation = %v, %v", gone, err)
	}
	if _, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDatabase after delete: %v, want NotFound", err)
	}
	if _, err := doc.Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("the deleted database's document: %v, want NotFound", err)
	}
}

// TestFirestoreAdminIndexes creates a composite index, which is READY at
// once, lists, reads and deletes it. A query that needs it is answered
// before and after: the emulator needs no index.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateIndex, google.firestore.admin.v1.FirestoreAdmin/ListIndexes, google.firestore.admin.v1.FirestoreAdmin/GetIndex, google.firestore.admin.v1.FirestoreAdmin/DeleteIndex
func TestFirestoreAdminIndexes(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	fs := firestoreClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/databases/(default)/collectionGroups/cities"

	query := func(when string) {
		t.Helper()
		if _, err := fs.Collection("cities").Where("country", "==", "PT").OrderBy("pop", firestore.Desc).Documents(ctx).GetAll(); err != nil {
			t.Errorf("a query that needs a composite index, %s: %v", when, err)
		}
	}
	query("before the index")
	op, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: parent, Index: &adminpb.Index{
		QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{
			{FieldPath: "country", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "pop", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_DESCENDING}},
		}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	idx, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateIndex's operation: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetIndex() != idx.Name || md.GetState() != adminpb.OperationState_SUCCESSFUL {
		t.Errorf("the operation's metadata = %v, %v", md, err)
	}
	if idx.State != adminpb.Index_READY || len(idx.Fields) != 3 || idx.Fields[2].FieldPath != "__name__" {
		t.Errorf("the index = %v, want READY, with __name__ appended", idx)
	}
	query("with the index")
	if _, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: parent, Index: &adminpb.Index{
		QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{
			{FieldPath: "country", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}},
		}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a one-field composite index: %v, want InvalidArgument", err)
	}
	got, err := c.GetIndex(ctx, &adminpb.GetIndexRequest{Name: idx.Name})
	if err != nil || got.Name != idx.Name {
		t.Fatalf("GetIndex = %v, %v", got, err)
	}
	it := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: parent})
	var names []string
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListIndexes: %v", err)
		}
		names = append(names, x.Name)
	}
	if len(names) != 1 || names[0] != idx.Name {
		t.Errorf("ListIndexes = %v, want %s", names, idx.Name)
	}
	if err := c.DeleteIndex(ctx, &adminpb.DeleteIndexRequest{Name: idx.Name}); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
	if _, err := c.GetIndex(ctx, &adminpb.GetIndexRequest{Name: idx.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIndex after delete: %v, want NotFound", err)
	}
	query("after the index is deleted")
}

// TestFirestoreAdminFields reads a field's inherited settings, sets a TTL
// policy and a single-field index exemption on it, lists the overrides and
// lifts them.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/GetField, google.firestore.admin.v1.FirestoreAdmin/UpdateField, google.firestore.admin.v1.FirestoreAdmin/ListFields
func TestFirestoreAdminFields(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	cg := "projects/" + h.Project() + "/databases/(default)/collectionGroups/sessions"
	name := cg + "/fields/expireAt"

	f, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: name})
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if !f.GetIndexConfig().GetUsesAncestorConfig() || len(f.GetIndexConfig().GetIndexes()) != 3 || f.GetTtlConfig() != nil {
		t.Errorf("an untouched field = %v, want the inherited three indexes and no TTL", f)
	}

	op, err := c.UpdateField(ctx, &adminpb.UpdateFieldRequest{
		Field:      &adminpb.Field{Name: name, TtlConfig: &adminpb.Field_TtlConfig{}, IndexConfig: &adminpb.Field_IndexConfig{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl_config", "index_config"}}})
	if err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	f, err = op.Wait(ctx)
	if err != nil {
		t.Fatalf("UpdateField's operation: %v", err)
	}
	if f.GetTtlConfig().GetState() != adminpb.Field_TtlConfig_ACTIVE || f.GetIndexConfig().GetUsesAncestorConfig() || len(f.GetIndexConfig().GetIndexes()) != 0 {
		t.Errorf("after the update = %v, want an active TTL and no indexes", f)
	}
	if md, err := op.Metadata(); err != nil || len(md.GetIndexConfigDeltas()) != 3 || md.GetTtlConfigDelta().GetChangeType() != adminpb.FieldOperationMetadata_TtlConfigDelta_ADD {
		t.Errorf("the operation's metadata = %v, %v, want three index removals and a TTL added", md, err)
	}
	for _, filter := range []string{"ttlConfig:*", "indexConfig.usesAncestorConfig:false"} {
		it := c.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: cg, Filter: filter})
		got, err := it.Next()
		if err != nil || got.Name != name {
			t.Errorf("ListFields %s = %v, %v", filter, got, err)
		}
	}
	if _, err := c.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: cg}).Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListFields without a filter: %v, want InvalidArgument", err)
	}

	op, err = c.UpdateField(ctx, &adminpb.UpdateFieldRequest{Field: &adminpb.Field{Name: name},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl_config", "index_config"}}})
	if err == nil {
		f, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("lifting the overrides: %v", err)
	}
	if !f.GetIndexConfig().GetUsesAncestorConfig() || f.GetTtlConfig() != nil {
		t.Errorf("after lifting = %v, want the inherited settings", f)
	}
	if _, err := c.ListFields(ctx, &adminpb.ListFieldsRequest{Parent: cg, Filter: "ttlConfig:*"}).Next(); err != iterator.Done {
		t.Errorf("ListFields after lifting: %v, want none", err)
	}
}

// TestFirestoreAdminBackupSchedules (#1461): a daily and a weekly backup
// schedule are created on `(default)` through the official Firestore Admin
// client, listed and got, a second daily one is refused ALREADY_EXISTS, a
// retention beyond Google's 7-day cap for a daily schedule is
// INVALID_ARGUMENT, the daily one's retention is updated and its recurrence
// cannot be, and both are deleted. Their backups are
// TestFirestoreBackupRestoreClone's.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/GetBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/ListBackupSchedules, google.firestore.admin.v1.FirestoreAdmin/UpdateBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/DeleteBackupSchedule
func TestFirestoreAdminBackupSchedules(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	db := "projects/" + h.Project() + "/databases/(default)"
	day := 24 * time.Hour
	dailyRec := &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}
	// A schedule a previous run left behind would make the daily one a second.
	if l, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db}); err == nil {
		for _, s := range l.BackupSchedules {
			_ = c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: s.Name})
		}
	}
	d, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db,
		BackupSchedule: &adminpb.BackupSchedule{Retention: durationpb.New(7 * day), Recurrence: dailyRec}})
	if err != nil || !strings.HasPrefix(d.GetName(), db+"/backupSchedules/") || d.GetCreateTime() == nil {
		t.Fatalf("CreateBackupSchedule (daily) = %v, %v", d, err)
	}
	t.Cleanup(func() {
		_ = c.DeleteBackupSchedule(h.Context(), &adminpb.DeleteBackupScheduleRequest{Name: d.GetName()})
	})
	w, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db,
		BackupSchedule: &adminpb.BackupSchedule{Retention: durationpb.New(14 * 7 * day),
			Recurrence: &adminpb.BackupSchedule_WeeklyRecurrence{WeeklyRecurrence: &adminpb.WeeklyRecurrence{Day: dayofweek.DayOfWeek_SATURDAY}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule (weekly): %v", err)
	}
	if _, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db,
		BackupSchedule: &adminpb.BackupSchedule{Retention: durationpb.New(day), Recurrence: dailyRec}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second daily schedule: %v, want AlreadyExists", err)
	}
	if _, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db,
		BackupSchedule: &adminpb.BackupSchedule{Retention: durationpb.New(8 * day), Recurrence: dailyRec}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("8 days of daily retention: %v, want InvalidArgument", err)
	}
	l, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil || len(l.GetBackupSchedules()) != 2 {
		t.Fatalf("ListBackupSchedules = %v, %v", l, err)
	}
	g, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: w.GetName()})
	if err != nil || g.GetWeeklyRecurrence().GetDay() != dayofweek.DayOfWeek_SATURDAY || g.GetRetention().AsDuration() != 14*7*day {
		t.Errorf("GetBackupSchedule (weekly) = %v, %v", g, err)
	}
	u, err := c.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{
		BackupSchedule: &adminpb.BackupSchedule{Name: d.GetName(), Retention: durationpb.New(2 * day)},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"retention"}}})
	if err != nil || u.GetRetention().AsDuration() != 2*day || u.GetDailyRecurrence() == nil {
		t.Errorf("UpdateBackupSchedule = %v, %v", u, err)
	}
	if _, err := c.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{
		BackupSchedule: &adminpb.BackupSchedule{Name: w.GetName(), Recurrence: dailyRec},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"daily_recurrence"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("changing the recurrence: %v, want InvalidArgument", err)
	}
	for _, n := range []string{d.GetName(), w.GetName()} {
		if err := c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: n}); err != nil {
			t.Fatalf("DeleteBackupSchedule %s: %v", n, err)
		}
		if _, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: n}); status.Code(err) != codes.NotFound {
			t.Errorf("GetBackupSchedule after delete: %v, want NotFound", err)
		}
	}
}
