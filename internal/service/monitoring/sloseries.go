package monitoring

import (
	"context"
	"math"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/genproto/googleapis/type/calendarperiod"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// SLO time series (#1966): ListTimeSeries' select_slo_* filters
// (docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring/api/timeseries-selectors,
// read 2026-11-20), computed from the SLO's service-level indicator over
// the local series:
//
//   - select_slo_health: good / total in each alignment period;
//   - select_slo_compliance: good / total since the compliance period
//     began (the rolling period before each point, or the calendar period
//     it falls in, in UTC);
//   - select_slo_budget: the error budget left, (1 - goal) * total - bad,
//     in requests, or in seconds for a windows-based SLI;
//   - select_slo_budget_fraction: 1 - bad / ((1 - goal) * total), from
//     negative infinity to 1 (1 when there is nothing yet);
//   - select_slo_burn_rate(SLO, LOOKBACK): (bad / total over the lookback)
//     / (1 - goal);
//   - select_slo_counts: good and bad in each alignment period, as two
//     series labelled event_type.
//
// A request-based SLI counts requests: goodTotalRatio's filters aligned
// with ALIGN_DELTA and summed, or distributionCut's values in range. A
// windows-based SLI counts windowPeriod windows: good by goodBadMetricFilter
// (every point true), goodTotalRatioThreshold (request-based performance at
// or above the threshold), or metricMeanInRange / metricSumInRange. A
// window with no data is not counted. basicSli needs the automatic metrics
// of a Google-run service, which the instance does not produce, and is
// refused.

var sloSelectorRE = regexp.MustCompile(`^\s*(select_slo_(?:health|compliance|budget|budget_fraction|burn_rate|counts))\s*\(\s*"([^"]+)"\s*(?:,\s*"([^"]+)"\s*)?\)\s*$`)

// isSLOSelector is whether a filter is a select_slo_* selector.
func isSLOSelector(filter string) bool {
	return strings.HasPrefix(strings.TrimSpace(filter), "select_slo_")
}

// sloSelector is a parsed select_slo_* filter.
type sloSelector struct {
	fn, slo  string
	lookback time.Duration
}

func parseSLOSelector(filter string) (sloSelector, error) {
	m := sloSelectorRE.FindStringSubmatch(filter)
	if m == nil {
		return sloSelector{}, apierror.InvalidArgument("Field filter had an invalid value of %q: expected select_slo_health, select_slo_compliance, select_slo_budget, select_slo_budget_fraction, select_slo_counts(\"SLO_NAME\") or select_slo_burn_rate(\"SLO_NAME\", \"LOOKBACK\")", filter)
	}
	sel := sloSelector{fn: m[1], slo: m[2]}
	if err := sloName("filter", sel.slo); err != nil {
		return sel, err
	}
	switch {
	case sel.fn == "select_slo_burn_rate" && m[3] == "":
		return sel, apierror.InvalidArgument("Field filter had an invalid value of %q: select_slo_burn_rate takes a lookback period, as \"3600s\"", filter)
	case sel.fn != "select_slo_burn_rate" && m[3] != "":
		return sel, apierror.InvalidArgument("Field filter had an invalid value of %q: %s takes one argument", filter, sel.fn)
	case m[3] != "":
		d, err := time.ParseDuration(m[3])
		if err != nil || d <= 0 {
			return sel, apierror.InvalidArgument("Field filter had an invalid value of %q: the lookback period %q must be a positive duration in ns, us, ms, s, m or h", filter, m[3])
		}
		sel.lookback = d
	}
	return sel, nil
}

// sloTimeSeries answers ListTimeSeries for a select_slo_* filter.
func (s *Server) sloTimeSeries(ctx context.Context, project string, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	sel, err := parseSLOSelector(req.GetFilter())
	if err != nil {
		return nil, err
	}
	if projectOfName(sel.slo) != project {
		return nil, apierror.InvalidArgument("Field filter had an invalid value: the SLO %s is not in project %s", sel.slo, project)
	}
	s.mu.Lock()
	slo, _ := s.get(kindSLOs, sel.slo).(*monitoringpb.ServiceLevelObjective)
	if slo != nil {
		slo = clone(slo)
	}
	s.mu.Unlock()
	if slo == nil {
		return nil, apierror.NotFound("Service level objective %s was not found", sel.slo)
	}
	if slo.GetServiceLevelIndicator().GetBasicSli() != nil || slo.GetServiceLevelIndicator().GetWindowsBased().GetGoodTotalRatioThreshold().GetBasicSliPerformance() != nil {
		return nil, apierror.Unimplemented("SLO %s uses a basicSli, whose automatic metrics this instance does not produce; use a requestBased or windowsBased SLI", sel.slo)
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
		return nil, apierror.InvalidArgument("Field interval.endTime had an invalid value: the end time is before the start time")
	}
	period := time.Minute
	if p := req.GetAggregation().GetAlignmentPeriod(); p != nil {
		period = p.AsDuration()
		if period < time.Minute {
			return nil, apierror.InvalidArgument("Field aggregation.alignmentPeriod had an invalid value of \"%s\": The alignment period must be at least 60 seconds", period)
		}
	}
	c := sloCounter{s: s, project: project, slo: slo}
	var good, bad []*monitoringpb.Point
	var vals []*monitoringpb.Point
	for _, t := range windowEnds(period, start, end) {
		var from time.Time
		switch sel.fn {
		case "select_slo_health", "select_slo_counts":
			from = t.Add(-period)
		case "select_slo_burn_rate":
			from = t.Add(-sel.lookback)
		default:
			from = compliancePeriodStart(slo, t)
		}
		g, tot, err := c.counts(ctx, from, t)
		if err != nil {
			return nil, err
		}
		pt := func(v float64) *monitoringpb.Point {
			return &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(from), EndTime: timestamppb.New(t)}, Value: typed(tDouble, v)}
		}
		budget := (1 - slo.GetGoal()) * tot
		switch sel.fn {
		case "select_slo_counts":
			if tot > 0 {
				good = append(good, pt(g))
				bad = append(bad, pt(tot-g))
			}
		case "select_slo_health", "select_slo_compliance":
			if tot > 0 {
				vals = append(vals, pt(g/tot))
			}
		case "select_slo_burn_rate":
			if tot > 0 {
				vals = append(vals, pt((tot-g)/tot/(1-slo.GetGoal())))
			}
		case "select_slo_budget":
			v := budget - (tot - g)
			if c.windowed() {
				v *= c.window().Seconds()
			}
			vals = append(vals, pt(v))
		case "select_slo_budget_fraction":
			f := 1.0
			if budget > 0 {
				f = 1 - (tot-g)/budget
			} else if tot-g > 0 {
				f = math.Inf(-1)
			}
			vals = append(vals, pt(f))
		}
	}
	res := &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": project}}
	mk := func(pts []*monitoringpb.Point, labels map[string]string) *monitoringpb.TimeSeries {
		return &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: sel.fn, Labels: labels}, Resource: res,
			MetricKind: metricpb.MetricDescriptor_GAUGE, ValueType: tDouble, Points: pts}
	}
	resp := &monitoringpb.ListTimeSeriesResponse{}
	switch {
	case sel.fn == "select_slo_counts":
		if len(good) > 0 {
			resp.TimeSeries = append(resp.TimeSeries, mk(good, map[string]string{"event_type": "good"}), mk(bad, map[string]string{"event_type": "bad"}))
		}
	case len(vals) > 0:
		resp.TimeSeries = append(resp.TimeSeries, mk(vals, nil))
	}
	if req.GetView() == monitoringpb.ListTimeSeriesRequest_HEADERS {
		for _, ts := range resp.TimeSeries {
			ts.Points = nil
		}
	}
	return resp, nil
}

// compliancePeriodStart is when the compliance period holding t began.
func compliancePeriodStart(slo *monitoringpb.ServiceLevelObjective, t time.Time) time.Time {
	if r := slo.GetRollingPeriod(); r != nil {
		return t.Add(-r.AsDuration())
	}
	u := t.UTC()
	day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	switch slo.GetCalendarPeriod() {
	case calendarperiod.CalendarPeriod_DAY:
		return day
	case calendarperiod.CalendarPeriod_WEEK:
		wd := (int(day.Weekday()) + 6) % 7 // Monday first
		return day.AddDate(0, 0, -wd)
	case calendarperiod.CalendarPeriod_FORTNIGHT:
		wd := (int(day.Weekday()) + 6) % 7
		monday := day.AddDate(0, 0, -wd)
		_, week := monday.ISOWeek()
		if week%2 == 0 {
			monday = monday.AddDate(0, 0, -7)
		}
		return monday
	case calendarperiod.CalendarPeriod_MONTH:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	case calendarperiod.CalendarPeriod_QUARTER:
		return time.Date(u.Year(), time.Month((int(u.Month())-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
	case calendarperiod.CalendarPeriod_HALF:
		return time.Date(u.Year(), time.Month((int(u.Month())-1)/6*6+1), 1, 0, 0, 0, 0, time.UTC)
	case calendarperiod.CalendarPeriod_YEAR:
		return time.Date(u.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return day
}

// sloCounter counts an SLI's good and total units.
type sloCounter struct {
	s       *Server
	project string
	slo     *monitoringpb.ServiceLevelObjective
}

func (c sloCounter) windowed() bool {
	return c.slo.GetServiceLevelIndicator().GetWindowsBased() != nil
}

func (c sloCounter) window() time.Duration {
	if w := c.slo.GetServiceLevelIndicator().GetWindowsBased().GetWindowPeriod(); w != nil {
		return w.AsDuration()
	}
	return time.Minute
}

// counts are the good and total units in (from, to].
func (c sloCounter) counts(ctx context.Context, from, to time.Time) (good, total float64, err error) {
	sli := c.slo.GetServiceLevelIndicator()
	if rb := sli.GetRequestBased(); rb != nil {
		return c.requestCounts(ctx, rb, from, to)
	}
	wb := sli.GetWindowsBased()
	if wb == nil {
		return 0, 0, nil
	}
	w := c.window()
	if w < time.Minute {
		w = time.Minute
	}
	// Whole windows ending at multiples of the window period.
	first := from.Truncate(w)
	if first.Before(from) {
		first = first.Add(w)
	}
	for t := first.Add(w); !t.After(to); t = t.Add(w) {
		ok, has, err := c.goodWindow(ctx, wb, t.Add(-w), t)
		if err != nil {
			return 0, 0, err
		}
		if !has {
			continue
		}
		total++
		if ok {
			good++
		}
		if total > 100000 {
			break
		}
	}
	return good, total, nil
}

// sumAligned is filter's series aligned over (from, to] and summed.
func (c sloCounter) aligned(ctx context.Context, filter string, aligner monitoringpb.Aggregation_Aligner, reducer monitoringpb.Aggregation_Reducer, from, to time.Time) (*monitoringpb.Point, error) {
	period := to.Sub(from)
	if period < time.Minute {
		period = time.Minute
	}
	req := &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + c.project, Filter: filter,
		Interval:    &monitoringpb.TimeInterval{StartTime: timestamppb.New(to), EndTime: timestamppb.New(to)},
		Aggregation: &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(period), PerSeriesAligner: aligner, CrossSeriesReducer: reducer}}
	resp, err := c.s.ListTimeSeries(ctx, req)
	if err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	for _, ts := range resp.GetTimeSeries() {
		if len(ts.GetPoints()) > 0 {
			return ts.GetPoints()[0], nil
		}
	}
	return nil, nil
}

func (c sloCounter) deltaSum(ctx context.Context, filter string, from, to time.Time) (float64, error) {
	p, err := c.aligned(ctx, filter, monitoringpb.Aggregation_ALIGN_DELTA, monitoringpb.Aggregation_REDUCE_SUM, from, to)
	if err != nil {
		return 0, err
	}
	return valueOf(p.GetValue()), nil
}

func (c sloCounter) requestCounts(ctx context.Context, rb *monitoringpb.RequestBasedSli, from, to time.Time) (good, total float64, err error) {
	if r := rb.GetGoodTotalRatio(); r != nil {
		var g, b, t float64
		if f := r.GetGoodServiceFilter(); f != "" {
			if g, err = c.deltaSum(ctx, f, from, to); err != nil {
				return
			}
		}
		if f := r.GetBadServiceFilter(); f != "" {
			if b, err = c.deltaSum(ctx, f, from, to); err != nil {
				return
			}
		}
		if f := r.GetTotalServiceFilter(); f != "" {
			if t, err = c.deltaSum(ctx, f, from, to); err != nil {
				return
			}
		}
		switch {
		case r.GetTotalServiceFilter() == "":
			return g, g + b, nil
		case r.GetGoodServiceFilter() == "":
			return max(t-b, 0), t, nil
		}
		return min(g, t), t, nil
	}
	dc := rb.GetDistributionCut()
	p, err := c.aligned(ctx, dc.GetDistributionFilter(), monitoringpb.Aggregation_ALIGN_DELTA, monitoringpb.Aggregation_REDUCE_SUM, from, to)
	if err != nil || p == nil {
		return 0, 0, err
	}
	d := p.GetValue().GetDistributionValue()
	return distributionInRange(d, dc.GetRange()), float64(d.GetCount()), nil
}

// distributionInRange counts a distribution's values in [min, max], by
// whole buckets inside the range.
func distributionInRange(d *distribution.Distribution, r *monitoringpb.Range) float64 {
	lo, hi := r.GetMin(), r.GetMax()
	if r.GetMax() == 0 && r.GetMin() == 0 {
		hi = math.Inf(1)
	}
	var n float64
	for i, cnt := range d.GetBucketCounts() {
		blo, bhi := bucketBounds(d.GetBucketOptions(), i)
		if blo >= lo && bhi <= hi || len(d.GetBucketCounts()) == 1 && d.GetMean() >= lo && d.GetMean() <= hi {
			n += float64(cnt)
		}
	}
	return n
}

// goodWindow is whether one window is good, and whether it has data.
func (c sloCounter) goodWindow(ctx context.Context, wb *monitoringpb.WindowsBasedSli, from, to time.Time) (good, has bool, err error) {
	inRange := func(v float64, r *monitoringpb.Range) bool { return v >= r.GetMin() && v <= r.GetMax() }
	switch {
	case wb.GetGoodBadMetricFilter() != "":
		p, err := c.aligned(ctx, wb.GetGoodBadMetricFilter(), monitoringpb.Aggregation_ALIGN_FRACTION_TRUE, monitoringpb.Aggregation_REDUCE_MEAN, from, to)
		if err != nil || p == nil {
			return false, false, err
		}
		return valueOf(p.GetValue()) >= 1, true, nil
	case wb.GetGoodTotalRatioThreshold() != nil:
		t := wb.GetGoodTotalRatioThreshold()
		g, tot, err := c.requestCounts(ctx, t.GetPerformance(), from, to)
		if err != nil || tot == 0 {
			return false, false, err
		}
		return g/tot >= t.GetThreshold(), true, nil
	case wb.GetMetricMeanInRange() != nil:
		m := wb.GetMetricMeanInRange()
		p, err := c.aligned(ctx, m.GetTimeSeries(), monitoringpb.Aggregation_ALIGN_MEAN, monitoringpb.Aggregation_REDUCE_MEAN, from, to)
		if err != nil || p == nil {
			return false, false, err
		}
		return inRange(valueOf(p.GetValue()), m.GetRange()), true, nil
	case wb.GetMetricSumInRange() != nil:
		m := wb.GetMetricSumInRange()
		p, err := c.aligned(ctx, m.GetTimeSeries(), monitoringpb.Aggregation_ALIGN_SUM, monitoringpb.Aggregation_REDUCE_SUM, from, to)
		if err != nil || p == nil {
			return false, false, err
		}
		return inRange(valueOf(p.GetValue()), m.GetRange()), true, nil
	}
	return false, false, nil
}
