package main

// AlloyDB's 2026-11-25 console parity round 2 (#2251), as Google's console
// has each:
//
//   - System insights, a cluster's tab: the Cluster (all instances) view
//     and each instance's, each with its summary cards (Storage, Active
//     node count, CPU utilization, Peak connections, Transaction ID
//     utilization, Replication lag), its metrics charts and its events
//     timeline (docs.cloud.google.com/alloydb/docs/monitor-instance and
//     docs.cloud.google.com/alloydb/docs/reference/system-insights-metrics,
//     read 2026-11-25), every figure read with ListTimeSeries from the
//     instance's Cloud Monitoring, which records the alloydb.googleapis.com
//     metrics from each running server (monitoringalloydb.go);
//   - the Overview's metrics chart, its metric chosen from a list (the
//     same page's "View cluster metrics on the AlloyDB cluster Overview
//     page");
//   - Query insights, a cluster's tab: an instance's Database load - all
//     top queries chart and its Queries table (Query string, Database,
//     Load by total time, Avg execution time (ms), Times called, Avg rows
//     fetched), filtered by Database and User, and Edit with Query lengths,
//     Query plan sampling, Maximum sampling rate, Store client IP addresses
//     and Store application tags, ending in Update instance
//     (docs.cloud.google.com/alloydb/docs/using-query-insights);
//   - the Overview's Maintenance, with Edit: Maintenance window (Any
//     window, or a day and an hour, UTC), ending in Update, and the
//     Clusters list's Maintenance column
//     (docs.cloud.google.com/alloydb/docs/maintenance-windows);
//   - an instance row's Edit, whose Advanced configuration options have
//     Google's database flags: Add flag, a flag chosen from the New
//     database flag list with its value, and each flag's delete, ending in
//     Update instance (docs.cloud.google.com/alloydb/docs/instance-configure-database-flags);
//   - Create backup's Description and labels, and a backup's Edit labels
//     (docs.cloud.google.com/alloydb/docs/backup/create-on-demand).
//
// Left out, as no reading backs them here (docs/gaps.md): the latency
// charts, wait events, Log errors, the query plans and tags of Query
// insights, Gemini, and Database Migration Service's entry points (that
// API is not served).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// alloyDBChart is one chart: its metric, the resource it is read on, the
// labels it is narrowed to, and how its minute is read.
type alloyDBChart struct {
	label, metric, unit string
	resource            string
	labels              map[string]string
	gauge, mean         bool
	scale, max          float64
}

func (c alloyDBChart) sql() sqlChart {
	return sqlChart{label: c.label, metric: c.metric, unit: c.unit, gauge: c.gauge, scale: c.scale, max: c.max, mean: c.mean}
}

var (
	alloyDBChartCPU = alloyDBChart{label: "Max CPU utilization", metric: monitoring.AlloyDBCPUMax, unit: "percent", resource: monitoring.AlloyDBInstanceResource, gauge: true, scale: 1, max: 100}
	alloyDBChartMem = alloyDBChart{label: "Available memory", metric: monitoring.AlloyDBMemAvailable, unit: "bytes", resource: monitoring.AlloyDBInstanceResource, gauge: true, scale: 1}
	alloyDBChartCon = alloyDBChart{label: "Number of connections", metric: monitoring.AlloyDBBackendsByState, unit: "connections", resource: monitoring.AlloyDBInstanceResource, gauge: true, scale: 1}
	alloyDBChartTx  = alloyDBChart{label: "Transaction count", metric: monitoring.AlloyDBTransactions, unit: "transactions per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1}
)

// alloyDBInstanceCharts are an instance's System insights charts, in the
// metrics reference's order.
var alloyDBInstanceCharts = []alloyDBChart{
	{label: "Database load", metric: monitoring.AlloyDBExecTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource, scale: 1},
	alloyDBChartCPU, alloyDBChartMem,
	{label: "Queries IO wait time", metric: monitoring.AlloyDBIOTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource, scale: 1},
	alloyDBChartCon,
	{label: "Number of tuples written", metric: monitoring.AlloyDBTuplesWritten, unit: "rows per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Oldest transaction age", metric: monitoring.AlloyDBOldestXact, unit: "transactions", resource: monitoring.AlloyDBInstanceResource, labels: map[string]string{"type": "running"}, gauge: true, scale: 1},
	{label: "Transaction ID utilization", metric: monitoring.AlloyDBXIDUtilization, unit: "percent", resource: monitoring.AlloyDBInstanceResource, gauge: true, scale: 1},
	{label: "Temporary files used for writing data", metric: monitoring.AlloyDBTempFiles, unit: "files per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Number of new connections added to the instance", metric: monitoring.AlloyDBNewConnections, unit: "connections per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Statements executed count", metric: monitoring.AlloyDBStatements, unit: "statements per minute", resource: monitoring.AlloyDBDatabaseResource, scale: 1},
	alloyDBChartTx,
	{label: "Blocks hit", metric: monitoring.AlloyDBBlksHit, unit: "blocks per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Blocks miss", metric: monitoring.AlloyDBBlksRead, unit: "blocks per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Deadlock count", metric: monitoring.AlloyDBDeadlocks, unit: "deadlocks per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Limit on connections per node", metric: monitoring.AlloyDBConnLimit, unit: "connections", resource: monitoring.AlloyDBInstanceResource, gauge: true, scale: 1},
}

// alloyDBClusterCharts are the Cluster (all instances) view's charts.
var alloyDBClusterCharts = []alloyDBChart{
	{label: "Database load", metric: monitoring.AlloyDBExecTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource, scale: 1},
	{label: "Mean CPU utilization", metric: monitoring.AlloyDBCPUAvg, unit: "percent", resource: monitoring.AlloyDBInstanceResource, gauge: true, mean: true, scale: 1, max: 100},
	{label: "Available memory", metric: monitoring.AlloyDBMemAvailable, unit: "bytes", resource: monitoring.AlloyDBInstanceResource, gauge: true, mean: true, scale: 1},
	{label: "Cluster storage", metric: monitoring.AlloyDBStorage, unit: "bytes", resource: monitoring.AlloyDBClusterResource, gauge: true, scale: 1},
	alloyDBChartCon,
	{label: "Number of tuples written", metric: monitoring.AlloyDBTuplesWritten, unit: "rows per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	alloyDBChartTx,
	{label: "Read pool replication lag", metric: monitoring.AlloyDBReplicationLag, unit: "milliseconds", resource: monitoring.AlloyDBInstanceResource, gauge: true, mean: true, scale: 1},
	{label: "Active nodes", metric: monitoring.AlloyDBNodes, unit: "nodes", resource: monitoring.AlloyDBInstanceResource, labels: map[string]string{"status": "up"}, gauge: true, scale: 1},
}

// alloyDBOverviewCharts are the Overview's chart choices.
var alloyDBOverviewCharts = []alloyDBChart{
	alloyDBChartCPU, alloyDBChartMem, alloyDBChartCon, alloyDBChartTx,
	{label: "Cluster storage", metric: monitoring.AlloyDBStorage, unit: "bytes", resource: monitoring.AlloyDBClusterResource, gauge: true, scale: 1},
}

// alloyDBMetricsNote says where the figures come from.
const alloyDBMetricsNote = "Cloud Monitoring's alloydb.googleapis.com metrics for this cluster, read from its running servers every 60 seconds " +
	"(the charts over the last hour, the summary cards over the last day); Metrics Explorer reads the same time series."

const alloyDBNoMonitoring = "Cloud Monitoring is not served by this instance, so the cluster's metrics are not recorded. Start the instance with monitoring to chart them."

// alloyDBScope narrows a read to a cluster, and to one of its instances
// when instance is not "".
type alloyDBScope struct {
	project, location, cluster, instance string
}

func alloyDBScopeOf(c *alloydbpb.Cluster) alloyDBScope {
	parts := strings.Split(c.GetName(), "/")
	return alloyDBScope{project: parts[1], location: parts[3], cluster: parts[5]}
}

// alloyDBRead reads metric on resource within sc over [start, now].
func alloyDBRead(ctx context.Context, a monitoringAdmin, sc alloyDBScope, metric, resource string, labels map[string]string, start time.Time) ([]*monitoringpb.TimeSeries, error) {
	srv, err := a.server()
	if err != nil {
		return nil, err
	}
	filter := fmt.Sprintf(`metric.type = %q AND resource.type = %q AND resource.labels.cluster_id = %q AND resource.labels.location = %q`,
		metric, resource, sc.cluster, sc.location)
	if sc.instance != "" && resource != monitoring.AlloyDBClusterResource {
		filter += fmt.Sprintf(` AND resource.labels.instance_id = %q`, sc.instance)
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		filter += fmt.Sprintf(` AND metric.labels.%s = %q`, k, labels[k])
	}
	resp, err := srv.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + sc.project, Filter: filter,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(time.Now().UTC())},
		View:     monitoringpb.ListTimeSeriesRequest_FULL, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	return resp.GetTimeSeries(), nil
}

// alloyDBCharts draws charts within sc over the last hour.
func alloyDBCharts(ctx context.Context, a monitoringAdmin, sc alloyDBScope, charts []alloyDBChart) []console.ChartSeries {
	if _, err := a.server(); err != nil {
		var out []console.ChartSeries
		for _, c := range charts {
			out = append(out, console.ChartSeries{Label: c.label, Unit: c.unit, Max: c.max, Unavailable: alloyDBNoMonitoring})
		}
		return out
	}
	end := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	var out []console.ChartSeries
	for _, c := range charts {
		ts, err := alloyDBRead(ctx, a, sc, c.metric, c.resource, c.labels, end.Add(-sqlChartWindow*time.Minute))
		if err != nil {
			out = append(out, console.ChartSeries{Label: c.label, Unit: c.unit, Unavailable: err.Error()})
			continue
		}
		out = append(out, sqlChartSeries(c.sql(), ts, end))
	}
	return out
}

// alloyDBMinutes is metric's readings within sc over the last day, a
// value a minute: a gauge's readings summed over the series (or their
// mean), a delta's points summed.
func alloyDBMinutes(ctx context.Context, a monitoringAdmin, sc alloyDBScope, c alloyDBChart) ([]float64, error) {
	ts, err := alloyDBRead(ctx, a, sc, c.metric, c.resource, c.labels, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	by := map[int64][]float64{}
	for _, s := range ts {
		for _, p := range s.GetPoints() {
			m := p.GetInterval().GetEndTime().AsTime().Unix() / 60
			by[m] = append(by[m], sqlPointValue(p.GetValue()))
		}
	}
	keys := make([]int64, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]float64, 0, len(keys))
	for _, k := range keys {
		sum := 0.0
		for _, v := range by[k] {
			sum += v
		}
		if c.mean {
			sum /= float64(len(by[k]))
		}
		out = append(out, sum)
	}
	return out, nil
}

func pctOf(vs []float64, q float64) float64 {
	s := append([]float64{}, vs...)
	sort.Float64s(s)
	return s[int(math.Ceil(q*float64(len(s))))-1]
}

const alloyDBNoReadings = "No readings yet: the first is a minute after the server starts serving."

// alloyDBView is one System insights view: the cluster, or one instance.
type alloyDBView struct {
	id, label string
	scope     alloyDBScope
	kind      string // "cluster", "primary" or "read pool"
	instance  *alloydbpb.Instance
}

// alloyDBSummary is a view's summary cards, as the monitor-instance
// page's table puts them on each dashboard.
func alloyDBSummary(ctx context.Context, a monitoringAdmin, v alloyDBView, primary string) []console.Property {
	card := func(label string, f func() (string, error)) console.Property {
		val, err := f()
		switch {
		case err != nil:
			val = "Not available: " + err.Error()
		case val == "":
			val = alloyDBNoReadings
		}
		return console.Property{Label: label, Value: val}
	}
	last := func(c alloyDBChart) (float64, bool, error) {
		vs, err := alloyDBMinutes(ctx, a, v.scope, c)
		if err != nil || len(vs) == 0 {
			return 0, false, err
		}
		return vs[len(vs)-1], true, nil
	}
	storage := card("Storage", func() (string, error) {
		n, ok, err := last(alloyDBChart{metric: monitoring.AlloyDBStorage, resource: monitoring.AlloyDBClusterResource, gauge: true})
		if !ok {
			return "", err
		}
		return humanBytes(int64(n)), nil
	})
	nodes := card("Active node count", func() (string, error) {
		n, ok, err := last(alloyDBChart{metric: monitoring.AlloyDBNodes, resource: monitoring.AlloyDBInstanceResource, labels: map[string]string{"status": "up"}, gauge: true})
		if !ok {
			return "", err
		}
		return strconv.Itoa(int(n)), nil
	})
	cpu := card("CPU utilization - P99 | P50", func() (string, error) {
		vs, err := alloyDBMinutes(ctx, a, v.scope, alloyDBChartCPU)
		if err != nil || len(vs) == 0 {
			return "", err
		}
		return fmt.Sprintf("%.2f%% | %.2f%%", pctOf(vs, 0.99), pctOf(vs, 0.5)), nil
	})
	peak := card("Peak connections", func() (string, error) {
		vs, err := alloyDBMinutes(ctx, a, v.scope, alloyDBChart{metric: monitoring.AlloyDBConnections, resource: monitoring.AlloyDBInstanceResource, gauge: true})
		if err != nil || len(vs) == 0 {
			return "", err
		}
		limit, ok, err := last(alloyDBChart{metric: monitoring.AlloyDBConnLimit, resource: monitoring.AlloyDBInstanceResource, gauge: true})
		if !ok {
			return "", err
		}
		return fmt.Sprintf("%d / %d", int64(pctOf(vs, 1)), int64(limit)), nil
	})
	xid := card("Transaction ID utilization", func() (string, error) {
		n, ok, err := last(alloyDBChart{metric: monitoring.AlloyDBXIDUtilization, resource: monitoring.AlloyDBInstanceResource, gauge: true})
		if !ok {
			return "", err
		}
		return fmt.Sprintf("%.4f%%", n), nil
	})
	switch v.kind {
	case "cluster":
		return []console.Property{storage, nodes, peak}
	case "primary":
		return []console.Property{cpu, peak, xid}
	}
	lag := card("Replication lag", func() (string, error) {
		sc := v.scope
		sc.instance = primary
		vs, err := alloyDBMinutes(ctx, a, sc, alloyDBChart{metric: monitoring.AlloyDBReplicationLag, resource: monitoring.AlloyDBInstanceResource,
			labels: map[string]string{"replica_instance_id": v.scope.instance}, gauge: true})
		if err != nil || len(vs) == 0 {
			return "", err
		}
		sum := 0.0
		for _, x := range vs {
			sum += x
		}
		return fmt.Sprintf("%.0f ms", sum/float64(len(vs))), nil
	})
	return []console.Property{nodes, cpu, peak, xid, lag}
}

// alloyDBEvents is the events timeline: the cluster's operations in the
// last day, newest first, by Google's operation types, each with its
// resource, status and times.
func alloyDBEvents(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, instance string) console.PropertyGroup {
	g := console.PropertyGroup{Heading: "Events timeline"}
	loc := strings.Join(strings.Split(c.GetName(), "/")[:4], "/")
	resp, err := s.Operations().ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: loc, PageSize: 1000})
	if err != nil {
		g.Properties = []console.Property{{Label: "Events", Value: "Not available: " + consoleErr(err)}}
		return g
	}
	since := time.Now().Add(-24 * time.Hour)
	type ev struct {
		at   time.Time
		prop console.Property
	}
	var evs []ev
	for _, op := range resp.GetOperations() {
		md := &alloydbpb.OperationMetadata{}
		if op.GetMetadata().UnmarshalTo(md) != nil || md.GetCreateTime().AsTime().Before(since) {
			continue
		}
		target := md.GetTarget()
		backup := strings.Contains(target, "/backups/")
		switch {
		case instance != "" && target != c.GetName()+"/instances/"+instance:
			continue
		case backup && !alloyDBBackupOf(ctx, s, target, c.GetName()):
			continue
		case !backup && target != c.GetName() && !strings.HasPrefix(target, c.GetName()+"/"):
			continue
		}
		_, typ := alloyDBEventName(md.GetVerb(), target)
		if typ == "" {
			continue
		}
		status := "Running"
		if op.GetDone() {
			status = "Succeeded"
			if op.GetError() != nil {
				status = "Failed"
			}
		}
		val := typ + " on " + lastSegment(target) + ": " + status
		if md.GetEndTime() != nil {
			val += ", ended " + kmsTime(md.GetEndTime())
		}
		evs = append(evs, ev{md.GetCreateTime().AsTime(), console.Property{Label: kmsTime(md.GetCreateTime()), Value: val}})
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.After(evs[j].at) })
	for _, e := range evs {
		g.Properties = append(g.Properties, e.prop)
	}
	if len(g.Properties) == 0 {
		g.Properties = []console.Property{{Label: "Events", Value: "None in the last day"}}
	}
	return g
}

// alloyDBBackupOf is whether backup name is of cluster.
func alloyDBBackupOf(ctx context.Context, s *alloydb.Server, name, cluster string) bool {
	b, err := s.GetBackup(ctx, &alloydbpb.GetBackupRequest{Name: name})
	return err == nil && b.GetClusterName() == cluster
}

// alloyDBEventName is Google's event name and operation type of an
// operation's verb on target ("" for one Google's timeline has none of).
func alloyDBEventName(verb, target string) (string, string) {
	kind := "Cluster"
	switch {
	case strings.Contains(target, "/instances/"):
		kind = "Instance"
	case strings.Contains(target, "/backups/"):
		kind = "Backup"
	}
	switch verb {
	case "create", "update", "restart", "failover", "promote":
	default:
		return "", ""
	}
	if kind == "Backup" && verb != "create" && verb != "update" || kind == "Cluster" && (verb == "restart" || verb == "failover") ||
		kind == "Instance" && verb == "promote" {
		return "", ""
	}
	return kind + " " + verb, strings.ToUpper(kind + "_" + verb)
}

// alloyDBSystemInsights is the System insights tab: a view for the
// cluster and one for each instance.
func alloyDBSystemInsights(ctx context.Context, s *alloydb.Server, a monitoringAdmin, c *alloydbpb.Cluster, ins []*alloydbpb.Instance) console.Section {
	sc := alloyDBScopeOf(c)
	views := []alloyDBView{{id: "si-cluster", label: "Cluster (all instances)", scope: sc, kind: "cluster"}}
	primary := ""
	for _, in := range ins {
		v := alloyDBView{id: "si-" + lastSegment(in.GetName()), scope: sc, instance: in}
		v.scope.instance = lastSegment(in.GetName())
		if in.GetInstanceType() == alloydbpb.Instance_READ_POOL {
			v.kind, v.label = "read pool", "Read pool instance: "+lastSegment(in.GetName())
		} else {
			v.kind, v.label = "primary", alloyDBInstanceType(in.GetInstanceType())+" instance: "+lastSegment(in.GetName())
			primary = lastSegment(in.GetName())
		}
		views = append(views, v)
	}
	sec := console.Section{ID: "system-insights", Label: "System insights", Note: alloyDBMetricsNote}
	if _, err := a.server(); err != nil {
		sec.Kind = console.KindProperties
		sec.Unavailable = alloyDBNoMonitoring
		return sec
	}
	for _, v := range views {
		charts := alloyDBInstanceCharts
		if v.kind == "cluster" {
			charts = alloyDBClusterCharts
		}
		series := alloyDBCharts(ctx, a, v.scope, charts)
		if v.kind == "read pool" {
			lag := alloyDBChart{label: "Read pool replication lag", metric: monitoring.AlloyDBReplicationLag, unit: "milliseconds",
				resource: monitoring.AlloyDBInstanceResource, labels: map[string]string{"replica_instance_id": v.scope.instance}, gauge: true, scale: 1}
			psc := v.scope
			psc.instance = primary
			series = append(series, alloyDBCharts(ctx, a, psc, []alloyDBChart{lag})...)
		}
		// The summary cards, the charts (one at a time, chosen from the
		// Chart list) and the events timeline.
		tab := console.Section{ID: v.id, Label: v.label, Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Summary", Properties: alloyDBSummary(ctx, a, v, primary)},
				alloyDBEvents(ctx, s, c, v.scope.instance)},
			Series: series}
		sec.Tabs = append(sec.Tabs, tab)
	}
	return sec
}

// alloyDBOverviewSeries are the Overview's chart choices.
func alloyDBOverviewSeries(ctx context.Context, a monitoringAdmin, c *alloydbpb.Cluster, primary string) []console.ChartSeries {
	sc := alloyDBScopeOf(c)
	sc.instance = primary
	return alloyDBCharts(ctx, a, sc, alloyDBOverviewCharts)
}

// ---- Query insights ----

// alloyDBQueryInsights is the Query insights tab: one tab an instance,
// the primary first, as Google's instance selector lists them.
func alloyDBQueryInsights(ctx context.Context, s *alloydb.Server, a monitoringAdmin, c *alloydbpb.Cluster, ins []*alloydbpb.Instance) console.Section {
	sec := console.Section{ID: "query-insights", Label: "Query insights",
		Note: "Each normalized query's totals since the server's statistics were reset, from pg_stat_statements on the instance's server; " +
			"Google's page totals them over a chosen time range. The Database load chart is Cloud Monitoring's " +
			"database/postgresql/insights/aggregate metrics over the last hour."}
	if len(ins) == 0 {
		sec.Listing = console.Listing{Prompt: "Query insights shows an instance's queries: create the cluster's primary instance first."}
		return sec
	}
	for _, in := range ins {
		sec.Tabs = append(sec.Tabs, alloyDBQueryInsightsTab(ctx, s, a, c, in))
	}
	return sec
}

func alloyDBQueryInsightsTab(ctx context.Context, s *alloydb.Server, a monitoringAdmin, c *alloydbpb.Cluster, in *alloydbpb.Instance) console.Section {
	id := lastSegment(in.GetName())
	tab := console.Section{ID: "qi-" + id, Label: alloyDBInstanceType(in.GetInstanceType()) + ": " + id,
		Actions: []console.Action{alloyDBEditInsightsAction(in)}}
	l := console.Listing{TableTitle: "Queries", NameColumn: "Query string", Noun: "queries",
		Columns:         []string{"Database", "User", "Load by total time", "Avg execution time (ms)", "Times called", "Avg rows fetched"},
		OptionalColumns: []string{"Load by CPU", "Load by IO wait"}}
	if in.GetState() != alloydbpb.Instance_READY {
		l.Unavailable = "The instance is " + alloyDBState(in.GetState()) + ": its server is not serving."
		tab.Listing = l
		return tab
	}
	stats, err := s.QueryInsights(ctx, in.GetName())
	if err != nil {
		l.Unavailable = "cannot read the server's statistics: " + consoleErr(err)
		tab.Listing = l
		return tab
	}
	var total, cpu, io float64
	dbs, users := map[string]bool{}, map[string]bool{}
	for _, q := range stats {
		total, cpu, io = total+q.TotalMs, cpu+q.CPUMs, io+q.IOMs
	}
	share := func(v, of float64) string {
		if of <= 0 {
			return "0%"
		}
		return fmt.Sprintf("%.1f%%", v*100/of)
	}
	for _, q := range stats {
		rows := "0"
		if q.Calls > 0 {
			rows = strconv.FormatFloat(float64(q.Rows)/float64(q.Calls), 'f', 2, 64)
		}
		dbs[q.Database], users[q.User] = true, true
		l.Items = append(l.Items, console.Resource{Name: q.Query, Fields: map[string]string{
			"Database": q.Database, "User": q.User, "Load by total time": share(q.TotalMs, total), "Load by CPU": share(q.CPUMs, cpu),
			"Load by IO wait": share(q.IOMs, io), "Avg execution time (ms)": fmt.Sprintf("%.3f", q.MeanMs),
			"Times called": strconv.FormatInt(q.Calls, 10), "Avg rows fetched": rows}})
	}
	l.Total = len(l.Items)
	// Google's Database and User filters.
	l.Menus = []console.ListMenu{{Label: "Database", Key: "Database", Options: sortedSet(dbs)}, {Label: "User", Key: "User", Options: sortedSet(users)}}
	tab.Listing = l
	tab.SeriesTitle = "Database load - all top queries"
	tab.Series = alloyDBLoadSeries(ctx, a, c, in)
	return tab
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// alloyDBLoadSeries is Google's Database load - all top queries: over the
// last hour, a point a minute, the seconds the server's queries spent
// each second, from the insights/aggregate DELTAs (CPU and CPU wait the
// execution time less its IO and lock time), with the machine's CPU
// capacity.
func alloyDBLoadSeries(ctx context.Context, a monitoringAdmin, c *alloydbpb.Cluster, in *alloydbpb.Instance) []console.ChartSeries {
	if _, err := a.server(); err != nil {
		return []console.ChartSeries{{Label: "Database load", Unavailable: alloyDBNoMonitoring}}
	}
	sc := alloyDBScopeOf(c)
	sc.instance = lastSegment(in.GetName())
	end := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-sqlChartWindow * time.Minute)
	rate := func(metric string) ([]float64, []bool, error) {
		ts, err := alloyDBRead(ctx, a, sc, metric, monitoring.AlloyDBDatabaseResource, nil, start)
		if err != nil {
			return nil, nil, err
		}
		sums, seen := make([]float64, sqlChartWindow), make([]bool, sqlChartWindow)
		for _, s := range ts {
			for _, p := range s.GetPoints() {
				i := int(p.GetInterval().GetEndTime().AsTime().Sub(start) / time.Minute)
				if i < 0 || i >= sqlChartWindow {
					continue
				}
				span := p.GetInterval().GetEndTime().AsTime().Sub(p.GetInterval().GetStartTime().AsTime()).Seconds()
				if span <= 0 {
					span = 60
				}
				sums[i] += sqlPointValue(p.GetValue()) / 1e6 / span
				seen[i] = true
			}
		}
		return sums, seen, nil
	}
	exec, seen, err := rate(monitoring.AlloyDBExecTime)
	if err != nil {
		return []console.ChartSeries{{Label: "Database load", Unavailable: err.Error()}}
	}
	io, _, _ := rate(monitoring.AlloyDBIOTime)
	lock, _, _ := rate(monitoring.AlloyDBLockTime)
	cores := float64(in.GetMachineConfig().GetCpuCount())
	val := func(s []float64, i int) float64 {
		if s == nil {
			return 0
		}
		return s[i]
	}
	mk := func(label string, v func(i int) float64) console.ChartSeries {
		out := console.ChartSeries{Label: label}
		for i := 0; i < sqlChartWindow; i++ {
			pt := console.ChartPoint{At: start.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)}
			if seen[i] {
				x := v(i)
				pt.Value = &x
			}
			out.Points = append(out.Points, pt)
		}
		return out
	}
	return []console.ChartSeries{
		mk("CPU capacity", func(int) float64 { return cores }),
		mk("CPU and CPU wait", func(i int) float64 { return math.Max(0, exec[i]-val(io, i)-val(lock, i)) }),
		mk("IO wait", func(i int) float64 { return val(io, i) }),
		mk("Lock wait", func(i int) float64 { return val(lock, i) }),
	}
}

// alloyDBEditInsightsAction is Query insights' Edit for one instance.
func alloyDBEditInsightsAction(in *alloydbpb.Instance) console.Action {
	q := in.GetQueryInsightsConfig()
	if q == nil {
		// Query insights' documented defaults (query-insights-overview).
		q = &alloydbpb.Instance_QueryInsightsInstanceConfig{RecordApplicationTags: proto.Bool(true), RecordClientAddress: proto.Bool(true),
			QueryStringLength: 1024, QueryPlansPerMinute: proto.Uint32(5)}
	}
	length := q.GetQueryStringLength()
	if length == 0 {
		length = 1024
	}
	rate := uint32(5)
	if q.QueryPlansPerMinute != nil {
		rate = q.GetQueryPlansPerMinute()
	}
	bstr := func(b bool) string { return strconv.FormatBool(b) }
	id := lastSegment(in.GetName())
	return console.Action{ID: "edit-query-insights-" + id, Label: "Edit", Title: "Edit Query insights: " + id, Submit: "Update instance",
		Fields: []console.Field{
			{Name: "instance", Type: "hidden", Default: id},
			{Name: "queryStringLength", Label: "Query lengths", Type: "number", Required: true, Default: strconv.Itoa(int(length)),
				Pattern: `^(25[6-9]|2[6-9][0-9]|[3-9][0-9]{2}|[1-3][0-9]{3}|4[0-4][0-9]{2}|4500)$`,
				Help:    "256 to 4500 bytes of each query kept for analysis (1024 by default). Higher limits take more memory."},
			{Name: "planSampling", Label: "Query plan sampling", Type: "checkbox", Default: bstr(rate > 0),
				Help: "Samples the plans of up to the maximum sampling rate of queries a minute (queryPlansPerMinute). No query plans are sampled locally."},
			{Name: "queryPlansPerMinute", Label: "Maximum sampling rate", Type: "number", Default: strconv.Itoa(int(max(rate, 1))), ShowWhen: "planSampling",
				Pattern: `^([1-9]|1[0-9]|20)$`, Help: "1 to 20 queries a minute."},
			{Name: "recordClientAddress", Label: "Store client IP addresses", Type: "checkbox", Default: bstr(q.GetRecordClientAddress())},
			{Name: "recordApplicationTags", Label: "Store application tags", Type: "checkbox", Default: bstr(q.GetRecordApplicationTags())},
		}}
}

func (p alloyDBProvider) editQueryInsights(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, values map[string]string) error {
	name := c.GetName() + "/instances/" + strings.TrimSpace(values["instance"])
	n, err := strconv.Atoi(strings.TrimSpace(values["queryStringLength"]))
	if err != nil || n < 256 || n > 4500 {
		return fmt.Errorf("query lengths are 256 to 4500 bytes")
	}
	rate := uint32(0)
	if on(values["planSampling"]) {
		r, err := strconv.Atoi(strings.TrimSpace(values["queryPlansPerMinute"]))
		if err != nil || r < 1 || r > 20 {
			return fmt.Errorf("the maximum sampling rate is 1 to 20")
		}
		rate = uint32(r)
	}
	cfg := &alloydbpb.Instance_QueryInsightsInstanceConfig{QueryStringLength: uint32(n), QueryPlansPerMinute: proto.Uint32(rate),
		RecordClientAddress: proto.Bool(on(values["recordClientAddress"])), RecordApplicationTags: proto.Bool(on(values["recordApplicationTags"]))}
	op, err := s.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: &alloydbpb.Instance{Name: name, QueryInsightsConfig: cfg},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"query_insights_config"}}})
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 30*time.Second)
}

func on(v string) bool { return v == "true" || v == "on" }

// ---- Maintenance ----

var alloyDBDays = []string{"ANY", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

func alloyDBDayLabel(d string) string {
	if d == "ANY" {
		return "Any window"
	}
	return d[:1] + strings.ToLower(d[1:])
}

// alloyDBMaintenanceWindow is the cluster's window as Google words it.
func alloyDBMaintenanceWindow(c *alloydbpb.Cluster) string {
	ws := c.GetMaintenanceUpdatePolicy().GetMaintenanceWindows()
	if len(ws) == 0 {
		return "Any window"
	}
	w := ws[0]
	return fmt.Sprintf("%s, %02d:%02d UTC", alloyDBDayLabel(w.GetDay().String()), w.GetStartTime().GetHours(), w.GetStartTime().GetMinutes())
}

// alloyDBUpcomingMaintenance is the cluster's next maintenance: none is
// ever scheduled on this instance's servers.
func alloyDBUpcomingMaintenance(c *alloydbpb.Cluster) string {
	if t := c.GetMaintenanceSchedule().GetStartTime(); t != nil {
		return kmsTime(t)
	}
	return "None scheduled"
}

// alloyDBMaintenanceGroup is the Overview's Maintenance.
func alloyDBMaintenanceGroup(c *alloydbpb.Cluster) console.PropertyGroup {
	deny := "None"
	if ds := c.GetMaintenanceUpdatePolicy().GetDenyMaintenancePeriods(); len(ds) > 0 {
		d := ds[0]
		deny = fmt.Sprintf("%04d-%02d-%02d to %04d-%02d-%02d, %02d:%02d UTC", d.GetStartDate().GetYear(), d.GetStartDate().GetMonth(),
			d.GetStartDate().GetDay(), d.GetEndDate().GetYear(), d.GetEndDate().GetMonth(), d.GetEndDate().GetDay(),
			d.GetTime().GetHours(), d.GetTime().GetMinutes())
	}
	return console.PropertyGroup{Heading: "Maintenance", Properties: []console.Property{
		{Label: "Maintenance window", Value: alloyDBMaintenanceWindow(c), Action: "edit-maintenance", ActionText: "Edit"},
		{Label: "Upcoming maintenance", Value: alloyDBUpcomingMaintenance(c)},
		{Label: "Deny maintenance period", Value: deny}}}
}

// alloyDBMaintenanceAction is the Maintenance's Edit.
func alloyDBMaintenanceAction(c *alloydbpb.Cluster) console.Action {
	day, hour := "ANY", "0"
	if ws := c.GetMaintenanceUpdatePolicy().GetMaintenanceWindows(); len(ws) > 0 {
		day, hour = ws[0].GetDay().String(), strconv.Itoa(int(ws[0].GetStartTime().GetHours()))
	}
	labels := map[string]string{}
	for _, d := range alloyDBDays {
		labels[d] = alloyDBDayLabel(d)
	}
	hours := make([]string, 24)
	hourLabels := map[string]string{}
	for h := range hours {
		hours[h] = strconv.Itoa(h)
		hourLabels[hours[h]] = fmt.Sprintf("%02d:00 - %02d:00 UTC", h, (h+1)%24)
	}
	return console.Action{ID: "edit-maintenance", Label: "Edit maintenance", Title: "Edit maintenance", Submit: "Update", Linked: true,
		Fields: []console.Field{
			{Name: "day", Label: "Maintenance window", Type: "select", Options: alloyDBDays, OptionLabels: labels, Default: day,
				Help: "The day of the week, in UTC, maintenance starts on. Any window lets maintenance start at any time outside 6 AM to 10 PM on weekdays, in the region's local time."},
			{Name: "hour", Label: "Hour of the day", Type: "select", Options: hours, OptionLabels: hourLabels, Default: hour,
				ShowWhen: "day=MONDAY|TUESDAY|WEDNESDAY|THURSDAY|FRIDAY|SATURDAY|SUNDAY",
				Help:     "Maintenance starts no later than an hour after this time."},
		}}
}

func (p alloyDBProvider) editMaintenance(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, values map[string]string) error {
	policy := proto.Clone(c.GetMaintenanceUpdatePolicy()).(*alloydbpb.MaintenanceUpdatePolicy)
	if policy == nil {
		policy = &alloydbpb.MaintenanceUpdatePolicy{}
	}
	day := strings.TrimSpace(values["day"])
	if day == "" || day == "ANY" {
		policy.MaintenanceWindows = nil
	} else {
		d, ok := dayofweek.DayOfWeek_value[day]
		if !ok || d == 0 {
			return fmt.Errorf("choose a day of the week, or Any window")
		}
		h, err := strconv.Atoi(strings.TrimSpace(values["hour"]))
		if err != nil || h < 0 || h > 23 {
			return fmt.Errorf("choose an hour of the day, 0 to 23")
		}
		policy.MaintenanceWindows = []*alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{{Day: dayofweek.DayOfWeek(d),
			StartTime: &timeofday.TimeOfDay{Hours: int32(h)}}}
	}
	op, err := s.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: c.GetName(), MaintenanceUpdatePolicy: policy},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"maintenance_update_policy"}}})
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 30*time.Second)
}

// ---- An instance row's Edit, with Google's database flags ----

// alloyDBFlagNames are the flags ListSupportedDatabaseFlags names.
func alloyDBFlagNames(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster) []string {
	loc := strings.Join(strings.Split(c.GetName(), "/")[:4], "/")
	resp, err := s.ListSupportedDatabaseFlags(ctx, &alloydbpb.ListSupportedDatabaseFlagsRequest{Parent: loc, PageSize: 1000})
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range resp.GetSupportedDatabaseFlags() {
		for _, v := range f.GetSupportedDbVersions() {
			if v == c.GetDatabaseVersion() {
				out = append(out, f.GetFlagName())
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// alloyDBFlagsDefault is an instance's flags as the group's rows.
func alloyDBFlagsDefault(flags map[string]string) string {
	if len(flags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('[')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"flag":%s,"value":%s}`, strconv.Quote(k), strconv.Quote(flags[k]))
	}
	b.WriteByte(']')
	return b.String()
}

// alloyDBInstanceEdit is an instance row's Edit: its machine (and a read
// pool's node count), and under Advanced configuration options its
// database flags and managed connection pooling.
func alloyDBInstanceEdit(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, in *alloydbpb.Instance) console.Action {
	const advanced = "Advanced configuration options"
	cpu := strconv.Itoa(int(in.GetMachineConfig().GetCpuCount()))
	fields := []console.Field{
		{Name: "cpu", Label: "Machine", Type: "select", Options: alloyDBCPUs, OptionLabels: alloyDBCPULabels(), Default: cpu},
	}
	if in.GetInstanceType() == alloydbpb.Instance_READ_POOL {
		fields = append(fields, console.Field{Name: "nodes", Label: "Node count", Type: "number", Default: strconv.Itoa(int(in.GetReadPoolConfig().GetNodeCount())),
			Pattern: `^([1-9]|1[0-9]|20)$`, Help: "1 to 20 nodes."})
	}
	fields = append(fields, console.Field{Name: "flags", Label: "Flags", Type: console.GroupFieldType, Section: advanced, AddLabel: "Add flag",
		RemoveLabel: "Delete", Default: alloyDBFlagsDefault(in.GetDatabaseFlags()),
		Help: "Changing a flag that needs a restart restarts the instance.",
		Group: []console.Field{
			{Name: "flag", Label: "New database flag", Type: "select", Options: alloyDBFlagNames(ctx, s, c)},
			{Name: "value", Label: "Value", Type: "text", Help: "the flag's value"},
		}})
	if in.GetInstanceType() != alloydbpb.Instance_SECONDARY {
		pooling := strconv.FormatBool(in.GetConnectionPoolConfig().GetEnabled())
		var pf []string
		for k, v := range in.GetConnectionPoolConfig().GetFlags() {
			pf = append(pf, k+"="+v)
		}
		sort.Strings(pf)
		fields = append(fields,
			console.Field{Name: "pooling", Label: "Enable Managed Connection Pooling", Type: "checkbox", Section: advanced, Default: pooling,
				Help: "A pooler beside the instance, on port 6432; the connectors and the Auth Proxy are pooled too."},
			console.Field{Name: "poolFlags", Label: "Connection pooling flags", Type: "text", Section: advanced, ShowWhen: "pooling", Default: strings.Join(pf, ","),
				Help: "name=value pairs, separated by commas: pool_mode, max_pool_size, min_pool_size, max_client_connections, ..."})
	}
	a := console.Action{ID: "edit", Label: "Edit", Title: "Edit " + alloyDBInstanceType(in.GetInstanceType()) + " instance " + lastSegment(in.GetName()),
		Submit: "Update instance", Fields: fields}
	if in.GetState() != alloydbpb.Instance_READY && in.GetState() != alloydbpb.Instance_STOPPED {
		a.Unavailable = "The instance is " + alloyDBState(in.GetState()) + "."
	}
	return a
}

// alloyDBInstanceRowActions are an instance row's actions.
func alloyDBInstanceRowActions(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, in *alloydbpb.Instance) []console.Action {
	return []console.Action{alloyDBInstanceEdit(ctx, s, c, in)}
}

// editInstanceRow is an instance row's Edit.
func (p alloyDBProvider) editInstanceRow(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, instance string, values map[string]string) error {
	name := c.GetName() + "/instances/" + instance
	in, err := s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: name})
	if err != nil {
		return err
	}
	rows, err := console.ParseGroup(values["flags"])
	if err != nil {
		return fmt.Errorf("flags: %w", err)
	}
	flags := map[string]string{}
	for _, r := range rows {
		f := strings.TrimSpace(r["flag"])
		if f == "" {
			continue
		}
		if _, dup := flags[f]; dup {
			return fmt.Errorf("the flag %s is set twice", f)
		}
		flags[f] = strings.TrimSpace(r["value"])
	}
	cpu, err := strconv.Atoi(values["cpu"])
	if err != nil {
		return fmt.Errorf("choose a machine")
	}
	next := &alloydbpb.Instance{Name: name, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: int32(cpu)}, DatabaseFlags: flags}
	paths := []string{"machine_config.cpu_count", "database_flags"}
	if in.GetInstanceType() == alloydbpb.Instance_READ_POOL {
		n, err := strconv.Atoi(strings.TrimSpace(values["nodes"]))
		if err != nil {
			return fmt.Errorf("node count: %v", err)
		}
		next.ReadPoolConfig = &alloydbpb.Instance_ReadPoolConfig{NodeCount: int32(n)}
		paths = append(paths, "read_pool_config.node_count")
	}
	if in.GetInstanceType() != alloydbpb.Instance_SECONDARY {
		next.ConnectionPoolConfig = &alloydbpb.Instance_ConnectionPoolConfig{Enabled: on(values["pooling"]), Flags: map[string]string{}}
		for _, kv := range strings.Split(values["poolFlags"], ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
				next.ConnectionPoolConfig.Flags[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		paths = append(paths, "connection_pool_config")
	}
	op, err := s.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: next, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 5*time.Second)
}

// ---- Backups: description and labels ----

// alloyDBLabelField is Labels, key and value rows (Add label), with
// Google's label rules.
func alloyDBLabelField(def string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Default: def,
		KeyPattern: `^\p{Ll}[\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`, ValuePattern: `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`,
		KeyHelp:   "A key is 1 to 63 lowercase letters, digits, _ and -, starting with a lowercase letter.",
		ValueHelp: "A value is 0 to 63 lowercase letters, digits, _ and -.", MaxEntries: 64}
}

// alloyDBEditBackupLabels is a backup's Edit labels.
func alloyDBEditBackupLabels(b *alloydbpb.Backup) console.Action {
	return console.Action{ID: "edit-labels", Label: "Edit labels", Title: "Edit labels: " + lastSegment(b.GetName()), Submit: "Save",
		Fields: []console.Field{alloyDBLabelField(console.FormatMap(b.GetLabels()))}}
}

func alloyDBUpdateBackupLabels(ctx context.Context, s *alloydb.Server, name string, values map[string]string) error {
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	op, err := s.UpdateBackup(ctx, &alloydbpb.UpdateBackupRequest{Backup: &alloydbpb.Backup{Name: name, Labels: labels},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 10*time.Second)
}
