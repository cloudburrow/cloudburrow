package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// loggingConsole serves a console with the three Cloud Logging admin screens
// over an in-process Logging service, as up wires them.
func loggingConsole(t *testing.T) (*httptest.Server, *loggingService) {
	t.Helper()
	st := logging.NewStore(100)
	svc := &loggingService{store: st, api: logging.NewServer(st), admin: logging.NewAdmin(store.NewMemory())}
	svc.api.UseAdmin(svc.admin)
	screen := loggingAdminScreen{svc: svc}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil,
		loggingMetricsProvider{screen}, loggingSinksProvider{screen}, loggingExclusionsProvider{screen}, loggingBucketsProvider{screen},
		loggingSettingsProvider{screen}).Handler())
	t.Cleanup(srv.Close)
	return srv, svc
}

func consolePatch(t *testing.T, u string, path []string, values map[string]string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"Path": path, "Values": values})
	req, _ := http.NewRequest(http.MethodPatch, u, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestConsoleLoggingMetrics: a counter and a distribution created through the
// console are what MetricsServiceV2 returns; the metric page counts the
// entries its filter matches; Edit changes the filter and is refused a bad
// one with the API's message; Delete removes it, and another project's metric
// is refused.
func TestConsoleLoggingMetrics(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	base := srv.URL + "/api/resources/logging-metrics?project=" + project
	if code, out := consolePost(t, base, map[string]string{"name": "errors", "filter": "severity>=ERROR", "type": "Counter", "disabled": "false"}); code != http.StatusOK {
		t.Fatalf("create counter = %d %s", code, out)
	}
	if code, out := consolePost(t, base, map[string]string{"name": "latency/ms", "filter": "severity>=INFO", "type": "Distribution",
		"value_extractor": `REGEXP_EXTRACT(textPayload, "took ([0-9]+)ms")`, "buckets": "linear", "bucket_values": "10,5,0",
		"labels": `{"path":"EXTRACT(labels.path)"}`}); code != http.StatusOK {
		t.Fatalf("create distribution = %d %s", code, out)
	}
	m, err := svc.admin.MetricsServer().GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: "projects/proj-1/metrics/latency%2Fms"})
	if err != nil || m.GetMetricDescriptor().GetValueType() != metric.MetricDescriptor_DISTRIBUTION ||
		m.GetBucketOptions().GetLinearBuckets().GetNumFiniteBuckets() != 10 || m.GetLabelExtractors()["path"] != "EXTRACT(labels.path)" {
		t.Fatalf("the API reads the distribution as %v, %v", m, err)
	}
	if code, out := consolePost(t, base, map[string]string{"name": "bad", "filter": "jsonPayload.x:y"}); code == http.StatusOK || !strings.Contains(out, "not supported") {
		t.Errorf("a filter the API refuses = %d %s; want its message", code, out)
	}

	if _, err := svc.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/app",
		Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries:  []*loggingpb.LogEntry{{Severity: 500}, {Severity: 500}, {Severity: 200}}}); err != nil {
		t.Fatal(err)
	}
	var d console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-metrics?project="+project+"&name="+url.QueryEscape("projects/proj-1/metrics/errors"), &d)
	if d.Edit == nil || !strings.Contains(loggingDetailText(d), "2 entries") || !strings.Contains(loggingDetailText(d), "no time series") {
		t.Errorf("the metric page = %+v; want an edit form, 2 matching entries and the no-time-series note", d)
	}

	edit := srv.URL + "/api/resources/logging-metrics?project=" + project
	if code, out := consolePatch(t, edit, []string{"projects/proj-1/metrics/errors"}, map[string]string{"filter": "severity>=WARNING", "disabled": "true"}); code != http.StatusOK {
		t.Fatalf("edit = %d %s", code, out)
	}
	m, _ = svc.admin.MetricsServer().GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: "projects/proj-1/metrics/errors"})
	if m.GetFilter() != "severity>=WARNING" || !m.GetDisabled() {
		t.Errorf("after edit the API reads %v", m)
	}
	if code, _ := consolePatch(t, edit, []string{"projects/proj-1/metrics/errors"}, map[string]string{"filter": "a OR b"}); code == http.StatusOK {
		t.Error("an edit to a filter the API refuses succeeded")
	}

	if code, _ := consoleDelete(t, srv, "logging-metrics", project, "projects/other-1/metrics/errors"); code == http.StatusOK {
		t.Error("deleting another project's metric succeeded")
	}
	if code, out := consoleDelete(t, srv, "logging-metrics", project, "projects/proj-1/metrics/errors"); code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, out)
	}
	var l console.Listing
	getJSON200(t, base, &l)
	if len(l.Items) != 1 || l.Items[0].Name != "projects/proj-1/metrics/latency%2Fms" {
		t.Errorf("listing after delete = %+v", l.Items)
	}
}

func loggingDetailText(d console.Detail) string {
	b, _ := json.Marshal(d)
	return string(b)
}

// TestConsoleLoggingSinksAndExclusions: a sink and an exclusion created,
// edited and deleted through the console are what ConfigServiceV2 returns,
// and the exclusion drops what it matches.
func TestConsoleLoggingSinksAndExclusions(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	sinks := srv.URL + "/api/resources/logging-sinks?project=" + project
	if code, out := consolePost(t, sinks, map[string]string{"name": "to-gcs", "destination": "storage.googleapis.com/b", "filter": "severity>=ERROR"}); code != http.StatusOK {
		t.Fatalf("create sink = %d %s", code, out)
	}
	if code, out := consolePost(t, sinks, map[string]string{"name": "bad", "destination": "example.com/x"}); code == http.StatusOK || !strings.Contains(out, "destination") {
		t.Errorf("a bad destination = %d %s", code, out)
	}
	if code, out := consolePatch(t, sinks, []string{"projects/proj-1/sinks/to-gcs"}, map[string]string{
		"destination": "pubsub.googleapis.com/projects/proj-1/topics/t", "filter": "", "description": "d", "disabled": "true"}); code != http.StatusOK {
		t.Fatalf("edit sink = %d %s", code, out)
	}
	sk, err := svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/proj-1/sinks/to-gcs"})
	if err != nil || sk.GetDestination() != "pubsub.googleapis.com/projects/proj-1/topics/t" || !sk.GetDisabled() || sk.GetFilter() != "" {
		t.Errorf("after edit the API reads %v, %v", sk, err)
	}
	var sd console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-sinks?project="+project+"&name="+url.QueryEscape("projects/proj-1/sinks/to-gcs"), &sd)
	if sd.Edit == nil || !strings.Contains(loggingDetailText(sd), "Nothing delivered yet") || !strings.Contains(loggingDetailText(sd), "serviceAccount:service-proj-1@") {
		t.Errorf("the sink page = %+v", sd)
	}
	if code, out := consoleDelete(t, srv, "logging-sinks", project, "projects/proj-1/sinks/to-gcs"); code != http.StatusOK {
		t.Fatalf("delete sink = %d %s", code, out)
	}

	excl := srv.URL + "/api/resources/logging-exclusions?project=" + project
	if code, out := consolePost(t, excl, map[string]string{"name": "no-debug", "filter": "severity=DEBUG"}); code != http.StatusOK {
		t.Fatalf("create exclusion = %d %s", code, out)
	}
	if _, err := svc.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/app",
		Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{{Severity: 100}}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.api.CountMatching(project, "severity=DEBUG", time.Time{}); n != 0 {
		t.Errorf("stored %d excluded entries", n)
	}
	if code, out := consolePatch(t, excl, []string{"projects/proj-1/exclusions/no-debug"}, map[string]string{"filter": "severity=INFO", "disabled": "true"}); code != http.StatusOK {
		t.Fatalf("edit exclusion = %d %s", code, out)
	}
	e, _ := svc.admin.ConfigServer().GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: "projects/proj-1/exclusions/no-debug"})
	if e.GetFilter() != "severity=INFO" || !e.GetDisabled() {
		t.Errorf("after edit the API reads %v", e)
	}
	if code, out := consoleDelete(t, srv, "logging-exclusions", project, "projects/proj-1/exclusions/no-debug"); code != http.StatusOK {
		t.Fatalf("delete exclusion = %d %s", code, out)
	}
	var l console.Listing
	getJSON200(t, excl, &l)
	if len(l.Items) != 0 {
		t.Errorf("exclusions after delete = %+v", l.Items)
	}
}

// TestConsoleLoggingBuckets (#1264): the Logs Storage screen lists _Default
// and _Required; a bucket created there is what ConfigServiceV2 returns; its
// page lists the _AllLogs view; Edit changes the retention; Delete leaves it
// Delete requested with Undelete offered, which makes it active again;
// _Required has no Edit, and another project's bucket is refused.
func TestConsoleLoggingBuckets(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	base := srv.URL + "/api/resources/logging-buckets?project=" + project
	var l console.Listing
	getJSON200(t, base, &l)
	if len(l.Items) != 2 {
		t.Fatalf("buckets of a new project = %+v; want _Default and _Required", l.Items)
	}
	if code, out := consolePost(t, base, map[string]string{"name": "app", "location": "us-central1", "retention": "14", "description": "d"}); code != http.StatusOK {
		t.Fatalf("create bucket = %d %s", code, out)
	}
	name := "projects/proj-1/locations/us-central1/buckets/app"
	b, err := svc.admin.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name})
	if err != nil || b.GetRetentionDays() != 14 || b.GetDescription() != "d" {
		t.Fatalf("the API reads %v, %v", b, err)
	}
	var d console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-buckets?project="+project+"&name="+url.QueryEscape(name), &d)
	if d.Edit == nil || !strings.Contains(fmt.Sprint(d.Sections), "_AllLogs") {
		t.Errorf("the bucket page = %+v; want Edit and the _AllLogs view", d)
	}
	if code, out := consolePatch(t, base, []string{name}, map[string]string{"description": "d", "retention": "60"}); code != http.StatusOK {
		t.Fatalf("edit bucket = %d %s", code, out)
	}
	if b, _ := svc.admin.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name}); b.GetRetentionDays() != 60 {
		t.Errorf("after edit the API reads %v", b)
	}
	if code, out := consoleDelete(t, srv, "logging-buckets", project, name); code != http.StatusOK {
		t.Fatalf("delete bucket = %d %s", code, out)
	}
	getJSON200(t, base, &l)
	offered := false
	for _, it := range l.Items {
		if it.Name == name {
			offered = it.Fields["State"] == bucketDeleteRequested && len(it.Actions) == 1 && it.Actions[0].ID == "undelete"
		}
	}
	if !offered {
		t.Errorf("a deleted bucket's row = %+v; want Delete requested with Undelete", l.Items)
	}
	if code, out := consolePost(t, srv.URL+"/api/actions/logging-buckets?project="+project, map[string]string{"Name": name, "Action": "undelete"}); code != http.StatusOK {
		t.Fatalf("undelete = %d %s", code, out)
	}
	if b, _ := svc.admin.ConfigServer().GetBucket(ctx, &loggingpb.GetBucketRequest{Name: name}); b.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE {
		t.Errorf("after undelete the API reads %v", b)
	}
	var req console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-buckets?project="+project+"&name="+url.QueryEscape("projects/proj-1/locations/global/buckets/_Required"), &req)
	if req.Edit != nil {
		t.Errorf("_Required offers Edit")
	}
	if code, _ := consoleDelete(t, srv, "logging-buckets", project, "projects/other-1/locations/global/buckets/x"); code == http.StatusOK {
		t.Errorf("deleting another project's bucket succeeded")
	}
}

// TestConsoleLoggingViews (#1283): a bucket's page offers Create view, and a
// view made there is what ConfigServiceV2 returns; its row offers Edit, which
// changes the filter and description, and Delete, which removes it. _AllLogs
// offers neither, _Required's page offers no Create view, and an action the
// page does not offer, such as Delete on _Default's _AllLogs, is refused.
func TestConsoleLoggingViews(t *testing.T) {
	srv, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	bucket := "projects/proj-1/locations/global/buckets/_Default"
	detailURL := func(b string) string {
		return srv.URL + "/api/detail/logging-buckets?project=" + project + "&name=" + url.QueryEscape(b)
	}
	acts := srv.URL + "/api/actions/logging-buckets?project=" + project
	var d console.Detail
	getJSON200(t, detailURL(bucket), &d)
	if len(d.Actions) != 1 || d.Actions[0].ID != viewCreate {
		t.Fatalf("_Default's page offers %+v; want Create view", d.Actions)
	}
	viewRow := func(d console.Detail, id string) *console.Resource {
		for _, s := range d.Sections {
			for i, r := range s.Listing.Items {
				if s.ID == "views" && r.Name == id {
					return &s.Listing.Items[i]
				}
			}
		}
		return nil
	}
	if r := viewRow(d, "_AllLogs"); r == nil || len(r.Actions) != 0 {
		t.Errorf("_AllLogs row = %+v; want it listed with no actions", r)
	}

	if code, out := consolePost(t, acts, map[string]any{"Path": []string{bucket}, "Action": viewCreate,
		"Values": map[string]string{"id": "errors", "filter": `severity>=ERROR`, "description": "just errors"}}); code != http.StatusOK {
		t.Fatalf("create view = %d %s", code, out)
	}
	name := bucket + "/views/errors"
	v, err := svc.admin.ConfigServer().GetView(ctx, &loggingpb.GetViewRequest{Name: name})
	if err != nil || v.GetFilter() != "severity>=ERROR" || v.GetDescription() != "just errors" {
		t.Fatalf("the API reads %v, %v", v, err)
	}
	d = console.Detail{}
	getJSON200(t, detailURL(bucket), &d)
	r := viewRow(d, "errors")
	if r == nil || len(r.Actions) != 2 || r.Actions[0].ID != viewEdit || r.Actions[1].ID != viewDelete ||
		r.Actions[0].Fields[0].Default != "severity>=ERROR" {
		t.Fatalf("the new view's row = %+v; want Edit with its filter, and Delete", r)
	}
	if code, out := consolePost(t, acts, map[string]any{"Path": []string{bucket, "errors"}, "Action": viewEdit,
		"Values": map[string]string{"filter": "severity>=WARNING", "description": "warnings"}}); code != http.StatusOK {
		t.Fatalf("edit view = %d %s", code, out)
	}
	if v, _ := svc.admin.ConfigServer().GetView(ctx, &loggingpb.GetViewRequest{Name: name}); v.GetFilter() != "severity>=WARNING" || v.GetDescription() != "warnings" {
		t.Errorf("after edit the API reads %v", v)
	}
	if code, out := consolePost(t, acts, map[string]any{"Path": []string{bucket, "errors"}, "Action": viewDelete}); code != http.StatusOK {
		t.Fatalf("delete view = %d %s", code, out)
	}
	if _, err := svc.admin.ConfigServer().GetView(ctx, &loggingpb.GetViewRequest{Name: name}); err == nil {
		t.Error("the deleted view still reads back")
	}

	if code, _ := consolePost(t, acts, map[string]any{"Path": []string{bucket, "_AllLogs"}, "Action": viewDelete}); code == http.StatusOK {
		t.Error("deleting _Default's _AllLogs was accepted")
	}
	if code, out := consolePost(t, acts, map[string]any{"Path": []string{bucket}, "Action": viewCreate,
		"Values": map[string]string{"id": "_mine"}}); code != http.StatusBadRequest || !strings.Contains(out, "reserved") {
		t.Errorf("a reserved view ID = %d %s; want the API's refusal", code, out)
	}
	d = console.Detail{}
	getJSON200(t, detailURL("projects/proj-1/locations/global/buckets/_Required"), &d)
	if len(d.Actions) != 0 {
		t.Errorf("_Required's page offers %+v; want nothing", d.Actions)
	}
	if code, _ := consolePost(t, acts, map[string]any{"Path": []string{"projects/other-1/locations/global/buckets/_Default"}, "Action": viewCreate,
		"Values": map[string]string{"id": "x"}}); code == http.StatusOK {
		t.Error("creating a view on another project's bucket was accepted")
	}
}
