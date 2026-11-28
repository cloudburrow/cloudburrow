package main

// AlloyDB's insights pages, round 3 (#2253): the parts #2251 left out for
// want of a reading, now backed by the instance servers' own log,
// auto_explain and pg_stat_activity (internal/service/alloydb/telemetry.go)
// as Google's console has each
// (docs.cloud.google.com/alloydb/docs/using-query-insights and
// docs.cloud.google.com/alloydb/docs/monitor-instance, read 2026-11-25):
//
//   - System insights' Log errors card: the error logs (severity ERROR and
//     above) of the period, the servers' lines in Cloud Logging, opening
//     Logs Explorer on them; the Query latency, Wait time and Wait count
//     charts, and an instance's Node CPU usage and Node database
//     availability; the events timeline's user events;
//   - Query insights' Tags table (Action, Controller, Framework, Route,
//     Application, DB Driver, Database, Load by total time, Avg execution
//     time (ms), Times called), with the Database and Client address
//     filters;
//   - a query's or tag's details: Database load, specific query (its
//     databases and users), Latency (P50, P95 and P99 over the last hour),
//     P99 latency across the cluster (each instance's), the Query plan
//     samples, and a sampled plan's operations, each with its latency,
//     rows returned, loops, cost and shared blocks.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// ---- System insights ----

// alloyDBLogFilter is Logs Explorer's filter for the error logs of a
// cluster, or of one instance when instance is not "".
func alloyDBLogFilter(sc alloyDBScope, instance string) string {
	f := fmt.Sprintf(`resource.type="alloydb.googleapis.com/Instance" AND resource.labels.cluster_id=%q AND logName="projects/%s/logs/alloydb.googleapis.com%%2Fpostgres.log" AND severity>=ERROR`,
		sc.cluster, sc.project)
	if instance != "" {
		f += fmt.Sprintf(` AND resource.labels.instance_id=%q`, instance)
	}
	return f
}

// alloyDBLogErrors is the Log errors card: the error logs of the last day.
func alloyDBLogErrors(logs *loggingService, sc alloyDBScope, instance string) console.Property {
	card := console.Property{Label: "Log errors"}
	if logs == nil || logs.api == nil {
		card.Value = "Cloud Logging is not served by this instance, so the servers' logs are not recorded."
		return card
	}
	filter := alloyDBLogFilter(sc, instance)
	n, err := logs.api.CountMatching(sc.project, filter, time.Now().Add(-24*time.Hour))
	if err != nil {
		card.Value = "Not available: " + err.Error()
		return card
	}
	card.Value = strconv.Itoa(n)
	card.Link = "/logs?project=" + url.QueryEscape(sc.project) + "&query=" + url.QueryEscape(filter)
	return card
}

// alloyDBLatencyCharts are a view's Query latency charts: P50, P95 and
// P99 of the aggregate latency distributions.
var alloyDBLatencyPercentiles = []struct {
	label string
	q     float64
}{{"P50", 0.5}, {"P95", 0.95}, {"P99", 0.99}}

// alloyDBWaitCharts are the wait event charts.
var alloyDBWaitCharts = []alloyDBChart{
	{label: "Wait time", metric: monitoring.AlloyDBWaitTime, unit: "microseconds per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
	{label: "Wait count", metric: monitoring.AlloyDBWaitCount, unit: "waits per minute", resource: monitoring.AlloyDBInstanceResource, scale: 1},
}

// alloyDBNodeCharts are an instance's node charts, where its node is its
// server.
var alloyDBNodeCharts = []alloyDBChart{
	{label: "Node CPU usage", metric: monitoring.AlloyDBNodeCPU, unit: "percent", resource: monitoring.AlloyDBNodeResource, gauge: true, scale: 1, max: 100},
	{label: "Node database availability", metric: monitoring.AlloyDBNodeUptime, unit: "ratio", resource: monitoring.AlloyDBNodeResource, gauge: true, scale: 1, max: 1},
}

// alloyDBLatencySeries are percentiles of metric's distributions within sc
// over the last hour, a point a minute, in milliseconds: the series'
// bucket counts summed each minute.
func alloyDBLatencySeries(ctx context.Context, a monitoringAdmin, sc alloyDBScope, metric, prefix string, labels map[string]string) []console.ChartSeries {
	var out []console.ChartSeries
	if _, err := a.server(); err != nil {
		for _, p := range alloyDBLatencyPercentiles {
			out = append(out, console.ChartSeries{Label: prefix + p.label, Unit: "ms", Unavailable: alloyDBNoMonitoring})
		}
		return out
	}
	end := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-sqlChartWindow * time.Minute)
	ts, err := alloyDBRead(ctx, a, sc, metric, monitoring.AlloyDBDatabaseResource, labels, start)
	if err != nil {
		for _, p := range alloyDBLatencyPercentiles {
			out = append(out, console.ChartSeries{Label: prefix + p.label, Unit: "ms", Unavailable: err.Error()})
		}
		return out
	}
	minutes := make([][]int64, sqlChartWindow)
	for _, s := range ts {
		for _, pt := range s.GetPoints() {
			i := int(pt.GetInterval().GetEndTime().AsTime().Sub(start) / time.Minute)
			d := pt.GetValue().GetDistributionValue()
			if i < 0 || i >= sqlChartWindow || d == nil {
				continue
			}
			if minutes[i] == nil {
				minutes[i] = make([]int64, len(d.GetBucketCounts()))
			}
			for j, c := range d.GetBucketCounts() {
				if j < len(minutes[i]) {
					minutes[i][j] += c
				}
			}
		}
	}
	for _, p := range alloyDBLatencyPercentiles {
		cs := console.ChartSeries{Label: prefix + p.label, Unit: "ms"}
		for i := 0; i < sqlChartWindow; i++ {
			pt := console.ChartPoint{At: start.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)}
			if minutes[i] != nil {
				v := alloydb.Percentile(minutes[i], p.q) / 1000
				pt.Value = &v
			}
			cs.Points = append(cs.Points, pt)
		}
		out = append(out, cs)
	}
	return out
}

// alloyDBRound3Series are a System insights view's round-3 charts.
func alloyDBRound3Series(ctx context.Context, a monitoringAdmin, v alloyDBView) []console.ChartSeries {
	out := alloyDBLatencySeries(ctx, a, v.scope, monitoring.AlloyDBLatencies, "Query latency - ", nil)
	out = append(out, alloyDBCharts(ctx, a, v.scope, alloyDBWaitCharts)...)
	if v.kind != "cluster" && v.instance != nil && alloyDBNodeIsServer(v.instance) {
		out = append(out, alloyDBCharts(ctx, a, v.scope, alloyDBNodeCharts)...)
	}
	return out
}

// alloyDBNodeIsServer is whether in's one node is its server, so its
// node/* metrics are published: a primary, or a read pool of one node.
func alloyDBNodeIsServer(in *alloydbpb.Instance) bool {
	return in.GetInstanceType() != alloydbpb.Instance_READ_POOL || in.GetReadPoolConfig().GetNodeCount() == 1
}

// alloyDBUserEvents are the events timeline's user events, as Google
// names them (USER_CREATE, USER_UPDATE, USER_DELETE).
func alloyDBUserEvents(s *alloydb.Server, c *alloydbpb.Cluster) []alloyDBEvent {
	var out []alloyDBEvent
	for _, e := range s.UserEvents(c.GetName()) {
		out = append(out, alloyDBEvent{at: e.At, prop: console.Property{Label: kmsTime(timestamppb.New(e.At)),
			Value: e.Type + " on " + lastSegment(e.User) + ": Succeeded"}})
	}
	return out
}

// alloyDBEvent is one events timeline entry.
type alloyDBEvent struct {
	at   time.Time
	prop console.Property
}

// ---- Query insights: Tags ----

// alloyDBTagRow is one tag set's totals in one database from one client.
type alloyDBTagRow struct {
	hash, database, client string
	tags                   alloydb.Tags
	calls                  int64
	totalUs                float64
	lockUs                 int64
}

// alloyDBTagRows are instance's tags' totals since the collector started
// reading its server.
func alloyDBTagRows(t alloydb.Telemetry) []alloyDBTagRow {
	by := map[[3]string]*alloyDBTagRow{}
	for _, l := range t.PerTag {
		k := [3]string{l.TagHash, l.Database, l.Client}
		r := by[k]
		if r == nil {
			r = &alloyDBTagRow{hash: l.TagHash, database: l.Database, client: l.Client, tags: t.TagValues[l.TagHash]}
			by[k] = r
		}
		r.calls += l.Hist.Count
		r.totalUs += l.Hist.Sum
	}
	for _, l := range t.Locks {
		if r := by[[3]string{l.TagHash, l.Database, l.Client}]; r != nil && l.TagHash != "" {
			r.lockUs += l.Us
		}
	}
	out := make([]alloyDBTagRow, 0, len(by))
	for _, r := range by {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].totalUs != out[j].totalUs {
			return out[i].totalUs > out[j].totalUs
		}
		return out[i].hash+out[i].database+out[i].client < out[j].hash+out[j].database+out[j].client
	})
	return out
}

// alloyDBTagsTab is Query insights' Tags table for one instance.
func alloyDBTagsTab(s *alloydb.Server, c *alloydbpb.Cluster, in *alloydbpb.Instance) console.Section {
	id := lastSegment(in.GetName())
	tab := console.Section{ID: "qt-" + id, Label: "Tags: " + id,
		Note: "Each application tag set's executions since CloudBurrow started reading the server, from the sqlcommenter comments in " +
			"the statements its log records; lock wait from pg_stat_activity, sampled every second."}
	l := console.Listing{TableTitle: "Tags", NameColumn: "Action", Noun: "tags", RowsOpenable: true,
		Columns: []string{"Controller", "Framework", "Route", "Application", "DB Driver", "Database", "Load by total time",
			"Avg execution time (ms)", "Times called"},
		OptionalColumns: []string{"Client address", "Load by lock wait"}}
	if in.GetState() != alloydbpb.Instance_READY {
		l.Unavailable = "The instance is " + alloyDBState(in.GetState()) + ": its server is not serving."
		tab.Listing = l
		return tab
	}
	if !in.GetQueryInsightsConfig().GetRecordApplicationTags() {
		l.Prompt = "Store application tags is off for this instance: turn it on with Query insights' Edit to record tags."
		tab.Listing = l
		return tab
	}
	t := s.TelemetryOf(in.GetName())
	if !t.Reading {
		l.Prompt = "No readings yet: CloudBurrow reads the server a few seconds after it starts serving."
		tab.Listing = l
		return tab
	}
	rows := alloyDBTagRows(t)
	var total float64
	var locks int64
	for _, r := range rows {
		total += r.totalUs
		locks += r.lockUs
	}
	share := func(v, of float64) string {
		if of <= 0 {
			return "0%"
		}
		return fmt.Sprintf("%.1f%%", v*100/of)
	}
	dbs, clients := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		dbs[r.database], clients[orDash(r.client)] = true, true
		avg := 0.0
		if r.calls > 0 {
			avg = r.totalUs / float64(r.calls) / 1000
		}
		l.Items = append(l.Items, console.Resource{Name: orDash(r.tags.Action), Opens: []string{c.GetName(), "tag", id, r.hash},
			Fields: map[string]string{"Controller": orDash(r.tags.Controller), "Framework": orDash(r.tags.Framework),
				"Route": orDash(r.tags.Route), "Application": orDash(r.tags.Application), "DB Driver": orDash(r.tags.DBDriver),
				"Database": r.database, "Client address": orDash(r.client), "Load by total time": share(r.totalUs, total),
				"Load by lock wait": share(float64(r.lockUs), float64(locks)), "Avg execution time (ms)": fmt.Sprintf("%.3f", avg),
				"Times called": strconv.FormatInt(r.calls, 10)}})
	}
	l.Total = len(l.Items)
	l.Menus = []console.ListMenu{{Label: "Database", Key: "Database", Options: sortedSet(dbs)},
		{Label: "Client address", Key: "Client address", Options: sortedSet(clients)}}
	tab.Listing = l
	return tab
}

// ---- a query's details ----

// alloyDBQueryDetail is a query's details page: [cluster, "query",
// instance, query_hash], and a plan sample's below it.
func (p alloyDBProvider) alloyDBQueryDetail(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, path []string) console.Detail {
	instance, hash := path[2], path[3]
	name := c.GetName() + "/instances/" + instance
	in, err := s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + consoleErr(err)}
	}
	id, err := strconv.ParseUint(hash, 10, 64)
	if err != nil {
		return console.Detail{Unavailable: "no query " + hash}
	}
	queryID := int64(id)
	if len(path) == 6 && path[4] == "plan" {
		return alloyDBPlanDetail(s, name, queryID, path[5])
	}
	t := s.TelemetryOf(name)
	stats, _ := s.QueryInsights(ctx, name)
	text := t.Queries[queryID]
	overview := console.Listing{TableTitle: "Database load, specific query", NameColumn: "Database", Noun: "databases",
		Columns: []string{"User", "Avg execution time (ms)", "Times called", "Avg rows fetched"}}
	dbs, users := map[string]bool{}, map[string]bool{}
	var calls int64
	var totalMs float64
	for _, q := range stats {
		if q.QueryID != queryID {
			continue
		}
		if text == "" {
			text = q.Query
		}
		rows := "0"
		if q.Calls > 0 {
			rows = strconv.FormatFloat(float64(q.Rows)/float64(q.Calls), 'f', 2, 64)
		}
		calls, totalMs = calls+q.Calls, totalMs+q.TotalMs
		dbs[q.Database], users[q.User] = true, true
		overview.Items = append(overview.Items, console.Resource{Name: q.Database, Fields: map[string]string{"User": q.User,
			"Avg execution time (ms)": fmt.Sprintf("%.3f", q.MeanMs), "Times called": strconv.FormatInt(q.Calls, 10), "Avg rows fetched": rows}})
	}
	overview.Total = len(overview.Items)
	overview.Menus = []console.ListMenu{{Label: "Database", Key: "Name", Options: sortedSet(dbs)}, {Label: "User", Key: "User", Options: sortedSet(users)}}
	if text == "" {
		text = "(query " + hash + ")"
	}
	avg := "-"
	if calls > 0 {
		avg = fmt.Sprintf("%.3f", totalMs/float64(calls))
	}
	sc := alloyDBScopeOf(c)
	sc.instance = instance
	summary := []console.Property{{Label: "Query", Value: truncateQuery(text, alloyDBQueryLength(in))}, {Label: "Instance", Value: instance},
		{Label: "Times called", Value: strconv.FormatInt(calls, 10)}, {Label: "Avg execution time (ms)", Value: avg}}
	load := console.Section{ID: "query-load", Label: "Database load", Listing: overview, SeriesTitle: "Database load, specific query",
		Series: alloyDBQueryLoadSeries(ctx, p.metrics, sc, hash)}
	latency := console.Section{ID: "query-latency", Label: "Latency", SeriesTitle: "Latency",
		Series:  alloyDBLatencySeries(ctx, p.metrics, sc, monitoring.AlloyDBQueryLatencies, "", map[string]string{"query_hash": hash}),
		Listing: alloyDBClusterP99(ctx, s, p.metrics, c, "query_hash", hash),
		Note: "Each execution's wall-clock latency from the server's log (its duration line), as Cloud Monitoring's " +
			"per-query latencies metric records it a minute at a time."}
	plans := console.Section{ID: "query-plans", Label: "Query plan samples", Listing: alloyDBPlanListing(s, c, in, queryID, hash),
		Note: fmt.Sprintf("Plans auto_explain sampled with EXPLAIN ANALYZE, at most %d a minute (the instance's maximum sampling rate).",
			alloyDBPlanRate(in))}
	return console.Detail{Summary: summary, Sections: []console.Section{load, latency, plans}}
}

func alloyDBQueryLength(in *alloydbpb.Instance) int {
	if n := int(in.GetQueryInsightsConfig().GetQueryStringLength()); n > 0 {
		return n
	}
	return 1024
}

func alloyDBPlanRate(in *alloydbpb.Instance) int {
	q := in.GetQueryInsightsConfig()
	if q == nil || q.QueryPlansPerMinute == nil {
		return 5
	}
	return int(q.GetQueryPlansPerMinute())
}

// alloyDBQueryLoadSeries is a query's load over the last hour from its
// perquery DELTAs: CPU and CPU wait (execution less IO and lock), IO wait
// and lock wait, in seconds a second.
func alloyDBQueryLoadSeries(ctx context.Context, a monitoringAdmin, sc alloyDBScope, hash string) []console.ChartSeries {
	charts := []alloyDBChart{
		{label: "Execution time", metric: monitoring.AlloyDBQueryExecTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource,
			labels: map[string]string{"query_hash": hash}, scale: 1},
		{label: "IO wait", metric: monitoring.AlloyDBQueryIOTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource,
			labels: map[string]string{"query_hash": hash}, scale: 1},
		{label: "Lock wait", metric: monitoring.AlloyDBQueryLockTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource,
			labels: map[string]string{"query_hash": hash}, scale: 1},
	}
	return alloyDBCharts(ctx, a, sc, charts)
}

// alloyDBClusterP99 is P99 latency across the cluster: each serving
// instance's P50, P95 and P99 of the key over the last hour.
func alloyDBClusterP99(ctx context.Context, s *alloydb.Server, a monitoringAdmin, c *alloydbpb.Cluster, label, value string) console.Listing {
	l := console.Listing{TableTitle: "P99 latency across the cluster", NameColumn: "Instance", Noun: "instances",
		Columns: []string{"P50 (ms)", "P95 (ms)", "P99 (ms)", "Executions"}}
	metric := monitoring.AlloyDBQueryLatencies
	if label == "tag_hash" {
		metric = monitoring.AlloyDBTagLatencies
	}
	ins, _ := alloyDBInstances(ctx, s, c.GetName())
	if _, err := a.server(); err != nil {
		l.Unavailable = alloyDBNoMonitoring
		return l
	}
	for _, in := range ins {
		sc := alloyDBScopeOf(c)
		sc.instance = lastSegment(in.GetName())
		ts, err := alloyDBRead(ctx, a, sc, metric, monitoring.AlloyDBDatabaseResource, map[string]string{label: value}, time.Now().Add(-time.Hour))
		if err != nil {
			continue
		}
		var counts []int64
		var n int64
		for _, series := range ts {
			for _, pt := range series.GetPoints() {
				d := pt.GetValue().GetDistributionValue()
				if d == nil {
					continue
				}
				if counts == nil {
					counts = make([]int64, len(d.GetBucketCounts()))
				}
				for j, b := range d.GetBucketCounts() {
					if j < len(counts) {
						counts[j] += b
					}
				}
				n += d.GetCount()
			}
		}
		if n == 0 {
			continue
		}
		ms := func(q float64) string { return fmt.Sprintf("%.3f", alloydb.Percentile(counts, q)/1000) }
		l.Items = append(l.Items, console.Resource{Name: sc.instance, Fields: map[string]string{"P50 (ms)": ms(0.5), "P95 (ms)": ms(0.95),
			"P99 (ms)": ms(0.99), "Executions": strconv.FormatInt(n, 10)}})
	}
	l.Total = len(l.Items)
	if len(l.Items) == 0 {
		l.Prompt = "No executions in the last hour."
	}
	return l
}

// alloyDBPlanListing is a query's Query plan samples, newest first.
func alloyDBPlanListing(s *alloydb.Server, c *alloydbpb.Cluster, in *alloydbpb.Instance, queryID int64, hash string) console.Listing {
	l := console.Listing{TableTitle: "Query plan samples", NameColumn: "Sampled at", Noun: "plan samples", RowsOpenable: true,
		Columns: []string{"Duration (ms)", "Database", "User", "Client address"}}
	if alloyDBPlanRate(in) == 0 {
		l.Prompt = "Query plan sampling is off for this instance: turn it on with Query insights' Edit."
		return l
	}
	for _, p := range s.QueryPlans(in.GetName(), queryID) {
		l.Items = append(l.Items, console.Resource{Name: p.At.UTC().Format("2006-01-02 15:04:05.000 UTC"),
			Opens: []string{c.GetName(), "query", lastSegment(in.GetName()), hash, "plan", strconv.FormatInt(p.At.UnixNano(), 10)},
			Fields: map[string]string{"Duration (ms)": fmt.Sprintf("%.3f", p.DurationMs), "Database": p.Database, "User": p.User,
				"Client address": orDash(p.Client)}})
	}
	l.Total = len(l.Items)
	if len(l.Items) == 0 {
		l.Prompt = "No plans sampled yet: auto_explain samples a few of the query's executions a minute."
	}
	return l
}

// alloyDBPlanNode is one operation of EXPLAIN (FORMAT JSON).
type alloyDBPlanNode struct {
	NodeType     string            `json:"Node Type"`
	Relation     string            `json:"Relation Name"`
	Index        string            `json:"Index Name"`
	StartupCost  float64           `json:"Startup Cost"`
	TotalCost    float64           `json:"Total Cost"`
	PlanRows     float64           `json:"Plan Rows"`
	ActualTotal  *float64          `json:"Actual Total Time"`
	ActualRows   *float64          `json:"Actual Rows"`
	ActualLoops  *float64          `json:"Actual Loops"`
	SharedHit    *int64            `json:"Shared Hit Blocks"`
	SharedRead   *int64            `json:"Shared Read Blocks"`
	Plans        []alloyDBPlanNode `json:"Plans"`
	Relationship string            `json:"Parent Relationship"`
}

// alloyDBPlanDetail is one sampled plan: its operations, each with its
// latency, rows, loops, cost and shared blocks.
func alloyDBPlanDetail(s *alloydb.Server, instance string, queryID int64, at string) console.Detail {
	var plan *alloydb.PlanSample
	for _, p := range s.QueryPlans(instance, queryID) {
		if strconv.FormatInt(p.At.UnixNano(), 10) == at {
			p := p
			plan = &p
		}
	}
	if plan == nil {
		return console.Detail{Unavailable: "this plan sample is no longer kept: the server keeps its most recent samples."}
	}
	return alloyDBPlanPage(*plan)
}

// alloyDBPlanPage is a sampled plan's page: auto_explain's JSON, an
// operation a row, depth-first.
func alloyDBPlanPage(sample alloydb.PlanSample) console.Detail {
	plan := &sample
	var doc struct {
		Text      string          `json:"Query Text"`
		Plan      alloyDBPlanNode `json:"Plan"`
		Planning  *float64        `json:"Planning Time"`
		Execution *float64        `json:"Execution Time"`
	}
	_ = json.Unmarshal(plan.Plan, &doc)
	ms := func(f *float64) string {
		if f == nil {
			return "-"
		}
		return fmt.Sprintf("%.3f", *f)
	}
	summary := []console.Property{{Label: "Query", Value: doc.Text}, {Label: "Sampled at", Value: plan.At.UTC().Format("2006-01-02 15:04:05.000 UTC")},
		{Label: "Duration (ms)", Value: fmt.Sprintf("%.3f", plan.DurationMs)}, {Label: "Database", Value: plan.Database},
		{Label: "User", Value: plan.User}}
	if doc.Planning != nil {
		summary = append(summary, console.Property{Label: "Planning time (ms)", Value: ms(doc.Planning)})
	}
	l := console.Listing{TableTitle: "Query plan", NameColumn: "Operation", Noun: "operations",
		Columns: []string{"Latency (ms)", "Rows returned", "Estimated rows", "Loops", "Cost", "Shared hit blocks", "Shared read blocks", "Relation"}}
	var walk func(n alloyDBPlanNode, depth int)
	walk = func(n alloyDBPlanNode, depth int) {
		num := func(f *float64) string {
			if f == nil {
				return "-"
			}
			return strconv.FormatFloat(*f, 'f', -1, 64)
		}
		blocks := func(b *int64) string {
			if b == nil {
				return "-"
			}
			return strconv.FormatInt(*b, 10)
		}
		rel := n.Relation
		if n.Index != "" {
			rel = strings.TrimSpace(rel + " " + n.Index)
		}
		l.Items = append(l.Items, console.Resource{Name: strings.Repeat("→ ", depth) + n.NodeType, Fields: map[string]string{
			"Latency (ms)": ms(n.ActualTotal), "Rows returned": num(n.ActualRows), "Estimated rows": strconv.FormatFloat(n.PlanRows, 'f', -1, 64),
			"Loops": num(n.ActualLoops), "Cost": fmt.Sprintf("%.2f..%.2f", n.StartupCost, n.TotalCost),
			"Shared hit blocks": blocks(n.SharedHit), "Shared read blocks": blocks(n.SharedRead), "Relation": orDash(rel)}})
		for _, c := range n.Plans {
			walk(c, depth+1)
		}
	}
	walk(doc.Plan, 0)
	l.Total = len(l.Items)
	raw, _ := json.MarshalIndent(json.RawMessage(plan.Plan), "", "  ")
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "plan", Label: "Query plan", Listing: l},
		{ID: "plan-json", Label: "JSON", Kind: console.KindText, Language: "json", Text: string(raw)},
	}}
}

// ---- a tag's details ----

// alloyDBTagDetail is a tag set's details page: [cluster, "tag",
// instance, tag_hash].
func (p alloyDBProvider) alloyDBTagDetail(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, path []string) console.Detail {
	instance, hash := path[2], path[3]
	name := c.GetName() + "/instances/" + instance
	t := s.TelemetryOf(name)
	tags, ok := t.TagValues[hash]
	if !ok {
		return console.Detail{Unavailable: "no tag " + hash + " has been read on this instance."}
	}
	summary := []console.Property{{Label: "Action", Value: orDash(tags.Action)}, {Label: "Controller", Value: orDash(tags.Controller)},
		{Label: "Framework", Value: orDash(tags.Framework)}, {Label: "Route", Value: orDash(tags.Route)},
		{Label: "Application", Value: orDash(tags.Application)}, {Label: "DB Driver", Value: orDash(tags.DBDriver)}, {Label: "Instance", Value: instance}}
	byDB := console.Listing{TableTitle: "Database load, specific tags", NameColumn: "Database", Noun: "databases",
		Columns: []string{"User", "Client address", "Avg execution time (ms)", "Times called"}}
	clients := map[string]bool{}
	for _, l := range t.PerTag {
		if l.TagHash != hash || l.Hist.Count == 0 {
			continue
		}
		clients[orDash(l.Client)] = true
		byDB.Items = append(byDB.Items, console.Resource{Name: l.Database, Fields: map[string]string{"User": l.User, "Client address": orDash(l.Client),
			"Avg execution time (ms)": fmt.Sprintf("%.3f", l.Hist.Sum/float64(l.Hist.Count)/1000), "Times called": strconv.FormatInt(l.Hist.Count, 10)}})
	}
	byDB.Total = len(byDB.Items)
	byDB.Menus = []console.ListMenu{{Label: "Client address", Key: "Client address", Options: sortedSet(clients)}}
	sc := alloyDBScopeOf(c)
	sc.instance = instance
	load := console.Section{ID: "tag-load", Label: "Database load", Listing: byDB, SeriesTitle: "Database load, specific tags",
		Series: alloyDBCharts(ctx, p.metrics, sc, []alloyDBChart{
			{label: "Execution time", metric: monitoring.AlloyDBTagExecTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource,
				labels: map[string]string{"tag_hash": hash}, scale: 1},
			{label: "Lock wait", metric: monitoring.AlloyDBTagLockTime, unit: "microseconds per minute", resource: monitoring.AlloyDBDatabaseResource,
				labels: map[string]string{"tag_hash": hash}, scale: 1}})}
	latency := console.Section{ID: "tag-latency", Label: "Latency", SeriesTitle: "Latency",
		Series:  alloyDBLatencySeries(ctx, p.metrics, sc, monitoring.AlloyDBTagLatencies, "", map[string]string{"tag_hash": hash}),
		Listing: alloyDBClusterP99(ctx, s, p.metrics, c, "tag_hash", hash)}
	return console.Detail{Summary: summary, Sections: []console.Section{load, latency}}
}
