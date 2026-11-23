package monitoring

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeLBSource struct{ vals []LBCustomMetricValue }

func (f *fakeLBSource) CustomMetricValues() []LBCustomMetricValue { return f.vals }

// TestLBBuiltinServesDryRunCustomMetrics (#2160): a dry-run custom
// metric's values are GAUGE points of lb_custom_metric, one per sample
// time, found by ListTimeSeries with a metric label filter.
func TestLBBuiltinServesDryRunCustomMetrics(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	src := &fakeLBSource{vals: []LBCustomMetricValue{{Project: "lb-project", BackendServiceID: "42", BackendServiceName: "web", Location: "global",
		GroupName: "neg-a", GroupType: "NETWORK_ENDPOINT_GROUP", Scope: "us-central1-a", ScopeType: "ZONE", Metric: "queue", DryRun: true,
		Value: 0.7, At: t0}}}
	b := NewLBBuiltin(src)
	b.Sample()
	b.Sample()
	src.vals[0].Value, src.vals[0].At = 0.5, t0.Add(time.Minute)
	b.Sample()
	s, err := NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	s.AddBuiltin(b)
	resp, err := s.ListTimeSeries(context.Background(), &monitoringpb.ListTimeSeriesRequest{Name: "projects/lb-project",
		Filter:   `metric.type = "` + LBCustomMetric + `" AND metric.labels.dry_run = "true" AND metric.labels.custom_metric_name = "queue"`,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(t0.Add(-time.Hour)), EndTime: timestamppb.New(time.Now())}})
	if err != nil || len(resp.GetTimeSeries()) != 1 {
		t.Fatalf("ListTimeSeries = %v, %v", resp, err)
	}
	ts := resp.GetTimeSeries()[0]
	if ts.GetResource().GetType() != "gce_backend_service" || ts.GetResource().GetLabels()["backend_service_id"] != "42" ||
		ts.GetMetric().GetLabels()["backend_name"] != "neg-a" || len(ts.GetPoints()) != 2 || ts.GetPoints()[0].GetValue().GetDoubleValue() != 0.5 {
		t.Fatalf("series = %v", ts)
	}
	md, err := s.GetMetricDescriptor(context.Background(), &monitoringpb.GetMetricDescriptorRequest{Name: "projects/lb-project/metricDescriptors/" + LBCustomMetric})
	if err != nil || md.GetDisplayName() != "Backend load balancing custom metrics" {
		t.Fatalf("descriptor = %v, %v", md, err)
	}
}
