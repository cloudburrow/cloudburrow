//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/label"
	"google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func loggingClientOptions(h *Harness) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func wantLoggingCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Errorf("%s: %v, want %s", what, err, want)
	}
}

// covers: google.logging.v2.MetricsServiceV2/CreateLogMetric
// covers: google.logging.v2.MetricsServiceV2/GetLogMetric
// covers: google.logging.v2.MetricsServiceV2/ListLogMetrics
// covers: google.logging.v2.MetricsServiceV2/UpdateLogMetric
// covers: google.logging.v2.MetricsServiceV2/DeleteLogMetric
//
// TestLoggingLogBasedMetrics (#1226): through the official MetricsClient, a
// counter metric with the default descriptor and a distribution metric with
// a label extractor, a value extractor and explicit buckets are created, read
// back with their output-only fields, listed in pages, updated (and created
// by UpdateLogMetric when absent) and deleted; the documented validation
// errors are INVALID_ARGUMENT, a duplicate ALREADY_EXISTS and a missing
// metric NOT_FOUND.
func TestLoggingLogBasedMetrics(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := vlogging.NewMetricsClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	parent := "projects/" + h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano())
	counterID, distID := "errors-"+suffix, "latency/"+suffix

	counter, err := c.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: parent, Metric: &loggingpb.LogMetric{
		Name: counterID, Description: "errors", Filter: `resource.type="global" AND severity>=ERROR`,
	}})
	if err != nil {
		t.Fatalf("CreateLogMetric counter: %v", err)
	}
	d := counter.GetMetricDescriptor()
	if d.GetMetricKind() != metric.MetricDescriptor_DELTA || d.GetValueType() != metric.MetricDescriptor_INT64 || d.GetUnit() != "1" ||
		d.GetType() != "logging.googleapis.com/user/"+counterID || counter.GetCreateTime() == nil || counter.GetUpdateTime() == nil {
		t.Errorf("counter = %v; want a DELTA INT64 descriptor of unit 1 typed logging.googleapis.com/user/%s, with times", counter, counterID)
	}

	dist := &loggingpb.LogMetric{
		Name: distID, Filter: `logName="` + parent + `/logs/app"`,
		MetricDescriptor: &metric.MetricDescriptor{
			MetricKind: metric.MetricDescriptor_DELTA, ValueType: metric.MetricDescriptor_DISTRIBUTION, Unit: "ms",
			Labels: []*label.LabelDescriptor{{Key: "path", ValueType: label.LabelDescriptor_STRING}},
		},
		LabelExtractors: map[string]string{"path": "EXTRACT(jsonPayload.path)"},
		ValueExtractor:  `REGEXP_EXTRACT(jsonPayload.latency, "([0-9.]+)ms")`,
		BucketOptions: &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
			ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{Bounds: []float64{10, 100, 1000}},
		}},
	}
	if _, err := c.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: parent, Metric: dist}); err != nil {
		t.Fatalf("CreateLogMetric distribution: %v", err)
	}
	got, err := c.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: parent + "/metrics/" + distID})
	if err != nil {
		t.Fatalf("GetLogMetric: %v", err)
	}
	if got.GetValueExtractor() != dist.GetValueExtractor() || got.GetLabelExtractors()["path"] != "EXTRACT(jsonPayload.path)" ||
		len(got.GetBucketOptions().GetExplicitBuckets().GetBounds()) != 3 || got.GetMetricDescriptor().GetUnit() != "ms" {
		t.Errorf("GetLogMetric = %v; want the distribution as created", got)
	}
	// The escaped form of the ID names the same metric.
	if _, err := c.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: parent + "/metrics/latency%2F" + suffix}); err != nil {
		t.Errorf("GetLogMetric escaped: %v", err)
	}

	var listed []string
	it := c.ListLogMetrics(ctx, &loggingpb.ListLogMetricsRequest{Parent: parent, PageSize: 1})
	for {
		m, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLogMetrics: %v", err)
		}
		listed = append(listed, m.GetName())
	}
	seen := map[string]bool{}
	for _, n := range listed {
		seen[n] = true
	}
	if !seen[counterID] || !seen[distID] {
		t.Errorf("ListLogMetrics = %v; want %s and %s", listed, counterID, distID)
	}

	counter.Disabled = true
	counter.Filter = `severity>=WARNING`
	upd, err := c.UpdateLogMetric(ctx, &loggingpb.UpdateLogMetricRequest{MetricName: parent + "/metrics/" + counterID, Metric: counter})
	if err != nil {
		t.Fatalf("UpdateLogMetric: %v", err)
	}
	if !upd.GetDisabled() || upd.GetFilter() != "severity>=WARNING" || !upd.GetCreateTime().AsTime().Equal(counter.GetCreateTime().AsTime()) {
		t.Errorf("UpdateLogMetric = %v; want disabled, the new filter and the same create_time", upd)
	}
	createdID := "upserted-" + suffix
	if _, err := c.UpdateLogMetric(ctx, &loggingpb.UpdateLogMetricRequest{MetricName: parent + "/metrics/" + createdID,
		Metric: &loggingpb.LogMetric{Name: createdID, Filter: "severity=INFO"}}); err != nil {
		t.Errorf("UpdateLogMetric of an absent metric: %v, want it created", err)
	}

	// Refusals.
	_, err = c.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: parent, Metric: &loggingpb.LogMetric{Name: counterID, Filter: "severity=INFO"}})
	wantLoggingCode(t, "a duplicate", err, codes.AlreadyExists)
	for what, m := range map[string]*loggingpb.LogMetric{
		"no filter":                   {Name: "nofilter-" + suffix},
		"a leading slash":             {Name: "/x" + suffix, Filter: "severity=INFO"},
		"a filter it cannot evaluate": {Name: "bad-" + suffix, Filter: "jsonPayload.x:y"},
		"a distribution with no value_extractor": {Name: "d1-" + suffix, Filter: "severity=INFO",
			MetricDescriptor: &metric.MetricDescriptor{MetricKind: metric.MetricDescriptor_DELTA, ValueType: metric.MetricDescriptor_DISTRIBUTION},
			BucketOptions:    dist.GetBucketOptions()},
		"a regex with two groups": {Name: "d2-" + suffix, Filter: "severity=INFO", ValueExtractor: `REGEXP_EXTRACT(textPayload, "(a)(b)")`,
			MetricDescriptor: &metric.MetricDescriptor{MetricKind: metric.MetricDescriptor_DELTA, ValueType: metric.MetricDescriptor_DISTRIBUTION},
			BucketOptions:    dist.GetBucketOptions()},
		"a label with no extractor": {Name: "l1-" + suffix, Filter: "severity=INFO",
			MetricDescriptor: &metric.MetricDescriptor{Labels: []*label.LabelDescriptor{{Key: "k"}}}},
		"a GAUGE kind": {Name: "g1-" + suffix, Filter: "severity=INFO",
			MetricDescriptor: &metric.MetricDescriptor{MetricKind: metric.MetricDescriptor_GAUGE}},
	} {
		_, err := c.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: parent, Metric: m})
		wantLoggingCode(t, what, err, codes.InvalidArgument)
	}
	dist.MetricDescriptor.ValueType = metric.MetricDescriptor_INT64
	dist.ValueExtractor, dist.BucketOptions = "", nil
	_, err = c.UpdateLogMetric(ctx, &loggingpb.UpdateLogMetricRequest{MetricName: parent + "/metrics/" + distID, Metric: dist})
	wantLoggingCode(t, "changing value_type", err, codes.InvalidArgument)

	for _, id := range []string{counterID, distID, createdID} {
		if err := c.DeleteLogMetric(ctx, &loggingpb.DeleteLogMetricRequest{MetricName: parent + "/metrics/" + id}); err != nil {
			t.Errorf("DeleteLogMetric %s: %v", id, err)
		}
	}
	_, err = c.GetLogMetric(ctx, &loggingpb.GetLogMetricRequest{MetricName: parent + "/metrics/" + counterID})
	wantLoggingCode(t, "a deleted metric", err, codes.NotFound)
	err = c.DeleteLogMetric(ctx, &loggingpb.DeleteLogMetricRequest{MetricName: parent + "/metrics/" + counterID})
	wantLoggingCode(t, "deleting twice", err, codes.NotFound)
}

// covers: google.logging.v2.ConfigServiceV2/CreateSink
// covers: google.logging.v2.ConfigServiceV2/GetSink
// covers: google.logging.v2.ConfigServiceV2/ListSinks
// covers: google.logging.v2.ConfigServiceV2/UpdateSink
// covers: google.logging.v2.ConfigServiceV2/DeleteSink
//
// TestLoggingSinks (#1226): through the official ConfigClient, a sink is
// stored with its destination, filter and embedded exclusion, listed,
// updated through an update_mask and deleted; a bad destination or filter is
// INVALID_ARGUMENT. Nothing is delivered to the destination.
func TestLoggingSinks(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	parent := "projects/" + h.Project()
	id := fmt.Sprint("sink-", time.Now().UnixNano())
	sk, err := c.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{
		Name: id, Destination: "storage.googleapis.com/my-bucket", Filter: "severity>=ERROR",
		Exclusions: []*loggingpb.LogExclusion{{Name: "no-debug", Filter: "severity=DEBUG"}},
	}})
	if err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	if sk.GetCreateTime() == nil || sk.GetDestination() != "storage.googleapis.com/my-bucket" {
		t.Errorf("CreateSink = %v", sk)
	}
	got, err := c.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: parent + "/sinks/" + id})
	if err != nil || got.GetFilter() != "severity>=ERROR" || len(got.GetExclusions()) != 1 {
		t.Errorf("GetSink = %v, %v", got, err)
	}
	found := false
	it := c.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: parent})
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSinks: %v", err)
		}
		found = found || s.GetName() == id
	}
	if !found {
		t.Errorf("ListSinks has no %s", id)
	}
	upd, err := c.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: parent + "/sinks/" + id,
		Sink:       &loggingpb.LogSink{Destination: "pubsub.googleapis.com/projects/" + h.Project() + "/topics/t", Disabled: true, Filter: "ignored=1"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"destination", "disabled"}}})
	if err != nil {
		t.Fatalf("UpdateSink: %v", err)
	}
	if !upd.GetDisabled() || upd.GetFilter() != "severity>=ERROR" || upd.GetDestination() != "pubsub.googleapis.com/projects/"+h.Project()+"/topics/t" {
		t.Errorf("UpdateSink = %v; want only destination and disabled changed", upd)
	}
	_, err = c.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{Name: id + "x", Destination: "example.com/x"}})
	wantLoggingCode(t, "a bad destination", err, codes.InvalidArgument)
	_, err = c.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: &loggingpb.LogSink{Name: id, Destination: "storage.googleapis.com/b"}})
	wantLoggingCode(t, "a duplicate", err, codes.AlreadyExists)
	_, err = c.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: parent + "/sinks/" + id, Sink: &loggingpb.LogSink{},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"writer_identity"}}})
	wantLoggingCode(t, "an output-only path", err, codes.InvalidArgument)
	if err := c.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + id}); err != nil {
		t.Errorf("DeleteSink: %v", err)
	}
	_, err = c.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: parent + "/sinks/" + id})
	wantLoggingCode(t, "a deleted sink", err, codes.NotFound)
}

// covers: google.logging.v2.ConfigServiceV2/CreateExclusion
// covers: google.logging.v2.ConfigServiceV2/GetExclusion
// covers: google.logging.v2.ConfigServiceV2/ListExclusions
// covers: google.logging.v2.ConfigServiceV2/UpdateExclusion
// covers: google.logging.v2.ConfigServiceV2/DeleteExclusion
//
// TestLoggingExclusions (#1226): an exclusion made with the official
// ConfigClient drops the matching entries written after it, as the _Default
// sink does, until it is disabled through UpdateExclusion; it is listed, and
// deleted. A missing filter and an empty update_mask are INVALID_ARGUMENT.
func TestLoggingExclusions(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	lc := loggingAPIClient(t, h)
	parent := "projects/" + h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano())
	id, logName := "excl-"+suffix, parent+"/logs/excluded-"+suffix
	if _, err := c.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: parent, Exclusion: &loggingpb.LogExclusion{
		Name: id, Filter: `logName="` + logName + `"`,
	}}); err != nil {
		t.Fatalf("CreateExclusion: %v", err)
	}
	write := func(text string) {
		t.Helper()
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: logName, Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatalf("WriteLogEntries: %v", err)
		}
	}
	read := func() []string {
		t.Helper()
		var out []string
		it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{parent}, Filter: `logName="` + logName + `"`})
		for {
			e, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListLogEntries: %v", err)
			}
			out = append(out, e.GetTextPayload())
		}
	}
	write("dropped")
	if got := read(); len(got) != 0 {
		t.Errorf("with the exclusion enabled, read %v; want nothing", got)
	}
	upd, err := c.UpdateExclusion(ctx, &loggingpb.UpdateExclusionRequest{Name: parent + "/exclusions/" + id,
		Exclusion: &loggingpb.LogExclusion{Disabled: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
	if err != nil || !upd.GetDisabled() || upd.GetFilter() == "" {
		t.Fatalf("UpdateExclusion = %v, %v; want disabled with its filter", upd, err)
	}
	write("kept")
	if got := read(); len(got) != 1 || got[0] != "kept" {
		t.Errorf("with the exclusion disabled, read %v; want [kept]", got)
	}
	if g, err := c.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: parent + "/exclusions/" + id}); err != nil || !g.GetDisabled() {
		t.Errorf("GetExclusion = %v, %v", g, err)
	}
	found := false
	it := c.ListExclusions(ctx, &loggingpb.ListExclusionsRequest{Parent: parent})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListExclusions: %v", err)
		}
		found = found || e.GetName() == id
	}
	if !found {
		t.Errorf("ListExclusions has no %s", id)
	}
	_, err = c.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: parent, Exclusion: &loggingpb.LogExclusion{Name: "nofilter-" + suffix}})
	wantLoggingCode(t, "no filter", err, codes.InvalidArgument)
	_, err = c.UpdateExclusion(ctx, &loggingpb.UpdateExclusionRequest{Name: parent + "/exclusions/" + id, Exclusion: &loggingpb.LogExclusion{}})
	wantLoggingCode(t, "an empty update_mask", err, codes.InvalidArgument)
	if err := c.DeleteExclusion(ctx, &loggingpb.DeleteExclusionRequest{Name: parent + "/exclusions/" + id}); err != nil {
		t.Errorf("DeleteExclusion: %v", err)
	}
	_, err = c.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: parent + "/exclusions/" + id})
	wantLoggingCode(t, "a deleted exclusion", err, codes.NotFound)
}
