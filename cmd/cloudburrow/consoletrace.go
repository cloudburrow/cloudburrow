package main

// Cloud Trace's console (#2118), over the Cloud Trace server this process
// runs, called in process, in the layout Google documents
// (docs.cloud.google.com/trace/docs/finding-traces and
// /trace/docs/viewing-details, read 2026-11-21):
//
//   - Trace Explorer (/traces/explorer): the toolbar's trace scope, time
//     range and Trace ID; the Span filters pane (service name, span name,
//     span status, duration, span kind) and the Filter bar, which takes the
//     trace filter language (filter.go of internal/service/trace); a chart
//     of the spans' durations over time; and the Spans table. "On a new
//     project, the default trace scope lists the _AllSpans view for the
//     project": the search reads every project the selected trace scope
//     names, the project's default trace scope unless another is chosen.
//   - The trace details flyout: the timeline of the trace's spans, each
//     with its service and a latency bar ("A circle on a latency bar
//     indicates that a log entry or event is associated with the span"),
//     and the selected span's Attributes, Logs & Events (the log entries
//     whose trace is the trace and whose spanId is the span, from this
//     instance's Cloud Logging, and the span's annotations) and Metadata &
//     Links tabs.
//
// Choices Google does not document (UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/finding-traces)): an entry of
// the trace with no spanId is shown on the root span; the service is the
// span's service.name attribute; and at most 1000 spans are listed, the
// newest first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/trace"
)

// traceExplorerProvider is the Trace Explorer and its trace details.
type traceExplorerProvider struct {
	svc *traceService
	// logging is the instance's Cloud Logging, nil when it is not served:
	// the trace scopes and the spans' log entries.
	logging *loggingService
}

func (traceExplorerProvider) ID() string    { return "trace-explorer" }
func (traceExplorerProvider) Title() string { return "Trace explorer" }

// maxExplorerSpans bounds one search's Spans table.
const maxExplorerSpans = 1000

var errTraceNotStarted = errors.New("Cloud Trace has not started")

func (p traceExplorerProvider) api() (*trace.Server, error) {
	a := p.svc.API()
	if a == nil {
		return nil, errTraceNotStarted
	}
	return a, nil
}

func (p traceExplorerProvider) admin() *logging.Admin {
	if p.logging == nil {
		return nil
	}
	return p.logging.Admin()
}

// traceScopeOption is one entry of the toolbar's trace scope menu.
type traceScopeOption struct {
	Name     string
	Projects []string
	Default  bool
}

// scopes are the project's trace scopes, the default one marked; just the
// project's own _Default without Cloud Logging, which keeps them.
func (p traceExplorerProvider) scopes(project string) []traceScopeOption {
	own := traceScopeOption{Name: defaultTraceScopeName(project), Projects: []string{project}}
	a := p.admin()
	if a == nil {
		own.Default = true
		return []traceScopeOption{own}
	}
	var resp struct {
		TraceScopes []logging.TraceScope `json:"traceScopes"`
	}
	if err := callObservabilityJSON(a, http.MethodGet, traceScopesPath(project)+"?pageSize=1000", nil, &resp); err != nil || len(resp.TraceScopes) == 0 {
		own.Default = true
		return []traceScopeOption{own}
	}
	def := loggingTraceScopesProvider{loggingAdminScreen{svc: p.logging}}.defaultScope(project)
	var out []traceScopeOption
	for _, ts := range resp.TraceScopes {
		o := traceScopeOption{Name: ts.Name, Default: ts.Name == def}
		for _, r := range ts.ResourceNames {
			o.Projects = append(o.Projects, strings.TrimPrefix(r, "projects/"))
		}
		out = append(out, o)
	}
	return out
}

// scopeProjects are the projects a named scope holds, or the default
// scope's for "".
func (p traceExplorerProvider) scopeProjects(project, scope string) ([]string, string, error) {
	opts := p.scopes(project)
	for _, o := range opts {
		if (scope == "" && o.Default) || o.Name == scope || strings.HasSuffix(o.Name, "/traceScopes/"+scope) {
			return o.Projects, o.Name, nil
		}
	}
	if scope == "" && len(opts) > 0 {
		return opts[0].Projects, opts[0].Name, nil
	}
	return nil, "", fmt.Errorf("trace scope %s does not exist in project %s", scope, project)
}

// List is the toolbar's trace scope menu.
func (p traceExplorerProvider) List(_ context.Context, project string) (console.Listing, error) {
	if _, err := p.api(); err != nil {
		return console.Listing{}, err
	}
	out := console.Listing{Columns: []string{"Resources", "Default"}, NameColumn: "Trace scope", Noun: "trace scopes"}
	if project == "" {
		out.Prompt = "Choose a project in the toolbar."
		return out, nil
	}
	for _, o := range p.scopes(project) {
		def := ""
		if o.Default {
			def = "Default"
		}
		out.Items = append(out.Items, console.Resource{Name: o.Name, Fields: map[string]string{"Resources": strings.Join(o.Projects, ", "), "Default": def}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// QueryForm is the explorer's controls; the page draws them itself, in
// Google's layout, and this is their contract.
func (traceExplorerProvider) QueryForm(path []string) (string, []console.Field) {
	if len(path) == 1 && path[0] == "trace" {
		return "Open trace", []console.Field{
			{Name: "trace", Label: "Trace ID", Type: "text", Required: true},
			{Name: "traceProject", Label: "Project", Type: "text"},
			{Name: "scope", Label: "Trace scope", Type: "text"},
		}
	}
	return "Run query", []console.Field{
		{Name: "scope", Label: "Trace scope", Type: "text"},
		{Name: "range", Label: "Time range (seconds)", Type: "text"},
		{Name: "filter", Label: "Filter", Type: "text", Help: "The trace filter language: root:, span:, latency:, label:, KEY:VALUE, +exact, ^root only."},
		{Name: "service", Label: "Service name", Type: "text"},
		{Name: "spanName", Label: "Span name", Type: "text"},
		{Name: "status", Label: "Span status", Type: "select", Options: []string{"", "OK", "Error"}},
		{Name: "minDuration", Label: "Minimum duration (ms)", Type: "text"},
		{Name: "kind", Label: "Span kind", Type: "select", Options: []string{"", "INTERNAL", "SERVER", "CLIENT", "PRODUCER", "CONSUMER"}},
		// A heatmap cell's selection.
		{Name: "maxDuration", Label: "Maximum duration (ms)", Type: "text"},
		{Name: "cellStart", Label: "Interval start (epoch ms)", Type: "text"},
		{Name: "cellEnd", Label: "Interval end (epoch ms)", Type: "text"},
	}
}

func spanService(sp *trace.Span) string {
	if v, ok := sp.V2.GetAttributes().GetAttributeMap()["service.name"]; ok {
		return trace.AttrString(v)
	}
	return ""
}

func spanStatus(sp *trace.Span) string {
	if st := sp.V2.GetStatus(); st != nil && st.GetCode() != 0 {
		return "Error"
	}
	return "OK"
}

func spanKind(sp *trace.Span) string {
	k := sp.V2.GetSpanKind().String()
	if k == "SPAN_KIND_UNSPECIFIED" {
		return "UNSPECIFIED"
	}
	return k
}

func durationMs(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
}

// Build runs the explorer: ["spans"] is the Spans table of a search,
// ["heatmap"] its latency heatmap and Chart view series, ["grouped"] its
// Grouped table; ["trace"] is one trace's details.
func (p traceExplorerProvider) Build(_ context.Context, project string, path []string, values map[string]string) (console.Listing, error) {
	a, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		return console.Listing{Prompt: "Choose a project in the toolbar."}, nil
	}
	switch {
	case len(path) == 1 && path[0] == "spans":
		return p.spans(a, project, values, time.Now())
	case len(path) == 1 && path[0] == "heatmap":
		return p.heatmap(a, project, values, time.Now())
	case len(path) == 1 && path[0] == "grouped":
		return p.grouped(a, project, values, time.Now())
	case len(path) == 1 && path[0] == "trace":
		return p.details(a, project, values)
	}
	return console.Listing{}, fmt.Errorf("Trace explorer has no query at %v", path)
}

// spanQuery is one search's matched spans, the newest first, with the
// time range it read and the trace scope it read through.
type spanQuery struct {
	spans    []*trace.Span
	projects []string
	scope    string
	start    time.Time // zero for no lower bound
}

func msValue(values map[string]string, key, what string) (time.Duration, bool, error) {
	v := values[key]
	if v == "" {
		return 0, false, nil
	}
	ms, err := strconv.ParseFloat(v, 64)
	if err != nil || ms < 0 {
		return 0, false, fmt.Errorf("%s %q is not a number of milliseconds", what, v)
	}
	return time.Duration(ms * float64(time.Millisecond)), true, nil
}

// match runs a search: the trace scope, the Filter bar, the time range and
// the Span filters, and a heatmap cell's selection (cellStart and cellEnd,
// epoch milliseconds of the spans' start; minDuration and maxDuration).
func (p traceExplorerProvider) match(a *trace.Server, project string, values map[string]string, now time.Time) (spanQuery, error) {
	projects, scopeName, err := p.scopeProjects(project, values["scope"])
	if err != nil {
		return spanQuery{}, err
	}
	f, err := trace.ParseFilter(values["filter"])
	if err != nil {
		return spanQuery{}, err
	}
	q := trace.Query{Projects: projects, Filter: f}
	if r, err := strconv.Atoi(values["range"]); err == nil && r > 0 {
		q.Start = now.Add(-time.Duration(r) * time.Second)
	} else if values["range"] != "" {
		return spanQuery{}, fmt.Errorf("time range %q is not a number of seconds", values["range"])
	}
	minDur, _, err := msValue(values, "minDuration", "minimum duration")
	if err != nil {
		return spanQuery{}, err
	}
	maxDur, hasMax, err := msValue(values, "maxDuration", "maximum duration")
	if err != nil {
		return spanQuery{}, err
	}
	var cellStart, cellEnd int64
	for key, dst := range map[string]*int64{"cellStart": &cellStart, "cellEnd": &cellEnd} {
		if v := values[key]; v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return spanQuery{}, fmt.Errorf("%s %q is not a time in epoch milliseconds", key, v)
			}
			*dst = n
		}
	}
	var spans []*trace.Span
	for _, t := range a.Search(q) {
		for _, sp := range t.Spans {
			if !q.Start.IsZero() && sp.End().Before(q.Start) {
				continue
			}
			if v := values["service"]; v != "" && spanService(sp) != v {
				continue
			}
			if v := values["spanName"]; v != "" && !strings.Contains(sp.Name(), v) {
				continue
			}
			if v := values["status"]; v != "" && spanStatus(sp) != v {
				continue
			}
			if v := values["kind"]; v != "" && spanKind(sp) != v {
				continue
			}
			d := sp.End().Sub(sp.Start())
			if d < minDur || (hasMax && d >= maxDur) {
				continue
			}
			if ms := sp.Start().UnixMilli(); (cellStart != 0 && ms < cellStart) || (cellEnd != 0 && ms >= cellEnd) {
				continue
			}
			spans = append(spans, sp)
		}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start().After(spans[j].Start()) })
	return spanQuery{spans: spans, projects: projects, scope: scopeName, start: q.Start}, nil
}

func (p traceExplorerProvider) spans(a *trace.Server, project string, values map[string]string, now time.Time) (console.Listing, error) {
	m, err := p.match(a, project, values, now)
	if err != nil {
		return console.Listing{}, err
	}
	spans := m.spans
	out := console.Listing{NameColumn: "Span name", Noun: "spans",
		Columns: []string{"Start time", "Service", "Duration (ms)", "Status", "Span kind", "Trace ID", "Project"}}
	out.Total = len(spans)
	if len(spans) > maxExplorerSpans {
		spans = spans[:maxExplorerSpans]
	}
	for _, sp := range spans {
		out.Items = append(out.Items, console.Resource{Name: sp.Name(), Fields: map[string]string{
			"Start time": sp.Start().UTC().Format(time.RFC3339Nano), "Service": orDash(spanService(sp)),
			"Duration (ms)": durationMs(sp.End().Sub(sp.Start())), "Status": spanStatus(sp), "Span kind": spanKind(sp),
			"Trace ID": sp.TraceID, "Project": sp.Project, "Span ID": sp.SpanID,
			"startMs": strconv.FormatInt(sp.Start().UnixMilli(), 10)}})
	}
	out.Note = fmt.Sprintf("trace scope %s: %s", m.scope, strings.Join(m.projects, ", "))
	return out, nil
}

// heatmapLatencyEdges are the heatmap's latency rows, in milliseconds,
// each row from one edge to the next and the last open above. UNVERIFIED
// (docs re-read 2026-11-21: https://docs.cloud.google.com/trace/docs/
// finding-traces): Google does not publish its buckets; these are
// logarithmic, 1-2-5 per decade.
var heatmapLatencyEdges = []float64{0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000, 60000}

// heatmapColumns is the number of time intervals across the range.
const heatmapColumns = 30

// percentile is the nearest-rank percentile of sorted durations.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func fms(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

// heatmap is the search's spans by start time and latency: "the heatmap
// uses color to represent the number of spans in a cell", each cell a time
// interval and latency range ("cell" items); and per interval the 50th,
// 90th, 95th and 99th percentiles of the span duration and the rate of OK
// and error spans ("interval" items), for the Chart view's Span duration
// (percentile) and Span rate.
func (p traceExplorerProvider) heatmap(a *trace.Server, project string, values map[string]string, now time.Time) (console.Listing, error) {
	m, err := p.match(a, project, values, now)
	if err != nil {
		return console.Listing{}, err
	}
	out := console.Listing{Noun: "cells", Columns: []string{"t0", "t1", "lo", "hi", "count"}, Total: len(m.spans)}
	from, to := m.start, now
	if from.IsZero() {
		if len(m.spans) == 0 {
			return out, nil
		}
		from = m.spans[len(m.spans)-1].Start()
		if s := m.spans[0].Start(); s.After(to) {
			to = s
		}
	}
	width := to.Sub(from) / heatmapColumns
	if width <= 0 {
		width = time.Second
	}
	type cellKey struct{ col, row int }
	cells := map[cellKey]int{}
	cols := make([][]float64, heatmapColumns)
	errs := make([]int, heatmapColumns)
	for _, sp := range m.spans {
		col := int(sp.Start().Sub(from) / width)
		col = max(0, min(col, heatmapColumns-1))
		ms := float64(sp.End().Sub(sp.Start())) / float64(time.Millisecond)
		row := sort.SearchFloat64s(heatmapLatencyEdges, ms)
		if row == len(heatmapLatencyEdges) || heatmapLatencyEdges[row] != ms {
			row--
		}
		row = max(0, row)
		cells[cellKey{col, row}]++
		cols[col] = append(cols[col], ms)
		if spanStatus(sp) == "Error" {
			errs[col]++
		}
	}
	colStart := func(c int) int64 { return from.Add(time.Duration(c) * width).UnixMilli() }
	keys := make([]cellKey, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].col != keys[j].col {
			return keys[i].col < keys[j].col
		}
		return keys[i].row < keys[j].row
	})
	for _, k := range keys {
		hi := ""
		if k.row+1 < len(heatmapLatencyEdges) {
			hi = fms(heatmapLatencyEdges[k.row+1])
		}
		out.Items = append(out.Items, console.Resource{Name: "cell", Fields: map[string]string{
			"t0": strconv.FormatInt(colStart(k.col), 10), "t1": strconv.FormatInt(colStart(k.col+1), 10),
			"lo": fms(heatmapLatencyEdges[k.row]), "hi": hi, "row": strconv.Itoa(k.row), "count": strconv.Itoa(cells[k])}})
	}
	secs := width.Seconds()
	for c := range heatmapColumns {
		d := cols[c]
		sort.Float64s(d)
		out.Items = append(out.Items, console.Resource{Name: "interval", Fields: map[string]string{
			"t0": strconv.FormatInt(colStart(c), 10), "t1": strconv.FormatInt(colStart(c+1), 10), "count": strconv.Itoa(len(d)),
			"p50": fms(percentile(d, 50)), "p90": fms(percentile(d, 90)), "p95": fms(percentile(d, 95)), "p99": fms(percentile(d, 99)),
			"okRate": strconv.FormatFloat(float64(len(d)-errs[c])/secs, 'g', 6, 64), "errorRate": strconv.FormatFloat(float64(errs[c])/secs, 'g', 6, 64)}})
	}
	edges := make([]string, len(heatmapLatencyEdges))
	for i, e := range heatmapLatencyEdges {
		edges[i] = fms(e)
	}
	out.Note = strings.Join(edges, ",")
	return out, nil
}

// grouped is the Grouped table: "The Grouped table groups spans by their
// name and by service or workload name", each row with its latency
// statistics. Its columns are UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/finding-traces names only
// average latency): the span count, errors, the average and the 50th, 95th
// and 99th percentiles; the row's slowest span opens its trace.
func (p traceExplorerProvider) grouped(a *trace.Server, project string, values map[string]string, now time.Time) (console.Listing, error) {
	m, err := p.match(a, project, values, now)
	if err != nil {
		return console.Listing{}, err
	}
	type group struct {
		service, name string
		ms            []float64
		errors        int
		slowest       *trace.Span
	}
	groups := map[[2]string]*group{}
	for _, sp := range m.spans {
		k := [2]string{spanService(sp), sp.Name()}
		g := groups[k]
		if g == nil {
			g = &group{service: k[0], name: k[1]}
			groups[k] = g
		}
		d := sp.End().Sub(sp.Start())
		g.ms = append(g.ms, float64(d)/float64(time.Millisecond))
		if spanStatus(sp) == "Error" {
			g.errors++
		}
		if g.slowest == nil || d > g.slowest.End().Sub(g.slowest.Start()) {
			g.slowest = sp
		}
	}
	out := console.Listing{NameColumn: "Span name", Noun: "span groups", Total: len(groups),
		Columns: []string{"Service/workload", "Spans", "Errors", "Average (ms)", "P50 (ms)", "P95 (ms)", "P99 (ms)"}}
	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].ms) != len(list[j].ms) {
			return len(list[i].ms) > len(list[j].ms)
		}
		return list[i].service+"\x00"+list[i].name < list[j].service+"\x00"+list[j].name
	})
	for _, g := range list {
		sort.Float64s(g.ms)
		sum := 0.0
		for _, v := range g.ms {
			sum += v
		}
		out.Items = append(out.Items, console.Resource{Name: g.name, Fields: map[string]string{
			"Service/workload": orDash(g.service), "Spans": strconv.Itoa(len(g.ms)), "Errors": strconv.Itoa(g.errors),
			"Average (ms)": fms(sum / float64(len(g.ms))), "P50 (ms)": fms(percentile(g.ms, 50)),
			"P95 (ms)": fms(percentile(g.ms, 95)), "P99 (ms)": fms(percentile(g.ms, 99)),
			"Trace ID": g.slowest.TraceID, "Project": g.slowest.Project, "Span ID": g.slowest.SpanID}})
	}
	out.Note = fmt.Sprintf("trace scope %s: %s", m.scope, strings.Join(m.projects, ", "))
	return out, nil
}

// traceEvent is one row of a span's Logs & Events tab.
type traceEvent struct {
	Kind     string `json:"kind"` // "log" or "event"
	Time     string `json:"time"`
	Severity string `json:"severity,omitempty"`
	Text     string `json:"text"`
	Log      string `json:"log,omitempty"`
}

func entryText(e *loggingpb.LogEntry) string {
	if t := e.GetTextPayload(); t != "" {
		return t
	}
	if j := e.GetJsonPayload(); j != nil {
		if m, ok := j.GetFields()["message"]; ok && m.GetStringValue() != "" {
			return m.GetStringValue()
		}
		if b, err := protojson.Marshal(j); err == nil {
			return string(b)
		}
	}
	return ""
}

// traceLogs are the log entries of a trace, by span ID ("" for none).
func (p traceExplorerProvider) traceLogs(project, traceID string) map[string][]traceEvent {
	out := map[string][]traceEvent{}
	if p.logging == nil || p.logging.api == nil {
		return out
	}
	want := "projects/" + project + "/traces/" + traceID
	for _, e := range p.logging.api.ProjectEntries(project, time.Time{}, time.Now().Add(24*time.Hour)) {
		if e.GetTrace() != want && e.GetTrace() != traceID {
			continue
		}
		out[strings.ToLower(e.GetSpanId())] = append(out[strings.ToLower(e.GetSpanId())], traceEvent{Kind: "log",
			Time: e.GetTimestamp().AsTime().UTC().Format(time.RFC3339Nano), Severity: e.GetSeverity().String(),
			Text: entryText(e), Log: e.GetLogName()})
	}
	return out
}

func (p traceExplorerProvider) details(a *trace.Server, project string, values map[string]string) (console.Listing, error) {
	id := strings.ToLower(strings.TrimSpace(values["trace"]))
	if len(id) != 32 {
		return console.Listing{}, fmt.Errorf("trace ID %q is not 32 hexadecimal characters", values["trace"])
	}
	projects := []string{project}
	if tp := values["traceProject"]; tp != "" {
		projects = []string{tp}
	} else if ps, _, err := p.scopeProjects(project, values["scope"]); err == nil {
		projects = ps
	}
	var t trace.Trace
	found := false
	for _, pr := range projects {
		if t, found = a.Get(pr, id); found {
			break
		}
	}
	if !found {
		return console.Listing{}, fmt.Errorf("trace %s was not found in %s", id, strings.Join(projects, ", "))
	}
	logs := p.traceLogs(t.Project, id)
	root := t.Root()
	// The timeline: depth first from the root, children by start time.
	children := map[string][]*trace.Span{}
	ids := map[string]bool{}
	for _, sp := range t.Spans {
		ids[sp.SpanID] = true
	}
	var tops []*trace.Span
	for _, sp := range t.Spans {
		if par := sp.V2.GetParentSpanId(); par != "" && ids[par] {
			children[par] = append(children[par], sp)
		} else {
			tops = append(tops, sp)
		}
	}
	byStart := func(ss []*trace.Span) {
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].Start().Before(ss[j].Start()) })
	}
	byStart(tops)
	start := t.Start()
	out := console.Listing{NameColumn: "Name", Noun: "spans",
		Columns: []string{"Service", "Start (ms)", "Duration (ms)", "Status", "Span kind", "Span ID"}}
	var walk func(sp *trace.Span, depth int)
	seen := map[*trace.Span]bool{}
	walk = func(sp *trace.Span, depth int) {
		if seen[sp] {
			return
		}
		seen[sp] = true
		attrs := map[string]string{}
		for k, v := range sp.V2.GetAttributes().GetAttributeMap() {
			attrs[k] = trace.AttrString(v)
		}
		// Never nil: the page reads "events" as a JSON array.
		evs := append([]traceEvent{}, logs[sp.SpanID]...)
		if sp == root {
			evs = append(evs, logs[""]...)
		}
		for _, te := range sp.V2.GetTimeEvents().GetTimeEvent() {
			if ann := te.GetAnnotation(); ann != nil {
				evs = append(evs, traceEvent{Kind: "event", Time: te.GetTime().AsTime().UTC().Format(time.RFC3339Nano), Text: ann.GetDescription().GetValue()})
			}
		}
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time < evs[j].Time })
		attrJSON, _ := json.Marshal(attrs)
		evJSON, _ := json.Marshal(evs)
		var links []string
		for _, l := range sp.V2.GetLinks().GetLink() {
			links = append(links, l.GetTraceId()+"/"+l.GetSpanId())
		}
		status := spanStatus(sp)
		if st := sp.V2.GetStatus(); st != nil && st.GetMessage() != "" {
			status += ": " + st.GetMessage()
		}
		out.Items = append(out.Items, console.Resource{Name: sp.Name(), Fields: map[string]string{
			"Service": orDash(spanService(sp)), "Start (ms)": durationMs(sp.Start().Sub(start)),
			"Duration (ms)": durationMs(sp.End().Sub(sp.Start())), "Status": status, "Span kind": spanKind(sp),
			"Span ID": sp.SpanID, "Parent span ID": orDash(sp.V2.GetParentSpanId()), "depth": strconv.Itoa(depth),
			"Project": sp.Project, "Trace ID": sp.TraceID,
			"Start time": sp.Start().UTC().Format(time.RFC3339Nano), "End time": sp.End().UTC().Format(time.RFC3339Nano),
			"Instrumentation scope": orDash(attrs["otel.scope.name"]), "Links": orDash(strings.Join(links, ", ")),
			"attributes": string(attrJSON), "events": string(evJSON)}})
		kids := children[sp.SpanID]
		byStart(kids)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	for _, sp := range tops {
		walk(sp, 0)
	}
	out.Total = len(out.Items)
	out.Note = fmt.Sprintf("trace %s in project %s: %d spans, %s ms", id, t.Project, len(t.Spans), durationMs(t.End().Sub(start)))
	return out, nil
}
