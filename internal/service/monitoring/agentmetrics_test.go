package monitoring

import (
	"context"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
)

// The Ops Agent's metrics (#2139): agent.googleapis.com/ time series are
// written against a gce_instance as the agent writes them, readable with
// their descriptor made from the write; every other domain Google defines
// is still refused, as is an agent metric on another resource, and an
// agent descriptor cannot be created or deleted.
func TestOpsAgentMetricsAreWritten(t *testing.T) {
	s, now := newTestServer(t, "")
	const m = "agent.googleapis.com/cpu/utilization"
	ts := tseries(m, map[string]string{"cpu_number": "0", "cpu_state": "user"}, gaugePoint(*now, 12.5))
	ts.Resource = &monitoredres.MonitoredResource{Type: "gce_instance",
		Labels: map[string]string{"project_id": "p1", "instance_id": "4242", "zone": "us-central1-a"}}
	if err := write(s, ts); err != nil {
		t.Fatal(err)
	}
	md, err := s.GetMetricDescriptor(context.Background(), &monitoringpb.GetMetricDescriptorRequest{Name: "projects/p1/metricDescriptors/" + m})
	if err != nil || md.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || md.GetValueType() != metricpb.MetricDescriptor_DOUBLE {
		t.Fatalf("descriptor: %v %v", md, err)
	}
	it := s.series["p1"]
	if len(it) != 1 {
		t.Fatalf("series: %v", it)
	}
	if s.countDescriptors("p1") != 0 {
		t.Errorf("an agent descriptor counts against the project's quota")
	}
	global := tseries(m, nil, gaugePoint(now.Add(10e9), 1))
	wantCode(t, write(s, global), codes.InvalidArgument, "not defined for the resource type")
	wantCode(t, write(s, tseries("compute.googleapis.com/instance/cpu", nil, gaugePoint(*now, 1))), codes.InvalidArgument, "not supported on the metric domain")
	wantCode(t, write(s, tseries("agent.googleapis.com/", nil, gaugePoint(*now, 1))), codes.InvalidArgument, "non-empty path")
	_, err = s.CreateMetricDescriptor(context.Background(), &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/p1",
		MetricDescriptor: &metricpb.MetricDescriptor{Type: "agent.googleapis.com/x", MetricKind: metricpb.MetricDescriptor_GAUGE, ValueType: metricpb.MetricDescriptor_INT64}})
	if err == nil {
		t.Error("an agent descriptor was created")
	}
	_, err = s.DeleteMetricDescriptor(context.Background(), &monitoringpb.DeleteMetricDescriptorRequest{Name: "projects/p1/metricDescriptors/" + m})
	wantCode(t, err, codes.InvalidArgument, "cannot be deleted")
}
