package logging

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// fakeLinked is LinkedDatasets in memory: dataset -> rows.
type fakeLinked struct {
	mu       sync.Mutex
	datasets map[string][]map[string]any
	views    map[string][]LinkedView
}

func newFakeLinked() *fakeLinked {
	return &fakeLinked{datasets: map[string][]map[string]any{}, views: map[string][]LinkedView{}}
}

func (f *fakeLinked) Create(_ context.Context, project, dataset, _ string, fields []Field, views []LinkedView) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.datasets[project+":"+dataset]; ok {
		return ErrDatasetExists
	}
	f.datasets[project+":"+dataset] = []map[string]any{}
	f.views[project+":"+dataset] = views
	return nil
}

func (f *fakeLinked) Exists(_ context.Context, project, dataset string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.datasets[project+":"+dataset]
	return ok, nil
}

// viewIDs are the dataset's views.
func (f *fakeLinked) viewIDs(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, v := range f.views[key] {
		out = append(out, v.ID)
	}
	return out
}

// inView are the json_payload.msg of the rows that list view.
func inView(rows []map[string]any, view string) []string {
	var out []string
	for _, r := range rows {
		ids, _ := r[LinkedViewsColumn].([]any)
		if slices.Contains(ids, any(view)) {
			jp, _ := r["json_payload"].(map[string]any)
			msg, _ := jp["msg"].(string)
			out = append(out, msg)
		}
	}
	return out
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
	delete(f.views, project+":"+dataset)
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

// TestLinkedDatasetsFollowTheBucket (#1435): the dataset has a view per log
// view, made, changed and dropped with it; each row lists the views whose
// filter matches it; DeleteLog and a retention change remove rows; a
// dataset BigQuery lost is made again; a folder's bucket cannot have Log
// Analytics, so cannot be linked; a filter CloudBurrow does not evaluate is
// refused on a linked bucket.
func TestLinkedDatasetsFollowTheBucket(t *testing.T) {
	ctx := context.Background()
	a := NewAdmin(store.NewMemory())
	srv := NewServer(NewStore(100))
	srv.UseAdmin(a)
	cfg := a.ConfigServer()
	fake := newFakeLinked()
	a.UseLinkedDatasets(fake)
	global := "projects/proj-1/locations/global"
	bucket := global + "/buckets/la"
	if _, err := cfg.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: "la",
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{
		Name: "to-la", Destination: "logging.googleapis.com/" + bucket}}); err != nil {
		t.Fatal(err)
	}
	write := func(log, msg string, sev ltype.LogSeverity) {
		t.Helper()
		js, _ := structpb.NewStruct(map[string]any{"msg": msg})
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/" + log,
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries:  []*loggingpb.LogEntry{{Severity: sev, Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: js}}}}); err != nil {
			t.Fatal(err)
		}
		srv.waitDelivery()
	}
	write("app", "a1", ltype.LogSeverity_ERROR)
	write("other", "o1", ltype.LogSeverity_INFO)
	if _, err := cfg.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: bucket, ViewId: "errors",
		View: &loggingpb.LogView{Filter: "severity>=ERROR"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: bucket, LinkId: "la_link"}); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	key := "proj-1:la_link"
	if got := strings.Join(fake.viewIDs(key), ","); got != "_AllLogs,errors" {
		t.Errorf("views = %s; want _AllLogs,errors", got)
	}
	for _, v := range fake.views[key] {
		if !strings.Contains(v.Query, "'"+v.ID+"' IN UNNEST(`_cloudburrow_views`)") || !strings.Contains(v.Query, "INTERVAL 30 DAY") {
			t.Errorf("view %s query = %s", v.ID, v.Query)
		}
	}
	check := func(when, view, want string) {
		t.Helper()
		rows, _ := fake.rows(key)
		if got := strings.Join(inView(rows, view), ","); got != want {
			t.Errorf("%s: %s holds %q; want %q", when, view, got, want)
		}
	}
	check("after CreateLink", "_AllLogs", "a1,o1")
	check("after CreateLink", "errors", "a1")
	write("app", "a2", ltype.LogSeverity_CRITICAL)
	write("other", "o2", ltype.LogSeverity_DEBUG)
	check("after writes", "errors", "a1,a2")
	check("after writes", "_AllLogs", "a1,o1,a2,o2")

	if _, err := cfg.UpdateView(ctx, &loggingpb.UpdateViewRequest{Name: bucket + "/views/errors",
		View: &loggingpb.LogView{Filter: "severity<=INFO"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"filter"}}}); err != nil {
		t.Fatal(err)
	}
	check("after UpdateView", "errors", "o1,o2")
	if _, err := cfg.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: bucket, ViewId: "odd",
		View: &loggingpb.LogView{Filter: "SEARCH(\"x\")"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateView with a filter CloudBurrow does not evaluate on a linked bucket = %v; want UNIMPLEMENTED", err)
	}
	if _, err := cfg.DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: bucket + "/views/errors"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.viewIDs(key), ","); got != "_AllLogs" {
		t.Errorf("views after DeleteView = %s", got)
	}

	if _, err := srv.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: "projects/proj-1/logs/other"}); err != nil {
		t.Fatal(err)
	}
	check("after DeleteLog", "_AllLogs", "a1,a2")

	// An entry older than a shorter retention leaves the table.
	old, _ := structpb.NewStruct(map[string]any{"msg": "old"})
	if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/app",
		Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{{
			Timestamp: timestamppb.New(time.Now().Add(-5 * 24 * time.Hour)), Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: old}}}}); err != nil {
		t.Fatal(err)
	}
	srv.waitDelivery()
	check("with an entry 5 days old", "_AllLogs", "a1,a2,old")
	if _, err := cfg.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: bucket, Bucket: &loggingpb.LogBucket{RetentionDays: 3},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}}); err != nil {
		t.Fatal(err)
	}
	check("after a 3-day retention", "_AllLogs", "a1,a2")
	if q := fake.views[key][0].Query; !strings.Contains(q, "INTERVAL 3 DAY") {
		t.Errorf("_AllLogs after a 3-day retention = %s", q)
	}

	// BigQuery lost the dataset (an emulator restart): it is made again.
	_ = fake.Delete(ctx, "proj-1", "la_link")
	if n := a.EnsureLinkedDatasets(ctx); n != 1 {
		t.Errorf("EnsureLinkedDatasets made %d; want 1", n)
	}
	check("after the dataset was made again", "_AllLogs", "a1,a2")
	if n := a.EnsureLinkedDatasets(ctx); n != 0 {
		t.Errorf("EnsureLinkedDatasets with every dataset there made %d; want 0", n)
	}

	// Only a project's bucket can have Log Analytics.
	a.UseHierarchy(fakeHierarchy{})
	folder := "folders/10/locations/global/buckets/_Default"
	if _, err := cfg.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: folder, Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"analytics_enabled"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Log Analytics on a folder's bucket = %v; want FAILED_PRECONDITION", err)
	}
	for _, p := range []string{folder, "organizations/1/locations/global/buckets/_Default"} {
		if _, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: p, LinkId: "x"}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("CreateLink on %s = %v; want FAILED_PRECONDITION", p, err)
		}
	}
}

// clockLinked is a fakeLinked with BigQuery's clock, which earlier tests
// on an instance may have moved ahead of the Logging clock.
type clockLinked struct {
	*fakeLinked
	bqMu sync.Mutex
	bq   time.Time
}

func (c *clockLinked) Now(context.Context) (time.Time, error) {
	c.bqMu.Lock()
	defer c.bqMu.Unlock()
	return c.bq, nil
}

func (c *clockLinked) advance(d time.Duration) {
	c.bqMu.Lock()
	c.bq = c.bq.Add(d)
	c.bqMu.Unlock()
}

// TestLinkedDatasetsRetentionByTheLoggingClock (#1602): a linked dataset's
// views keep the bucket's retention by the Logging clock, which a
// timestamp is measured against, and not by BigQuery's: with BigQuery's
// clock 49 hours ahead, a 2-day retention still holds the entries just
// written. When either clock moves, the views are made again.
func TestLinkedDatasetsRetentionByTheLoggingClock(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	a := NewAdmin(store.NewMemory())
	a.now = func() time.Time { return start }
	srv := NewServer(NewStore(100))
	srv.now = a.now
	srv.UseAdmin(a)
	cfg := a.ConfigServer()
	fake := &clockLinked{fakeLinked: newFakeLinked(), bq: start.Add(49 * time.Hour)}
	a.UseLinkedDatasets(fake)
	global := "projects/proj-1/locations/global"
	bucket := global + "/buckets/la"
	if _, err := cfg.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: global, BucketId: "la",
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{
		Name: "to-la", Destination: "logging.googleapis.com/" + bucket}}); err != nil {
		t.Fatal(err)
	}
	write := func(msg string, at time.Time) {
		t.Helper()
		js, _ := structpb.NewStruct(map[string]any{"msg": msg})
		e := &loggingpb.LogEntry{Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: js}}
		if !at.IsZero() {
			e.Timestamp = timestamppb.New(at)
		}
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/app",
			Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{e}}); err != nil {
			t.Fatal(err)
		}
		srv.waitDelivery()
	}
	write("old", start.Add(-3*24*time.Hour))
	write("e1", time.Time{})
	if _, err := cfg.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: bucket, LinkId: "la_link"}); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	key := "proj-1:la_link"
	query := func() string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.views[key][0].Query
	}
	// The Logging clock, CURRENT_TIMESTAMP() less BigQuery's 49 hours.
	logNow := "TIMESTAMP_SUB(TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 176400 SECOND), INTERVAL "
	if q := query(); !strings.Contains(q, logNow+"30 DAY)") {
		t.Errorf("_AllLogs = %s; want its retention by the Logging clock", q)
	}
	if _, err := cfg.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: bucket, Bucket: &loggingpb.LogBucket{RetentionDays: 2},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}}); err != nil {
		t.Fatal(err)
	}
	if q := query(); !strings.Contains(q, logNow+"2 DAY)") {
		t.Errorf("_AllLogs after a 2-day retention = %s", q)
	}
	rows, _ := fake.rows(key)
	if got := strings.Join(inView(rows, "_AllLogs"), ","); got != "e1" {
		t.Errorf("after a 2-day retention, _AllLogs holds %q; want e1", got)
	}
	if n := a.EnsureLinkedDatasets(ctx); n != 0 {
		t.Errorf("EnsureLinkedDatasets with neither clock moved made %d; want 0", n)
	}

	// BigQuery's clock moves an hour: the views follow.
	fake.advance(time.Hour)
	if n := a.EnsureLinkedDatasets(ctx); n != 1 {
		t.Errorf("EnsureLinkedDatasets after BigQuery's clock moved made %d; want 1", n)
	}
	if q := query(); !strings.Contains(q, "INTERVAL 180000 SECOND") {
		t.Errorf("_AllLogs after BigQuery's clock moved = %s", q)
	}
	// The Logging clock moves 3 days: e1 is past the retention, and the
	// views measure from the moved clock.
	srv.Advance(ctx, 3*24*time.Hour)
	if q := query(); !strings.Contains(q, "INTERVAL -79200 SECOND") {
		t.Errorf("_AllLogs after the Logging clock moved = %s", q)
	}
	rows, _ = fake.rows(key)
	if got := strings.Join(inView(rows, "_AllLogs"), ","); got != "" {
		t.Errorf("after the Logging clock moved past the retention, _AllLogs holds %q; want none", got)
	}
}
