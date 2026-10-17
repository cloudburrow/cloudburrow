//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btadminpb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/firestore"
	fsadminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/spanner"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// untilServed retries call while the tunnel reconnects to the restarted
// front: the first calls may be refused UNAVAILABLE.
func untilServed(t *testing.T, call func() error) error {
	t.Helper()
	var err error
	for deadline := time.Now().Add(time.Minute); ; {
		err = call()
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}

// TestSpannerFrontRestartKeepsItsState (#1514): what the Spanner front
// keeps on its pod's emptyDir is still there after a restart of the
// front's container alone, the emulator still running. Through the
// official clients: a backup, restored after the restart into a database
// with its rows; a backup schedule; a database role; drop protection; an
// instance's updated display name; a custom instance configuration; and a
// database's IAM policy.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/GetBackup, google.spanner.admin.database.v1.DatabaseAdmin/RestoreDatabase, google.spanner.admin.database.v1.DatabaseAdmin/GetBackupSchedule, google.spanner.admin.database.v1.DatabaseAdmin/ListDatabaseRoles, google.spanner.admin.database.v1.DatabaseAdmin/GetIamPolicy, google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/GetInstance
func TestSpannerFrontRestartKeepsItsState(t *testing.T) {
	f := newSpannerAdmin(t, "cb-kept", "keptdb")
	ctx := f.ctx
	ddl := func(stmts ...string) {
		t.Helper()
		op, err := f.db.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: f.database, Statements: stmts})
		if err == nil {
			err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("UpdateDatabaseDdl(%v): %v", stmts, err)
		}
	}
	ddl(`CREATE TABLE Notes (Id INT64 NOT NULL, N INT64 NOT NULL, Note STRING(MAX)) PRIMARY KEY (Id, N), INTERLEAVE IN PARENT Items ON DELETE CASCADE`,
		`ALTER TABLE Items ADD COLUMN Twice INT64 AS (Id * 2) STORED`,
		"CREATE ROLE keeper", "GRANT SELECT ON TABLE Items TO ROLE keeper")
	dc, err := spanner.NewClient(ctx, f.database, spannerOpts(f.addr)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Items", []string{"Id", "Name"}, []any{1, "one"}),
		spanner.Insert("Notes", []string{"Id", "N", "Note"}, []any{1, 1, "kept note"}),
	}); err != nil {
		t.Fatal(err)
	}
	dc.Close()
	want := spannerRows(t, ctx, f.addr, f.database)
	bop, err := f.db.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: f.instance, BackupId: "kept",
		Backup: &databasepb.Backup{Database: f.database, ExpireTime: timestamppb.New(time.Now().Add(48 * time.Hour))}})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if _, err := bop.Wait(ctx); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	t.Cleanup(func() {
		_ = f.db.DeleteBackup(context.Background(), &databasepb.DeleteBackupRequest{Name: f.instance + "/backups/kept"})
	})
	schedule := f.database + "/backupSchedules/kept"
	if _, err := f.db.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: f.database, BackupScheduleId: "kept",
		BackupSchedule: &databasepb.BackupSchedule{
			Spec:              &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: "0 2 * * *"}}},
			RetentionDuration: durationpb.New(7 * 24 * time.Hour),
			BackupTypeSpec:    &databasepb.BackupSchedule_FullBackupSpec{FullBackupSpec: &databasepb.FullBackupSpec{}}}}); err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	uop, err := f.inst.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: f.instance, DisplayName: "kept across"},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	project := "projects/" + f.project
	base, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: project + "/instanceConfigs/regional-us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	custom := project + "/instanceConfigs/custom-cb-kept"
	replicas := append(append([]*instancepb.ReplicaInfo{}, base.GetReplicas()...),
		&instancepb.ReplicaInfo{Location: "us-east1", Type: instancepb.ReplicaInfo_READ_ONLY})
	cop, err := f.inst.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{Parent: project, InstanceConfigId: "custom-cb-kept",
		InstanceConfig: &instancepb.InstanceConfig{Name: custom, DisplayName: "CB kept", BaseConfig: base.GetName(), Replicas: replicas}})
	if err != nil {
		t.Fatalf("CreateInstanceConfig: %v", err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatalf("CreateInstanceConfig: %v", err)
	}
	t.Cleanup(func() {
		_ = f.inst.DeleteInstanceConfig(context.Background(), &instancepb.DeleteInstanceConfigRequest{Name: custom})
	})
	if _, err := f.db.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: f.database, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/spanner.databaseReader", Members: []string{"user:kept@example.com"}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}

	restartFront(t, "spanner", "spanner")

	var b *databasepb.Backup
	if err := untilServed(t, func() (err error) {
		b, err = f.db.GetBackup(ctx, &databasepb.GetBackupRequest{Name: f.instance + "/backups/kept"})
		return err
	}); err != nil || b.GetState() != databasepb.Backup_READY {
		t.Fatalf("GetBackup after the restart = %v, %v", b, err)
	}
	rop, err := f.db.RestoreDatabase(ctx, &databasepb.RestoreDatabaseRequest{Parent: f.instance, DatabaseId: "keptrestored",
		Source: &databasepb.RestoreDatabaseRequest_Backup{Backup: b.GetName()}})
	if err != nil {
		t.Fatalf("RestoreDatabase after the restart: %v", err)
	}
	restored, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("RestoreDatabase after the restart: %v", err)
	}
	if got := spannerRows(t, ctx, f.addr, restored.GetName()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the database restored after the restart holds %v, want %v", got, want)
	}
	if s, err := f.db.GetBackupSchedule(ctx, &databasepb.GetBackupScheduleRequest{Name: schedule}); err != nil || s.GetSpec().GetCronSpec().GetText() != "0 2 * * *" {
		t.Errorf("GetBackupSchedule after the restart = %v, %v", s, err)
	}
	roles := map[string]bool{}
	it := f.db.ListDatabaseRoles(ctx, &databasepb.ListDatabaseRolesRequest{Parent: f.database})
	for {
		r, err := it.Next()
		if err != nil {
			break
		}
		roles[strings.TrimPrefix(r.GetName(), f.database+"/databaseRoles/")] = true
	}
	if !roles["keeper"] {
		t.Errorf("ListDatabaseRoles after the restart = %v, want keeper", roles)
	}
	if in, err := f.inst.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: f.instance}); err != nil || in.GetDisplayName() != "kept across" {
		t.Errorf("GetInstance after the restart = %v, %v; want the updated display name", in, err)
	}
	if c, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: custom}); err != nil || c.GetDisplayName() != "CB kept" {
		t.Errorf("GetInstanceConfig after the restart = %v, %v", c, err)
	}
	if p, err := f.db.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.database}); err != nil || len(p.GetBindings()) != 1 {
		t.Errorf("GetIamPolicy after the restart = %v, %v", p, err)
	}
}

// TestBigtableFrontRestartKeepsItsState (#1514): what the Bigtable front
// keeps on its pod's emptyDir is still there after a restart of the
// front's container alone, the emulator still running and holding the
// tables. Through the official clients: the instance, a backup restored
// after the restart into a table with its cells, a snapshot, a deleted
// table undeleted after the restart with its cells, an authorized view, a
// logical view and a table's IAM policy.
//
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/GetInstance, google.bigtable.admin.v2.BigtableTableAdmin/GetBackup, google.bigtable.admin.v2.BigtableTableAdmin/RestoreTable, google.bigtable.admin.v2.BigtableTableAdmin/GetSnapshot, google.bigtable.admin.v2.BigtableTableAdmin/UndeleteTable, google.bigtable.admin.v2.BigtableTableAdmin/GetAuthorizedView, google.bigtable.admin.v2.BigtableInstanceAdmin/GetLogicalView, google.bigtable.admin.v2.BigtableTableAdmin/GetIamPolicy
func TestBigtableFrontRestartKeepsItsState(t *testing.T) {
	h := New(t)
	admin, c := bigtableTable(t, h, "keptsrc") // asks for the Bigtable endpoint first
	ctx := h.Context()
	iac := bigtableInstanceAdmin(t, h)
	const keptInstance = "cb-kept-inst"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: keptInstance, DisplayName: "CB kept",
		ClusterId: keptInstance + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(context.Background(), keptInstance) })
	parent := fmt.Sprintf("projects/%s/instances/%s", h.Project(), bigtableInstance)
	const family = "info"
	write := func(tbl, v string) {
		m := bigtable.NewMutation()
		m.Set(family, "q", bigtable.Timestamp(9000), []byte(v))
		if err := c.Open(tbl).Apply(ctx, "r1", m); err != nil {
			t.Fatal(err)
		}
	}
	write("keptsrc", "backed up")
	const cluster = "c1"
	if err := admin.CreateBackup(ctx, "keptsrc", cluster, "keptb", time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteBackup(context.Background(), cluster, "keptb") })
	if err := admin.SnapshotTable(ctx, "keptsrc", cluster, "keptsnap", 0); err != nil {
		t.Fatalf("SnapshotTable: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteSnapshot(context.Background(), cluster, "keptsnap") })
	if err := admin.CreateTable(ctx, "keptgone"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "keptgone", family); err != nil {
		t.Fatal(err)
	}
	write("keptgone", "deleted")
	if err := admin.DeleteTable(ctx, "keptgone"); err != nil {
		t.Fatal(err)
	}
	ta := admin.TableAdminClientV2()
	if _, err := ta.CreateAuthorizedView(ctx, &btadminpb.CreateAuthorizedViewRequest{Parent: parent + "/tables/keptsrc", AuthorizedViewId: "keptav",
		AuthorizedView: &btadminpb.AuthorizedView{AuthorizedView: &btadminpb.AuthorizedView_SubsetView_{SubsetView: &btadminpb.AuthorizedView_SubsetView{RowPrefixes: [][]byte{[]byte("r")}}}}}); err != nil {
		t.Fatalf("CreateAuthorizedView: %v", err)
	}
	// A logical view belongs to an instance the front keeps.
	keptParent := fmt.Sprintf("projects/%s/instances/%s", h.Project(), keptInstance)
	lvAdmin, err := bigtable.NewAdminClient(ctx, h.Project(), keptInstance)
	if err != nil {
		t.Fatal(err)
	}
	defer lvAdmin.Close()
	if err := lvAdmin.CreateTable(ctx, "lvsrc"); err != nil {
		t.Fatal(err)
	}
	if err := lvAdmin.CreateColumnFamily(ctx, "lvsrc", family); err != nil {
		t.Fatal(err)
	}
	ia := iac.InstanceAdminClientV2()
	if _, err := ia.CreateLogicalView(ctx, &btadminpb.CreateLogicalViewRequest{Parent: keptParent, LogicalViewId: "keptlv",
		LogicalView: &btadminpb.LogicalView{Query: "SELECT _key FROM lvsrc"}}); err != nil {
		t.Fatalf("CreateLogicalView: %v", err)
	}
	t.Cleanup(func() {
		_ = ia.DeleteLogicalView(context.Background(), &btadminpb.DeleteLogicalViewRequest{Name: keptParent + "/logicalViews/keptlv"})
	})
	if _, err := ta.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: parent + "/tables/keptsrc", Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigtable.reader", Members: []string{"user:kept@example.com"}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}

	restartFront(t, "bigtable", "bigtable")

	var in *bigtable.InstanceInfo
	if err := untilServed(t, func() (err error) { in, err = iac.InstanceInfo(ctx, keptInstance); return err }); err != nil || in.DisplayName != "CB kept" {
		t.Fatalf("InstanceInfo after the restart = %+v, %v", in, err)
	}
	if err := admin.RestoreTable(ctx, "keptrestored", cluster, "keptb"); err != nil {
		t.Fatalf("RestoreTable after the restart: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), "keptrestored") })
	read := func(tbl string) string {
		r, err := c.Open(tbl).ReadRow(ctx, "r1")
		if err != nil || len(r[family]) != 1 {
			t.Errorf("ReadRow(%s) = %v, %v", tbl, r, err)
			return ""
		}
		return string(r[family][0].Value)
	}
	if got := read("keptrestored"); got != "backed up" {
		t.Errorf("the table restored after the restart holds %q", got)
	}
	if s, err := admin.SnapshotInfo(ctx, cluster, "keptsnap"); err != nil || s.SourceTable != "keptsrc" {
		t.Errorf("SnapshotInfo after the restart = %+v, %v", s, err)
	}
	op, err := ta.UndeleteTable(ctx, &btadminpb.UndeleteTableRequest{Name: parent + "/tables/keptgone"})
	if err != nil {
		t.Fatalf("UndeleteTable after the restart: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("UndeleteTable after the restart: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), "keptgone") })
	if got := read("keptgone"); got != "deleted" {
		t.Errorf("the table undeleted after the restart holds %q", got)
	}
	if _, err := ta.GetAuthorizedView(ctx, &btadminpb.GetAuthorizedViewRequest{Name: parent + "/tables/keptsrc/authorizedViews/keptav"}); err != nil {
		t.Errorf("GetAuthorizedView after the restart: %v", err)
	}
	if lv, err := ia.GetLogicalView(ctx, &btadminpb.GetLogicalViewRequest{Name: keptParent + "/logicalViews/keptlv"}); err != nil || lv.GetQuery() != "SELECT _key FROM lvsrc" {
		t.Errorf("GetLogicalView after the restart = %v, %v", lv, err)
	}
	if p, err := ta.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent + "/tables/keptsrc"}); err != nil || len(p.GetBindings()) != 1 {
		t.Errorf("GetIamPolicy after the restart = %v, %v", p, err)
	}
}

// TestFirestoreFrontRestartKeepsItsState (#1514): what the Firestore
// Admin API keeps on its pod's emptyDir is still there after a restart of
// the front's container alone, the emulator still running and holding the
// documents. Through the official clients: a named database, a composite
// index, a backup schedule, and the backup it took, restored after the
// restart into a database with its documents.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/GetDatabase, google.firestore.admin.v1.FirestoreAdmin/GetIndex, google.firestore.admin.v1.FirestoreAdmin/GetBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/ListBackups, google.firestore.admin.v1.FirestoreAdmin/RestoreDatabase
func TestFirestoreFrontRestartKeepsItsState(t *testing.T) {
	h := New(t)
	ac := firestoreAdmin(t, h) // asks for the Firestore endpoint first
	_ = firestoreClient(t, h)  // and points the client at it
	ctx := h.Context()
	project := "projects/" + h.Project()
	op, err := ac.CreateDatabase(ctx, &fsadminpb.CreateDatabaseRequest{Parent: project, DatabaseId: "kept-across",
		Database: &fsadminpb.Database{LocationId: "nam5", Type: fsadminpb.Database_FIRESTORE_NATIVE}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ac.DeleteDatabase(context.Background(), &fsadminpb.DeleteDatabaseRequest{Name: db.GetName()})
	})
	iop, err := ac.CreateIndex(ctx, &fsadminpb.CreateIndexRequest{Parent: db.GetName() + "/collectionGroups/cities",
		Index: &fsadminpb.Index{QueryScope: fsadminpb.Index_COLLECTION, Fields: []*fsadminpb.Index_IndexField{
			{FieldPath: "country", ValueMode: &fsadminpb.Index_IndexField_Order_{Order: fsadminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "pop", ValueMode: &fsadminpb.Index_IndexField_Order_{Order: fsadminpb.Index_IndexField_DESCENDING}}}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	idx, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	fc := firestoreDatabaseClient(t, h, "kept-across")
	for i := 1; i <= 3; i++ {
		if _, err := fc.Collection("kept").Doc(fmt.Sprintf("d%d", i)).Set(ctx, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	sched, err := ac.CreateBackupSchedule(ctx, &fsadminpb.CreateBackupScheduleRequest{Parent: db.GetName(), BackupSchedule: &fsadminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &fsadminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &fsadminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}

	restartFront(t, "firestore", "firestore")

	var got *fsadminpb.Database
	if err := untilServed(t, func() (err error) {
		got, err = ac.GetDatabase(ctx, &fsadminpb.GetDatabaseRequest{Name: db.GetName()})
		return err
	}); err != nil || got.GetUid() != db.GetUid() {
		t.Fatalf("GetDatabase after the restart = %v, %v; want uid %s", got, err, db.GetUid())
	}
	if i, err := ac.GetIndex(ctx, &fsadminpb.GetIndexRequest{Name: idx.GetName()}); err != nil || len(i.GetFields()) != 3 {
		t.Errorf("GetIndex after the restart = %v, %v", i, err)
	}
	if s, err := ac.GetBackupSchedule(ctx, &fsadminpb.GetBackupScheduleRequest{Name: sched.GetName()}); err != nil || s.GetRetention().AsDuration() != 24*time.Hour {
		t.Errorf("GetBackupSchedule after the restart = %v, %v", s, err)
	}
	lb, err := ac.ListBackups(ctx, &fsadminpb.ListBackupsRequest{Parent: project + "/locations/-"})
	if err != nil {
		t.Fatalf("ListBackups after the restart: %v", err)
	}
	var backup *fsadminpb.Backup
	for _, b := range lb.GetBackups() {
		if b.GetDatabase() == db.GetName() {
			backup = b
		}
	}
	if backup == nil || backup.GetStats().GetDocumentCount() != 3 {
		t.Fatalf("ListBackups after the restart = %v; want the schedule's backup of 3 documents", lb.GetBackups())
	}
	rop, err := ac.RestoreDatabase(ctx, &fsadminpb.RestoreDatabaseRequest{Parent: project, DatabaseId: "kept-restored", Backup: backup.GetName()})
	if err != nil {
		t.Fatalf("RestoreDatabase after the restart: %v", err)
	}
	restored, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("RestoreDatabase after the restart: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ac.DeleteDatabase(context.Background(), &fsadminpb.DeleteDatabaseRequest{Name: restored.GetName()})
	})
	rc := firestoreDatabaseClient(t, h, "kept-restored")
	docs, err := rc.Collection("kept").Documents(ctx).GetAll()
	if err != nil || len(docs) != 3 {
		t.Errorf("the database restored after the restart holds %d documents, %v; want 3", len(docs), err)
	}
}

// firestoreDatabaseClient is the official client for the database id,
// through the endpoint firestoreClient set.
func firestoreDatabaseClient(t *testing.T, h *Harness, id string) *firestore.Client {
	t.Helper()
	c, err := firestore.NewClientWithDatabase(h.Context(), h.Project(), id)
	if err != nil {
		t.Fatalf("firestore.NewClientWithDatabase(%s): %v", id, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestResetClearsTheFrontsState (#1514): `POST /admin/reset` for spanner,
// bigtable and firestore forgets what their fronts keep: a Spanner custom
// instance configuration, a Bigtable instance and a Firestore backup
// schedule are NOT_FOUND after it, through the official clients.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig, google.bigtable.admin.v2.BigtableInstanceAdmin/GetInstance, google.firestore.admin.v1.FirestoreAdmin/GetBackupSchedule
func TestResetClearsTheFrontsState(t *testing.T) {
	f := newSpannerAdmin(t, "cb-reset", "resetdb") // asks for the Spanner endpoint first
	h := New(t)
	control := h.Endpoint(EnvControl)
	iac := bigtableInstanceAdmin(t, h)
	fa := firestoreAdmin(t, h)
	ctx := f.ctx
	project := "projects/" + f.project
	base, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: project + "/instanceConfigs/regional-us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	custom := project + "/instanceConfigs/custom-cb-reset"
	cop, err := f.inst.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{Parent: project, InstanceConfigId: "custom-cb-reset",
		InstanceConfig: &instancepb.InstanceConfig{Name: custom, DisplayName: "CB reset", BaseConfig: base.GetName(),
			Replicas: append(append([]*instancepb.ReplicaInfo{}, base.GetReplicas()...), &instancepb.ReplicaInfo{Location: "us-east1", Type: instancepb.ReplicaInfo_READ_ONLY})}})
	if err == nil {
		_, err = cop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateInstanceConfig: %v", err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "cb-reset-inst", DisplayName: "CB reset",
		ClusterId: "cb-reset-inst-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	sched, err := fa.CreateBackupSchedule(ctx, &fsadminpb.CreateBackupScheduleRequest{Parent: project + "/databases/(default)", BackupSchedule: &fsadminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &fsadminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &fsadminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}

	if code, body := adminReset(t, control, "service=spanner,bigtable,firestore"); code != 200 {
		t.Fatalf("reset = %d %s", code, body)
	}
	if _, err := f.inst.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: custom}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstanceConfig after the reset = %v, want NotFound", err)
	}
	if _, err := iac.InstanceInfo(ctx, "cb-reset-inst"); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after the reset = %v, want NotFound", err)
	}
	if _, err := fa.GetBackupSchedule(ctx, &fsadminpb.GetBackupScheduleRequest{Name: sched.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBackupSchedule after the reset = %v, want NotFound", err)
	}
}
