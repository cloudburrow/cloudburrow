package monitoring

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T, state string) (*Server, *time.Time) {
	t.Helper()
	s, err := NewServer(state)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func gaugePoint(end time.Time, v float64) *monitoringpb.Point {
	return &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(end)},
		Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}}}
}

func tseries(metric string, labels map[string]string, p *monitoringpb.Point) *monitoringpb.TimeSeries {
	return &monitoringpb.TimeSeries{
		Metric:   &metricpb.Metric{Type: metric, Labels: labels},
		Resource: &monitoredres.MonitoredResource{Type: "global"},
		Points:   []*monitoringpb.Point{p},
	}
}

func write(s *Server, ts ...*monitoringpb.TimeSeries) error {
	_, err := s.CreateTimeSeries(context.Background(), &monitoringpb.CreateTimeSeriesRequest{Name: "projects/p1", TimeSeries: ts})
	return err
}

func wantCode(t *testing.T, err error, c codes.Code, substr string) {
	t.Helper()
	if status.Code(err) != c || !strings.Contains(status.Convert(err).Message(), substr) {
		t.Fatalf("got %v, want %s containing %q", err, c, substr)
	}
}

func TestWriteRules(t *testing.T) {
	s, now := newTestServer(t, "")
	const m = "custom.googleapis.com/test/latency"
	if err := write(s, tseries(m, map[string]string{"env": "dev"}, gaugePoint(*now, 1))); err != nil {
		t.Fatal(err)
	}
	// Auto-created descriptor.
	md, err := s.GetMetricDescriptor(context.Background(), &monitoringpb.GetMetricDescriptorRequest{Name: "projects/p1/metricDescriptors/" + m})
	if err != nil {
		t.Fatal(err)
	}
	if md.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || md.GetValueType() != metricpb.MetricDescriptor_DOUBLE ||
		md.GetDescription() != "Auto created custom metric." || len(md.GetLabels()) != 1 || md.GetLabels()[0].GetKey() != "env" {
		t.Fatalf("auto-created descriptor %v", md)
	}
	// Too soon, out of order, two points, duplicates, wrong type, future, past.
	wantCode(t, write(s, tseries(m, map[string]string{"env": "dev"}, gaugePoint(now.Add(2*time.Second), 2))), codes.FailedPrecondition, "more frequently")
	wantCode(t, write(s, tseries(m, map[string]string{"env": "dev"}, gaugePoint(now.Add(-time.Minute), 2))), codes.InvalidArgument, "Points must be written in order")
	two := tseries(m, nil, gaugePoint(*now, 1))
	two.Points = append(two.Points, gaugePoint(*now, 2))
	wantCode(t, write(s, two), codes.InvalidArgument, "Only one point can be written per TimeSeries per request")
	a, b := tseries(m, map[string]string{"env": "x"}, gaugePoint(*now, 1)), tseries(m, map[string]string{"env": "x"}, gaugePoint(*now, 2))
	wantCode(t, write(s, a, b), codes.InvalidArgument, "Duplicate TimeSeries encountered")
	intPoint := tseries(m, map[string]string{"env": "y"}, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(*now)},
		Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 3}}})
	wantCode(t, write(s, intPoint), codes.InvalidArgument, "must be DOUBLE, but is INT64")
	wantCode(t, write(s, tseries(m, map[string]string{"env": "z"}, gaugePoint(now.Add(6*time.Minute), 1))), codes.InvalidArgument, "5m into the future")
	wantCode(t, write(s, tseries(m, map[string]string{"env": "z"}, gaugePoint(now.Add(-26*time.Hour), 1))), codes.InvalidArgument, "24 hours in the past")
	wantCode(t, write(s, tseries("compute.googleapis.com/instance/cpu", nil, gaugePoint(*now, 1))), codes.InvalidArgument, "not supported on the metric domain")
	bad := tseries(m, map[string]string{"env": "z"}, gaugePoint(*now, 1))
	bad.Resource = &monitoredres.MonitoredResource{Type: "cloud_run_revision"}
	wantCode(t, write(s, bad), codes.InvalidArgument, "unrecognized resource type")
	gs := tseries(m, map[string]string{"env": "z"}, gaugePoint(*now, 1))
	gs.Points[0].Interval.StartTime = timestamppb.New(now.Add(-time.Second))
	wantCode(t, write(s, gs), codes.InvalidArgument, "must be equal to the end time")
	var many []*monitoringpb.TimeSeries
	for i := 0; i < 201; i++ {
		many = append(many, tseries(m, map[string]string{"env": string(rune('a' + i%26)), "n": strings.Repeat("x", i/26)}, gaugePoint(*now, 1)))
	}
	wantCode(t, write(s, many...), codes.InvalidArgument, "200")
	// Five seconds later is fine.
	if err := write(s, tseries(m, map[string]string{"env": "dev"}, gaugePoint(now.Add(5*time.Second), 2))); err != nil {
		t.Fatal(err)
	}
	// DELTA and STRING are refused for user-defined metrics.
	_, err = s.CreateMetricDescriptor(context.Background(), &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/p1",
		MetricDescriptor: &metricpb.MetricDescriptor{Type: "custom.googleapis.com/d", MetricKind: metricpb.MetricDescriptor_DELTA, ValueType: metricpb.MetricDescriptor_INT64}})
	wantCode(t, err, codes.InvalidArgument, "DELTA")
}

func TestPartialWriteSummary(t *testing.T) {
	s, now := newTestServer(t, "")
	const m = "custom.googleapis.com/partial"
	ok := tseries(m, map[string]string{"k": "a"}, gaugePoint(*now, 1))
	bad := tseries(m, map[string]string{"k": "b"}, gaugePoint(now.Add(time.Hour), 1))
	err := write(s, ok, bad)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
	var sum *monitoringpb.CreateTimeSeriesSummary
	for _, d := range status.Convert(err).Details() {
		if x, ok := d.(*monitoringpb.CreateTimeSeriesSummary); ok {
			sum = x
		}
	}
	if sum == nil || sum.GetTotalPointCount() != 2 || sum.GetSuccessPointCount() != 1 {
		t.Fatalf("summary %v", sum)
	}
}

func listSeries(t *testing.T, s *Server, req *monitoringpb.ListTimeSeriesRequest) []*monitoringpb.TimeSeries {
	t.Helper()
	if req.Name == "" {
		req.Name = "projects/p1"
	}
	resp, err := s.ListTimeSeries(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetTimeSeries()
}

func TestListAlignReduce(t *testing.T) {
	s, now := newTestServer(t, "")
	const m = "custom.googleapis.com/q"
	start := *now
	// Two series, a point every 10s for 3 minutes: zone a has i, b has 2i.
	for i := 0; i < 18; i++ {
		*now = start.Add(time.Duration(i) * 10 * time.Second)
		for _, z := range []struct {
			zone string
			mul  float64
		}{{"a", 1}, {"b", 2}} {
			if err := write(s, tseries(m, map[string]string{"zone": z.zone}, gaugePoint(*now, float64(i)*z.mul))); err != nil {
				t.Fatal(err)
			}
		}
	}
	end := start.Add(170 * time.Second)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}
	raw := listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "` + m + `" AND metric.labels.zone = "a"`, Interval: iv})
	if len(raw) != 1 || len(raw[0].GetPoints()) != 18 || raw[0].GetPoints()[0].GetValue().GetDoubleValue() != 17 {
		t.Fatalf("raw: %v", raw)
	}
	// Newest first.
	if !raw[0].Points[0].Interval.EndTime.AsTime().Equal(end) {
		t.Fatalf("first point at %v", raw[0].Points[0].Interval.EndTime.AsTime())
	}
	agg := &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MEAN}
	aligned := listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "` + m + `" metric.label.zone = one_of("a")`, Interval: iv, Aggregation: agg})
	// Windows end at 170s, 110s, 50s: (110,170] has 120..170 => mean 14.5.
	if len(aligned) != 1 || len(aligned[0].Points) != 3 || aligned[0].Points[0].GetValue().GetDoubleValue() != 14.5 {
		t.Fatalf("aligned: %v", aligned)
	}
	agg.CrossSeriesReducer = monitoringpb.Aggregation_REDUCE_SUM
	reduced := listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "` + m + `"`, Interval: iv, Aggregation: agg})
	if len(reduced) != 1 || reduced[0].Points[0].GetValue().GetDoubleValue() != 14.5*3 || len(reduced[0].GetMetric().GetLabels()) != 0 {
		t.Fatalf("reduced: %v", reduced)
	}
	agg.GroupByFields = []string{"metric.label.zone"}
	grouped := listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "` + m + `"`, Interval: iv, Aggregation: agg,
		View: monitoringpb.ListTimeSeriesRequest_HEADERS})
	if len(grouped) != 2 || grouped[0].GetMetric().GetLabels()["zone"] != "a" || len(grouped[0].Points) != 0 {
		t.Fatalf("grouped: %v", grouped)
	}
	// Errors.
	_, err := s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `resource.type = "global"`, Interval: iv})
	wantCode(t, err, codes.InvalidArgument, "metric type")
	_, err = s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `metric.type = "` + m + `"`,
		Interval: iv, Aggregation: &monitoringpb.Aggregation{CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM}})
	wantCode(t, err, codes.InvalidArgument, "per-series aligner")
	_, err = s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `metric.type = "` + m + `"`,
		Interval: iv, Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(10 * time.Second), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MEAN}})
	wantCode(t, err, codes.InvalidArgument, "at least 60 seconds")
	_, err = s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `metric.type = "` + m + `"`,
		Interval: iv, Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_RATE}})
	wantCode(t, err, codes.InvalidArgument, "cannot be applied")
	_, err = s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/p1", Filter: `metric.type = "custom.googleapis.com/none"`, Interval: iv})
	wantCode(t, err, codes.NotFound, "Cannot find metric")
}

func TestCumulativeDeltaAndRate(t *testing.T) {
	s, now := newTestServer(t, "")
	const m = "custom.googleapis.com/count"
	_, err := s.CreateMetricDescriptor(context.Background(), &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/p1",
		MetricDescriptor: &metricpb.MetricDescriptor{Type: m, MetricKind: metricpb.MetricDescriptor_CUMULATIVE, ValueType: metricpb.MetricDescriptor_INT64,
			Labels: []*labelpb.LabelDescriptor{{Key: "k"}}}})
	if err != nil {
		t.Fatal(err)
	}
	st := *now
	for i := 1; i <= 12; i++ {
		*now = st.Add(time.Duration(i) * 10 * time.Second)
		p := &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(st), EndTime: timestamppb.New(*now)},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(i * 6)}}}
		if err := write(s, tseries(m, nil, p)); err != nil {
			t.Fatal(err)
		}
	}
	end := st.Add(120 * time.Second)
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(st), EndTime: timestamppb.New(end)}
	out := listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "` + m + `"`, Interval: iv,
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_RATE}})
	// (60s,120s]: 72 - 36 = 36 over 60s.
	if len(out) != 1 || math.Abs(out[0].Points[0].GetValue().GetDoubleValue()-0.6) > 1e-9 || out[0].GetMetricKind() != metricpb.MetricDescriptor_GAUGE {
		t.Fatalf("rate: %v", out)
	}
	// A cumulative point must keep the start time or reset after the last end.
	*now = now.Add(10 * time.Second)
	p := &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(st.Add(time.Second)), EndTime: timestamppb.New(*now)},
		Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 1}}}
	wantCode(t, write(s, tseries(m, nil, p)), codes.InvalidArgument, "start time")
}

func TestDistributionPercentile(t *testing.T) {
	opts := &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_LinearBuckets{
		LinearBuckets: &distribution.Distribution_BucketOptions_Linear{NumFiniteBuckets: 10, Width: 10, Offset: 0}}}
	var vs []float64
	for i := 0; i < 100; i++ {
		vs = append(vs, float64(i))
	}
	d := newDistribution(opts, vs)
	if d.GetCount() != 100 || d.GetMean() != 49.5 {
		t.Fatalf("distribution %v", d)
	}
	if p := distPercentile(d, 50); math.Abs(p-50) > 1 {
		t.Fatalf("p50 = %v", p)
	}
}

func TestPersistence(t *testing.T) {
	state := filepath.Join(t.TempDir(), "monitoring.json")
	s, now := newTestServer(t, state)
	if err := write(s, tseries("custom.googleapis.com/kept", nil, gaugePoint(*now, 7))); err != nil {
		t.Fatal(err)
	}
	s2, _ := newTestServer(t, state)
	out := listSeries(t, s2, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "custom.googleapis.com/kept"`,
		Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(*now)}})
	if len(out) != 1 || out[0].Points[0].GetValue().GetDoubleValue() != 7 {
		t.Fatalf("after restart: %v", out)
	}
	if _, err := s2.DeleteMetricDescriptor(context.Background(), &monitoringpb.DeleteMetricDescriptorRequest{Name: "projects/p1/metricDescriptors/custom.googleapis.com/kept"}); err != nil {
		t.Fatal(err)
	}
	_, err := s2.GetMetricDescriptor(context.Background(), &monitoringpb.GetMetricDescriptorRequest{Name: "projects/p1/metricDescriptors/custom.googleapis.com/kept"})
	wantCode(t, err, codes.NotFound, "Could not find descriptor")
}

func TestFilters(t *testing.T) {
	sub := subject{project: "p", metricType: "custom.googleapis.com/a/b", resourceType: "gce_instance",
		metricLabels: map[string]string{"k": "v1", "n": "5"}, resourceLabels: map[string]string{"zone": "us-east1-b"}}
	for f, want := range map[string]bool{
		`metric.type = "custom.googleapis.com/a/b"`:                                true,
		`metric.type = starts_with("custom.googleapis.com/")`:                      true,
		`metric.type != starts_with("custom.googleapis.com/")`:                     false,
		`resource.type = "gce_instance" AND resource.labels.zone = ends_with("b")`: true,
		`metric.labels.k = "v2" OR metric.labels.k = "v1"`:                         true,
		`NOT metric.labels.k = "v1"`:                                               false,
		`(metric.labels.k = "x" OR metric.label.k = "v1") resource.type : "gce"`:   true,
		`metric.labels.n > 3`:                                                      true,
		`metric.labels.k = monitoring.regex.full_match("v[0-9]")`:                  true,
		`metric.labels.missing = "x"`:                                              false,
	} {
		p, err := parseFilter(f, filterSeries)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if got := p.match(sub); got != want {
			t.Errorf("%s = %v, want %v", f, got, want)
		}
	}
	for _, bad := range []string{`metric.type =`, `foo = "x"`, `metadata.user_labels.a = "b"`, `metric.type = "a" (`} {
		if _, err := parseFilter(bad, filterSeries); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v", bad, err)
		}
	}
}

type fakeLogs struct{ samples []LogSample }

func (f fakeLogs) LogMetrics(string) []LogMetric {
	return []LogMetric{{Name: "errors", Labels: []*labelpb.LabelDescriptor{{Key: "code"}}}}
}

func (f fakeLogs) LogMetricSamples(_, _ string, from, to time.Time) ([]LogSample, error) {
	var out []LogSample
	for _, s := range f.samples {
		if !s.Time.Before(from) && !s.Time.After(to) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f fakeLogs) LogEntries(string, time.Time, time.Time) []LogEntry { return nil }

func TestLogBasedMetric(t *testing.T) {
	s, now := newTestServer(t, "")
	logs := fakeLogs{}
	for i := 0; i < 5; i++ {
		code := "500"
		if i%2 == 0 {
			code = "503"
		}
		logs.samples = append(logs.samples, LogSample{Time: t0.Add(time.Duration(i) * 10 * time.Second), Labels: map[string]string{"code": code}})
	}
	s.AddBuiltin(NewLogBuiltin(logs))
	resp, err := s.ListMetricDescriptors(context.Background(), &monitoringpb.ListMetricDescriptorsRequest{Name: "projects/p1",
		Filter: `metric.type = starts_with("logging.googleapis.com/user/")`})
	if err != nil || len(resp.GetMetricDescriptors()) != 1 || resp.GetMetricDescriptors()[0].GetMetricKind() != metricpb.MetricDescriptor_DELTA {
		t.Fatalf("descriptors %v %v", resp, err)
	}
	_, err = s.DeleteMetricDescriptor(context.Background(), &monitoringpb.DeleteMetricDescriptorRequest{Name: "projects/p1/metricDescriptors/logging.googleapis.com/user/errors"})
	wantCode(t, err, codes.InvalidArgument, "Built-in")
	iv := &monitoringpb.TimeInterval{StartTime: timestamppb.New(t0.Add(-time.Minute)), EndTime: timestamppb.New(now.Add(time.Minute))}
	out := listSeries(t, s, &monitoringpb.ListTimeSeriesRequest{Filter: `metric.type = "logging.googleapis.com/user/errors"`, Interval: iv,
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(5 * time.Minute), PerSeriesAligner: monitoringpb.Aggregation_ALIGN_DELTA,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM, GroupByFields: []string{"metric.label.code"}}})
	got := map[string]int64{}
	for _, ts := range out {
		for _, p := range ts.Points {
			got[ts.GetMetric().GetLabels()["code"]] += p.GetValue().GetInt64Value()
		}
	}
	if got["503"] != 3 || got["500"] != 2 {
		t.Fatalf("log-based counts %v (%v)", got, out)
	}
}
