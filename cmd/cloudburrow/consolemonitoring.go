package main

// Cloud Monitoring's console pages (#1939), over the Monitoring API v3
// server this process runs, called in process:
//
//   - Metrics explorer (/monitoring/metrics-explorer), in the layout of
//     Google's builder (docs.cloud.google.com/monitoring/charts/metrics-explorer):
//     Select a metric (the resource type, then the metric), Filter,
//     Aggregation (the per-series aligner and its alignment period, the
//     cross-series reducer and the labels it groups by) and the time range,
//     drawn as a line chart with the table of its series below. Each run
//     is one ListTimeSeries. Its PromQL tab (#1991) runs a range query
//     over the same series (consolemonitoringpromql.go).
//   - Metrics management (/monitoring/metrics-management)
//     (docs.cloud.google.com/monitoring/metrics-management): the project's
//     metric descriptors, Create for a user-defined metric
//     (custom.googleapis.com) and Delete for one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const monitoringNotStarted = "Cloud Monitoring has not started yet"

// monitoringAPIOf is the instance's MetricService, or nil before it starts.
func monitoringAPIOf(svc *monitoringService) func() monitoringpb.MetricServiceServer {
	return func() monitoringpb.MetricServiceServer {
		if svc == nil {
			return nil
		}
		if a := svc.API(); a != nil {
			return a
		}
		return nil
	}
}

// monitoringScreen is what both pages share: the API they call.
type monitoringScreen struct {
	api func() monitoringpb.MetricServiceServer
}

func (s monitoringScreen) server() (monitoringpb.MetricServiceServer, error) {
	if s.api == nil {
		return nil, errors.New(monitoringNotStarted)
	}
	a := s.api()
	if a == nil {
		return nil, errors.New(monitoringNotStarted)
	}
	return a, nil
}

// descriptors is every metric descriptor of a project, by type.
func (s monitoringScreen) descriptors(ctx context.Context, project string) ([]*metricpb.MetricDescriptor, error) {
	a, err := s.server()
	if err != nil {
		return nil, err
	}
	var out []*metricpb.MetricDescriptor
	token := ""
	for range 100 {
		resp, err := a.ListMetricDescriptors(ctx, &monitoringpb.ListMetricDescriptorsRequest{Name: "projects/" + project, PageSize: 1000, PageToken: token})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetMetricDescriptors()...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetType() < out[j].GetType() })
	return out, nil
}

// userDefined is whether a metric type is one a project defines, which is
// what DeleteMetricDescriptor accepts.
func userDefined(metricType string) bool {
	return strings.HasPrefix(metricType, "custom.googleapis.com/") || strings.HasPrefix(metricType, "external.googleapis.com/") ||
		strings.HasPrefix(metricType, "workload.googleapis.com/")
}

func labelKeys(ls []*label.LabelDescriptor) string {
	var keys []string
	for _, l := range ls {
		keys = append(keys, l.GetKey())
	}
	return strings.Join(keys, ", ")
}

// ---- Metrics management ----

type monitoringMetricsProvider struct{ monitoringScreen }

func (monitoringMetricsProvider) ID() string    { return "monitoring-metrics" }
func (monitoringMetricsProvider) Title() string { return "Metrics management" }

func (p monitoringMetricsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Metric kind", "Value type", "Unit", "Labels", "Description"},
		NameColumn: "Metric", Noun: "metric descriptors",
		Note: "User-defined metrics (custom.googleapis.com) can be created and deleted here; the built-in metrics are the ones this instance measures."}
	if _, err := p.server(); err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Cloud Monitoring lists metric descriptors per project. Choose one in the toolbar."
		return out, nil
	}
	ds, err := p.descriptors(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, d := range ds {
		out.Items = append(out.Items, console.Resource{Name: d.GetType(), Fields: map[string]string{
			"Display name": orDash(d.GetDisplayName()), "Metric kind": d.GetMetricKind().String(), "Value type": d.GetValueType().String(),
			"Unit": orDash(d.GetUnit()), "Labels": orDash(labelKeys(d.GetLabels())), "Description": orDash(d.GetDescription()),
		}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// metricTypePattern is the part of a user-defined metric's type after
// custom.googleapis.com/: path segments of letters, digits, _ and -, as
// Google's naming rules for user-defined metrics allow.
const metricTypePattern = `^[A-Za-z0-9_][A-Za-z0-9_.\/\-]{0,170}$`

func (monitoringMetricsProvider) CreateForm() (string, []console.Field) {
	return "Create metric", []console.Field{
		{Name: "type", Label: "Metric type", Type: "text", Required: true, Pattern: metricTypePattern,
			Help: "After custom.googleapis.com/, such as store/daily_sales."},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "metricKind", Label: "Metric kind", Type: "select", Required: true, Default: "GAUGE", Options: []string{"GAUGE", "CUMULATIVE"},
			Help: "User-defined metrics are GAUGE or CUMULATIVE."},
		{Name: "valueType", Label: "Value type", Type: "select", Required: true, Default: "DOUBLE",
			Options: []string{"BOOL", "INT64", "DOUBLE", "STRING", "DISTRIBUTION"},
			Help:    "BOOL and STRING metrics must be GAUGE."},
		{Name: "unit", Label: "Unit", Type: "text", Help: "UCUM, such as s, By or 1; empty for none."},
		{Name: "labels", Label: "Labels", Type: "map",
			Help: "Each label's key and its description. Keys start with a lowercase letter; values are strings."},
	}
}

func (p monitoringMetricsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	a, err := p.server()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a metric")
	}
	suffix := strings.TrimPrefix(strings.TrimSpace(values["type"]), "custom.googleapis.com/")
	if !regexp.MustCompile(metricTypePattern).MatchString(suffix) {
		return "", fmt.Errorf("metric type %q: use letters, digits, _, - and / after custom.googleapis.com/", suffix)
	}
	kind, ok := metricpb.MetricDescriptor_MetricKind_value[values["metricKind"]]
	if !ok {
		return "", fmt.Errorf("metric kind %q is not GAUGE or CUMULATIVE", values["metricKind"])
	}
	vt, ok := metricpb.MetricDescriptor_ValueType_value[values["valueType"]]
	if !ok {
		return "", fmt.Errorf("value type %q is not one of BOOL, INT64, DOUBLE, STRING and DISTRIBUTION", values["valueType"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	var keys []string
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lds []*label.LabelDescriptor
	for _, k := range keys {
		lds = append(lds, &label.LabelDescriptor{Key: k, ValueType: label.LabelDescriptor_STRING, Description: labels[k]})
	}
	t := "custom.googleapis.com/" + suffix
	d, err := a.CreateMetricDescriptor(ctx, &monitoringpb.CreateMetricDescriptorRequest{Name: "projects/" + project,
		MetricDescriptor: &metricpb.MetricDescriptor{Type: t, Name: "projects/" + project + "/metricDescriptors/" + t,
			DisplayName: strings.TrimSpace(values["displayName"]), Description: strings.TrimSpace(values["description"]),
			MetricKind: metricpb.MetricDescriptor_MetricKind(kind), ValueType: metricpb.MetricDescriptor_ValueType(vt),
			Unit: strings.TrimSpace(values["unit"]), Labels: lds}})
	if err != nil {
		return "", err
	}
	return d.GetType(), nil
}

// Delete removes a user-defined metric's descriptor; a built-in one is
// refused here, as Google's page offers no delete for it.
func (p monitoringMetricsProvider) Delete(ctx context.Context, project, name string) error {
	a, err := p.server()
	if err != nil {
		return err
	}
	if !userDefined(name) {
		return fmt.Errorf("%s is a built-in metric; only user-defined metrics can be deleted", name)
	}
	_, err = a.DeleteMetricDescriptor(ctx, &monitoringpb.DeleteMetricDescriptorRequest{Name: "projects/" + project + "/metricDescriptors/" + name})
	return err
}

func (monitoringMetricsProvider) DeleteLabel() string { return "Delete metric" }

// ---- Metrics explorer ----

type monitoringExplorerProvider struct{ monitoringScreen }

func (monitoringExplorerProvider) ID() string    { return "monitoring-explorer" }
func (monitoringExplorerProvider) Title() string { return "Metrics explorer" }

// List is the Select a metric menu: every metric of the project with the
// resource types it is written against and its labels.
func (p monitoringExplorerProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Metric kind", "Value type", "Unit", "Resource types", "Labels"},
		NameColumn: "Metric", Noun: "metrics"}
	if _, err := p.server(); err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Metrics explorer charts one project's metrics. Choose one in the toolbar."
		return out, nil
	}
	ds, err := p.descriptors(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, d := range ds {
		out.Items = append(out.Items, console.Resource{Name: d.GetType(), Fields: map[string]string{
			"Display name": orDash(d.GetDisplayName()), "Metric kind": d.GetMetricKind().String(), "Value type": d.GetValueType().String(),
			"Unit": d.GetUnit(), "Resource types": strings.Join(d.GetMonitoredResourceTypes(), ", "), "Labels": labelKeys(d.GetLabels()),
		}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// explorerAligners and explorerReducers are the Aggregation menus, in the
// API's names.
var (
	explorerAligners = []string{"ALIGN_NONE", "ALIGN_DELTA", "ALIGN_RATE", "ALIGN_INTERPOLATE", "ALIGN_NEXT_OLDER", "ALIGN_MIN", "ALIGN_MAX",
		"ALIGN_MEAN", "ALIGN_COUNT", "ALIGN_SUM", "ALIGN_STDDEV", "ALIGN_COUNT_TRUE", "ALIGN_COUNT_FALSE", "ALIGN_FRACTION_TRUE",
		"ALIGN_PERCENTILE_99", "ALIGN_PERCENTILE_95", "ALIGN_PERCENTILE_50", "ALIGN_PERCENTILE_05", "ALIGN_PERCENT_CHANGE"}
	explorerReducers = []string{"REDUCE_NONE", "REDUCE_MEAN", "REDUCE_MIN", "REDUCE_MAX", "REDUCE_SUM", "REDUCE_STDDEV", "REDUCE_COUNT",
		"REDUCE_COUNT_TRUE", "REDUCE_COUNT_FALSE", "REDUCE_FRACTION_TRUE", "REDUCE_PERCENTILE_99", "REDUCE_PERCENTILE_95",
		"REDUCE_PERCENTILE_50", "REDUCE_PERCENTILE_05"}
	// explorerRanges are the time range picker's windows, in seconds.
	explorerRanges = []string{"300", "900", "3600", "21600", "86400", "604800"}
)

// QueryForm is the builder's controls. The page draws them itself, in
// Google's layout; this is their contract.
func (monitoringExplorerProvider) QueryForm(path []string) (string, []console.Field) {
	if len(path) == 1 && path[0] == "promql" {
		return "Run query", promqlQueryFields()
	}
	if len(path) != 1 || path[0] != "time-series" {
		return "", nil
	}
	return "Apply", []console.Field{
		{Name: "resource", Label: "Resource type", Type: "text"},
		{Name: "metric", Label: "Metric", Type: "text", Required: true},
		{Name: "filter", Label: "Filter", Type: "text",
			Help: `ANDed terms in the Monitoring filter syntax, such as metric.labels.env = "prod".`},
		{Name: "aligner", Label: "Aligner", Type: "select", Options: explorerAligners, Default: "ALIGN_NONE"},
		{Name: "alignmentPeriod", Label: "Alignment period (seconds)", Type: "number", Default: "60"},
		{Name: "reducer", Label: "Reducer", Type: "select", Options: explorerReducers, Default: "REDUCE_NONE"},
		{Name: "groupBy", Label: "Group by", Type: "text", Help: "Comma separated, such as metric.labels.env, resource.labels.zone."},
		{Name: "range", Label: "Time range (seconds)", Type: "select", Options: explorerRanges, Default: "3600"},
	}
}

// explorerFilter is the ListTimeSeries filter the builder's choices make.
func explorerFilter(values map[string]string) (string, error) {
	metric := strings.TrimSpace(values["metric"])
	if metric == "" {
		return "", errors.New("select a metric")
	}
	if strings.ContainsAny(metric, `"\`) {
		return "", fmt.Errorf("metric type %q is not a metric type", metric)
	}
	terms := []string{fmt.Sprintf("metric.type = %q", metric)}
	if r := strings.TrimSpace(values["resource"]); r != "" {
		if strings.ContainsAny(r, `"\`) {
			return "", fmt.Errorf("resource type %q is not a resource type", r)
		}
		terms = append(terms, fmt.Sprintf("resource.type = %q", r))
	}
	if f := strings.TrimSpace(values["filter"]); f != "" {
		terms = append(terms, "("+f+")")
	}
	return strings.Join(terms, " AND "), nil
}

// explorerRequest is the ListTimeSeries call the builder makes, at now.
func explorerRequest(project string, values map[string]string, now time.Time) (*monitoringpb.ListTimeSeriesRequest, error) {
	filter, err := explorerFilter(values)
	if err != nil {
		return nil, err
	}
	rng := int64(3600)
	if v := strings.TrimSpace(values["range"]); v != "" {
		if rng, err = strconv.ParseInt(v, 10, 64); err != nil || rng <= 0 {
			return nil, fmt.Errorf("time range %q is not a number of seconds", v)
		}
	}
	req := &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: filter, PageSize: 1000,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-time.Duration(rng) * time.Second)), EndTime: timestamppb.New(now)},
		View:     monitoringpb.ListTimeSeriesRequest_FULL}
	aligner := values["aligner"]
	reducer := values["reducer"]
	if aligner == "" {
		aligner = "ALIGN_NONE"
	}
	if reducer == "" {
		reducer = "REDUCE_NONE"
	}
	al, ok := monitoringpb.Aggregation_Aligner_value[aligner]
	if !ok {
		return nil, fmt.Errorf("aligner %q is not one of %s", aligner, strings.Join(explorerAligners, ", "))
	}
	rd, ok := monitoringpb.Aggregation_Reducer_value[reducer]
	if !ok {
		return nil, fmt.Errorf("reducer %q is not one of %s", reducer, strings.Join(explorerReducers, ", "))
	}
	var groupBy []string
	for _, g := range strings.Split(values["groupBy"], ",") {
		if g = strings.TrimSpace(g); g != "" {
			groupBy = append(groupBy, g)
		}
	}
	if al != 0 || rd != 0 || len(groupBy) > 0 {
		period := int64(60)
		if v := strings.TrimSpace(values["alignmentPeriod"]); v != "" {
			if period, err = strconv.ParseInt(v, 10, 64); err != nil || period <= 0 {
				return nil, fmt.Errorf("alignment period %q is not a number of seconds", v)
			}
		}
		req.Aggregation = &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(time.Duration(period) * time.Second),
			PerSeriesAligner: monitoringpb.Aggregation_Aligner(al), CrossSeriesReducer: monitoringpb.Aggregation_Reducer(rd),
			GroupByFields: groupBy}
	}
	return req, nil
}

// pointValue is a point's number for the chart, and its text for the
// table: a distribution charts its mean, a string charts nothing.
func pointValue(v *monitoringpb.TypedValue) (float64, string) {
	switch x := v.GetValue().(type) {
	case *monitoringpb.TypedValue_BoolValue:
		if x.BoolValue {
			return 1, "true"
		}
		return 0, "false"
	case *monitoringpb.TypedValue_Int64Value:
		return float64(x.Int64Value), strconv.FormatInt(x.Int64Value, 10)
	case *monitoringpb.TypedValue_DoubleValue:
		return x.DoubleValue, strconv.FormatFloat(x.DoubleValue, 'g', 6, 64)
	case *monitoringpb.TypedValue_StringValue:
		return math.NaN(), x.StringValue
	case *monitoringpb.TypedValue_DistributionValue:
		d := x.DistributionValue
		return d.GetMean(), fmt.Sprintf("count %d, mean %s", d.GetCount(), strconv.FormatFloat(d.GetMean(), 'g', 6, 64))
	}
	return math.NaN(), "—"
}

// explorerPoint is one point as the page charts it: the end of its
// interval and its value, null for one with no number.
type explorerPoint struct {
	At    string   `json:"at"`
	Value *float64 `json:"value"`
}

// Build runs the builder: path ["time-series"] is the chart's
// ListTimeSeries, one row per series with its labels, its latest value and
// its points (oldest first) for the chart; ["resource-types"] is the
// Select a metric menu's resource types.
func (p monitoringExplorerProvider) Build(ctx context.Context, project string, path []string, values map[string]string) (console.Listing, error) {
	a, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		return console.Listing{Prompt: "Choose a project in the toolbar."}, nil
	}
	if len(path) == 1 && path[0] == "resource-types" {
		return p.resourceTypes(ctx, a, project)
	}
	if len(path) == 1 && path[0] == "promql" {
		return promqlListing(ctx, a, project, values, time.Now())
	}
	if len(path) != 1 || path[0] != "time-series" {
		return console.Listing{}, fmt.Errorf("Metrics explorer has no query at %v", path)
	}
	req, err := explorerRequest(project, values, time.Now())
	if err != nil {
		return console.Listing{}, err
	}
	var series []*monitoringpb.TimeSeries
	for range 20 {
		resp, err := a.ListTimeSeries(ctx, req)
		if err != nil {
			return console.Listing{}, err
		}
		series = append(series, resp.GetTimeSeries()...)
		if req.PageToken = resp.GetNextPageToken(); req.PageToken == "" {
			break
		}
	}
	cols := map[string]bool{}
	out := console.Listing{NameColumn: "Time series", Noun: "time series"}
	for _, ts := range series {
		fields := map[string]string{}
		var parts []string
		for k, v := range ts.GetResource().GetLabels() {
			key := "resource.labels." + k
			fields[key], cols[key] = v, true
		}
		for k, v := range ts.GetMetric().GetLabels() {
			key := "metric.labels." + k
			fields[key], cols[key] = v, true
		}
		var pts []explorerPoint
		pp := ts.GetPoints()
		// The API answers newest first; the chart draws oldest first.
		for i := len(pp) - 1; i >= 0; i-- {
			n, _ := pointValue(pp[i].GetValue())
			pt := explorerPoint{At: pp[i].GetInterval().GetEndTime().AsTime().UTC().Format(time.RFC3339Nano)}
			if !math.IsNaN(n) && !math.IsInf(n, 0) {
				pt.Value = &n
			}
			pts = append(pts, pt)
		}
		fields["Value"] = "—"
		if len(pp) > 0 {
			_, fields["Value"] = pointValue(pp[0].GetValue())
		}
		b, err := json.Marshal(pts)
		if err != nil {
			return console.Listing{}, err
		}
		fields["points"] = string(b)
		fields["Metric kind"], fields["Value type"] = ts.GetMetricKind().String(), ts.GetValueType().String()
		keys := make([]string, 0, len(fields))
		for k := range fields {
			if strings.Contains(k, ".labels.") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k[strings.LastIndex(k, ".")+1:]+"="+fields[k])
		}
		name := ts.GetMetric().GetType()
		if len(parts) > 0 {
			name = strings.Join(parts, ", ")
		}
		out.Items = append(out.Items, console.Resource{Name: name, Fields: fields})
	}
	for k := range cols {
		out.Columns = append(out.Columns, k)
	}
	sort.Strings(out.Columns)
	out.Columns = append(out.Columns, "Value")
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	out.Note = "filter: " + req.GetFilter()
	return out, nil
}

func (p monitoringExplorerProvider) resourceTypes(ctx context.Context, a monitoringpb.MetricServiceServer, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Labels"}, NameColumn: "Resource type", Noun: "resource types"}
	token := ""
	for range 100 {
		resp, err := a.ListMonitoredResourceDescriptors(ctx, &monitoringpb.ListMonitoredResourceDescriptorsRequest{Name: "projects/" + project, PageSize: 1000, PageToken: token})
		if err != nil {
			return console.Listing{}, err
		}
		for _, d := range resp.GetResourceDescriptors() {
			out.Items = append(out.Items, console.Resource{Name: d.GetType(), Fields: map[string]string{
				"Display name": orDash(d.GetDisplayName()), "Labels": labelKeys(d.GetLabels())}})
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}
