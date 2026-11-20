package monitoring

import (
	"math"
	"sort"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Alignment and reduction as monitoring/v3/common.proto documents each
// Aligner and Reducer. An alignment period with no data produces no point,
// except for ALIGN_DELTA and ALIGN_RATE, which Google documents as "created
// by interpolation" (fillGap), and ALIGN_PERCENT_CHANGE compares the period's mean with the previous
// period's, without Google's 10-minute moving mean.

var (
	gauge      = metricpb.MetricDescriptor_GAUGE
	delta      = metricpb.MetricDescriptor_DELTA
	cumulative = metricpb.MetricDescriptor_CUMULATIVE
	tInt       = metricpb.MetricDescriptor_INT64
	tDouble    = metricpb.MetricDescriptor_DOUBLE
	tBool      = metricpb.MetricDescriptor_BOOL
	tDist      = metricpb.MetricDescriptor_DISTRIBUTION
)

func numeric(v metricpb.MetricDescriptor_ValueType) bool { return v == tInt || v == tDouble }

func asFloat(v *monitoringpb.TypedValue) float64 {
	switch x := v.GetValue().(type) {
	case *monitoringpb.TypedValue_Int64Value:
		return float64(x.Int64Value)
	case *monitoringpb.TypedValue_DoubleValue:
		return x.DoubleValue
	case *monitoringpb.TypedValue_BoolValue:
		if x.BoolValue {
			return 1
		}
		return 0
	case *monitoringpb.TypedValue_DistributionValue:
		return x.DistributionValue.GetMean()
	}
	return 0
}

func typed(vt metricpb.MetricDescriptor_ValueType, f float64) *monitoringpb.TypedValue {
	if vt == tInt {
		return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(math.Round(f))}}
	}
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: f}}
}

func intValue(n int) *monitoringpb.TypedValue {
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(n)}}
}

func distValue(d *distribution.Distribution) *monitoringpb.TypedValue {
	return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{DistributionValue: d}}
}

func refuseAligner(a monitoringpb.Aggregation_Aligner, k metricpb.MetricDescriptor_MetricKind, v metricpb.MetricDescriptor_ValueType) error {
	return apierror.InvalidArgument("Field aggregation.perSeriesAligner had an invalid value of %q: The aligner cannot be applied to metrics with kind %s and value type %s.", a, k, v)
}

// alignerOutput checks that aligner applies to (kind, vt) and returns the
// aligned series' kind and value type.
func alignerOutput(a monitoringpb.Aggregation_Aligner, k metricpb.MetricDescriptor_MetricKind, v metricpb.MetricDescriptor_ValueType) (metricpb.MetricDescriptor_MetricKind, metricpb.MetricDescriptor_ValueType, error) {
	gd := k == gauge || k == delta
	switch a {
	case monitoringpb.Aggregation_ALIGN_DELTA:
		if (k == cumulative || k == delta) && (numeric(v) || v == tDist) {
			return delta, v, nil
		}
	case monitoringpb.Aggregation_ALIGN_RATE:
		if (k == cumulative || k == delta) && (numeric(v) || v == tDist) {
			return gauge, tDouble, nil
		}
	case monitoringpb.Aggregation_ALIGN_INTERPOLATE:
		if k == gauge && numeric(v) {
			return gauge, v, nil
		}
	case monitoringpb.Aggregation_ALIGN_NEXT_OLDER:
		if k == gauge {
			return gauge, v, nil
		}
	case monitoringpb.Aggregation_ALIGN_MIN, monitoringpb.Aggregation_ALIGN_MAX:
		if gd && numeric(v) {
			return gauge, v, nil
		}
	case monitoringpb.Aggregation_ALIGN_MEAN, monitoringpb.Aggregation_ALIGN_STDDEV, monitoringpb.Aggregation_ALIGN_PERCENT_CHANGE:
		if gd && numeric(v) {
			return gauge, tDouble, nil
		}
	case monitoringpb.Aggregation_ALIGN_COUNT:
		if gd && (numeric(v) || v == tBool) {
			return gauge, tInt, nil
		}
	case monitoringpb.Aggregation_ALIGN_SUM:
		if gd && (numeric(v) || v == tDist) {
			return gauge, v, nil
		}
	case monitoringpb.Aggregation_ALIGN_COUNT_TRUE, monitoringpb.Aggregation_ALIGN_COUNT_FALSE:
		if k == gauge && v == tBool {
			return gauge, tInt, nil
		}
	case monitoringpb.Aggregation_ALIGN_FRACTION_TRUE:
		if k == gauge && v == tBool {
			return gauge, tDouble, nil
		}
	case monitoringpb.Aggregation_ALIGN_PERCENTILE_99, monitoringpb.Aggregation_ALIGN_PERCENTILE_95,
		monitoringpb.Aggregation_ALIGN_PERCENTILE_50, monitoringpb.Aggregation_ALIGN_PERCENTILE_05:
		if gd && v == tDist {
			return gauge, tDouble, nil
		}
	default:
		return k, v, apierror.InvalidArgument("Field aggregation.perSeriesAligner had an invalid value of %q", a)
	}
	return k, v, refuseAligner(a, k, v)
}

var alignerPercentile = map[monitoringpb.Aggregation_Aligner]float64{
	monitoringpb.Aggregation_ALIGN_PERCENTILE_99: 99, monitoringpb.Aggregation_ALIGN_PERCENTILE_95: 95,
	monitoringpb.Aggregation_ALIGN_PERCENTILE_50: 50, monitoringpb.Aggregation_ALIGN_PERCENTILE_05: 5,
}

var reducerPercentile = map[monitoringpb.Aggregation_Reducer]float64{
	monitoringpb.Aggregation_REDUCE_PERCENTILE_99: 99, monitoringpb.Aggregation_REDUCE_PERCENTILE_95: 95,
	monitoringpb.Aggregation_REDUCE_PERCENTILE_50: 50, monitoringpb.Aggregation_REDUCE_PERCENTILE_05: 5,
}

// aggregate aligns each series and reduces across them.
func aggregate(in []*monitoringpb.TimeSeries, agg aggregation, md *metricpb.MetricDescriptor, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if agg.aligner == monitoringpb.Aggregation_ALIGN_NONE {
		return in, nil
	}
	if _, _, err := alignerOutput(agg.aligner, md.GetMetricKind(), md.GetValueType()); err != nil {
		return nil, err
	}
	out := make([]*monitoringpb.TimeSeries, 0, len(in))
	for _, ts := range in {
		a, err := align(ts, agg.aligner, agg.period, start, end)
		if err != nil {
			return nil, err
		}
		if len(a.Points) > 0 {
			out = append(out, a)
		}
	}
	if agg.reducer == monitoringpb.Aggregation_REDUCE_NONE {
		return out, nil
	}
	return reduce(out, agg)
}

// windowEnds are the aligned points' end times: end, end-period, ... while
// the window still overlaps [start, end].
func windowEnds(period time.Duration, start, end time.Time) []time.Time {
	var out []time.Time
	for t := end; ; t = t.Add(-period) {
		out = append(out, t)
		if !t.Add(-period).After(start) || len(out) >= 100000 {
			break
		}
	}
	return out
}

func align(ts *monitoringpb.TimeSeries, a monitoringpb.Aggregation_Aligner, period time.Duration, start, end time.Time) (*monitoringpb.TimeSeries, error) {
	kind, vt, err := alignerOutput(a, ts.GetMetricKind(), ts.GetValueType())
	if err != nil {
		return nil, err
	}
	out := clone(ts)
	out.Points, out.MetricKind, out.ValueType = nil, kind, vt
	if a == monitoringpb.Aggregation_ALIGN_RATE {
		out.Unit = ""
		if ts.GetUnit() != "" {
			out.Unit = ts.GetUnit() + "/s"
		}
	}
	if a == monitoringpb.Aggregation_ALIGN_COUNT || a == monitoringpb.Aggregation_ALIGN_COUNT_TRUE || a == monitoringpb.Aggregation_ALIGN_COUNT_FALSE ||
		a == monitoringpb.Aggregation_ALIGN_FRACTION_TRUE || a == monitoringpb.Aggregation_ALIGN_PERCENT_CHANGE {
		out.Unit = ""
		if a == monitoringpb.Aggregation_ALIGN_PERCENT_CHANGE {
			out.Unit = "%"
		}
	}
	pts := ts.GetPoints()
	sort.SliceStable(pts, func(i, j int) bool { return endOf(pts[i]).Before(endOf(pts[j])) })
	for _, t := range windowEnds(period, start, end) {
		lo := t.Add(-period)
		var win []*monitoringpb.Point
		var before *monitoringpb.Point
		var after *monitoringpb.Point
		for _, p := range pts {
			e := endOf(p)
			switch {
			case !e.After(lo):
				before = p
			case !e.After(t):
				win = append(win, p)
			case after == nil:
				after = p
			}
		}
		v, ok := alignWindow(a, ts, win, before, after, t, period)
		if !ok {
			continue
		}
		iv := &monitoringpb.TimeInterval{EndTime: timestamppb.New(t)}
		if kind == delta {
			iv.StartTime = timestamppb.New(lo)
		}
		out.Points = append(out.Points, &monitoringpb.Point{Interval: iv, Value: v})
	}
	return out, nil
}

// fillGap is ALIGN_DELTA's or ALIGN_RATE's value for a window (t-period, t]
// with no point, between the points before and after it (common.proto:
// "If the selected alignment period results in periods with no data, then
// the aligned value for such a period is created by interpolation"). A
// cumulative series' value is interpolated linearly between the two
// points, so the window's delta is its share of their difference; a delta
// series' next point is spread evenly over the time since the point
// before, so the window gets its share of that point's value. Without a
// point on each side, or across a cumulative reset, there is no point.
// Distributions are not interpolated.
func fillGap(a monitoringpb.Aggregation_Aligner, ts *monitoringpb.TimeSeries, before, after *monitoringpb.Point, t time.Time, period time.Duration) (*monitoringpb.TypedValue, bool) {
	vt := ts.GetValueType()
	if before == nil || after == nil || !numeric(vt) {
		return nil, false
	}
	span := endOf(after).Sub(endOf(before))
	if span <= 0 {
		return nil, false
	}
	var d float64
	switch ts.GetMetricKind() {
	case cumulative:
		if !after.GetInterval().GetStartTime().AsTime().Equal(before.GetInterval().GetStartTime().AsTime()) {
			return nil, false
		}
		d = (asFloat(after.GetValue()) - asFloat(before.GetValue())) * float64(period) / float64(span)
	case delta:
		d = asFloat(after.GetValue()) * float64(period) / float64(span)
	default:
		return nil, false
	}
	if a == monitoringpb.Aggregation_ALIGN_RATE {
		return typed(tDouble, d/period.Seconds()), true
	}
	return typed(vt, d), true
}

func endOf(p *monitoringpb.Point) time.Time { return p.GetInterval().GetEndTime().AsTime() }

// alignWindow is one aligned value from the points in (t-period, t].
func alignWindow(a monitoringpb.Aggregation_Aligner, ts *monitoringpb.TimeSeries, win []*monitoringpb.Point, before, after *monitoringpb.Point, t time.Time, period time.Duration) (*monitoringpb.TypedValue, bool) {
	vt := ts.GetValueType()
	if len(win) == 0 && (a == monitoringpb.Aggregation_ALIGN_DELTA || a == monitoringpb.Aggregation_ALIGN_RATE) {
		return fillGap(a, ts, before, after, t, period)
	}
	if len(win) == 0 && a != monitoringpb.Aggregation_ALIGN_INTERPOLATE {
		return nil, false
	}
	values := func() []float64 {
		out := make([]float64, len(win))
		for i, p := range win {
			out[i] = asFloat(p.GetValue())
		}
		return out
	}
	switch a {
	case monitoringpb.Aggregation_ALIGN_DELTA, monitoringpb.Aggregation_ALIGN_RATE:
		var d *monitoringpb.TypedValue
		if ts.GetMetricKind() == cumulative {
			last := win[len(win)-1]
			var base *monitoringpb.Point
			if before != nil && before.GetInterval().GetStartTime().AsTime().Equal(last.GetInterval().GetStartTime().AsTime()) {
				base = before
			}
			d = subtract(vt, last.GetValue(), base)
		} else {
			d = sumValues(vt, win)
		}
		// After a filled gap (fillGap gave the empty windows their share),
		// this window keeps only its own share of the change since before,
		// so the gap is not counted twice.
		if lo := t.Add(-period); before != nil && numeric(vt) && lo.Sub(endOf(before)) >= period {
			first := win[0]
			share := float64(endOf(first).Sub(lo)) / float64(endOf(first).Sub(endOf(before)))
			switch {
			case ts.GetMetricKind() == cumulative && before.GetInterval().GetStartTime().AsTime().Equal(first.GetInterval().GetStartTime().AsTime()):
				d = typed(vt, asFloat(d)-(asFloat(first.GetValue())-asFloat(before.GetValue()))*(1-share))
			case ts.GetMetricKind() == delta:
				d = typed(vt, asFloat(d)-asFloat(first.GetValue())*(1-share))
			}
		}
		if a == monitoringpb.Aggregation_ALIGN_DELTA {
			return d, true
		}
		if vt == tDist {
			return typed(tDouble, float64(d.GetDistributionValue().GetCount())/period.Seconds()), true
		}
		return typed(tDouble, asFloat(d)/period.Seconds()), true
	case monitoringpb.Aggregation_ALIGN_INTERPOLATE:
		var prev *monitoringpb.Point
		if len(win) > 0 {
			prev = win[len(win)-1]
		}
		if prev == nil {
			return nil, false
		}
		if after == nil || endOf(prev).Equal(t) {
			return typed(vt, asFloat(prev.GetValue())), true
		}
		x0, x1 := endOf(prev), endOf(after)
		y0, y1 := asFloat(prev.GetValue()), asFloat(after.GetValue())
		frac := float64(t.Sub(x0)) / float64(x1.Sub(x0))
		return typed(vt, y0+(y1-y0)*frac), true
	case monitoringpb.Aggregation_ALIGN_NEXT_OLDER:
		return clone(win[len(win)-1].GetValue()), true
	case monitoringpb.Aggregation_ALIGN_MIN:
		vs := values()
		m := vs[0]
		for _, v := range vs {
			m = math.Min(m, v)
		}
		return typed(vt, m), true
	case monitoringpb.Aggregation_ALIGN_MAX:
		vs := values()
		m := vs[0]
		for _, v := range vs {
			m = math.Max(m, v)
		}
		return typed(vt, m), true
	case monitoringpb.Aggregation_ALIGN_MEAN:
		return typed(tDouble, mean(values())), true
	case monitoringpb.Aggregation_ALIGN_STDDEV:
		return typed(tDouble, stddev(values())), true
	case monitoringpb.Aggregation_ALIGN_COUNT:
		return intValue(len(win)), true
	case monitoringpb.Aggregation_ALIGN_SUM:
		return sumValues(vt, win), true
	case monitoringpb.Aggregation_ALIGN_COUNT_TRUE, monitoringpb.Aggregation_ALIGN_COUNT_FALSE, monitoringpb.Aggregation_ALIGN_FRACTION_TRUE:
		n := 0
		for _, p := range win {
			if p.GetValue().GetBoolValue() {
				n++
			}
		}
		switch a {
		case monitoringpb.Aggregation_ALIGN_COUNT_TRUE:
			return intValue(n), true
		case monitoringpb.Aggregation_ALIGN_COUNT_FALSE:
			return intValue(len(win) - n), true
		}
		return typed(tDouble, float64(n)/float64(len(win))), true
	case monitoringpb.Aggregation_ALIGN_PERCENT_CHANGE:
		if before == nil {
			return nil, false
		}
		cur, prev := mean(values()), asFloat(before.GetValue())
		switch {
		case cur == 0 && prev == 0:
			return typed(tDouble, 0), true
		case prev == 0:
			return typed(tDouble, math.Inf(1)), true
		}
		return typed(tDouble, (cur-prev)/prev*100), true
	}
	if p, ok := alignerPercentile[a]; ok {
		d := sumValues(tDist, win).GetDistributionValue()
		return typed(tDouble, distPercentile(d, p)), true
	}
	return nil, false
}

func mean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range vs {
		s += v
	}
	return s / float64(len(vs))
}

func stddev(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	m := mean(vs)
	s := 0.0
	for _, v := range vs {
		s += (v - m) * (v - m)
	}
	return math.Sqrt(s / float64(len(vs)))
}

func percentile(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	r := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(r))
	hi := int(math.Ceil(r))
	return s[lo] + (s[hi]-s[lo])*(r-float64(lo))
}

// sumValues sums points: numbers, or distributions merged.
func sumValues(vt metricpb.MetricDescriptor_ValueType, pts []*monitoringpb.Point) *monitoringpb.TypedValue {
	if vt == tDist {
		var d *distribution.Distribution
		for _, p := range pts {
			d = mergeDist(d, p.GetValue().GetDistributionValue())
		}
		return distValue(d)
	}
	s := 0.0
	for _, p := range pts {
		s += asFloat(p.GetValue())
	}
	return typed(vt, s)
}

// subtract is v minus base's value (nil base is zero).
func subtract(vt metricpb.MetricDescriptor_ValueType, v *monitoringpb.TypedValue, base *monitoringpb.Point) *monitoringpb.TypedValue {
	if base == nil {
		return clone(v)
	}
	if vt == tDist {
		return distValue(subDist(v.GetDistributionValue(), base.GetValue().GetDistributionValue()))
	}
	return typed(vt, asFloat(v)-asFloat(base.GetValue()))
}

// mergeDist is a and b combined; buckets are summed when their options
// agree, and dropped when they do not.
func mergeDist(a, b *distribution.Distribution) *distribution.Distribution {
	if a == nil {
		return clone(b)
	}
	if b == nil || b.GetCount() == 0 {
		return a
	}
	out := clone(a)
	n := a.GetCount() + b.GetCount()
	m := (a.GetMean()*float64(a.GetCount()) + b.GetMean()*float64(b.GetCount())) / float64(n)
	ssd := a.GetSumOfSquaredDeviation() + b.GetSumOfSquaredDeviation() +
		float64(a.GetCount())*(a.GetMean()-m)*(a.GetMean()-m) + float64(b.GetCount())*(b.GetMean()-m)*(b.GetMean()-m)
	out.Count, out.Mean, out.SumOfSquaredDeviation = n, m, ssd
	if a.GetRange() != nil && b.GetRange() != nil {
		out.Range = &distribution.Distribution_Range{Min: math.Min(a.GetRange().GetMin(), b.GetRange().GetMin()), Max: math.Max(a.GetRange().GetMax(), b.GetRange().GetMax())}
	} else {
		out.Range = nil
	}
	if sameOptions(a.GetBucketOptions(), b.GetBucketOptions()) {
		size := max(len(a.GetBucketCounts()), len(b.GetBucketCounts()))
		counts := make([]int64, size)
		for i, c := range a.GetBucketCounts() {
			counts[i] += c
		}
		for i, c := range b.GetBucketCounts() {
			counts[i] += c
		}
		out.BucketCounts = counts
	} else {
		out.BucketCounts = nil
	}
	out.Exemplars = nil
	return out
}

// subDist is a minus b, for cumulative distributions with one start time.
func subDist(a, b *distribution.Distribution) *distribution.Distribution {
	out := clone(a)
	n := a.GetCount() - b.GetCount()
	if n <= 0 {
		return &distribution.Distribution{BucketOptions: a.GetBucketOptions()}
	}
	sum := a.GetMean()*float64(a.GetCount()) - b.GetMean()*float64(b.GetCount())
	out.Count, out.Mean = n, sum/float64(n)
	out.SumOfSquaredDeviation = math.Max(0, a.GetSumOfSquaredDeviation()-b.GetSumOfSquaredDeviation())
	out.Range = nil
	if sameOptions(a.GetBucketOptions(), b.GetBucketOptions()) {
		counts := append([]int64(nil), a.GetBucketCounts()...)
		for i, c := range b.GetBucketCounts() {
			if i < len(counts) {
				counts[i] -= c
			}
		}
		out.BucketCounts = counts
	}
	out.Exemplars = nil
	return out
}

func sameOptions(a, b *distribution.Distribution_BucketOptions) bool {
	return a.String() == b.String()
}

// bucketBounds is bucket i's [lo, hi).
func bucketBounds(o *distribution.Distribution_BucketOptions, i int) (float64, float64) {
	inf := math.Inf(1)
	switch x := o.GetOptions().(type) {
	case *distribution.Distribution_BucketOptions_LinearBuckets:
		l := x.LinearBuckets
		n := int(l.GetNumFiniteBuckets())
		lo := func(k int) float64 { return l.GetOffset() + l.GetWidth()*float64(k) }
		switch {
		case i == 0:
			return -inf, lo(0)
		case i > n:
			return lo(n), inf
		}
		return lo(i - 1), lo(i)
	case *distribution.Distribution_BucketOptions_ExponentialBuckets:
		e := x.ExponentialBuckets
		n := int(e.GetNumFiniteBuckets())
		lo := func(k int) float64 { return e.GetScale() * math.Pow(e.GetGrowthFactor(), float64(k)) }
		switch {
		case i == 0:
			return -inf, lo(0)
		case i > n:
			return lo(n), inf
		}
		return lo(i - 1), lo(i)
	case *distribution.Distribution_BucketOptions_ExplicitBuckets:
		b := x.ExplicitBuckets.GetBounds()
		switch {
		case len(b) == 0:
			return -inf, inf
		case i == 0:
			return -inf, b[0]
		case i >= len(b):
			return b[len(b)-1], inf
		}
		return b[i-1], b[i]
	}
	return -inf, inf
}

// distPercentile estimates a percentile from a distribution's buckets,
// interpolating within the bucket it falls in; without buckets, the mean.
func distPercentile(d *distribution.Distribution, p float64) float64 {
	if d.GetCount() == 0 {
		return 0
	}
	if len(d.GetBucketCounts()) == 0 {
		return d.GetMean()
	}
	rank := p / 100 * float64(d.GetCount())
	cum := 0.0
	for i, c := range d.GetBucketCounts() {
		if c == 0 {
			continue
		}
		if cum+float64(c) >= rank {
			lo, hi := bucketBounds(d.GetBucketOptions(), i)
			if r := d.GetRange(); r != nil {
				lo, hi = math.Max(lo, r.GetMin()), math.Min(hi, r.GetMax())
			}
			switch {
			case math.IsInf(lo, -1) && math.IsInf(hi, 1):
				return d.GetMean()
			case math.IsInf(lo, -1):
				return hi
			case math.IsInf(hi, 1):
				return lo
			}
			return lo + (hi-lo)*(rank-cum)/float64(c)
		}
		cum += float64(c)
	}
	return d.GetMean()
}

// reducerOutput checks a reducer against aligned (kind, vt).
func reducerOutput(r monitoringpb.Aggregation_Reducer, v metricpb.MetricDescriptor_ValueType) (metricpb.MetricDescriptor_ValueType, error) {
	switch r {
	case monitoringpb.Aggregation_REDUCE_MEAN, monitoringpb.Aggregation_REDUCE_STDDEV:
		if numeric(v) || v == tDist {
			return tDouble, nil
		}
	case monitoringpb.Aggregation_REDUCE_MIN, monitoringpb.Aggregation_REDUCE_MAX:
		if numeric(v) {
			return v, nil
		}
	case monitoringpb.Aggregation_REDUCE_SUM:
		if numeric(v) || v == tDist {
			return v, nil
		}
	case monitoringpb.Aggregation_REDUCE_COUNT:
		return tInt, nil
	case monitoringpb.Aggregation_REDUCE_COUNT_TRUE, monitoringpb.Aggregation_REDUCE_COUNT_FALSE:
		if v == tBool {
			return tInt, nil
		}
	case monitoringpb.Aggregation_REDUCE_FRACTION_TRUE:
		if v == tBool {
			return tDouble, nil
		}
	default:
		if _, ok := reducerPercentile[r]; ok {
			if numeric(v) || v == tDist {
				return tDouble, nil
			}
		} else {
			return v, apierror.InvalidArgument("Field aggregation.crossSeriesReducer had an invalid value of %q", r)
		}
	}
	return v, apierror.InvalidArgument("Field aggregation.crossSeriesReducer had an invalid value of %q: The reducer cannot be applied to aligned values of type %s.", r, v)
}

// reduce combines aligned series that share their group-by values, at each
// aligned end time.
func reduce(in []*monitoringpb.TimeSeries, agg aggregation) ([]*monitoringpb.TimeSeries, error) {
	type group struct {
		head   *monitoringpb.TimeSeries
		points map[int64][]*monitoringpb.Point
		starts map[int64]*timestamppb.Timestamp
	}
	groups := map[string]*group{}
	var order []string
	for _, ts := range in {
		vt, err := reducerOutput(agg.reducer, ts.GetValueType())
		if err != nil {
			return nil, err
		}
		head := &monitoringpb.TimeSeries{
			Metric:     &metricpb.Metric{Type: ts.GetMetric().GetType(), Labels: map[string]string{}},
			Resource:   &monitoredres.MonitoredResource{Type: ts.GetResource().GetType(), Labels: map[string]string{}},
			MetricKind: ts.GetMetricKind(), ValueType: vt, Unit: ts.GetUnit(),
		}
		if agg.reducer == monitoringpb.Aggregation_REDUCE_COUNT || agg.reducer == monitoringpb.Aggregation_REDUCE_COUNT_TRUE ||
			agg.reducer == monitoringpb.Aggregation_REDUCE_COUNT_FALSE || agg.reducer == monitoringpb.Aggregation_REDUCE_FRACTION_TRUE {
			head.Unit = ""
		}
		for _, g := range agg.groupBy {
			scope, k, _ := groupField(g)
			switch scope {
			case "metric":
				if v, ok := ts.GetMetric().GetLabels()[k]; ok {
					head.Metric.Labels[k] = v
				}
			case "resource":
				if v, ok := ts.GetResource().GetLabels()[k]; ok {
					head.Resource.Labels[k] = v
				}
			}
		}
		key := seriesKey(head)
		g := groups[key]
		if g == nil {
			g = &group{head: head, points: map[int64][]*monitoringpb.Point{}, starts: map[int64]*timestamppb.Timestamp{}}
			groups[key] = g
			order = append(order, key)
		}
		for _, p := range ts.GetPoints() {
			t := endOf(p).UnixNano()
			g.points[t] = append(g.points[t], p)
			if p.GetInterval().GetStartTime() != nil {
				g.starts[t] = p.GetInterval().GetStartTime()
			}
		}
	}
	sort.Strings(order)
	var out []*monitoringpb.TimeSeries
	for _, key := range order {
		g := groups[key]
		var times []int64
		for t := range g.points {
			times = append(times, t)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		for _, t := range times {
			pts := g.points[t]
			v := reduceValues(agg.reducer, g.head.GetValueType(), pts)
			iv := &monitoringpb.TimeInterval{EndTime: timestamppb.New(time.Unix(0, t)), StartTime: g.starts[t]}
			g.head.Points = append(g.head.Points, &monitoringpb.Point{Interval: iv, Value: v})
		}
		out = append(out, g.head)
	}
	return out, nil
}

func reduceValues(r monitoringpb.Aggregation_Reducer, outType metricpb.MetricDescriptor_ValueType, pts []*monitoringpb.Point) *monitoringpb.TypedValue {
	isDist := len(pts) > 0 && pts[0].GetValue().GetDistributionValue() != nil
	vs := make([]float64, len(pts))
	for i, p := range pts {
		vs[i] = asFloat(p.GetValue())
	}
	switch r {
	case monitoringpb.Aggregation_REDUCE_MEAN:
		if isDist {
			return typed(tDouble, sumValues(tDist, pts).GetDistributionValue().GetMean())
		}
		return typed(tDouble, mean(vs))
	case monitoringpb.Aggregation_REDUCE_STDDEV:
		if isDist {
			d := sumValues(tDist, pts).GetDistributionValue()
			if d.GetCount() == 0 {
				return typed(tDouble, 0)
			}
			return typed(tDouble, math.Sqrt(d.GetSumOfSquaredDeviation()/float64(d.GetCount())))
		}
		return typed(tDouble, stddev(vs))
	case monitoringpb.Aggregation_REDUCE_MIN:
		m := vs[0]
		for _, v := range vs {
			m = math.Min(m, v)
		}
		return typed(outType, m)
	case monitoringpb.Aggregation_REDUCE_MAX:
		m := vs[0]
		for _, v := range vs {
			m = math.Max(m, v)
		}
		return typed(outType, m)
	case monitoringpb.Aggregation_REDUCE_SUM:
		if isDist {
			return sumValues(tDist, pts)
		}
		s := 0.0
		for _, v := range vs {
			s += v
		}
		return typed(outType, s)
	case monitoringpb.Aggregation_REDUCE_COUNT:
		return intValue(len(pts))
	case monitoringpb.Aggregation_REDUCE_COUNT_TRUE, monitoringpb.Aggregation_REDUCE_COUNT_FALSE, monitoringpb.Aggregation_REDUCE_FRACTION_TRUE:
		n := 0
		for _, p := range pts {
			if p.GetValue().GetBoolValue() {
				n++
			}
		}
		switch r {
		case monitoringpb.Aggregation_REDUCE_COUNT_TRUE:
			return intValue(n)
		case monitoringpb.Aggregation_REDUCE_COUNT_FALSE:
			return intValue(len(pts) - n)
		}
		return typed(tDouble, float64(n)/float64(len(pts)))
	}
	p := reducerPercentile[r]
	if isDist {
		return typed(tDouble, distPercentile(sumValues(tDist, pts).GetDistributionValue(), p))
	}
	return typed(tDouble, percentile(vs, p))
}
