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

// Build runs the explorer: ["spans"] is the Spans table and chart of a
// search; ["trace"] is one trace's details.
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
	case len(path) == 1 && path[0] == "trace":
		return p.details(a, project, values)
	}
	return console.Listing{}, fmt.Errorf("Trace explorer has no query at %v", path)
}

func (p traceExplorerProvider) spans(a *trace.Server, project string, values map[string]string, now time.Time) (console.Listing, error) {
	projects, scopeName, err := p.scopeProjects(project, values["scope"])
	if err != nil {
		return console.Listing{}, err
	}
	f, err := trace.ParseFilter(values["filter"])
	if err != nil {
		return console.Listing{}, err
	}
	q := trace.Query{Projects: projects, Filter: f}
	if r, err := strconv.Atoi(values["range"]); err == nil && r > 0 {
		q.Start = now.Add(-time.Duration(r) * time.Second)
	} else if values["range"] != "" {
		return console.Listing{}, fmt.Errorf("time range %q is not a number of seconds", values["range"])
	}
	var minDur time.Duration
	if v := values["minDuration"]; v != "" {
		ms, err := strconv.ParseFloat(v, 64)
		if err != nil || ms < 0 {
			return console.Listing{}, fmt.Errorf("minimum duration %q is not a number of milliseconds", v)
		}
		minDur = time.Duration(ms * float64(time.Millisecond))
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
			if sp.End().Sub(sp.Start()) < minDur {
				continue
			}
			spans = append(spans, sp)
		}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start().After(spans[j].Start()) })
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
	out.Note = fmt.Sprintf("trace scope %s: %s", scopeName, strings.Join(projects, ", "))
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
