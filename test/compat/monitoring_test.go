//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	distributionpb "google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EnvMonitoring is the host address of the Cloud Monitoring API v3
// (monitoring.googleapis.com, #1939), gRPC and JSON on one port.
const EnvMonitoring = "CLOUDBURROW_TEST_MONITORING"

func monitoringClient(t *testing.T, h *Harness) *monitoring.MetricClient {
	t.Helper()
	addr := h.Endpoint(EnvMonitoring)
	c, err := monitoring.NewMetricClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func listAll(t *testing.T, it *monitoring.TimeSeriesIterator) []*monitoringpb.TimeSeries {
	t.Helper()
	var out []*monitoringpb.TimeSeries
	for {
		ts, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("ListTimeSeries: %v", err)
		}
		out = append(out, ts)
	}
}

func wantMonitoringCode(t *testing.T, what string, err error, code codes.Code, substr string) {
	t.Helper()
	if status.Code(err) != code || !strings.Contains(status.Convert(err).Message(), substr) {
		t.Errorf("%s: %v; want %s containing %q", what, err, code, substr)
	}
}

// TestMonitoringCustomMetrics (#1939) drives Cloud Monitoring through
// cloud.google.com/go/monitoring/apiv3/v2 over gRPC:
//   - a CUMULATIVE INT64 descriptor with a label is created, got and listed
//     by a starts_with filter; a DELTA one is refused, as Google refuses it
//     for user-defined metrics;
//   - twelve points, ten seconds apart, are written to two series, one per
//     label value; a point written five seconds too soon is
//     FAILED_PRECONDITION, one out of order and two points in one series
//     INVALID_ARGUMENT;
//   - the series are read raw (newest first), aligned with ALIGN_RATE, and
//     reduced with REDUCE_SUM grouped by the label, and in the HEADERS view;
//   - a gauge written with no descriptor gets an auto-created one;
//   - the monitored resource descriptors are listed and one is got;
//   - the descriptors are deleted, and reading them is NOT_FOUND.
//
// covers: google.monitoring.v3.MetricService/CreateMetricDescriptor, google.monitoring.v3.MetricService/GetMetricDescriptor, google.monitoring.v3.MetricService/ListMetricDescriptors, google.monitoring.v3.MetricService/DeleteMetricDescriptor, google.monitoring.v3.MetricService/CreateTimeSeries, google.monitoring.v3.MetricService/ListTimeSeries, google.monitoring.v3.MetricService/ListMonitoredResourceDescriptors, google.monitoring.v3.MetricService/GetMonitoredResourceDescriptor
func TestMonitoringCustomMetrics(t *testing.T) {
	h := New(t)
	c := monitoringClient(t, h)
	ctx := h.Context()
	name := "projects/" + h.Project()
	n := time.Now().UnixNano() % 1e6
	counter := fmt.Sprintf("custom.googleapis.com/compat/requests_%d", n)

	md, err := c.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: name, MetricDescriptor: &metricpb.MetricDescriptor{
		Type: counter, MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64, Unit: "1",
		Description: "requests served", Labels: []*labelpb.LabelDescriptor{{Key: "zone", ValueType: labelpb.LabelDescriptor_STRING}}}})
	if err != nil {
		t.Fatalf("CreateMetricDescriptor: %v", err)
	}
	if md.GetName() != name+"/metricDescriptors/"+counter {
		t.Errorf("descriptor name %q", md.GetName())
	}
	if got, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: md.GetName()}); err != nil || got.GetDescription() != "requests served" {
		t.Errorf("GetMetricDescriptor = %v, %v", got, err)
	}
	found := false
	it := c.ListMetricDescriptors(ctx, &monitoringpb.ListMetricDescriptorsRequest{Name: name, Filter: `metric.type = starts_with("custom.googleapis.com/compat/")`})
	for {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMetricDescriptors: %v", err)
		}
		found = found || d.GetType() == counter
	}
	if !found {
		t.Errorf("ListMetricDescriptors did not list %s", counter)
	}
	_, err = c.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: name, MetricDescriptor: &metricpb.MetricDescriptor{
		Type: counter + "_delta", MetricKind: metricpb.MetricDescriptor_DELTA, ValueType: metricpb.MetricDescriptor_INT64}})
	wantMonitoringCode(t, "a DELTA user-defined metric", err, codes.InvalidArgument, "DELTA")

	// Points ten seconds apart over the last two minutes, in order.
	start := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
	point := func(end time.Time, v int64) *monitoringpb.Point {
		return &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}}}
	}
	ts := func(zone string, p *monitoringpb.Point) *monitoringpb.TimeSeries {
		return &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: counter, Labels: map[string]string{"zone": zone}},
			Resource: &mrpb.MonitoredResource{Type: "generic_task", Labels: map[string]string{
				"location": "us-central1", "namespace": "compat", "job": "api", "task_id": "1"}},
			Points: []*monitoringpb.Point{p}}
	}
	var last time.Time
	for i := 1; i <= 12; i++ {
		last = start.Add(time.Duration(i) * 10 * time.Second)
		if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: name, TimeSeries: []*monitoringpb.TimeSeries{
			ts("a", point(last, int64(i*6))), ts("b", point(last, int64(i*12))),
		}}); err != nil {
			t.Fatalf("CreateTimeSeries %d: %v", i, err)
		}
	}
	err = c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: name, TimeSeries: []*monitoringpb.TimeSeries{ts("a", point(last.Add(2*time.Second), 100))}})
	wantMonitoringCode(t, "a point 2s after the last", err, codes.FailedPrecondition, "more frequently")
	err = c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: name, TimeSeries: []*monitoringpb.TimeSeries{ts("a", point(last.Add(-time.Minute), 1))}})
	wantMonitoringCode(t, "a point out of order", err, codes.InvalidArgument, "written in order")
	two := ts("a", point(last.Add(time.Minute), 1))
	two.Points = append(two.Points, point(last.Add(2*time.Minute), 2))
	err = c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: name, TimeSeries: []*monitoringpb.TimeSeries{two}})
	wantMonitoringCode(t, "two points in one series", err, codes.InvalidArgument, "Only one point")

	filter := fmt.Sprintf(`metric.type = %q AND resource.type = "generic_task"`, counter)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(last)}
	raw := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: name, Filter: filter + ` AND metric.labels.zone = "a"`, Interval: iv}))
	if len(raw) != 1 || len(raw[0].GetPoints()) != 12 || raw[0].GetPoints()[0].GetValue().GetInt64Value() != 72 ||
		raw[0].GetMetricKind() != metricpb.MetricDescriptor_CUMULATIVE {
		t.Fatalf("raw read = %v; want one CUMULATIVE series, 12 points, newest (72) first", raw)
	}
	agg := &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_RATE}
	rate := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: name, Filter: filter + ` AND metric.labels.zone = "a"`,
		Interval: iv, Aggregation: agg}))
	// The last minute holds +36 in zone a: 0.6 per second.
	if len(rate) != 1 || math.Abs(rate[0].GetPoints()[0].GetValue().GetDoubleValue()-0.6) > 1e-9 || rate[0].GetMetricKind() != metricpb.MetricDescriptor_GAUGE {
		t.Fatalf("ALIGN_RATE = %v; want 0.6/s as a GAUGE", rate)
	}
	agg.CrossSeriesReducer = monitoringpb.Aggregation_REDUCE_SUM
	sum := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: name, Filter: filter, Interval: iv, Aggregation: agg}))
	if len(sum) != 1 || math.Abs(sum[0].GetPoints()[0].GetValue().GetDoubleValue()-1.8) > 1e-9 {
		t.Fatalf("REDUCE_SUM = %v; want 1.8/s over both zones", sum)
	}
	agg.GroupByFields = []string{"metric.label.zone"}
	heads := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: name, Filter: filter, Interval: iv, Aggregation: agg,
		View: monitoringpb.ListTimeSeriesRequest_HEADERS}))
	if len(heads) != 2 || len(heads[0].GetPoints()) != 0 || heads[0].GetMetric().GetLabels()["zone"] == "" {
		t.Fatalf("grouped HEADERS = %v; want two series by zone, no points", heads)
	}
	it2 := c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: name, Filter: `resource.type = "generic_task"`, Interval: iv})
	_, err = it2.Next()
	wantMonitoringCode(t, "a filter with no metric type", err, codes.InvalidArgument, "metric type")

	// A gauge with no descriptor.
	gauge := fmt.Sprintf("custom.googleapis.com/compat/temperature_%d", n)
	if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: name, TimeSeries: []*monitoringpb.TimeSeries{{
		Metric: &metricpb.Metric{Type: gauge, Labels: map[string]string{"room": "lab"}}, Resource: &mrpb.MonitoredResource{Type: "global"},
		Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.Now()},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: 21.5}}}}}}}); err != nil {
		t.Fatalf("CreateTimeSeries (gauge): %v", err)
	}
	auto, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: name + "/metricDescriptors/" + gauge})
	if err != nil || auto.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || auto.GetValueType() != metricpb.MetricDescriptor_DOUBLE ||
		auto.GetDescription() != "Auto created custom metric." || len(auto.GetLabels()) != 1 {
		t.Errorf("auto-created descriptor = %v, %v", auto, err)
	}

	// Monitored resource descriptors.
	rit := c.ListMonitoredResourceDescriptors(ctx, &monitoringpb.ListMonitoredResourceDescriptorsRequest{Name: name, Filter: `resource.type = starts_with("k8s_")`})
	k8s := 0
	for {
		d, err := rit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMonitoredResourceDescriptors: %v", err)
		}
		k8s++
		if !strings.HasPrefix(d.GetType(), "k8s_") {
			t.Errorf("filtered list has %s", d.GetType())
		}
	}
	if k8s != 4 {
		t.Errorf("%d k8s_ resource types; want k8s_cluster, k8s_container, k8s_node, k8s_pod", k8s)
	}
	if d, err := c.GetMonitoredResourceDescriptor(ctx, &monitoringpb.GetMonitoredResourceDescriptorRequest{Name: name + "/monitoredResourceDescriptors/gce_instance"}); err != nil || len(d.GetLabels()) != 3 {
		t.Errorf("GetMonitoredResourceDescriptor(gce_instance) = %v, %v", d, err)
	}

	for _, m := range []string{counter, gauge} {
		if err := c.DeleteMetricDescriptor(ctx, &monitoringpb.DeleteMetricDescriptorRequest{Name: name + "/metricDescriptors/" + m}); err != nil {
			t.Fatalf("DeleteMetricDescriptor(%s): %v", m, err)
		}
		_, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: name + "/metricDescriptors/" + m})
		wantMonitoringCode(t, "a deleted descriptor", err, codes.NotFound, "Could not find descriptor")
	}
}

// TestMonitoringLogBasedMetrics (#1939): a counter log-based metric with a
// label extractor and a distribution one, made with the Logging API's
// MetricsServiceV2 client, and entries
// written with cloud.google.com/go/logging after them, yield
// logging.googleapis.com/user/* series read through the official Monitoring
// client: counts per extracted label with ALIGN_DELTA and REDUCE_SUM, and
// the distribution's count and mean. Entries written before a metric
// existed are not counted, as on Google.
//
// covers: google.monitoring.v3.MetricService/ListTimeSeries, google.monitoring.v3.MetricService/GetMetricDescriptor
func TestMonitoringLogBasedMetrics(t *testing.T) {
	h := New(t)
	c := monitoringClient(t, h)
	ctx := h.Context()
	lopts := []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	parent := "projects/" + h.Project()
	lc, err := logging.NewClient(ctx, parent, lopts...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	lg := lc.Logger("checkout", logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	write := func(code int, ms float64) {
		t.Helper()
		if err := lg.LogSync(ctx, logging.Entry{Severity: logging.Error, Payload: map[string]any{"code": fmt.Sprint(code), "latency_ms": ms}}); err != nil {
			t.Fatalf("LogSync: %v", err)
		}
	}
	write(500, 1) // before the metrics exist: not counted
	n := time.Now().UnixNano() % 1e6
	errs, lat := fmt.Sprintf("checkout_errors_%d", n), fmt.Sprintf("checkout_latency_%d", n)
	logFilter := fmt.Sprintf(`logName = "%s/logs/checkout" AND severity >= ERROR`, parent)
	mc, err := vlogging.NewMetricsClient(ctx, lopts...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	if _, err := mc.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: parent, Metric: &loggingpb.LogMetric{
		Name: errs, Filter: logFilter, LabelExtractors: map[string]string{"code": "EXTRACT(jsonPayload.code)"},
		MetricDescriptor: &metricpb.MetricDescriptor{Labels: []*labelpb.LabelDescriptor{{Key: "code", ValueType: labelpb.LabelDescriptor_STRING}}}}}); err != nil {
		t.Fatalf("CreateLogMetric (counter): %v", err)
	}
	if _, err := mc.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: parent, Metric: &loggingpb.LogMetric{
		Name: lat, Filter: logFilter, ValueExtractor: "EXTRACT(jsonPayload.latency_ms)",
		MetricDescriptor: &metricpb.MetricDescriptor{MetricKind: metricpb.MetricDescriptor_DELTA, ValueType: metricpb.MetricDescriptor_DISTRIBUTION, Unit: "ms"},
		BucketOptions: &distributionpb.Distribution_BucketOptions{Options: &distributionpb.Distribution_BucketOptions_LinearBuckets{
			LinearBuckets: &distributionpb.Distribution_BucketOptions_Linear{NumFiniteBuckets: 10, Width: 10, Offset: 0}}}}}); err != nil {
		t.Fatalf("CreateLogMetric (distribution): %v", err)
	}
	time.Sleep(10 * time.Millisecond) // receive times after the metrics' create times
	for i, code := range []int{500, 503, 500, 500, 503} {
		write(code, float64(10*(i+1)))
	}

	end := time.Now().Add(time.Minute)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-10 * time.Minute)), EndTime: timestamppb.New(end)}
	md, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: parent + "/metricDescriptors/logging.googleapis.com/user/" + errs})
	if err != nil || md.GetMetricKind() != metricpb.MetricDescriptor_DELTA || md.GetValueType() != metricpb.MetricDescriptor_INT64 {
		t.Fatalf("the counter's descriptor = %v, %v; want DELTA INT64", md, err)
	}
	counts := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: parent,
		Filter: fmt.Sprintf(`metric.type = "logging.googleapis.com/user/%s"`, errs), Interval: iv,
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(10 * time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM, GroupByFields: []string{"metric.label.code"}}}))
	got := map[string]int64{}
	for _, ts := range counts {
		for _, p := range ts.GetPoints() {
			got[ts.GetMetric().GetLabels()["code"]] += p.GetValue().GetInt64Value()
		}
	}
	if got["500"] != 3 || got["503"] != 2 {
		t.Errorf("counts by code = %v; want 500:3 503:2 (the entry before the metric not counted)", got)
	}
	dists := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: parent,
		Filter: fmt.Sprintf(`metric.type = "logging.googleapis.com/user/%s"`, lat), Interval: iv,
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(10 * time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM}}))
	if len(dists) != 1 || len(dists[0].GetPoints()) != 1 {
		t.Fatalf("distribution series = %v", dists)
	}
	d := dists[0].GetPoints()[0].GetValue().GetDistributionValue()
	if d.GetCount() != 5 || d.GetMean() != 30 {
		t.Errorf("distribution = count %d mean %v; want 5 and 30", d.GetCount(), d.GetMean())
	}
}

// TestMonitoringRequestCounts (#1939): the calls this instance serves are
// serviceruntime.googleapis.com/api/request_count on consumed_api, in the
// instance's project, by service and response code.
//
// covers: google.monitoring.v3.MetricService/ListTimeSeries
func TestMonitoringRequestCounts(t *testing.T) {
	h := New(t)
	c := monitoringClient(t, h)
	ctx := h.Context()
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, cliArgs()...)...).Output()
	var st struct{ Project string }
	if err := json.Unmarshal(out, &st); err != nil || st.Project == "" {
		t.Fatalf("status gave no project: %s", out)
	}
	name := "projects/" + st.Project
	for i := 0; i < 3; i++ {
		_, _ = c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: name + "/metricDescriptors/custom.googleapis.com/absent"})
	}
	end := time.Now().Add(time.Minute)
	series := listAll(t, c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: name,
		Filter:   `metric.type = "serviceruntime.googleapis.com/api/request_count" AND resource.labels.service = "monitoring.googleapis.com" AND resource.labels.method = "GetMetricDescriptor" AND metric.labels.protocol = "grpc" AND metric.labels.grpc_status_code = "5"`,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-time.Hour)), EndTime: timestamppb.New(end)}}))
	var total int64
	for _, ts := range series {
		if ts.GetResource().GetType() != "consumed_api" || ts.GetMetric().GetLabels()["response_code"] != "404" {
			t.Errorf("series %v", ts)
		}
		for _, p := range ts.GetPoints() {
			total += p.GetValue().GetInt64Value()
		}
	}
	if total < 3 {
		t.Errorf("request_count of NOT_FOUND GetMetricDescriptor calls = %d; want at least 3", total)
	}
}

// TestTerraformMonitoringMetricDescriptor (#1939): with egress to Google
// blocked, a google_monitoring_metric_descriptor applies over the JSON API,
// plans clean, updates its description and labels in place, and destroys;
// the official client reads what Terraform made.
//
// covers: google.monitoring.v3.MetricService/CreateMetricDescriptor, google.monitoring.v3.MetricService/GetMetricDescriptor, google.monitoring.v3.MetricService/DeleteMetricDescriptor
func TestTerraformMonitoringMetricDescriptor(t *testing.T) {
	h := New(t)
	c := monitoringClient(t, h) // skips without CLOUDBURROW_TEST_MONITORING
	ctx := h.Context()
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	typ := fmt.Sprintf("custom.googleapis.com/tf/queue_depth_%d", n)
	conf := func(desc, extraLabel string) string {
		return fmt.Sprintf(`
resource "google_monitoring_metric_descriptor" "d" {
  project      = %[1]q
  description  = %[3]q
  display_name = "Queue depth"
  type         = %[2]q
  metric_kind  = "GAUGE"
  value_type   = "INT64"
  unit         = "1"
  labels {
    key         = "queue"
    value_type  = "STRING"
    description = "The queue."
  }
%[4]s}
`, m.project, typ, desc, extraLabel)
	}
	m.write(conf("Depth of the queue", ""))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/metricDescriptors/" + typ
	md, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: name})
	if err != nil || md.GetDisplayName() != "Queue depth" || md.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || len(md.GetLabels()) != 1 {
		t.Fatalf("the descriptor Terraform made = %v, %v", md, err)
	}
	m.planClean()
	m.write(conf("Messages waiting", `  labels {
    key        = "region"
    value_type = "STRING"
  }
`))
	m.apply()
	md, err = c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: name})
	if err != nil || md.GetDescription() != "Messages waiting" || len(md.GetLabels()) != 2 {
		t.Fatalf("after the update = %v, %v; want the new description and both labels", md, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: %v; want NOT_FOUND", err)
	}
}
