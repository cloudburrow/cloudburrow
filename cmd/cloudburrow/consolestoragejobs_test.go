package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// fakeCompanions is a stand-in for the three companion APIs and Cloud
// Storage's bucket list, answering the JSON the screens read, and
// recording each call ("METHOD path?query" and its body).
type fakeCompanions struct {
	mu    sync.Mutex
	calls []string
	jobs  map[string]map[string]any
}

func (f *fakeCompanions) record(r *http.Request) map[string]any {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, line+" "+string(body))
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	return m
}

func (f *fakeCompanions) called(prefix string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return c, true
		}
	}
	return "", false
}

func (f *fakeCompanions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	p := r.URL.Path
	switch {
	case p == "/storage/v1/b":
		write(map[string]any{"items": []any{map[string]any{"name": "src", "location": "US-CENTRAL1"},
			map[string]any{"name": "dst", "location": "US-CENTRAL1"}}})
	case p == "/cloudburrow/v1/clock":
		write(map[string]any{"now": "2026-11-19T10:00:00Z"})
	case p == "/cloudburrow/v1/clock:advance":
		write(map[string]any{"now": "2026-11-20T10:00:00Z"})
	// Storage Batch Operations.
	case p == "/v1/projects/p1/locations/global/jobs" && r.Method == http.MethodGet:
		write(map[string]any{"jobs": []any{f.jobs["j1"]}})
	case p == "/v1/projects/p1/locations/global/jobs" && r.Method == http.MethodPost:
		write(map[string]any{"name": "projects/p1/locations/global/operations/op1"})
	case p == "/v1/projects/p1/locations/global/jobs/j1" && r.Method == http.MethodGet:
		write(f.jobs["j1"])
	case p == "/v1/projects/p1/locations/global/jobs/j1/bucketOperations":
		write(map[string]any{"bucketOperations": []any{map[string]any{"name": "x/bucketOperations/src", "bucketName": "src",
			"state": "RUNNING", "counters": map[string]any{"totalObjectCount": "3", "succeededObjectCount": "1"}}}})
	case strings.HasPrefix(p, "/v1/projects/p1/locations/global/jobs/j1"):
		write(map[string]any{})
	// Storage Insights.
	case p == "/v1/projects/p1/locations/us-central1/reportConfigs" && r.Method == http.MethodGet:
		write(map[string]any{"reportConfigs": []any{map[string]any{
			"name": "projects/p1/locations/us-central1/reportConfigs/c1", "displayName": "nightly",
			"frequencyOptions": map[string]any{"frequency": "DAILY", "startDate": map[string]any{"year": 2026, "month": 11, "day": 19},
				"endDate": map[string]any{"year": 2026, "month": 12, "day": 1}},
			"csvOptions": map[string]any{"delimiter": ",", "headerRequired": true},
			"objectMetadataReportOptions": map[string]any{"metadataFields": []string{"project", "bucket", "name"},
				"storageFilters":            map[string]any{"bucket": "src"},
				"storageDestinationOptions": map[string]any{"bucket": "dst", "destinationPath": "reports"}}}}})
	case p == "/v1/projects/p1/locations/us-central1/reportConfigs" && r.Method == http.MethodPost:
		write(map[string]any{"name": "projects/p1/locations/us-central1/reportConfigs/c2"})
	case p == "/v1/projects/p1/locations/us-central1/reportConfigs/c1/reportDetails":
		write(map[string]any{"reportDetails": []any{map[string]any{"name": "x/reportDetails/d1", "shardsCount": "1",
			"reportPathPrefix": "gs://dst/reports/c1_2026-11-19T00:00", "status": map[string]any{},
			"targetDatetime": map[string]any{"year": 2026, "month": 11, "day": 19},
			"reportMetrics":  map[string]any{"processedRecordsCount": "4"}}}})
	case strings.HasPrefix(p, "/v1/projects/p1/locations/us-central1/reportConfigs/c1"):
		write(map[string]any{})
	// Storage Transfer Service.
	case p == "/v1/transferJobs" && r.Method == http.MethodGet:
		write(map[string]any{"transferJobs": []any{map[string]any{"name": "transferJobs/123", "projectId": "p1",
			"status": "ENABLED", "latestOperationName": "transferOperations/transferJobs-123-1",
			"transferSpec": map[string]any{"gcsDataSource": map[string]any{"bucketName": "src"},
				"gcsDataSink": map[string]any{"bucketName": "dst", "path": "copy/"}}}}})
	case p == "/v1/transferJobs" && r.Method == http.MethodPost:
		write(map[string]any{"name": "transferJobs/456"})
	case p == "/v1/transferJobs/123" && r.Method == http.MethodGet:
		write(map[string]any{"name": "transferJobs/123", "projectId": "p1", "status": "ENABLED"})
	case p == "/v1/transferOperations" || p == "/v1/transferOperations/transferJobs-123-1":
		op := map[string]any{"name": "transferOperations/transferJobs-123-1", "metadata": map[string]any{
			"status": "IN_PROGRESS", "transferJobName": "transferJobs/123",
			"counters": map[string]any{"objectsFoundFromSource": "5", "objectsCopiedToSink": "2"}}}
		if p == "/v1/transferOperations" {
			write(map[string]any{"operations": []any{op}})
		} else {
			write(op)
		}
	case p == "/v1/projects/p1/agentPools" && r.Method == http.MethodGet:
		write(map[string]any{"agentPools": []any{map[string]any{"name": "projects/p1/agentPools/transfer_service_default",
			"state": "CREATED", "bandwidthLimit": map[string]any{"limitMbps": "1"}}}})
	default:
		_ = body
		write(map[string]any{})
	}
}

func newFakeCompanions(t *testing.T) (*fakeCompanions, string) {
	t.Helper()
	f := &fakeCompanions{jobs: map[string]map[string]any{"j1": {
		"name": "projects/p1/locations/global/jobs/j1", "state": "RUNNING", "createTime": "2026-11-19T10:00:00Z",
		"bucketList":   map[string]any{"buckets": []any{map[string]any{"bucket": "src", "prefixList": map[string]any{"includedObjectPrefixes": []string{"logs/"}}}}},
		"deleteObject": map[string]any{"permanentObjectDeletionEnabled": false},
		"counters":     map[string]any{"totalObjectCount": "3", "succeededObjectCount": "1", "failedObjectCount": "1"},
		"errorSummaries": []any{map[string]any{"errorCode": "PERMISSION_DENIED", "errorCount": "1",
			"errorLogEntries": []any{map[string]any{"objectUri": "gs://src/logs/held", "errorDetails": []string{"object is under hold"}}}}},
	}}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, strings.TrimPrefix(srv.URL, "http://")
}

func TestConsoleStorageBatchOperations(t *testing.T) {
	ctx := context.Background()
	f, addr := newFakeCompanions(t)
	p := storageBatchProvider{endpoint: addr}
	l, err := p.List(ctx, "p1")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if r := l.Items[0]; r.Name != "j1" || r.Status != "RUNNING" || r.Fields["Operation"] != "Delete objects" || r.Fields["Objects"] != "1 of 3" {
		t.Errorf("row = %+v", r)
	}
	if a := p.Actions(l.Items[0]); len(a) != 1 || a[0].ID != actBatchCancel {
		t.Errorf("a running job's actions = %+v, want Cancel", a)
	}
	d, err := p.Detail(ctx, "p1", []string{"j1"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 3 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	if bops := d.Sections[1].Listing.Items; len(bops) != 1 || bops[0].Name != "src" {
		t.Errorf("bucket operations = %+v", bops)
	}
	if errs := d.Sections[2].Listing.Items; len(errs) != 1 || errs[0].Fields["Object"] != "gs://src/logs/held" {
		t.Errorf("errors = %+v", errs)
	}
	id, err := p.Create(ctx, "p1", map[string]string{"jobId": "hold-them", "bucket": "gs://src/", "source": "prefixes",
		"prefixes": "a/\nb/", "operation": "hold", "temporaryHold": "SET", "dryRun": "true"})
	if err != nil || id != "hold-them" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	c, _ := f.called("POST /v1/projects/p1/locations/global/jobs?jobId=hold-them")
	for _, want := range []string{`"putObjectHold":{"temporaryHold":"SET"}`, `"includedObjectPrefixes":["a/","b/"]`, `"dryRun":true`} {
		if !strings.Contains(c, want) {
			t.Errorf("create sent %s, want %s in it", c, want)
		}
	}
	if _, err := p.Create(ctx, "p1", map[string]string{"jobId": "Bad_ID", "bucket": "src"}); err == nil {
		t.Error("a job ID with capitals and underscores was accepted")
	}
	if _, err := p.Create(ctx, "p1", map[string]string{"jobId": "meta", "bucket": "src", "operation": "metadata"}); err == nil {
		t.Error("a metadata job with no field was accepted")
	}
	if err := p.ActAt(ctx, "p1", []string{"j1"}, actBatchCancel, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.called("POST /v1/projects/p1/locations/global/jobs/j1:cancel"); !ok {
		t.Error("Cancel did not call jobs.cancel")
	}
	if err := p.Delete(ctx, "p1", "j1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.called("DELETE /v1/projects/p1/locations/global/jobs/j1"); !ok {
		t.Error("Delete did not call jobs.delete")
	}
}

func TestConsoleStorageInventoryReports(t *testing.T) {
	ctx := context.Background()
	f, addr := newFakeCompanions(t)
	p := storageInventoryProvider{endpoint: addr, storage: addr}
	l, err := p.List(ctx, "p1")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if r := l.Items[0]; r.Name != "c1" || r.Fields["Destination"] != "gs://dst/reports/" || r.Fields["Location"] != "us-central1" || r.Fields["Format"] != "CSV" {
		t.Errorf("row = %+v", r)
	}
	d, err := p.Detail(ctx, "p1", []string{"c1"})
	if err != nil || d.Unavailable != "" || d.Edit == nil {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	if h := d.Sections[1].Listing.Items; len(h) != 1 || h[0].Status != "Succeeded" || h[0].Fields["Records"] != "4" {
		t.Errorf("report history = %+v", h)
	}
	id, err := p.Create(ctx, "p1", map[string]string{"sourceBucket": "gs://src/", "destinationBucket": "dst", "destinationPath": "/inv/",
		"format": "parquet", "field.size": "true", "frequency": "WEEKLY", "startDate": "2026-11-19", "endDate": "2026-12-19"})
	if err != nil || id != "c2" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	c, _ := f.called("POST /v1/projects/p1/locations/us-central1/reportConfigs")
	for _, want := range []string{`"parquetOptions":{}`, `"metadataFields":["project","bucket","name","size"]`, `"destinationPath":"inv"`,
		`"frequency":"WEEKLY"`, `"startDate":{"year":2026,"month":11,"day":19}`} {
		if !strings.Contains(c, want) {
			t.Errorf("create sent %s, want %s in it", c, want)
		}
	}
	if _, err := p.Create(ctx, "p1", map[string]string{"sourceBucket": "nope", "destinationBucket": "dst", "startDate": "2026-11-19", "endDate": "2026-11-19"}); err == nil {
		t.Error("a configuration of a bucket the project does not have was accepted")
	}
	if err := p.Edit(ctx, "p1", []string{"c1"}, map[string]string{"displayName": "weekly", "frequency": "WEEKLY",
		"startDate": "2026-11-19", "endDate": "2026-11-18"}); err == nil {
		t.Error("an end date before the start date was accepted")
	}
	if err := p.Edit(ctx, "p1", []string{"c1"}, map[string]string{"displayName": "weekly", "frequency": "WEEKLY",
		"startDate": "2026-11-19", "endDate": "2026-12-19"}); err != nil {
		t.Fatal(err)
	}
	if c, ok := f.called("PATCH /v1/projects/p1/locations/us-central1/reportConfigs/c1?updateMask="); !ok || !strings.Contains(c, `"displayName":"weekly"`) {
		t.Errorf("Edit sent %q", c)
	}
	if err := p.Delete(ctx, "p1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.called("DELETE /v1/projects/p1/locations/us-central1/reportConfigs/c1"); !ok {
		t.Error("Delete did not call reportConfigs.delete")
	}
	if err := p.ActAt(ctx, "p1", nil, actStorageJobsClock, map[string]string{"duration": "1d"}); err != nil {
		t.Fatal(err)
	}
	if c, ok := f.called("POST /cloudburrow/v1/clock:advance"); !ok || !strings.Contains(c, `"duration":"86400s"`) {
		t.Errorf("Advance clock sent %q", c)
	}
}

func TestConsoleStorageTransfer(t *testing.T) {
	ctx := context.Background()
	f, addr := newFakeCompanions(t)
	p := storageTransferProvider{endpoint: addr}
	l, err := p.List(ctx, "p1")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	if r := l.Items[0]; r.Name != "123" || r.Fields["Source"] != "gs://src/" || r.Fields["Destination"] != "gs://dst/copy/" ||
		r.Fields["Last run status"] != "IN_PROGRESS" || r.Fields["Schedule"] != "On demand" {
		t.Errorf("row = %+v", r)
	}
	if c, _ := f.called("GET /v1/transferJobs?"); !strings.Contains(c, "filter=") {
		t.Errorf("transferJobs.list was called without its filter: %q", c)
	}
	d, err := p.Detail(ctx, "p1", []string{"123"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	if runs := d.Sections[0].Listing.Items; len(runs) != 1 || runs[0].Fields["Copied"] != "2" || runs[0].Fields["Objects found"] != "5" {
		t.Errorf("run history = %+v", runs)
	}
	run := []string{"123", "transferJobs-123-1"}
	if a := p.DetailActions(ctx, "p1", run); len(a) != 2 || a[0].ID != actTransferPause || a[1].ID != actTransferCancel {
		t.Errorf("a running operation's actions = %+v", a)
	}
	if err := p.ActAt(ctx, "p1", run, actTransferPause, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.called("POST /v1/transferOperations/transferJobs-123-1:pause"); !ok {
		t.Error("Pause did not call transferOperations.pause")
	}
	if err := p.Act(ctx, "p1", "123", actTransferRun); err != nil {
		t.Fatal(err)
	}
	if c, ok := f.called("POST /v1/transferJobs/123:run"); !ok || !strings.Contains(c, `"projectId":"p1"`) {
		t.Errorf("Start a run sent %q", c)
	}
	if err := p.Act(ctx, "p1", "123", actTransferDisable); err != nil {
		t.Fatal(err)
	}
	if c, ok := f.called("PATCH /v1/transferJobs/123"); !ok || !strings.Contains(c, `"updateTransferJobFieldMask":"status"`) {
		t.Errorf("Disable sent %q", c)
	}
	name, err := p.Create(ctx, "p1", map[string]string{"sourceType": "gcs", "sourceBucket": "gs://src/in/", "sinkType": "gcs",
		"sinkBucket": "gs://dst/", "when": "now", "includePrefixes": "a/", "minAge": "1h", "overwrite": "ALWAYS",
		"deleteMode": "source", "topic": "projects/p1/topics/t", "notifySuccess": "true"})
	if err != nil || name != "456" {
		t.Fatalf("Create = %q, %v", name, err)
	}
	c, _ := f.called("POST /v1/transferJobs ")
	for _, want := range []string{`"gcsDataSource":{"bucketName":"src","path":"in/"}`, `"minTimeElapsedSinceLastModification":"3600s"`,
		`"deleteObjectsFromSourceAfterTransfer":true`, `"overwriteWhen":"ALWAYS"`,
		`"scheduleStartDate":{"day":19,"month":11,"year":2026}`, `"eventTypes":["TRANSFER_OPERATION_SUCCESS"]`} {
		if !strings.Contains(c, want) {
			t.Errorf("create sent %s, want %s in it", c, want)
		}
	}
	for _, src := range []string{"s3", "azure", "url"} {
		if _, err := p.Create(ctx, "p1", map[string]string{"sourceType": src, "sinkBucket": "dst"}); err == nil ||
			!strings.Contains(err.Error(), "keeps everything local") {
			t.Errorf("a %s source = %v, want the local-only refusal", src, err)
		}
	}
	_, fields := p.CreateForm()
	if fields[0].UnavailableOptions["s3"] != transferLocalOnly {
		t.Errorf("Amazon S3 is not drawn disabled with the reason: %+v", fields[0])
	}
	pools := storageAgentPoolsProvider{endpoint: addr}
	pl, err := pools.List(ctx, "p1")
	if err != nil || len(pl.Items) != 1 || pl.Items[0].Fields["Bandwidth limit"] != "1 MB/s" {
		t.Fatalf("agent pools = %+v, %v", pl, err)
	}
	if _, err := pools.Create(ctx, "p1", map[string]string{"agentPoolId": "slow-pool", "limitMbps": "2"}); err != nil {
		t.Fatal(err)
	}
	if c, ok := f.called("POST /v1/projects/p1/agentPools?agentPoolId=slow-pool"); !ok || !strings.Contains(c, `"limitMbps":"2"`) {
		t.Errorf("Create pool sent %q", c)
	}
	if err := pools.Edit(ctx, "p1", []string{"slow-pool"}, map[string]string{"limitMbps": "0"}); err == nil {
		t.Error("a bandwidth limit of 0 was accepted")
	}
}

// The companion screens are built when Cloud Storage is served and each
// API's port is bound, and not for a port that is not.
func TestConsoleStorageJobsProvidersFollowThePorts(t *testing.T) {
	cfg := config.Default()
	cfg.BindAddress = "0.0.0.0"
	if got := companionAddr(cfg, cfg.Endpoints.StorageTransfer); got != "127.0.0.1:9037" {
		t.Errorf("companionAddr = %q", got)
	}
	if got := companionAddr(cfg, -1); got != "" {
		t.Errorf("an unbound port's address = %q", got)
	}
	ids := func(ps []console.Provider) string {
		var out []string
		for _, p := range ps {
			out = append(out, p.ID())
		}
		return strings.Join(out, ",")
	}
	if got := ids(storageJobsProviders("s", "b", "i", "t")); got != "storage-batch-operations,storage-inventory-reports,storage-transfer,storage-transfer-agent-pools" {
		t.Errorf("providers = %s", got)
	}
	if got := ids(storageJobsProviders("s", "", "i", "")); got != "storage-inventory-reports" {
		t.Errorf("providers with only Storage Insights = %s", got)
	}
}
