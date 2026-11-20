package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

func monitoringScreenFixture(t *testing.T) (monitoringScreen, *monitoring.Server) {
	t.Helper()
	s, err := monitoring.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	return monitoringScreen{api: func() monitoringpb.MetricServiceServer { return s }}, s
}

// Metrics management's Create metric and Delete metric, and Metrics
// explorer's Select a metric, chart query (raw, then aligned and reduced)
// and resource types, over the server in process (#1939).
func TestConsoleMonitoringExplorerAndMetrics(t *testing.T) {
	ctx := context.Background()
	screen, srv := monitoringScreenFixture(t)
	mgmt, explorer := monitoringMetricsProvider{screen}, monitoringExplorerProvider{screen}
	const project = "mon-console"

	name, err := mgmt.Create(ctx, project, map[string]string{"type": "shop/sales", "displayName": "Sales", "metricKind": "GAUGE",
		"valueType": "DOUBLE", "unit": "1", "labels": `{"env":"the environment"}`})
	if err != nil {
		t.Fatal(err)
	}
	if name != "custom.googleapis.com/shop/sales" {
		t.Fatalf("Create metric = %q", name)
	}
	d, err := srv.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + name})
	if err != nil {
		t.Fatal(err)
	}
	if d.GetMetricKind() != metricpb.MetricDescriptor_GAUGE || len(d.GetLabels()) != 1 || d.GetLabels()[0].GetKey() != "env" {
		t.Errorf("the created descriptor = %v", d)
	}
	if _, err := mgmt.Create(ctx, project, map[string]string{"type": "bad type", "metricKind": "GAUGE", "valueType": "DOUBLE"}); err == nil {
		t.Error("Create metric took a type with a space")
	}

	now := time.Now()
	for i, at := range []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute)} {
		var series []*monitoringpb.TimeSeries
		for _, env := range []string{"prod", "dev"} {
			series = append(series, &monitoringpb.TimeSeries{
				Metric:   &metricpb.Metric{Type: name, Labels: map[string]string{"env": env}},
				Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}},
				Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(at)},
					Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: float64(i + 1)}}}},
			})
		}
		if _, err := srv.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + project, TimeSeries: series}); err != nil {
			t.Fatal(err)
		}
	}

	ml, err := mgmt.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range ml.Items {
		if it.Name == name {
			found = true
			if it.Fields["Metric kind"] != "GAUGE" || it.Fields["Value type"] != "DOUBLE" || it.Fields["Labels"] != "env" {
				t.Errorf("Metrics management's row = %v", it.Fields)
			}
		}
	}
	if !found {
		t.Fatalf("Metrics management lists no %s: %v", name, ml.Items)
	}
	el, err := explorer.List(ctx, project)
	if err != nil || len(el.Items) == 0 {
		t.Fatalf("Select a metric = %v, %v", el.Items, err)
	}

	raw, err := explorer.Build(ctx, project, []string{"time-series"}, map[string]string{"metric": name, "resource": "global", "range": "3600"})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Items) != 2 {
		t.Fatalf("the chart's series = %v", raw.Items)
	}
	for _, it := range raw.Items {
		var pts []explorerPoint
		if err := json.Unmarshal([]byte(it.Fields["points"]), &pts); err != nil || len(pts) != 2 {
			t.Fatalf("series %s points = %q (%v)", it.Name, it.Fields["points"], err)
		}
		if *pts[0].Value != 1 || *pts[1].Value != 2 || it.Fields["Value"] != "2" {
			t.Errorf("series %s: points %v, latest %q; want oldest first 1, 2 and latest 2", it.Name, it.Fields["points"], it.Fields["Value"])
		}
		if !strings.HasPrefix(it.Name, "env=") {
			t.Errorf("series name %q does not name its labels", it.Name)
		}
	}

	filtered, err := explorer.Build(ctx, project, []string{"time-series"}, map[string]string{"metric": name, "filter": `metric.labels.env = "prod"`})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Fields["metric.labels.env"] != "prod" {
		t.Errorf("the filtered chart = %v", filtered.Items)
	}

	reduced, err := explorer.Build(ctx, project, []string{"time-series"}, map[string]string{"metric": name,
		"aligner": "ALIGN_MEAN", "alignmentPeriod": "60", "reducer": "REDUCE_SUM"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reduced.Items) != 1 {
		t.Errorf("REDUCE_SUM with no group-by made %d series, want 1: %v", len(reduced.Items), reduced.Items)
	}
	if _, err := explorer.Build(ctx, project, []string{"time-series"}, map[string]string{"metric": name, "aligner": "ALIGN_SIDEWAYS"}); err == nil {
		t.Error("the explorer took an aligner that does not exist")
	}

	rt, err := explorer.Build(ctx, project, []string{"resource-types"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hasGlobal := false
	for _, it := range rt.Items {
		hasGlobal = hasGlobal || it.Name == "global"
	}
	if !hasGlobal {
		t.Errorf("the resource types have no global: %v", rt.Items)
	}

	if err := mgmt.Delete(ctx, project, "serviceruntime.googleapis.com/api/request_count"); err == nil {
		t.Error("Delete metric deleted a built-in metric")
	}
	if err := mgmt.Delete(ctx, project, name); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.GetMetricDescriptor(ctx, &monitoringpb.GetMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + name}); err == nil {
		t.Error("the deleted metric is still there")
	}

	// Before the service starts, both pages say so.
	idle := monitoringScreen{api: monitoringAPIOf(nil)}
	if _, err := (monitoringMetricsProvider{idle}).List(ctx, project); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Errorf("an idle Metrics management = %v", err)
	}
}

func TestExplorerFilter(t *testing.T) {
	for _, c := range []struct {
		values map[string]string
		want   string
	}{
		{map[string]string{"metric": "custom.googleapis.com/a"}, `metric.type = "custom.googleapis.com/a"`},
		{map[string]string{"metric": "custom.googleapis.com/a", "resource": "global", "filter": `metric.labels.x = "y"`},
			`metric.type = "custom.googleapis.com/a" AND resource.type = "global" AND (metric.labels.x = "y")`},
	} {
		got, err := explorerFilter(c.values)
		if err != nil || got != c.want {
			t.Errorf("explorerFilter(%v) = %q, %v; want %q", c.values, got, err, c.want)
		}
	}
	for _, bad := range []map[string]string{{}, {"metric": `a" OR "b`}} {
		if _, err := explorerFilter(bad); err == nil {
			t.Errorf("explorerFilter(%v) took it", bad)
		}
	}
}
