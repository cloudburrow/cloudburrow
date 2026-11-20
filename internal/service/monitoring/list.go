package monitoring

import (
	"context"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// ListTimeSeries reads the series of one metric type that a filter
// matches, in an interval, raw or aligned and reduced
// (docs.cloud.google.com/monitoring/api/v3/aggregation), each with its
// points newest first, or only their headers.
func (s *Server) ListTimeSeries(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	if strings.HasPrefix(req.GetName(), "organizations/") || strings.HasPrefix(req.GetName(), "folders/") {
		return nil, apierror.Unimplemented("Reading time series of an organization or a folder (%s) is not supported; read a project's", req.GetName())
	}
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetFilter()) == "" {
		return nil, apierror.InvalidArgument("Field filter had an invalid value of \"\": a filter must specify a metric type, as metric.type = \"TYPE\"")
	}
	if isSLOSelector(req.GetFilter()) {
		return s.sloTimeSeries(context.Background(), project, req)
	}
	f, err := parseFilter(req.GetFilter(), filterSeries)
	if err != nil {
		return nil, err
	}
	if f.metricType == "" {
		return nil, apierror.InvalidArgument("Field filter had an invalid value of %q: a filter must specify a single metric type, as metric.type = \"TYPE\"", req.GetFilter())
	}
	if req.GetOrderBy() != "" {
		return nil, apierror.InvalidArgument("Field orderBy had an invalid value of %q: orderBy is not supported and must be left blank", req.GetOrderBy())
	}
	iv := req.GetInterval()
	if iv.GetEndTime() == nil {
		return nil, apierror.InvalidArgument("Request was missing field interval.endTime")
	}
	end := iv.GetEndTime().AsTime()
	start := end
	if iv.GetStartTime() != nil {
		start = iv.GetStartTime().AsTime()
	}
	if start.After(end) {
		return nil, apierror.InvalidArgument("Field interval.endTime had an invalid value: the end time %s is before the start time %s", end.Format(rfc3339), start.Format(rfc3339))
	}
	agg, err := checkAggregation("aggregation", req.GetAggregation())
	if err != nil {
		return nil, err
	}
	second, err := checkAggregation("secondaryAggregation", req.GetSecondaryAggregation())
	if err != nil {
		return nil, err
	}
	if second.aligner != monitoringpb.Aggregation_ALIGN_NONE {
		return nil, apierror.Unimplemented("secondaryAggregation with a per-series aligner is not supported; give it a cross-series reducer only")
	}

	s.mu.Lock()
	md, builtin := s.lookup(project, f.metricType)
	var raw []*monitoringpb.TimeSeries
	readFrom := start.Add(-agg.period)
	if md != nil && !builtin {
		for _, sr := range s.series[project] {
			if sr.TS.GetMetric().GetType() == f.metricType {
				raw = append(raw, clone(sr.TS))
			}
		}
	}
	builtins := append([]Builtin(nil), s.builtins...)
	s.mu.Unlock()
	if md == nil {
		return nil, apierror.NotFound("Cannot find metric(s) that match type = %q. If a metric was created recently, it could take up to 10 minutes to become available. Please try again soon.", f.metricType)
	}
	if builtin {
		for _, b := range builtins {
			ts, err := b.Series(project, f.metricType, readFrom, end)
			if err != nil {
				return nil, err
			}
			raw = append(raw, ts...)
		}
	}
	// The filter, then the interval: a raw read keeps the points whose end
	// time is in [start, end]; an aligned one reads a period further back,
	// so the first window is whole.
	var matched []*monitoringpb.TimeSeries
	for _, ts := range raw {
		if !f.match(subject{project: project, metricType: ts.GetMetric().GetType(), resourceType: ts.GetResource().GetType(),
			metricLabels: ts.GetMetric().GetLabels(), resourceLabels: ts.GetResource().GetLabels()}) {
			continue
		}
		from := start
		if agg.aligner != monitoringpb.Aggregation_ALIGN_NONE {
			from = readFrom
		}
		var pts []*monitoringpb.Point
		for _, p := range ts.GetPoints() {
			e := p.GetInterval().GetEndTime().AsTime()
			if (e.After(from) || e.Equal(from)) && !e.After(end) {
				pts = append(pts, p)
			}
		}
		if len(pts) == 0 {
			continue
		}
		ts.Points = pts
		if ts.MetricKind == metricpb.MetricDescriptor_METRIC_KIND_UNSPECIFIED {
			ts.MetricKind = md.GetMetricKind()
		}
		if ts.ValueType == metricpb.MetricDescriptor_VALUE_TYPE_UNSPECIFIED {
			ts.ValueType = md.GetValueType()
		}
		if ts.Unit == "" {
			ts.Unit = md.GetUnit()
		}
		matched = append(matched, ts)
	}
	out, err := aggregate(matched, agg, md, start, end)
	if err != nil {
		return nil, err
	}
	if second.reducer != monitoringpb.Aggregation_REDUCE_NONE {
		second.aligner, second.period = agg.aligner, agg.period
		if out, err = reduce(out, second); err != nil {
			return nil, err
		}
	}
	// Newest first, as Google returns them.
	for _, ts := range out {
		sort.SliceStable(ts.Points, func(i, j int) bool {
			return ts.Points[i].GetInterval().GetEndTime().AsTime().After(ts.Points[j].GetInterval().GetEndTime().AsTime())
		})
	}
	byKey := map[string]*monitoringpb.TimeSeries{}
	keys := make([]string, 0, len(out))
	for _, ts := range out {
		k := seriesKey(ts)
		byKey[k] = ts
		keys = append(keys, k)
	}
	scope := "timeSeries|" + req.GetName() + "|" + req.GetFilter() + "|" + start.String() + "|" + end.String()
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("Field pageToken had an invalid value: %v", err)
	}
	resp := &monitoringpb.ListTimeSeriesResponse{NextPageToken: next, Unit: md.GetUnit()}
	for _, k := range page {
		ts := byKey[k]
		if req.GetView() == monitoringpb.ListTimeSeriesRequest_HEADERS {
			ts.Points = nil
		}
		resp.TimeSeries = append(resp.TimeSeries, ts)
	}
	return resp, nil
}

// aggregation is a checked Aggregation.
type aggregation struct {
	aligner monitoringpb.Aggregation_Aligner
	reducer monitoringpb.Aggregation_Reducer
	period  time.Duration
	groupBy []string
}

func checkAggregation(field string, a *monitoringpb.Aggregation) (aggregation, error) {
	out := aggregation{aligner: a.GetPerSeriesAligner(), reducer: a.GetCrossSeriesReducer(), groupBy: a.GetGroupByFields()}
	if a.GetAlignmentPeriod() != nil {
		out.period = a.GetAlignmentPeriod().AsDuration()
	}
	if field == "secondaryAggregation" {
		return out, nil
	}
	if out.reducer != monitoringpb.Aggregation_REDUCE_NONE && out.aligner == monitoringpb.Aggregation_ALIGN_NONE {
		return out, apierror.InvalidArgument("Field %s.perSeriesAligner had an invalid value of \"ALIGN_NONE\": a per-series aligner must be specified with a cross-series reducer", field)
	}
	if out.aligner != monitoringpb.Aggregation_ALIGN_NONE {
		if a.GetAlignmentPeriod() == nil {
			return out, apierror.InvalidArgument("Field %s.alignmentPeriod had an invalid value: an alignment period is required with the per-series aligner %s", field, out.aligner)
		}
		if out.period < 60*time.Second {
			return out, apierror.InvalidArgument("Field %s.alignmentPeriod had an invalid value of \"%s\": The alignment period must be at least 60 seconds", field, out.period)
		}
		if out.period > 104*7*24*time.Hour {
			return out, apierror.InvalidArgument("Field %s.alignmentPeriod had an invalid value of \"%s\": The alignment period must be at most 104 weeks", field, out.period)
		}
	} else {
		out.period = 0
	}
	for _, g := range out.groupBy {
		if _, _, ok := groupField(g); !ok {
			return out, apierror.InvalidArgument("Field %s.groupByFields had an invalid value of %q: group by metric.label.KEY, resource.label.KEY or resource.type", field, g)
		}
	}
	return out, nil
}

// groupField reads a group-by field: which labels it names and the key.
func groupField(g string) (scope, key string, ok bool) {
	for _, p := range []struct{ prefix, scope string }{
		{"metric.labels.", "metric"}, {"metric.label.", "metric"},
		{"resource.labels.", "resource"}, {"resource.label.", "resource"},
	} {
		if k, found := strings.CutPrefix(g, p.prefix); found && k != "" {
			k, _ = unquote(k)
			return p.scope, k, true
		}
	}
	if g == "resource.type" || g == "metric.type" {
		return "type", g, true
	}
	return "", "", false
}
