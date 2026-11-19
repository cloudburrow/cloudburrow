//go:build compat

package compat

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	dsadmin "cloud.google.com/go/datastore/admin/apiv1"
	dsadminpb "cloud.google.com/go/datastore/admin/apiv1/adminpb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Exports, imports, backups, restores and clones of a Datastore mode
// database (#1884): the Datastore Admin API, which the Datastore pod's
// front serves, exports and imports entities with entity filters through
// the instance's Cloud Storage, in Google's export layout; the Firestore
// Admin API's exports, imports, bulk deletes, backups, restores and clones
// of a Datastore mode database copy its entities through that front.

func datastoreAdminClient(t *testing.T, h *Harness) *dsadmin.DatastoreAdminClient {
	t.Helper()
	c, err := dsadmin.NewDatastoreAdminClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvDatastore)),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewDatastoreAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// inDatabase is ctx with the routing header naming the database, as
// Google's clients send for a named database.
func inDatabase(ctx context.Context, project, database string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-goog-request-params", "project_id="+project+"&database_id="+database)
}

type dsTask struct{ V string }

// seedKeys are the entities seedDatastore writes.
func seedKeys() []*datastore.Key {
	k3 := datastore.NameKey("Task", "t3", nil)
	k3.Namespace = "tenant"
	return []*datastore.Key{datastore.NameKey("Task", "t1", nil), datastore.NameKey("Task", "t2", nil), k3, datastore.NameKey("Note", "n1", nil)}
}

// entityNames is "namespace/Kind/name" of each of seedKeys the database
// holds, sorted, read by Lookup: the emulator's queries answer every
// database's entities, and its lookups the named database's alone.
func entityNames(t *testing.T, ctx context.Context, c *datastore.Client) []string {
	t.Helper()
	var out []string
	for _, k := range seedKeys() {
		var e dsTask
		err := c.Get(ctx, k, &e)
		if err == datastore.ErrNoSuchEntity {
			continue
		}
		if err != nil {
			t.Fatalf("get %v: %v", k, err)
		}
		out = append(out, k.Namespace+"/"+k.Kind+"/"+k.Name)
	}
	sort.Strings(out)
	return out
}

func seedDatastore(t *testing.T, ctx context.Context, c *datastore.Client) {
	t.Helper()
	if _, err := c.PutMulti(ctx, seedKeys(), []*dsTask{{"1"}, {"2"}, {"3"}, {"n"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func clearDatastore(t *testing.T, ctx context.Context, c *datastore.Client) {
	t.Helper()
	if err := c.DeleteMulti(ctx, seedKeys()); err != nil {
		t.Fatal(err)
	}
}

func objectNames(t *testing.T, ctx context.Context, bh *storage.BucketHandle, prefix string) []string {
	t.Helper()
	var out []string
	it := bh.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimPrefix(a.Name, prefix))
	}
}

const allEntities = "/Note/n1,/Task/t1,/Task/t2,tenant/Task/t3"

// TestDatastoreAdminExportImport (#1884): the official Datastore Admin
// client exports a Datastore mode database, whole and with an entity
// filter, in Google's layout, and imports an export back, whole and
// filtered; the official Datastore client reads each result. Its indexes
// are created, listed, read and deleted.
//
// covers: google.datastore.admin.v1.DatastoreAdmin/ExportEntities, google.datastore.admin.v1.DatastoreAdmin/ImportEntities
// covers: google.datastore.admin.v1.DatastoreAdmin/CreateIndex, google.datastore.admin.v1.DatastoreAdmin/GetIndex, google.datastore.admin.v1.DatastoreAdmin/ListIndexes, google.datastore.admin.v1.DatastoreAdmin/DeleteIndex
func TestDatastoreAdminExportImport(t *testing.T) {
	h := New(t)
	dsAddr := h.Endpoint(EnvDatastore)
	sc := firestoreStorage(t, h)
	ac := datastoreAdminClient(t, h)
	ctx := h.Context()
	project := h.Project()
	id := uniqueDB("dsexp")
	if _, err := createDB(t, h, id, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	bh := bucket(t, h, sc)
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	seedDatastore(t, ctx, dc)
	actx := inDatabase(ctx, project, id)

	op, err := ac.ExportEntities(actx, &dsadminpb.ExportEntitiesRequest{ProjectId: project, OutputUrlPrefix: "gs://" + bh.BucketName() + "/all"})
	if err != nil {
		t.Fatalf("ExportEntities: %v", err)
	}
	all, err := op.Wait(actx)
	if err != nil {
		t.Fatalf("ExportEntities wait: %v", err)
	}
	if !strings.HasPrefix(all.GetOutputUrl(), "gs://"+bh.BucketName()+"/all/") || !strings.HasSuffix(all.GetOutputUrl(), ".overall_export_metadata") {
		t.Fatalf("output_url = %q", all.GetOutputUrl())
	}
	if md, err := op.Metadata(); err != nil || md.GetProgressEntities().GetWorkCompleted() != 4 ||
		md.GetCommon().GetState() != dsadminpb.CommonMetadata_SUCCESSFUL {
		t.Errorf("export metadata = %v, %v", md, err)
	}
	folder := strings.TrimPrefix(all.GetOutputUrl()[:strings.LastIndex(all.GetOutputUrl(), "/")+1], "gs://"+bh.BucketName()+"/")
	got := strings.Join(objectNames(t, ctx, bh, folder), ",")
	stamp := strings.TrimSuffix(folder[len("all/"):], "/")
	if want := stamp + ".overall_export_metadata,all_namespaces/all_kinds/all_namespaces_all_kinds.export_metadata,all_namespaces/all_kinds/output-0"; got != want {
		t.Errorf("export files = %s, want %s", got, want)
	}

	op, err = ac.ExportEntities(actx, &dsadminpb.ExportEntitiesRequest{ProjectId: project, OutputUrlPrefix: "gs://" + bh.BucketName() + "/tasks",
		EntityFilter: &dsadminpb.EntityFilter{Kinds: []string{"Task"}, NamespaceIds: []string{""}}})
	if err != nil {
		t.Fatalf("filtered ExportEntities: %v", err)
	}
	tasks, err := op.Wait(actx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(objectNames(t, ctx, bh, "tasks/"), ","); !strings.Contains(got, "/default_namespace/kind_Task/default_namespace_kind_Task.export_metadata") {
		t.Errorf("the filtered export's files = %s", got)
	}

	clearDatastore(t, ctx, dc)
	op2, err := ac.ImportEntities(actx, &dsadminpb.ImportEntitiesRequest{ProjectId: project, InputUrl: all.GetOutputUrl(),
		EntityFilter: &dsadminpb.EntityFilter{Kinds: []string{"Task"}}})
	if err != nil {
		t.Fatalf("filtered ImportEntities: %v", err)
	}
	if err := op2.Wait(actx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entityNames(t, ctx, dc), ","); got != "/Task/t1,/Task/t2,tenant/Task/t3" {
		t.Errorf("after the Task import: %s", got)
	}
	op2, err = ac.ImportEntities(actx, &dsadminpb.ImportEntitiesRequest{ProjectId: project, InputUrl: all.GetOutputUrl()})
	if err != nil {
		t.Fatalf("ImportEntities: %v", err)
	}
	if err := op2.Wait(actx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entityNames(t, ctx, dc), ","); got != allEntities {
		t.Errorf("after the whole import: %s, want %s", got, allEntities)
	}
	var e dsTask
	if err := dc.Get(ctx, datastore.NameKey("Task", "t2", nil), &e); err != nil || e.V != "2" {
		t.Errorf("an imported entity = %v, %v", e, err)
	}
	// The filtered export holds the default namespace's Tasks alone.
	clearDatastore(t, ctx, dc)
	if op2, err = ac.ImportEntities(actx, &dsadminpb.ImportEntitiesRequest{ProjectId: project, InputUrl: tasks.GetOutputUrl()}); err != nil {
		t.Fatal(err)
	}
	if err := op2.Wait(actx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entityNames(t, ctx, dc), ","); got != "/Task/t1,/Task/t2" {
		t.Errorf("after importing the filtered export: %s", got)
	}
	if _, err := ac.ImportEntities(actx, &dsadminpb.ImportEntitiesRequest{ProjectId: project,
		InputUrl: "gs://" + bh.BucketName() + "/missing/missing.overall_export_metadata"}); err == nil {
		t.Error("an import of a missing export was accepted")
	}

	// Indexes.
	iop, err := ac.CreateIndex(actx, &dsadminpb.CreateIndexRequest{ProjectId: project, Index: &dsadminpb.Index{Kind: "Task",
		Ancestor: dsadminpb.Index_NONE, Properties: []*dsadminpb.Index_IndexedProperty{
			{Name: "V", Direction: dsadminpb.Index_ASCENDING}, {Name: "done", Direction: dsadminpb.Index_DESCENDING}}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	ix, err := iop.Wait(actx)
	if err != nil || ix.GetState() != dsadminpb.Index_READY || ix.GetIndexId() == "" {
		t.Fatalf("created index = %v, %v", ix, err)
	}
	if got, err := ac.GetIndex(actx, &dsadminpb.GetIndexRequest{ProjectId: project, IndexId: ix.GetIndexId()}); err != nil || got.GetKind() != "Task" {
		t.Errorf("GetIndex = %v, %v", got, err)
	}
	it := ac.ListIndexes(actx, &dsadminpb.ListIndexesRequest{ProjectId: project})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListIndexes: %v", err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("ListIndexes lists %d, want 1", n)
	}
	// (default) has none.
	if it := ac.ListIndexes(ctx, &dsadminpb.ListIndexesRequest{ProjectId: project}); true {
		if _, err := it.Next(); err != iterator.Done {
			t.Errorf("(default)'s indexes: %v, want none", err)
		}
	}
	dop, err := ac.DeleteIndex(actx, &dsadminpb.DeleteIndexRequest{ProjectId: project, IndexId: ix.GetIndexId()})
	if err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
	if _, err := dop.Wait(actx); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.GetIndex(actx, &dsadminpb.GetIndexRequest{ProjectId: project, IndexId: ix.GetIndexId()}); err == nil {
		t.Error("the deleted index is still served")
	}
}

// TestFirestoreAdminCopiesADatastoreModeDatabase (#1884): the Firestore
// Admin API exports a Datastore mode database's entities and imports them
// into another, bulk deletes a kind, backs it up with a schedule, restores
// the backup and clones the database; the official Datastore client reads
// each copy, and each new database is a Datastore mode one.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/ExportDocuments, google.firestore.admin.v1.FirestoreAdmin/ImportDocuments, google.firestore.admin.v1.FirestoreAdmin/BulkDeleteDocuments
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateBackupSchedule, google.firestore.admin.v1.FirestoreAdmin/ListBackups, google.firestore.admin.v1.FirestoreAdmin/RestoreDatabase, google.firestore.admin.v1.FirestoreAdmin/CloneDatabase
func TestFirestoreAdminCopiesADatastoreModeDatabase(t *testing.T) {
	h := New(t)
	dsAddr := h.Endpoint(EnvDatastore)
	sc := firestoreStorage(t, h)
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	project := h.Project()
	src, dst := uniqueDB("dssrc"), uniqueDB("dsdst")
	srcDB, err := createDB(t, h, src, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createDB(t, h, dst, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	bh := bucket(t, h, sc)
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	client := func(id string) *datastore.Client {
		dc, err := datastore.NewClientWithDatabase(ctx, project, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dc.Close() })
		return dc
	}
	sdc, ddc := client(src), client(dst)
	seedDatastore(t, ctx, sdc)

	prefix := "gs://" + bh.BucketName() + "/fsexport"
	op, err := c.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: srcDB.GetName(), OutputUriPrefix: prefix})
	if err != nil {
		t.Fatalf("ExportDocuments: %v", err)
	}
	res, err := op.Wait(ctx)
	if err != nil || res.GetOutputUriPrefix() != prefix {
		t.Fatalf("ExportDocuments = %v, %v", res, err)
	}
	if got := strings.Join(objectNames(t, ctx, bh, "fsexport/"), ","); got != "all_namespaces/all_kinds/all_namespaces_all_kinds.export_metadata,all_namespaces/all_kinds/output-0,fsexport.overall_export_metadata" {
		t.Errorf("the export's files = %s", got)
	}
	iop, err := c.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: "projects/" + project + "/databases/" + dst, InputUriPrefix: prefix})
	if err != nil {
		t.Fatalf("ImportDocuments: %v", err)
	}
	if err := iop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entityNames(t, ctx, ddc), ","); got != allEntities {
		t.Errorf("the imported database: %s, want %s", got, allEntities)
	}
	bop, err := c.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: "projects/" + project + "/databases/" + dst, CollectionIds: []string{"Task"}})
	if err != nil {
		t.Fatalf("BulkDeleteDocuments: %v", err)
	}
	if _, err := bop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entityNames(t, ctx, ddc), ","); got != "/Note/n1" {
		t.Errorf("after the bulk delete of Task: %s", got)
	}

	s, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: srcDB.GetName(), BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: s.Name}) })
	list, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil {
		t.Fatal(err)
	}
	var b *adminpb.Backup
	for _, x := range list.GetBackups() {
		if x.GetDatabase() == srcDB.GetName() {
			b = x
		}
	}
	if b == nil || b.GetStats().GetDocumentCount() != 4 {
		t.Fatalf("the source's backup = %v (of %v)", b, list.GetBackups())
	}
	restored, cloned := uniqueDB("dsrest"), uniqueDB("dsclon")
	rop, err := c.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: "projects/" + project, DatabaseId: restored, Backup: b.GetName()})
	if err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	cop, err := c.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: "projects/" + project, DatabaseId: cloned,
		PitrSnapshot: &adminpb.PitrSnapshot{Database: srcDB.GetName(), SnapshotTime: timestamppb.New(time.Now().UTC().Truncate(time.Minute))}})
	if err != nil {
		t.Fatalf("CloneDatabase: %v", err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{restored, cloned} {
		name := "projects/" + project + "/databases/" + id
		t.Cleanup(func() {
			if dop, err := c.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: name}); err == nil {
				_, _ = dop.Wait(context.Background())
			}
		})
		db, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: name})
		if err != nil || db.GetType() != adminpb.Database_DATASTORE_MODE {
			t.Errorf("%s = %v, %v; want a Datastore mode database", id, db, err)
		}
		if got := strings.Join(entityNames(t, ctx, client(id)), ","); got != allEntities {
			t.Errorf("%s holds %s, want %s", id, got, allEntities)
		}
	}
}

// TestGcloudDatastoreExportImport (#1884), in the emulators shard: gcloud
// datastore, configured by `cloudburrow gcloud-setup` alone (its datastore
// override), exports a kind of (default) with --kinds and --namespaces to
// the instance's Cloud Storage and imports the export back; the official
// Datastore client reads the result.
//
// covers: google.datastore.admin.v1.DatastoreAdmin/ExportEntities, google.datastore.admin.v1.DatastoreAdmin/ImportEntities
func TestGcloudDatastoreExportImport(t *testing.T) {
	h := New(t)
	dsAddr := h.Endpoint(EnvDatastore)
	sc := firestoreStorage(t, h)
	ctx := h.Context()
	project := h.Project()
	bh := bucket(t, h, sc)
	gc := gcloudAdminSession(t, project, "datastore")
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	kind := "GcloudExport" + strings.ReplaceAll(uniqueDB("k"), "-", "")
	keys := []*datastore.Key{datastore.NameKey(kind, "a", nil), datastore.NameKey(kind, "b", nil)}
	if _, err := dc.PutMulti(ctx, keys, []*dsTask{{"a"}, {"b"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.DeleteMulti(context.Background(), keys) })
	out := gc("datastore", "export", "gs://"+bh.BucketName()+"/gcloud", "--kinds", kind, "--namespaces", "(default)",
		"--format=value(response.outputUrl)")
	url := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
	if !strings.HasSuffix(url, ".overall_export_metadata") {
		t.Fatalf("gcloud datastore export printed %q", out)
	}
	if err := dc.DeleteMulti(ctx, keys); err != nil {
		t.Fatal(err)
	}
	gc("datastore", "import", url)
	var got []dsTask
	if _, err := dc.GetAll(ctx, datastore.NewQuery(kind), &got); err != nil || len(got) != 2 {
		t.Errorf("after gcloud's import: %v, %v", got, err)
	}
}
