package logging

import (
	"net/http/httptest"

	vlogging "cloud.google.com/go/logging/apiv2"
	"context"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func TestMetricValidation(t *testing.T) {
	ms := NewAdmin(store.NewMemory()).MetricsServer()
	ctx := context.Background()
	buckets := &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_LinearBuckets{
		LinearBuckets: &distribution.Distribution_BucketOptions_Linear{NumFiniteBuckets: 5, Width: 10}}}
	distDesc := func() *metric.MetricDescriptor {
		return &metric.MetricDescriptor{MetricKind: metric.MetricDescriptor_DELTA, ValueType: metric.MetricDescriptor_DISTRIBUTION}
	}
	for _, tc := range []struct {
		what string
		m    *loggingpb.LogMetric
		ok   bool
	}{
		{"a counter", &loggingpb.LogMetric{Name: "a/b", Filter: "severity>=ERROR"}, true},
		{"a distribution", &loggingpb.LogMetric{Name: "d", Filter: "severity>=ERROR", MetricDescriptor: distDesc(),
			ValueExtractor: "EXTRACT(jsonPayload.ms)", BucketOptions: buckets}, true},
		{"no name", &loggingpb.LogMetric{Filter: "severity>=ERROR"}, false},
		{"a 101-character name", &loggingpb.LogMetric{Name: string(make([]byte, 101)), Filter: "severity>=ERROR"}, false},
		{"a space in the name", &loggingpb.LogMetric{Name: "a b", Filter: "severity>=ERROR"}, false},
		{"no filter", &loggingpb.LogMetric{Name: "x"}, false},
		{"an OR filter", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO OR severity=ERROR"}, false},
		{"a counter with a value_extractor", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO", ValueExtractor: "EXTRACT(textPayload)"}, false},
		{"a distribution without buckets", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO", MetricDescriptor: distDesc(),
			ValueExtractor: "EXTRACT(jsonPayload.ms)"}, false},
		{"bad linear buckets", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO", MetricDescriptor: distDesc(),
			ValueExtractor: "EXTRACT(jsonPayload.ms)", BucketOptions: &distribution.Distribution_BucketOptions{
				Options: &distribution.Distribution_BucketOptions_LinearBuckets{LinearBuckets: &distribution.Distribution_BucketOptions_Linear{}}}}, false},
		{"an extractor that is no function", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO", MetricDescriptor: distDesc(),
			ValueExtractor: "jsonPayload.ms", BucketOptions: buckets}, false},
		{"an extractor key with no label", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO",
			LabelExtractors: map[string]string{"k": "EXTRACT(textPayload)"}}, false},
		{"a BOOL value type", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO",
			MetricDescriptor: &metric.MetricDescriptor{ValueType: metric.MetricDescriptor_BOOL}}, false},
		{"a bucket of another project", &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO",
			BucketName: "projects/other-1/locations/global/buckets/b"}, false},
	} {
		_, err := ms.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: "projects/proj-1", Metric: tc.m})
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.what, err)
		}
		if !tc.ok && status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", tc.what, err)
		}
	}
	got, err := ms.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: "projects/proj-1/metrics/a%2Fb"})
	if err != nil || got.GetMetricDescriptor().GetType() != "logging.googleapis.com/user/a/b" {
		t.Errorf("GetLogMetric escaped = %v, %v", got, err)
	}
	_, err = ms.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: "organizations/1", Metric: &loggingpb.LogMetric{Name: "x", Filter: "severity=INFO"}})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("an organization parent: %v, want UNIMPLEMENTED", err)
	}
}

// TestMetricsPersistAcrossAdmins: a metric written through one Admin is read
// by another over the same store, as a persistent instance's restart does.
func TestMetricsPersistAcrossAdmins(t *testing.T) {
	db := store.NewMemory()
	ctx := context.Background()
	if _, err := NewAdmin(db).MetricsServer().CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: "projects/proj-1",
		Metric: &loggingpb.LogMetric{Name: "m", Filter: "severity=INFO"}}); err != nil {
		t.Fatal(err)
	}
	ms, err := NewAdmin(db).Metrics("proj-1")
	if err != nil || len(ms) != 1 || ms[0].GetName() != "m" {
		t.Fatalf("Metrics = %v, %v", ms, err)
	}
}

func TestExclusionsDropEntriesAndCountMatching(t *testing.T) {
	st := NewStore(100)
	srv := NewServer(st)
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	ctx := context.Background()
	if _, err := a.ConfigServer().CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: "projects/proj-1",
		Exclusion: &loggingpb.LogExclusion{Name: "debug", Filter: "severity=DEBUG"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(sev string) {
		t.Helper()
		e := &loggingpb.LogEntry{Timestamp: timestamppb.New(now), Payload: &loggingpb.LogEntry_TextPayload{TextPayload: sev}}
		e.Severity = sevOf(sev)
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: "projects/proj-1/logs/l",
			Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{e}}); err != nil {
			t.Fatal(err)
		}
	}
	write("DEBUG")
	write("ERROR")
	if n := len(st.snapshot()); n != 1 {
		t.Fatalf("stored %d entries, want the ERROR one only", n)
	}
	if _, err := a.ConfigServer().UpdateExclusion(ctx, &loggingpb.UpdateExclusionRequest{Name: "projects/proj-1/exclusions/debug",
		Exclusion: &loggingpb.LogExclusion{Disabled: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); err != nil {
		t.Fatal(err)
	}
	write("DEBUG")
	if n, err := srv.CountMatching("proj-1", "severity>=DEBUG", now.Add(-time.Minute)); err != nil || n != 2 {
		t.Errorf("CountMatching = %d, %v; want 2", n, err)
	}
	if n, _ := srv.CountMatching("proj-1", "severity>=DEBUG", now.Add(time.Minute)); n != 0 {
		t.Errorf("CountMatching after the window = %d, want 0", n)
	}
}

func TestSinkUpdateMask(t *testing.T) {
	cs := NewAdmin(store.NewMemory()).ConfigServer()
	ctx := context.Background()
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{
		Name: "s", Destination: "bigquery.googleapis.com/projects/proj-1/datasets/d", Description: "keep"}}); err != nil {
		t.Fatal(err)
	}
	// An empty mask is destination,filter,include_children.
	got, err := cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: "projects/proj-1/sinks/s", Sink: &loggingpb.LogSink{
		Destination: "logging.googleapis.com/projects/proj-1/locations/global/buckets/b", Filter: "severity=INFO", Description: "dropped?"}})
	if err != nil || got.GetDescription() != "keep" || got.GetFilter() != "severity=INFO" {
		t.Errorf("UpdateSink with no mask = %v, %v", got, err)
	}
	for _, d := range []string{"", "storage.googleapis.com/", "bigquery.googleapis.com/projects/p", "pubsub.googleapis.com/topics/t"} {
		_, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{Name: "x", Destination: d}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("destination %q: %v, want INVALID_ARGUMENT", d, err)
		}
	}
	_, err = cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1", Sink: &loggingpb.LogSink{Name: "_x", Destination: "storage.googleapis.com/b"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a name starting with _: %v, want INVALID_ARGUMENT", err)
	}
}

func sevOf(s string) ltype.LogSeverity { return ltype.LogSeverity(ltype.LogSeverity_value[s]) }

// TestRESTMetricsAreTranscoded: the official REST MetricsClient creates and
// reads a metric over JSON, through the transcoder cmd/cloudburrow builds.
func TestRESTMetricsAreTranscoded(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	api := NewServer(NewStore(10))
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { api.Register(g); a.Register(g) }))
	defer srv.Close()
	ctx := context.Background()
	c, err := vlogging.NewMetricsRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: "projects/proj-1",
		Metric: &loggingpb.LogMetric{Name: "m", Filter: "severity>=ERROR"}}); err != nil {
		t.Fatalf("CreateLogMetric over JSON: %v", err)
	}
	m, err := c.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: "projects/proj-1/metrics/m"})
	if err != nil || m.GetFilter() != "severity>=ERROR" {
		t.Fatalf("GetLogMetric over JSON = %v, %v", m, err)
	}
}
