package monitoring

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Monitoring Query Language (#1966). Google deprecated MQL: customer
// support ended on July 22, 2025 and the console no longer creates MQL
// charts or alerting policies, but "MQL is not shut down" and the
// Monitoring API still runs MQL queries and creates MQL alerting policies
// (docs.cloud.google.com/stackdriver/docs/deprecations/mql and
// docs.cloud.google.com/monitoring/mql, read 2026-11-20). So
// QueryService.QueryTimeSeries and conditionMonitoringQueryLanguage are
// served, for the core of the language
// (docs.cloud.google.com/monitoring/mql/reference):
//
//	fetch RESOURCE[::METRIC] | metric 'METRIC'
//	| filter EXPR            (==, !=, =~, !~, &&, ||, !, parentheses)
//	| align ALIGNER[(PERIOD)] (rate, delta, mean_aligner, next_older, ...)
//	| every PERIOD | within DURATION
//	| group_by [COLUMNS][, REDUCER(val())] | group_by PERIOD[, REDUCER(val())]
//	| condition val() OP NUMBER ['UNIT']
//
// A query is run as the ListTimeSeries read it describes. Other table
// operations (join, value, top, union, ...) answer UNIMPLEMENTED.

// mqlQuery is a parsed query.
type mqlQuery struct {
	resource, metric string
	filters          []string
	aligner          monitoringpb.Aggregation_Aligner
	period           time.Duration
	within           time.Duration
	reducer          monitoringpb.Aggregation_Reducer
	groupBy          []string
	grouped          bool
	cond             *mqlCondition
}

type mqlCondition struct {
	cmp       monitoringpb.ComparisonType
	threshold float64
}

var mqlAligners = map[string]monitoringpb.Aggregation_Aligner{
	"rate": monitoringpb.Aggregation_ALIGN_RATE, "delta": monitoringpb.Aggregation_ALIGN_DELTA,
	"mean_aligner": monitoringpb.Aggregation_ALIGN_MEAN, "next_older": monitoringpb.Aggregation_ALIGN_NEXT_OLDER,
	"interpolate": monitoringpb.Aggregation_ALIGN_INTERPOLATE, "min_aligner": monitoringpb.Aggregation_ALIGN_MIN,
	"max_aligner": monitoringpb.Aggregation_ALIGN_MAX, "sum_aligner": monitoringpb.Aggregation_ALIGN_SUM,
	"count_aligner": monitoringpb.Aggregation_ALIGN_COUNT, "count_true_aligner": monitoringpb.Aggregation_ALIGN_COUNT_TRUE,
	"fraction_true_aligner": monitoringpb.Aggregation_ALIGN_FRACTION_TRUE,
}

var mqlReducers = map[string]monitoringpb.Aggregation_Reducer{
	"sum": monitoringpb.Aggregation_REDUCE_SUM, "mean": monitoringpb.Aggregation_REDUCE_MEAN,
	"max": monitoringpb.Aggregation_REDUCE_MAX, "min": monitoringpb.Aggregation_REDUCE_MIN,
	"count": monitoringpb.Aggregation_REDUCE_COUNT, "stddev": monitoringpb.Aggregation_REDUCE_STDDEV,
	"count_true": monitoringpb.Aggregation_REDUCE_COUNT_TRUE, "fraction_true": monitoringpb.Aggregation_REDUCE_FRACTION_TRUE,
	"percentile_99": monitoringpb.Aggregation_REDUCE_PERCENTILE_99, "percentile_95": monitoringpb.Aggregation_REDUCE_PERCENTILE_95,
	"percentile_50": monitoringpb.Aggregation_REDUCE_PERCENTILE_50, "percentile_05": monitoringpb.Aggregation_REDUCE_PERCENTILE_05,
}

func mqlError(q, why string, a ...any) error {
	return apierror.InvalidArgument("Query %q is not valid MQL: %s", q, fmt.Sprintf(why, a...))
}

// splitPipes splits a query at the | outside quotes.
func splitPipes(q string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	for _, r := range q {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '|':
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	return append(out, strings.TrimSpace(b.String()))
}

// mqlDuration reads 1m, 30s, 1h, 1d, 1w.
func mqlDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, false
	}
	unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
	for _, u := range []string{"ms", "s", "m", "h", "d", "w"} {
		if n, ok := strings.CutSuffix(s, u); ok {
			if f, err := strconv.ParseFloat(n, 64); err == nil && f > 0 {
				return time.Duration(f * float64(unit[u])), true
			}
		}
	}
	return 0, false
}

func mqlUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func parseMQL(q string) (*mqlQuery, error) {
	if strings.TrimSpace(q) == "" {
		return nil, apierror.InvalidArgument("Field query is required")
	}
	out := &mqlQuery{}
	for i, stage := range splitPipes(q) {
		op, arg, _ := strings.Cut(stage, " ")
		arg = strings.TrimSpace(arg)
		if i == 0 && op != "fetch" {
			return nil, mqlError(q, "a query starts with fetch")
		}
		switch op {
		case "fetch", "t":
			if i != 0 {
				return nil, mqlError(q, "fetch must be the first table operation")
			}
			res, m, ok := strings.Cut(arg, "::")
			out.resource = strings.TrimSpace(res)
			if ok {
				out.metric = mqlUnquote(m)
			}
			if out.resource == "" {
				return nil, mqlError(q, "fetch names a monitored resource type")
			}
		case "metric":
			out.metric = mqlUnquote(arg)
		case "filter":
			f, err := mqlFilter(q, arg)
			if err != nil {
				return nil, err
			}
			out.filters = append(out.filters, f)
		case "align":
			name, p, _ := strings.Cut(arg, "(")
			a, ok := mqlAligners[strings.TrimSpace(name)]
			if !ok {
				return nil, mqlError(q, "unknown aligner %q", name)
			}
			out.aligner = a
			if p = strings.TrimSuffix(strings.TrimSpace(p), ")"); p != "" {
				d, ok := mqlDuration(p)
				if !ok {
					return nil, mqlError(q, "bad alignment period %q", p)
				}
				out.period = d
			}
		case "every":
			d, ok := mqlDuration(arg)
			if !ok {
				return nil, mqlError(q, "bad period %q", arg)
			}
			out.period = d
		case "within":
			d, ok := mqlDuration(strings.Split(arg, ",")[0])
			if !ok {
				return nil, mqlError(q, "bad duration %q", arg)
			}
			out.within = d
		case "group_by":
			if err := out.parseGroupBy(q, arg); err != nil {
				return nil, err
			}
		case "condition":
			c, err := mqlParseCondition(q, arg)
			if err != nil {
				return nil, err
			}
			out.cond = c
		default:
			return nil, apierror.Unimplemented("The MQL table operation %q is not supported; use fetch, metric, filter, align, every, within, group_by and condition", op)
		}
	}
	if out.metric == "" {
		return nil, mqlError(q, "name a metric, as fetch RESOURCE::METRIC or | metric 'METRIC'")
	}
	return out, nil
}

func (m *mqlQuery) parseGroupBy(q, arg string) error {
	m.grouped = true
	rest := arg
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return mqlError(q, "unclosed [ in group_by")
		}
		for _, c := range strings.Split(rest[1:end], ",") {
			if c = strings.TrimSpace(c); c != "" {
				m.groupBy = append(m.groupBy, mqlColumn(c))
			}
		}
		rest = strings.TrimSpace(rest[end+1:])
	} else {
		p, r, _ := strings.Cut(rest, ",")
		d, ok := mqlDuration(p)
		if !ok {
			return mqlError(q, "group_by takes [COLUMNS] or a period")
		}
		m.period = d
		m.grouped = false
		if m.aligner == monitoringpb.Aggregation_ALIGN_NONE {
			m.aligner = monitoringpb.Aggregation_ALIGN_MEAN
		}
		rest = strings.TrimSpace(r)
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, ","))
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]")
	if _, after, ok := strings.Cut(rest, ":"); ok && !strings.Contains(rest[:strings.Index(rest, ":")], "(") {
		rest = after
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		m.reducer = monitoringpb.Aggregation_REDUCE_MEAN
		return nil
	}
	name, _, _ := strings.Cut(rest, "(")
	name = strings.TrimSpace(name)
	if !m.grouped {
		// A temporal group_by's function is the aligner.
		if a, ok := mqlAligners[name+"_aligner"]; ok {
			m.aligner = a
			return nil
		}
		if name == "mean" {
			m.aligner = monitoringpb.Aggregation_ALIGN_MEAN
			return nil
		}
		return mqlError(q, "unknown function %q", name)
	}
	r, ok := mqlReducers[name]
	if !ok {
		return mqlError(q, "unknown reducer %q", name)
	}
	m.reducer = r
	return nil
}

// mqlColumn is a column as a Cloud Monitoring filter or group-by field:
// resource.X and metric.X are labels; a bare name is a resource label.
func mqlColumn(c string) string {
	c = strings.TrimSpace(c)
	switch {
	case c == "resource.type" || c == "metric.type":
		return c
	case strings.HasPrefix(c, "resource."):
		return "resource.label." + strings.TrimPrefix(c, "resource.")
	case strings.HasPrefix(c, "metric."):
		return "metric.label." + strings.TrimPrefix(c, "metric.")
	}
	return "resource.label." + c
}

// mqlFilter translates an MQL filter expression to a Cloud Monitoring
// filter.
func mqlFilter(q, expr string) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(expr) {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')':
			b.WriteByte(' ')
			b.WriteByte(c)
			b.WriteByte(' ')
			i++
		case strings.HasPrefix(expr[i:], "&&"):
			b.WriteString(" AND ")
			i += 2
		case strings.HasPrefix(expr[i:], "||"):
			b.WriteString(" OR ")
			i += 2
		case c == '!' && !strings.HasPrefix(expr[i:], "!=") && !strings.HasPrefix(expr[i:], "!~"):
			b.WriteString(" NOT ")
			i++
		default:
			// column OP value
			j := i
			for j < len(expr) && (isIdent(expr[j]) || expr[j] == '.') {
				j++
			}
			col := expr[i:j]
			if col == "" {
				return "", mqlError(q, "unexpected %q in filter", expr[i:])
			}
			k := j
			for k < len(expr) && expr[k] == ' ' {
				k++
			}
			var op string
			for _, o := range []string{"==", "!=", "=~", "!~"} {
				if strings.HasPrefix(expr[k:], o) {
					op = o
				}
			}
			if op == "" {
				return "", mqlError(q, "expected ==, !=, =~ or !~ after %s", col)
			}
			k += 2
			for k < len(expr) && expr[k] == ' ' {
				k++
			}
			if k >= len(expr) || (expr[k] != '\'' && expr[k] != '"') {
				return "", mqlError(q, "expected a quoted string after %s %s", col, op)
			}
			end := strings.IndexByte(expr[k+1:], expr[k])
			if end < 0 {
				return "", mqlError(q, "unterminated string")
			}
			val := expr[k+1 : k+1+end]
			i = k + 2 + end
			sel := mqlColumn(col)
			v := strconv.Quote(val)
			switch op {
			case "==":
				fmt.Fprintf(&b, " %s = %s ", sel, v)
			case "!=":
				fmt.Fprintf(&b, " %s != %s ", sel, v)
			case "=~":
				fmt.Fprintf(&b, " %s = monitoring.regex.full_match(%s) ", sel, v)
			case "!~":
				fmt.Fprintf(&b, " NOT %s = monitoring.regex.full_match(%s) ", sel, v)
			}
		}
	}
	return "(" + strings.Join(strings.Fields(b.String()), " ") + ")", nil
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func mqlParseCondition(q, arg string) (*mqlCondition, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "val()")
	if !ok {
		return nil, apierror.Unimplemented("MQL conditions other than val() OP NUMBER are not supported")
	}
	rest = strings.TrimSpace(rest)
	ops := []struct {
		s string
		c monitoringpb.ComparisonType
	}{{">=", monitoringpb.ComparisonType_COMPARISON_GE}, {"<=", monitoringpb.ComparisonType_COMPARISON_LE},
		{"==", monitoringpb.ComparisonType_COMPARISON_EQ}, {"!=", monitoringpb.ComparisonType_COMPARISON_NE},
		{">", monitoringpb.ComparisonType_COMPARISON_GT}, {"<", monitoringpb.ComparisonType_COMPARISON_LT}}
	for _, o := range ops {
		if n, ok := strings.CutPrefix(rest, o.s); ok {
			f := strings.Fields(n)
			if len(f) == 0 {
				return nil, mqlError(q, "condition needs a number")
			}
			v, err := strconv.ParseFloat(f[0], 64)
			if err != nil {
				return nil, mqlError(q, "condition needs a number, not %q", f[0])
			}
			return &mqlCondition{cmp: o.c, threshold: v}, nil
		}
	}
	return nil, mqlError(q, "condition needs a comparison")
}

// run reads the query's series over the window ending at end, newest
// point first.
func (s *Server) runMQL(ctx context.Context, project string, m *mqlQuery, end time.Time, defWindow time.Duration) ([]*monitoringpb.TimeSeries, *metricpb.MetricDescriptor, error) {
	s.mu.Lock()
	md, _ := s.lookup(project, m.metric)
	s.mu.Unlock()
	if md == nil {
		return nil, nil, apierror.InvalidArgument("Query is not valid MQL: the metric %q does not exist in project %s", m.metric, project)
	}
	filter := fmt.Sprintf("metric.type = %q AND resource.type = %q", m.metric, m.resource)
	for _, f := range m.filters {
		filter += " AND " + f
	}
	agg := &monitoringpb.Aggregation{PerSeriesAligner: m.aligner}
	period := m.period
	if period == 0 {
		period = time.Minute
	}
	if agg.PerSeriesAligner == monitoringpb.Aggregation_ALIGN_NONE && (m.grouped || m.period > 0) {
		agg.PerSeriesAligner = monitoringpb.Aggregation_ALIGN_MEAN
		if md.GetMetricKind() != metricpb.MetricDescriptor_GAUGE {
			agg.PerSeriesAligner = monitoringpb.Aggregation_ALIGN_DELTA
		}
	}
	if agg.PerSeriesAligner != monitoringpb.Aggregation_ALIGN_NONE {
		agg.AlignmentPeriod = durationpb.New(period)
	}
	if m.grouped {
		agg.CrossSeriesReducer, agg.GroupByFields = m.reducer, m.groupBy
	}
	window := m.within
	if window == 0 {
		window = defWindow
	}
	req := &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: filter, Aggregation: agg, PageSize: 100000,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(end.Add(-window)), EndTime: timestamppb.New(end)}}
	var out []*monitoringpb.TimeSeries
	for {
		resp, err := s.ListTimeSeries(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, resp.GetTimeSeries()...)
		if resp.GetNextPageToken() == "" {
			break
		}
		req.PageToken = resp.GetNextPageToken()
	}
	return out, md, nil
}

// QueryTimeSeries runs an MQL query (QueryService).
func (s *Server) QueryTimeSeries(ctx context.Context, req *monitoringpb.QueryTimeSeriesRequest) (*monitoringpb.QueryTimeSeriesResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	m, err := parseMQL(req.GetQuery())
	if err != nil {
		return nil, err
	}
	series, md, err := s.runMQL(ctx, project, m, s.now(), time.Hour)
	if err != nil {
		return nil, err
	}
	// Columns: every resource label, then every metric label, sorted.
	rkeys, mkeys := map[string]bool{}, map[string]bool{}
	for _, ts := range series {
		for k := range ts.GetResource().GetLabels() {
			rkeys[k] = true
		}
		for k := range ts.GetMetric().GetLabels() {
			mkeys[k] = true
		}
	}
	desc := &monitoringpb.TimeSeriesDescriptor{}
	var cols []func(*monitoringpb.TimeSeries) string
	for _, k := range sortedKeys(rkeys) {
		k := k
		desc.LabelDescriptors = append(desc.LabelDescriptors, &label.LabelDescriptor{Key: "resource." + k, ValueType: label.LabelDescriptor_STRING})
		cols = append(cols, func(ts *monitoringpb.TimeSeries) string { return ts.GetResource().GetLabels()[k] })
	}
	for _, k := range sortedKeys(mkeys) {
		k := k
		desc.LabelDescriptors = append(desc.LabelDescriptors, &label.LabelDescriptor{Key: "metric." + k, ValueType: label.LabelDescriptor_STRING})
		cols = append(cols, func(ts *monitoringpb.TimeSeries) string { return ts.GetMetric().GetLabels()[k] })
	}
	valueKey := "value." + md.GetType()[strings.LastIndex(md.GetType(), "/")+1:]
	vt, kind := md.GetValueType(), md.GetMetricKind()
	if len(series) > 0 {
		vt, kind = series[0].GetValueType(), series[0].GetMetricKind()
	}
	if m.cond != nil {
		valueKey, vt, kind = valueKey+"_condition", metricpb.MetricDescriptor_BOOL, metricpb.MetricDescriptor_GAUGE
	}
	desc.PointDescriptors = []*monitoringpb.TimeSeriesDescriptor_ValueDescriptor{{Key: valueKey, ValueType: vt, MetricKind: kind, Unit: md.GetUnit()}}
	sort.SliceStable(series, func(i, j int) bool { return seriesKey(series[i]) < seriesKey(series[j]) })
	keys := make([]string, len(series))
	for i, ts := range series {
		keys[i] = seriesKey(ts)
	}
	page, next, err := paging.Page("query|"+req.GetName()+"|"+req.GetQuery(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("Field pageToken had an invalid value: %v", err)
	}
	want := map[string]bool{}
	for _, k := range page {
		want[k] = true
	}
	resp := &monitoringpb.QueryTimeSeriesResponse{TimeSeriesDescriptor: desc, NextPageToken: next}
	for _, ts := range series {
		if !want[seriesKey(ts)] {
			continue
		}
		d := &monitoringpb.TimeSeriesData{}
		for _, c := range cols {
			d.LabelValues = append(d.LabelValues, &monitoringpb.LabelValue{Value: &monitoringpb.LabelValue_StringValue{StringValue: c(ts)}})
		}
		for _, p := range ts.GetPoints() {
			v := p.GetValue()
			if m.cond != nil {
				v = &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_BoolValue{BoolValue: compare(m.cond.cmp, valueOf(v), m.cond.threshold)}}
			}
			d.PointData = append(d.PointData, &monitoringpb.TimeSeriesData_PointData{Values: []*monitoringpb.TypedValue{v}, TimeInterval: p.GetInterval()})
		}
		resp.TimeSeriesData = append(resp.TimeSeriesData, d)
	}
	return resp, nil
}

// checkMQLCondition validates an MQL condition: it parses, and it ends in
// a condition (the query "outputs a boolean stream").
func checkMQLCondition(field func(string) string, c *monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition) error {
	m, err := parseMQL(c.GetQuery())
	if err != nil {
		e := apierror.From(err)
		if e.Code == codes.Unimplemented {
			return apierror.Unimplemented("Field %s: %s", field("conditionMonitoringQueryLanguage.query"), e.Message)
		}
		return apierror.InvalidArgument("Field %s had an invalid value: %s", field("conditionMonitoringQueryLanguage.query"), e.Message)
	}
	if m.cond == nil {
		return apierror.InvalidArgument("Field %s had an invalid value of %q: an alerting query must output a boolean stream; end it with | condition val() OP VALUE", field("conditionMonitoringQueryLanguage.query"), c.GetQuery())
	}
	if d := c.GetDuration().AsDuration(); d < 0 || d%time.Minute != 0 || d > 24*time.Hour {
		return apierror.InvalidArgument("Field %s had an invalid value of %q: the duration must be a whole number of minutes, at most 24 hours", field("conditionMonitoringQueryLanguage.duration"), d)
	}
	if t := c.GetTrigger(); t.GetCount() < 0 || t.GetPercent() < 0 || t.GetPercent() > 100 {
		return apierror.InvalidArgument("Field %s had an invalid value", field("conditionMonitoringQueryLanguage.trigger"))
	}
	return nil
}

// evalMQL evaluates an MQL condition as a threshold: a series violates it
// while its condition has been true for the duration.
func (s *Server) evalMQL(ctx context.Context, project string, c *monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition, now time.Time) ([]violation, bool) {
	m, err := parseMQL(c.GetQuery())
	if err != nil || m.cond == nil {
		return nil, false
	}
	dur := c.GetDuration().AsDuration()
	recent := max(m.period, staleAfter)
	series, _, err := s.runMQL(ctx, project, m, now, dur+recent)
	if err != nil {
		return nil, false
	}
	if len(series) == 0 {
		switch c.GetEvaluationMissingData() {
		case monitoringpb.AlertPolicy_Condition_EVALUATION_MISSING_DATA_ACTIVE:
			return []violation{{key: "missing", resourceKey: "missing", summary: "No data arrived for the condition, which treats missing data as violating"}}, true
		case monitoringpb.AlertPolicy_Condition_EVALUATION_MISSING_DATA_NO_OP:
			return nil, false
		}
		return nil, true
	}
	var out []violation
	for _, ts := range series {
		pts := ts.GetPoints()
		if len(pts) == 0 || now.Sub(endOf(pts[0])) > recent || !compare(m.cond.cmp, valueOf(pts[0].GetValue()), m.cond.threshold) {
			continue
		}
		since := endOf(pts[0])
		for _, p := range pts[1:] {
			if !compare(m.cond.cmp, valueOf(p.GetValue()), m.cond.threshold) {
				break
			}
			since = endOf(p)
		}
		if dur > 0 && now.Sub(since) < dur {
			continue
		}
		v := valueOf(pts[0].GetValue())
		out = append(out, seriesViolation(ts, fmt.Sprintf("%g", v), fmt.Sprintf("%g", m.cond.threshold),
			fmt.Sprintf("MQL condition on %s is %s %g (currently %g)", ts.GetMetric().GetType(), comparisonWords(m.cond.cmp), m.cond.threshold, v)))
	}
	return trigger(c.GetTrigger(), out, len(series)), true
}
