package main

// Cloud SQL's charts (#2141): the Overview's chart, its metric chosen from
// a list with CPU utilization first, and System insights' charts and its
// CPU utilization - P99 | P50 card, read with ListTimeSeries from the
// instance's Cloud Monitoring, which records cloudsql.googleapis.com
// metrics from each instance's running server (monitoringsql.go). Metrics
// Explorer reads the same series.
// (https://docs.cloud.google.com/sql/docs/postgres/instance-info and
// https://docs.cloud.google.com/sql/docs/postgres/use-system-insights,
// read 2026-11-22.)

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	sqladminapi "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlChart is one chart: its label, metric, unit, how its minute is read,
// and a scale (cpu/utilization's fraction as a percent).
type sqlChart struct {
	label, metric, unit string
	gauge               bool
	scale, max          float64
	// engine limits it to "POSTGRES" or "MYSQL"; "" is every engine.
	engine string
	// pooling limits it to an instance with managed connection pooling
	// on (#2163); status, when set, is the series' status label; perDB
	// reads the cloudsql_instance_database resource (each database's
	// series); mean averages the series' readings, where the others sum.
	pooling, perDB, mean bool
	status               string
}

// sqlOverviewCharts are the Overview's chart choices, in Google's order.
var sqlOverviewCharts = []sqlChart{
	{label: "CPU utilization", metric: monitoring.SQLCPUUtilization, unit: "percent", gauge: true, scale: 100, max: 100},
	{label: "Memory usage", metric: monitoring.SQLMemoryUsage, unit: "bytes", gauge: true, scale: 1},
	{label: "Storage usage", metric: monitoring.SQLDiskBytesUsed, unit: "bytes", gauge: true, scale: 1},
	{label: "Ingress bytes", metric: monitoring.SQLReceivedBytes, unit: "bytes per minute", scale: 1},
	{label: "Egress bytes", metric: monitoring.SQLSentBytes, unit: "bytes per minute", scale: 1},
	{label: "Active connections", metric: monitoring.SQLPGBackends, unit: "connections", gauge: true, scale: 1, engine: "POSTGRES"},
	{label: "Active connections", metric: monitoring.SQLConnections, unit: "connections", gauge: true, scale: 1, engine: "MYSQL"},
	{label: "Transactions", metric: monitoring.SQLPGTransactions, unit: "transactions per minute", scale: 1, engine: "POSTGRES"},
	{label: "Queries", metric: monitoring.SQLMySQLQueries, unit: "queries per minute", scale: 1, engine: "MYSQL"},
	// Managed connection pooling's metrics (#2163), with pooling on.
	{label: "Connection pool threads (active)", metric: monitoring.SQLMySQLPoolThreads, unit: "threads", gauge: true, scale: 1, engine: "MYSQL", pooling: true, status: "active"},
	{label: "Connection pool threads (idle)", metric: monitoring.SQLMySQLPoolThreads, unit: "threads", gauge: true, scale: 1, engine: "MYSQL", pooling: true, status: "idle"},
	{label: "Connection pool pending connections", metric: monitoring.SQLMySQLPoolPending, unit: "connections", gauge: true, scale: 1, engine: "MYSQL", pooling: true},
	{label: "Connection pool average wait time", metric: monitoring.SQLMySQLPoolAvgWait, unit: "milliseconds", gauge: true, scale: 1, engine: "MYSQL", pooling: true},
	{label: "Client connections (active)", metric: monitoring.SQLPoolClientConns, unit: "connections", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true, status: "active"},
	{label: "Client connections (waiting)", metric: monitoring.SQLPoolClientConns, unit: "connections", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true, status: "waiting"},
	{label: "Server connections (active)", metric: monitoring.SQLPoolServerConns, unit: "connections", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true, status: "active"},
	{label: "Server connections (idle)", metric: monitoring.SQLPoolServerConns, unit: "connections", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true, status: "idle"},
	{label: "Number of connection pools", metric: monitoring.SQLPoolNumPools, unit: "pools", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true},
	{label: "Average wait time", metric: monitoring.SQLPoolClientAvgWait, unit: "microseconds", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true, mean: true},
	{label: "Client connection attempts count", metric: monitoring.SQLPoolClientAttempts, unit: "attempts since the count began", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true},
	{label: "Error count", metric: monitoring.SQLPoolClientErrors, unit: "errors since the count began", gauge: true, scale: 1, engine: "POSTGRES", pooling: true, perDB: true},
}

// sqlSystemCharts are System insights' charts.
var sqlSystemCharts = []sqlChart{
	{label: "CPU utilization", metric: monitoring.SQLCPUUtilization, unit: "percent", gauge: true, scale: 100, max: 100},
	{label: "Memory utilization", metric: monitoring.SQLMemoryUtil, unit: "percent", gauge: true, scale: 100, max: 100},
	{label: "Disk utilization", metric: monitoring.SQLDiskUtil, unit: "percent", gauge: true, scale: 100, max: 100},
	{label: "Connections", metric: monitoring.SQLPGBackends, unit: "connections", gauge: true, scale: 1, engine: "POSTGRES"},
	{label: "Connections", metric: monitoring.SQLConnections, unit: "connections", gauge: true, scale: 1, engine: "MYSQL"},
	{label: "Transactions", metric: monitoring.SQLPGTransactions, unit: "transactions per minute", scale: 1, engine: "POSTGRES"},
	{label: "Queries", metric: monitoring.SQLMySQLQueries, unit: "queries per minute", scale: 1, engine: "MYSQL"},
	{label: "Ingress bytes", metric: monitoring.SQLReceivedBytes, unit: "bytes per minute", scale: 1},
	{label: "Egress bytes", metric: monitoring.SQLSentBytes, unit: "bytes per minute", scale: 1},
}

// sqlChartWindow is the charts' span, in one-minute points.
const sqlChartWindow = 60

func sqlEngineOfInstance(in *sqladminapi.DatabaseInstance) string {
	return sqlEngineOf(in.DatabaseVersion)
}

// sqlSeries reads one metric of the instance over [start, now].
func sqlSeries(ctx context.Context, a monitoringAdmin, project, instance, metric string, start time.Time) ([]*monitoringpb.TimeSeries, error) {
	return sqlChartRead(ctx, a, project, instance, sqlChart{metric: metric}, start)
}

// sqlChartRead reads chart c's series of the instance over [start, now].
func sqlChartRead(ctx context.Context, a monitoringAdmin, project, instance string, c sqlChart, start time.Time) ([]*monitoringpb.TimeSeries, error) {
	srv, err := a.server()
	if err != nil {
		return nil, err
	}
	filter := fmt.Sprintf(`metric.type = %q AND resource.type = "cloudsql_database" AND resource.labels.database_id = %q`,
		c.metric, project+":"+instance)
	if c.perDB {
		filter = fmt.Sprintf(`metric.type = %q AND resource.type = "cloudsql_instance_database" AND resource.labels.resource_id = %q`,
			c.metric, project+":"+instance)
	}
	if c.status != "" {
		filter += fmt.Sprintf(` AND metric.labels.status = %q`, c.status)
	}
	resp, err := srv.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: filter,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(time.Now().UTC())},
		View:     monitoringpb.ListTimeSeriesRequest_FULL, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	return resp.GetTimeSeries(), nil
}

func sqlPointValue(v *monitoringpb.TypedValue) float64 {
	switch x := v.GetValue().(type) {
	case *monitoringpb.TypedValue_Int64Value:
		return float64(x.Int64Value)
	case *monitoringpb.TypedValue_DoubleValue:
		return x.DoubleValue
	}
	return 0
}

// sqlChartSeries is one chart over the last hour, a point a minute: a
// gauge's minute its last reading summed over the series (every
// database's connections), a delta's the sum of its points.
func sqlChartSeries(c sqlChart, series []*monitoringpb.TimeSeries, end time.Time) console.ChartSeries {
	start := end.Add(-sqlChartWindow * time.Minute)
	sums := make([]float64, sqlChartWindow)
	seen := make([]bool, sqlChartWindow)
	count := make([]int, sqlChartWindow)
	for _, ts := range series {
		last := map[int]time.Time{}
		val := map[int]float64{}
		for _, pt := range ts.GetPoints() {
			at := pt.GetInterval().GetEndTime().AsTime()
			i := int(at.Sub(start) / time.Minute)
			if i < 0 || i >= sqlChartWindow {
				continue
			}
			seen[i] = true
			if c.gauge {
				if last[i].IsZero() || at.After(last[i]) {
					last[i], val[i] = at, sqlPointValue(pt.GetValue())
				}
			} else {
				sums[i] += sqlPointValue(pt.GetValue())
			}
		}
		for i, v := range val {
			sums[i] += v
			count[i]++
		}
	}
	if c.mean {
		for i := range sums {
			if count[i] > 0 {
				sums[i] /= float64(count[i])
			}
		}
	}
	out := console.ChartSeries{Label: c.label, Unit: c.unit, Max: c.max}
	for i := range sums {
		pt := console.ChartPoint{At: start.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)}
		if seen[i] {
			v := sums[i] * c.scale
			pt.Value = &v
		}
		out.Points = append(out.Points, pt)
	}
	return out
}

// sqlCharts are the charts of in that apply to its engine.
func sqlCharts(ctx context.Context, a monitoringAdmin, project string, in *sqladminapi.DatabaseInstance, charts []sqlChart) ([]console.ChartSeries, string) {
	if _, err := a.server(); err != nil {
		return nil, "Cloud Monitoring is not served by this instance, so the instance's metrics are not recorded. Start the instance with monitoring to chart them."
	}
	engine := sqlEngineOfInstance(in)
	pooling := in.Settings != nil && in.Settings.ConnectionPoolConfig != nil && in.Settings.ConnectionPoolConfig.ConnectionPoolingEnabled
	now := time.Now().UTC()
	end := now.Truncate(time.Minute).Add(time.Minute)
	var out []console.ChartSeries
	for _, c := range charts {
		if c.engine != "" && c.engine != engine || c.pooling && !pooling {
			continue
		}
		ts, err := sqlChartRead(ctx, a, project, in.Name, c, end.Add(-sqlChartWindow*time.Minute))
		if err != nil {
			out = append(out, console.ChartSeries{Label: c.label, Unit: c.unit, Unavailable: err.Error()})
			continue
		}
		out = append(out, sqlChartSeries(c, ts, end))
	}
	return out, ""
}

// sqlCPUPercentiles is System insights' CPU utilization - P99 | P50 over
// the last day, from cpu/utilization's readings.
func sqlCPUPercentiles(ctx context.Context, a monitoringAdmin, project, instance string) (string, bool) {
	ts, err := sqlSeries(ctx, a, project, instance, monitoring.SQLCPUUtilization, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		return "", false
	}
	var vs []float64
	for _, s := range ts {
		for _, p := range s.GetPoints() {
			vs = append(vs, sqlPointValue(p.GetValue())*100)
		}
	}
	if len(vs) == 0 {
		return "", false
	}
	sort.Float64s(vs)
	pct := func(q float64) float64 { return vs[int(math.Ceil(q*float64(len(vs))))-1] }
	return fmt.Sprintf("%.2f%% | %.2f%%", pct(0.99), pct(0.5)), true
}

// sqlMetricsNote says where the charts come from.
const sqlMetricsNote = "Cloud Monitoring's cloudsql.googleapis.com metrics for this instance over the last hour, " +
	"read from its running server every 60 seconds; Metrics Explorer reads the same time series."

// sqlChartsOnOverview puts the chart on the Overview.
func (p cloudSQLInstancesProvider) sqlChartsOnOverview(ctx context.Context, project string, in *sqladminapi.DatabaseInstance, d console.Detail) console.Detail {
	if strings.EqualFold(in.InstanceType, "ON_PREMISES_INSTANCE") {
		return d
	}
	for i, s := range d.Sections {
		if s.ID != "overview" {
			continue
		}
		series, why := sqlCharts(ctx, p.metrics, project, in, sqlOverviewCharts)
		if why != "" {
			series = []console.ChartSeries{{Label: "CPU utilization", Unit: "percent", Unavailable: why}}
		}
		d.Sections[i].Series = series
		if d.Sections[i].Note == "" {
			d.Sections[i].Note = sqlMetricsNote
		}
	}
	return d
}

// sqlLoadSeries are Query insights' Database load chart (#2141), Google's
// "Database load - all top queries": over the last hour, a point a
// minute, the time the server's statements spent each second, from the
// postgresql/insights/aggregate metrics (CPU and CPU wait the execution
// time less its IO and lock time, IO wait, Lock wait), with the tier's
// CPU capacity. The series' names are Google's
// (https://docs.cloud.google.com/sql/docs/postgres/using-query-insights,
// read 2026-11-22); the axis' unit is UNVERIFIED (re-read 2026-11-22:
// the same page names none).
func sqlLoadSeries(ctx context.Context, a monitoringAdmin, project string, in *sqladminapi.DatabaseInstance) []console.ChartSeries {
	if _, err := a.server(); err != nil {
		return []console.ChartSeries{{Label: "Database load", Unavailable: "Cloud Monitoring is not served by this instance, so the server's load is not recorded. Start the instance with monitoring to chart it."}}
	}
	end := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-sqlChartWindow * time.Minute)
	rate := func(metric string) ([]float64, []bool, error) {
		srv, err := a.server()
		if err != nil {
			return nil, nil, err
		}
		filter := fmt.Sprintf(`metric.type = %q AND resource.type = "cloudsql_instance_database" AND resource.labels.resource_id = %q`,
			metric, project+":"+in.Name)
		resp, err := srv.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: filter,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start.Add(-5 * time.Minute)), EndTime: timestamppb.New(time.Now().UTC())},
			View:     monitoringpb.ListTimeSeriesRequest_FULL, PageSize: 1000})
		if err != nil {
			return nil, nil, err
		}
		sums, seen := make([]float64, sqlChartWindow), make([]bool, sqlChartWindow)
		for _, ts := range resp.GetTimeSeries() {
			pts := append([]*monitoringpb.Point{}, ts.GetPoints()...)
			sort.Slice(pts, func(i, j int) bool {
				return pts[i].GetInterval().GetEndTime().AsTime().Before(pts[j].GetInterval().GetEndTime().AsTime())
			})
			for k := 1; k < len(pts); k++ {
				prev, cur := pts[k-1], pts[k]
				at := cur.GetInterval().GetEndTime().AsTime()
				i := int(at.Sub(start) / time.Minute)
				if i < 0 || i >= sqlChartWindow || !cur.GetInterval().GetStartTime().AsTime().Equal(prev.GetInterval().GetStartTime().AsTime()) {
					continue
				}
				d := sqlPointValue(cur.GetValue()) - sqlPointValue(prev.GetValue())
				if d < 0 {
					continue
				}
				sums[i] += d / 1e6 / 60 // microseconds over the minute: seconds a second
				seen[i] = true
			}
		}
		return sums, seen, nil
	}
	exec, seenE, err := rate(monitoring.SQLPGInsightsExecTime)
	if err != nil {
		return []console.ChartSeries{{Label: "Database load", Unavailable: err.Error()}}
	}
	io, _, _ := rate(monitoring.SQLPGInsightsIOTime)
	lock, _, _ := rate(monitoring.SQLPGInsightsLockTime)
	cores := 0.0
	if in.Settings != nil {
		cores, _ = sqladmin.TierShape(in.Settings.Tier)
	}
	mk := func(label string, v func(i int) float64) console.ChartSeries {
		out := console.ChartSeries{Label: label}
		for i := 0; i < sqlChartWindow; i++ {
			pt := console.ChartPoint{At: start.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)}
			if seenE[i] {
				x := v(i)
				pt.Value = &x
			}
			out.Points = append(out.Points, pt)
		}
		return out
	}
	val := func(s []float64, i int) float64 {
		if s == nil {
			return 0
		}
		return s[i]
	}
	return []console.ChartSeries{
		mk("CPU capacity", func(int) float64 { return cores }),
		mk("CPU and CPU wait", func(i int) float64 { return math.Max(0, exec[i]-val(io, i)-val(lock, i)) }),
		mk("IO wait", func(i int) float64 { return val(io, i) }),
		mk("Lock wait", func(i int) float64 { return val(lock, i) }),
	}
}
