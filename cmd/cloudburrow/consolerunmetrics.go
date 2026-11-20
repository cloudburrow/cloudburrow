package main

// The Metrics tab of a Cloud Run service and of a Cloud Run function
// (#2049): Google's charts Request count, Request latencies and Container
// instance count (docs.cloud.google.com/run/docs/monitoring), each one
// ListTimeSeries through the official Monitoring client at this instance's
// Cloud Monitoring, over the last hour in one-minute alignment, as
// Google's charts query run.googleapis.com/request_count,
// request_latencies and container/instance_count.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	monitoringclient "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// runMetricsAddr is this instance's Cloud Monitoring address, set when it
// starts; nil, or "" from it, while it is not served.
var runMetricsAddr atomic.Pointer[func() string]

func setRunMetricsEndpoint(addr func() string) { runMetricsAddr.Store(&addr) }

// runMetricsQuery is one chart's query: the metric, how each series is
// aligned and how the series are combined.
type runMetricsQuery struct {
	metric  string
	aligner monitoringpb.Aggregation_Aligner
	reducer monitoringpb.Aggregation_Reducer
	groupBy []string
	// label names a series: the value of its first group-by label, or
	// this.
	label, unit string
}

// runMetricsQueries are Google's three charts, in Google's order.
func runMetricsQueries() []runMetricsQuery {
	return []runMetricsQuery{
		{metric: monitoring.RunRequestCount, aligner: monitoringpb.Aggregation_ALIGN_RATE, reducer: monitoringpb.Aggregation_REDUCE_SUM,
			groupBy: []string{"metric.label.response_code_class"}, label: "Request count", unit: "requests/s"},
		{metric: monitoring.RunRequestLatencies, aligner: monitoringpb.Aggregation_ALIGN_DELTA, reducer: monitoringpb.Aggregation_REDUCE_PERCENTILE_99,
			label: "Request latencies: 99%", unit: "ms"},
		{metric: monitoring.RunRequestLatencies, aligner: monitoringpb.Aggregation_ALIGN_DELTA, reducer: monitoringpb.Aggregation_REDUCE_PERCENTILE_95,
			label: "Request latencies: 95%", unit: "ms"},
		{metric: monitoring.RunRequestLatencies, aligner: monitoringpb.Aggregation_ALIGN_DELTA, reducer: monitoringpb.Aggregation_REDUCE_PERCENTILE_50,
			label: "Request latencies: 50%", unit: "ms"},
		{metric: monitoring.RunInstanceCount, aligner: monitoringpb.Aggregation_ALIGN_MAX, reducer: monitoringpb.Aggregation_REDUCE_SUM,
			groupBy: []string{"metric.label.state"}, label: "Container instance count", unit: "instances"},
	}
}

// runMetricsRequest is a chart's ListTimeSeries for one service.
func runMetricsRequest(q runMetricsQuery, project, location, service string, now time.Time) *monitoringpb.ListTimeSeriesRequest {
	return &monitoringpb.ListTimeSeriesRequest{
		Name: "projects/" + project,
		Filter: fmt.Sprintf(`metric.type = %q AND resource.type = "cloud_run_revision" AND resource.labels.service_name = %q AND resource.labels.location = %q`,
			q.metric, service, location),
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now)},
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Minute), PerSeriesAligner: q.aligner,
			CrossSeriesReducer: q.reducer, GroupByFields: q.groupBy},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}
}

// runChartSeries is a response's series as chart lines, oldest point
// first.
func runChartSeries(q runMetricsQuery, in []*monitoringpb.TimeSeries) []console.ChartSeries {
	var out []console.ChartSeries
	for _, ts := range in {
		label := q.label
		if len(q.groupBy) > 0 {
			key := q.groupBy[0][len("metric.label."):]
			if v := ts.GetMetric().GetLabels()[key]; v != "" {
				label += " (" + v + ")"
			}
		}
		cs := console.ChartSeries{Label: label, Unit: q.unit, Points: []console.ChartPoint{}}
		pts := ts.GetPoints()
		sort.Slice(pts, func(i, j int) bool {
			return pts[i].GetInterval().GetEndTime().AsTime().Before(pts[j].GetInterval().GetEndTime().AsTime())
		})
		for _, p := range pts {
			v := p.GetValue().GetDoubleValue()
			if _, ok := p.GetValue().GetValue().(*monitoringpb.TypedValue_Int64Value); ok {
				v = float64(p.GetValue().GetInt64Value())
			}
			cs.Points = append(cs.Points, console.ChartPoint{At: p.GetInterval().GetEndTime().AsTime().UTC().Format(time.RFC3339), Value: &v})
		}
		out = append(out, cs)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// runMetricsClient is the official Monitoring client at this instance's
// Cloud Monitoring.
func runMetricsClient(ctx context.Context) (*monitoringclient.MetricClient, error) {
	addr := ""
	if f := runMetricsAddr.Load(); f != nil && *f != nil {
		addr = (*f)()
	}
	if addr == "" {
		return nil, errors.New("Cloud Monitoring is not served on this instance")
	}
	return monitoringclient.NewMetricClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
}

// runMetricsSection is the Metrics tab of the Cloud Run service service
// (its ID) in project and location: a function's is its service's.
func runMetricsSection(ctx context.Context, project, location, service string) console.Section {
	sec := console.Section{ID: "metrics", Label: "Metrics", Kind: console.KindChart,
		Note: "The last hour, in one-minute points, from Cloud Monitoring's run.googleapis.com metrics: requests and their " +
			"latencies as the cluster's ingress counts them, and the revisions' container instances. Sampled every 60 seconds."}
	c, err := runMetricsClient(ctx)
	if err != nil {
		sec.Kind, sec.Note, sec.Text = console.KindText, "", "Metrics are not available: "+err.Error()
		return sec
	}
	defer func() { _ = c.Close() }()
	now := time.Now()
	for _, q := range runMetricsQueries() {
		var got []*monitoringpb.TimeSeries
		it := c.ListTimeSeries(ctx, runMetricsRequest(q, project, location, service, now))
		for {
			ts, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				sec.Kind, sec.Note, sec.Text = console.KindText, "", "Metrics are not available: "+err.Error()
				sec.Series = nil
				return sec
			}
			got = append(got, ts)
		}
		series := runChartSeries(q, got)
		if len(series) == 0 {
			series = []console.ChartSeries{{Label: q.label, Unit: q.unit, Points: []console.ChartPoint{}}}
		}
		sec.Series = append(sec.Series, series...)
	}
	return sec
}

// functionMetricsSection is a function's Metrics tab: its Cloud Run
// service's, the service a function runs as (named after the function
// until it has deployed).
func functionMetricsSection(ctx context.Context, fn *functionspb.Function) console.Section {
	name := fn.GetServiceConfig().GetService()
	if name == "" {
		name = strings.Replace(fn.GetName(), "/functions/", "/services/", 1)
	}
	parts := strings.Split(name, "/")
	if len(parts) != 6 {
		return console.Section{ID: "metrics", Label: "Metrics", Kind: console.KindText, Text: "Metrics are not available: the function has no service"}
	}
	return runMetricsSection(ctx, parts[1], parts[3], parts[5])
}
