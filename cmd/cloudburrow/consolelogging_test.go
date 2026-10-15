package main

import (
	"context"
	"encoding/json"
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
		loggingMetricsProvider{screen}, loggingSinksProvider{screen}, loggingExclusionsProvider{screen}).Handler())
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
	if sd.Edit == nil || !strings.Contains(loggingDetailText(sd), "delivers nothing") {
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
