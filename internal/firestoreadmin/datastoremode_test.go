package firestoreadmin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// fakeDatastoreFront is the Datastore front's copy paths over entities
// kept by database name.
type fakeDatastoreFront struct {
	mu       sync.Mutex
	entities map[string][][]byte
	calls    []string
}

func (f *fakeDatastoreFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	db := "projects/" + r.URL.Query().Get("project") + "/databases/" + r.URL.Query().Get("database")
	f.calls = append(f.calls, r.URL.Path+" "+db)
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case frontstate.DatastoreDumpPath:
		_, _ = w.Write(frontstate.Pack(f.entities[db]...))
	case frontstate.DatastoreLoadPath:
		parts, _ := frontstate.Unpack(body)
		f.entities[db] = append(f.entities[db], parts...)
		_, _ = w.Write([]byte(`{"entities":1}`))
	case frontstate.DatastoreExportPath:
		var req datastoreCopyRequest
		_ = json.Unmarshal(body, &req)
		if !req.Exact || req.OutputURIPrefix != "gs://bkt/x" || len(req.Namespaces) != 1 || req.Namespaces[0] != "ns" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":3,"message":"unexpected export request"}`))
			return
		}
		_, _ = w.Write([]byte(`{"entities":2,"bytes":20,"outputUri":"gs://bkt/x/x.overall_export_metadata"}`))
	case frontstate.DatastoreImportPath, frontstate.DatastoreDeletePath:
		_, _ = w.Write([]byte(`{"entities":2,"bytes":20}`))
	default:
		http.NotFound(w, r)
	}
}

func TestDatastoreModeCopiesGoThroughTheDatastoreFront(t *testing.T) {
	a, _, _ := cmekAdmin(t)
	front := &fakeDatastoreFront{entities: map[string][][]byte{"projects/p/databases/ds-db": {[]byte("e1"), []byte("e2")}}}
	srv := httptest.NewServer(front)
	defer srv.Close()
	a.SetDatastore(strings.TrimPrefix(srv.URL, "http://"))
	if err := create(a, "ds-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const db = "projects/p/databases/ds-db"

	// An export is the Datastore front's, in the named folder; namespaces
	// are allowed.
	op, err := a.ExportDocuments(ctx, &adminpb.ExportDocumentsRequest{Name: db, OutputUriPrefix: "gs://bkt/x", NamespaceIds: []string{"ns"}})
	if err != nil {
		t.Fatalf("ExportDocuments: %v", err)
	}
	resp := &adminpb.ExportDocumentsResponse{}
	if err := op.GetResponse().UnmarshalTo(resp); err != nil || resp.OutputUriPrefix != "gs://bkt/x" {
		t.Errorf("ExportDocuments response = %v, %v", resp, err)
	}
	if _, err := a.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: db, InputUriPrefix: "gs://bkt/x"}); err != nil {
		t.Errorf("ImportDocuments: %v", err)
	}
	if _, err := a.BulkDeleteDocuments(ctx, &adminpb.BulkDeleteDocumentsRequest{Name: db, CollectionIds: []string{"Task"}}); err != nil {
		t.Errorf("BulkDeleteDocuments: %v", err)
	}

	// A backup schedule's first backup is a copy of the entities.
	if _, err := a.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db,
		BackupSchedule: &adminpb.BackupSchedule{Retention: durationpb.New(24 * time.Hour),
			Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}}); err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	backups, err := a.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/p/locations/-"})
	if err != nil || len(backups.Backups) != 1 || backups.Backups[0].GetStats().GetDocumentCount() != 2 {
		t.Fatalf("ListBackups = %v, %v", backups, err)
	}

	// A restore and a clone are Datastore mode databases holding the
	// entities.
	if _, err := a.RestoreDatabase(ctx, &adminpb.RestoreDatabaseRequest{Parent: "projects/p", DatabaseId: "restored",
		Backup: backups.Backups[0].Name}); err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}
	if _, err := a.CloneDatabase(ctx, &adminpb.CloneDatabaseRequest{Parent: "projects/p", DatabaseId: "cloned",
		PitrSnapshot: &adminpb.PitrSnapshot{Database: db, SnapshotTime: timestamppb.New(a.now())}}); err != nil {
		t.Fatalf("CloneDatabase: %v", err)
	}
	for _, id := range []string{"restored", "cloned"} {
		got, err := a.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/p/databases/" + id})
		if err != nil || got.Type != adminpb.Database_DATASTORE_MODE {
			t.Errorf("%s = %v, %v; want a Datastore mode database", id, got, err)
		}
		front.mu.Lock()
		n := len(front.entities["projects/p/databases/"+id])
		front.mu.Unlock()
		if n != 2 {
			t.Errorf("%s holds %d entities, want 2", id, n)
		}
	}
}

func TestDatastoreModeCopiesNeedDatastore(t *testing.T) {
	a, _, _ := cmekAdmin(t)
	if err := create(a, "ds-db", &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExportDocuments(context.Background(), &adminpb.ExportDocumentsRequest{Name: "projects/p/databases/ds-db",
		OutputUriPrefix: "gs://bkt/x"}); err == nil || !strings.Contains(err.Error(), "Datastore is not served") {
		t.Errorf("ExportDocuments without Datastore: %v", err)
	}
}
