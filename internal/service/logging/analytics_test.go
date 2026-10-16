package logging

import (
	"context"
	"sync"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// fakeLinked is LinkedDatasets in memory: dataset -> rows.
type fakeLinked struct {
	mu       sync.Mutex
	datasets map[string][]map[string]any
}

func newFakeLinked() *fakeLinked { return &fakeLinked{datasets: map[string][]map[string]any{}} }

func (f *fakeLinked) Create(_ context.Context, project, dataset, _ string, fields []Field) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.datasets[project+":"+dataset]; ok {
		return ErrDatasetExists
	}
	f.datasets[project+":"+dataset] = []map[string]any{}
	return nil
}

func (f *fakeLinked) Append(_ context.Context, project, dataset string, rows []map[string]any, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.datasets[project+":"+dataset] = append(f.datasets[project+":"+dataset], rows...)
	return nil
}

func (f *fakeLinked) Delete(_ context.Context, project, dataset string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.datasets, project+":"+dataset)
	return nil
}

func (f *fakeLinked) rows(key string) ([]map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.datasets[key]
	return append([]map[string]any(nil), r...), ok
}

// TestLinkedDatasets (#1423): CreateLink makes the dataset with the
// bucket's entries so far, entries written after it are appended, a link
// without BigQuery, on a bucket without Log Analytics or of an existing
// dataset is refused, and DeleteLink deletes the dataset.
func TestLinkedDatasets(t *testing.T) {
	ctx := context.Background()
	a := NewAdmin(store.NewMemory())
	srv := NewServer(NewStore(100))
	srv.UseAdmin(a)
	cfg := a.ConfigServer()
	bucket := "projects/proj-1/locations/global/buckets/_Default"
	link := func(id string) error {
		_, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: bucket, LinkId: id})
		return err
	}
	write := func(text string) {
		t.Helper()
		js, _ := structpb.NewStruct(map[string]any{"msg": text, "n": 2})
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/app%2Fweb",
			Resource: &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "proj-1"}},
			Entries: []*loggingpb.LogEntry{{Severity: ltype.LogSeverity_WARNING, Labels: map[string]string{"k": "v"},
				Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: js}}}}); err != nil {
			t.Fatal(err)
		}
		srv.waitDelivery()
	}
	write("before")
	if err := link("l1"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateLink without BigQuery = %v; want FAILED_PRECONDITION", err)
	}
	fake := newFakeLinked()
	a.UseLinkedDatasets(fake)
	if err := link("l1"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateLink on _Default without Log Analytics = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := cfg.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: bucket, Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"analytics_enabled"}}}); err != nil {
		t.Fatal(err)
	}
	if err := link("my-link"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := link("my-link"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("second CreateLink = %v; want ALREADY_EXISTS", err)
	}
	if err := link("my_link"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateLink of an existing dataset = %v; want ALREADY_EXISTS", err)
	}
	rows, ok := fake.rows("proj-1:my_link")
	if !ok || len(rows) != 1 {
		t.Fatalf("rows after CreateLink = %v, %v; want the entry written before", rows, ok)
	}
	r := rows[0]
	if r["severity"] != "WARNING" || r["severity_number"] != int64(400) || r["log_id"] != "app/web" || r["log_name"] != "projects/proj-1/logs/app%2Fweb" {
		t.Errorf("row = %v", r)
	}
	if jp, _ := r["json_payload"].(map[string]any); jp["msg"] != "before" {
		t.Errorf("json_payload = %v", r["json_payload"])
	}
	if res, _ := r["resource"].(map[string]any); res["type"] != "global" {
		t.Errorf("resource = %v", r["resource"])
	}
	write("after")
	if rows, _ := fake.rows("proj-1:my_link"); len(rows) != 2 || rows[1]["json_payload"].(map[string]any)["msg"] != "after" {
		t.Fatalf("rows after a write = %v", rows)
	}
	if _, err := cfg.DeleteLink(ctx, &loggingpb.DeleteLinkRequest{Name: bucket + "/links/my-link"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.rows("proj-1:my_link"); ok {
		t.Error("DeleteLink left the dataset")
	}
}
