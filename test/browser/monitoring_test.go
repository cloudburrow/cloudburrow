//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// envMonitoring is the Cloud Monitoring API v3 endpoint (#1939), gRPC and
// JSON on one port, as the compat harness names it.
const envMonitoring = "CLOUDBURROW_TEST_MONITORING"

// TestMonitoringThroughThePage (#1939): a custom metric's points written
// with the official client are charted in Metrics explorer, picked from
// Select a metric, and listed in its table, one row per series; Metrics
// management creates a user-defined metric through Create metric and
// deletes it through the row's Delete metric, each read back with the
// client.
func TestMonitoringThroughThePage(t *testing.T) {
	needService(t, "monitoring-explorer")
	addr := endpoint(t, envMonitoring)
	ctx := context.Background()
	c, err := monitoring.NewMetricClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	project := uniqueProject(t)
	metricType := "custom.googleapis.com/browser/queue_depth"
	now := time.Now()
	// Two series, one point each per request, the second request newer.
	for i, at := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		var series []*monitoringpb.TimeSeries
		for _, env := range []string{"prod", "dev"} {
			series = append(series, &monitoringpb.TimeSeries{
				Metric:   &metricpb.Metric{Type: metricType, Labels: map[string]string{"env": env}},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: float64(10*i + len(env))}}}},
			})
		}
		if err := c.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: series}); err != nil {
			t.Fatalf("write the points: %v", err)
		}
	}

	p := open(t)
	p.navigate("/monitoring/metrics-explorer?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#explorer-metric option")].some((o) => o.value === %q)`, metricType))
	p.setField("#explorer-resource", "global")
	p.setField("#explorer-metric", metricType)
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 2`)
	p.waitFor(`document.querySelectorAll("#view svg.explorer-chart polyline").length === 2`)
	var rows string
	p.eval(`[...document.querySelectorAll("#view .explorer-table tbody tr")].map((r) => r.textContent).join("|")`, &rows)
	if !strings.Contains(rows, "env=prod") || !strings.Contains(rows, "env=dev") {
		t.Errorf("the explorer's table = %q, want a row for env=prod and env=dev", rows)
	}

	// A reducer over both series: one row.
	p.setField("#explorer-aligner", "ALIGN_MEAN")
	p.setField("#explorer-reducer", "REDUCE_SUM")
	p.clickText("#view .explorer-builder button", "Apply")
	p.waitFor(`document.querySelectorAll("#view .explorer-table tbody tr").length === 1`)

	// Metrics management: Create metric, then Delete metric.
	p.navigate("/monitoring/metrics-management?project=" + project)
	p.clickText("#view button", "Create metric")
	p.submitForm(map[string]string{"type": "browser/created", "metricKind": "GAUGE", "valueType": "INT64", "description": "from the page"}, "type")
	created := "custom.googleapis.com/browser/created"
	p.waitFor(fmt.Sprintf(`location.pathname === "/monitoring/metrics-management" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, created))
	d, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + created})
	if err != nil {
		t.Fatalf("read the metric Create metric made: %v", err)
	}
	if d.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || d.GetValueType() != metricpb.MetricDescriptor_INT64 || d.GetDescription() != "from the page" {
		t.Errorf("the metric Create metric made = %v", d)
	}
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, created)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Delete metric")
	p.confirmByName(created)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, created))
	if _, err := c.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + created}); err == nil {
		t.Error("the metric Delete metric deleted is still there")
	}
}
