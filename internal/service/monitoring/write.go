package monitoring

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// maxDistributionBuckets is the most buckets a written distribution may
// have ("Distribution metric has too many buckets").
const maxDistributionBuckets = 200

// pointValueType is the value type a point carries.
func pointValueType(v *monitoringpb.TypedValue) metricpb.MetricDescriptor_ValueType {
	switch v.GetValue().(type) {
	case *monitoringpb.TypedValue_BoolValue:
		return metricpb.MetricDescriptor_BOOL
	case *monitoringpb.TypedValue_Int64Value:
		return metricpb.MetricDescriptor_INT64
	case *monitoringpb.TypedValue_DoubleValue:
		return metricpb.MetricDescriptor_DOUBLE
	case *monitoringpb.TypedValue_StringValue:
		return metricpb.MetricDescriptor_STRING
	case *monitoringpb.TypedValue_DistributionValue:
		return metricpb.MetricDescriptor_DISTRIBUTION
	}
	return metricpb.MetricDescriptor_VALUE_TYPE_UNSPECIFIED
}

// CreateTimeSeries writes one point to each of up to 200 time series, with
// the rules of docs.cloud.google.com/monitoring/custom-metrics/creating-metrics
// and the errors of /monitoring/api/troubleshooting. A request whose shape
// is wrong writes nothing; otherwise each series is written or refused on
// its own, and the refusals come back as one error carrying a
// CreateTimeSeriesSummary, as Google's does.
func (s *Server) CreateTimeSeries(_ context.Context, req *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	all := req.GetTimeSeries()
	switch {
	case len(all) == 0:
		return nil, apierror.InvalidArgument("Request was missing field timeSeries")
	case len(all) > MaxSeriesPerWrite:
		return nil, apierror.InvalidArgument("The maximum number of TimeSeries objects per Create request is %d", MaxSeriesPerWrite)
	}
	seen := map[string]bool{}
	for _, ts := range all {
		if len(ts.GetPoints()) != 1 {
			return nil, apierror.InvalidArgument("Field timeSeries[%d].points had an invalid value: Only one point can be written per TimeSeries per request", indexOf(all, ts))
		}
		k := seriesKey(withProject(ts, project))
		if seen[k] {
			return nil, apierror.InvalidArgument("Field timeSeries[%d] had an invalid value: Duplicate TimeSeries encountered. Only one point can be written per TimeSeries per request.", indexOf(all, ts))
		}
		seen[k] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Labels written in one call are added to an auto-created descriptor as
	// the union across the call's series.
	autoLabels := map[string]map[string]bool{}
	for _, ts := range all {
		t := ts.GetMetric().GetType()
		if autoLabels[t] == nil {
			autoLabels[t] = map[string]bool{}
		}
		for k := range ts.GetMetric().GetLabels() {
			autoLabels[t][k] = true
		}
	}
	var failures []*monitoringpb.CreateTimeSeriesSummary_Error
	written := 0
	for i, in := range all {
		if err := s.writeOne(project, i, in, autoLabels, s.now); err != nil {
			st, _ := status.FromError(err)
			failures = append(failures, &monitoringpb.CreateTimeSeriesSummary_Error{
				Status: &spb.Status{Code: int32(st.Code()), Message: st.Message()}, PointCount: 1})
			continue
		}
		written++
	}
	if written > 0 {
		if err := s.save(); err != nil {
			return nil, apierror.Internal(err, "save Cloud Monitoring state")
		}
	}
	if len(failures) == 0 {
		return &emptypb.Empty{}, nil
	}
	summary := &monitoringpb.CreateTimeSeriesSummary{TotalPointCount: int32(len(all)), SuccessPointCount: int32(written), Errors: failures}
	code := codes.Code(failures[0].GetStatus().GetCode())
	msg := fmt.Sprintf("One or more TimeSeries could not be written: %s", failures[0].GetStatus().GetMessage())
	if len(failures) > 1 {
		msg = fmt.Sprintf("%s (and %d more)", msg, len(failures)-1)
	}
	e := &apierror.Error{Code: code, Message: msg}
	if a, err := anypb.New(summary); err == nil {
		e.Details = []*anypb.Any{a}
	}
	return nil, e
}

func indexOf(all []*monitoringpb.TimeSeries, ts *monitoringpb.TimeSeries) int {
	for i, t := range all {
		if t == ts {
			return i
		}
	}
	return -1
}

// withProject is ts with its resource's project_id filled in from the
// request's project, which a written resource may leave out.
func withProject(ts *monitoringpb.TimeSeries, project string) *monitoringpb.TimeSeries {
	out := clone(ts)
	if out.Resource != nil && hasResourceLabel(out.Resource.GetType(), "project_id") {
		if out.Resource.Labels == nil {
			out.Resource.Labels = map[string]string{}
		}
		if out.Resource.Labels["project_id"] == "" {
			out.Resource.Labels["project_id"] = project
		}
	}
	return out
}

// writeOne validates and stores one series' point. The caller holds mu.
func (s *Server) writeOne(project string, i int, in *monitoringpb.TimeSeries, autoLabels map[string]map[string]bool, now func() time.Time) error {
	ts := withProject(in, project)
	m := ts.GetMetric()
	t := m.GetType()
	if t == "" || !metricTypeRE.MatchString(t) {
		return apierror.InvalidArgument("The metric type must be a URL-formatted string with a domain and non-empty path")
	}
	if strings.HasPrefix(t, PromPrefix) && s.prom != nil {
		return s.writeProm(i, ts)
	}
	if !isUserDefined(t) {
		if md, _ := s.lookup(project, t); md != nil {
			return status.Errorf(codes.PermissionDenied, "User does not have permission to write to metric %s: only user-defined metrics can be written", t)
		}
		domain, _, _ := strings.Cut(t, "/")
		return apierror.InvalidArgument("user defined metrics are not supported on the metric domain %q", domain)
	}
	for k, v := range m.GetLabels() {
		if err := checkLabelKey(k); err != nil {
			return err
		}
		if len(v) > MaxLabelValue {
			return apierror.InvalidArgument("Field timeSeries[%d].metric.labels had an invalid value of %q: Label value exceeds the maximum string size of %d characters", i, k, MaxLabelValue)
		}
	}
	// The monitored resource.
	r := ts.GetResource()
	if r.GetType() == "" {
		return apierror.InvalidArgument("Field timeSeries[%d].resource had an invalid value: missing resource type", i)
	}
	if !writableResources[r.GetType()] {
		return apierror.InvalidArgument("Field timeSeries[%d].resource.type had an invalid value of %q: unrecognized resource type %q", i, r.GetType(), r.GetType())
	}
	for k := range r.GetLabels() {
		if !hasResourceLabel(r.GetType(), k) {
			return apierror.InvalidArgument("Field timeSeries[%d].resource.labels had an invalid value of %q: unrecognized resource label %q", i, k, k)
		}
	}
	// The point.
	p := ts.GetPoints()[0]
	vt := pointValueType(p.GetValue())
	switch vt {
	case metricpb.MetricDescriptor_VALUE_TYPE_UNSPECIFIED:
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: the point has no value", i)
	case metricpb.MetricDescriptor_STRING:
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: String values in custom metrics are not supported", i)
	case metricpb.MetricDescriptor_DISTRIBUTION:
		if err := checkDistribution(i, p.GetValue().GetDistributionValue()); err != nil {
			return err
		}
	}
	// The descriptor: the existing one, or one created from this write.
	name := descriptorName(project, t)
	md := s.descriptors[name]
	if md == nil {
		kind := ts.GetMetricKind()
		if kind == metricpb.MetricDescriptor_METRIC_KIND_UNSPECIFIED {
			kind = metricpb.MetricDescriptor_GAUGE
		}
		if kind != metricpb.MetricDescriptor_GAUGE && kind != metricpb.MetricDescriptor_CUMULATIVE {
			return apierror.InvalidArgument("Field timeSeries[%d].metricKind had an invalid value of %q: an auto-created metric must be GAUGE or CUMULATIVE", i, kind)
		}
		if ts.GetValueType() != metricpb.MetricDescriptor_VALUE_TYPE_UNSPECIFIED && ts.GetValueType() != vt {
			return apierror.InvalidArgument("value type for metric %s must be %s, but is %s", t, ts.GetValueType(), vt)
		}
		if vt == metricpb.MetricDescriptor_BOOL && kind != metricpb.MetricDescriptor_GAUGE {
			return apierror.InvalidArgument("metric kind for metric %s must be GAUGE, but is %s", t, kind)
		}
		if s.countDescriptors(project) >= MaxDescriptors {
			return &apierror.Error{Code: codes.ResourceExhausted, Message: "Your Metric Descriptors quota has been exhausted"}
		}
		md = &metricpb.MetricDescriptor{Name: name, Type: t, MetricKind: kind, ValueType: vt,
			Description: "Auto created custom metric."}
		s.descriptors[name] = md
	}
	if md.GetValueType() != vt {
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: value type for metric %s must be %s, but is %s", i, t, md.GetValueType(), vt)
	}
	if k := ts.GetMetricKind(); k != metricpb.MetricDescriptor_METRIC_KIND_UNSPECIFIED && k != md.GetMetricKind() {
		return apierror.InvalidArgument("Field timeSeries[%d].metricKind had an invalid value: metric kind for metric %s must be %s, but is %s", i, t, md.GetMetricKind(), k)
	}
	// Labels: written ones are added to the descriptor while it stays under
	// the limit; one that cannot be added is refused.
	known := map[string]bool{}
	for _, l := range md.GetLabels() {
		known[l.GetKey()] = true
	}
	var add []string
	for k := range autoLabels[t] {
		if !known[k] {
			add = append(add, k)
		}
	}
	for k := range m.GetLabels() {
		if !known[k] && len(md.GetLabels())+len(add) > MaxLabels {
			return apierror.InvalidArgument("Field timeSeries[%d].metric.labels had an invalid value: The metric %s has more than %d labels", i, t, MaxLabels)
		}
	}
	sort.Strings(add)
	for _, k := range add {
		md.Labels = append(md.Labels, &labelpb.LabelDescriptor{Key: k, ValueType: labelpb.LabelDescriptor_STRING})
		known[k] = true
	}
	// The interval.
	iv := p.GetInterval()
	if iv.GetEndTime() == nil {
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.endTime had an invalid value: Request was missing field interval.endTime", i)
	}
	end := iv.GetEndTime().AsTime()
	switch {
	case end.After(now().Add(MaxFuture)):
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.endTime had an invalid value: Data points cannot be written more than 5m into the future.", i)
	case end.Before(now().Add(-MaxPast)):
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.endTime had an invalid value: Data points cannot be written more than approximately 24 hours in the past", i)
	}
	pt := clone(p)
	switch md.GetMetricKind() {
	case metricpb.MetricDescriptor_GAUGE:
		if iv.GetStartTime() != nil && !iv.GetStartTime().AsTime().Equal(end) {
			return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.startTime had an invalid value: The start time must be equal to the end time (%s) for the gauge metric '%s'", i, end.Format(rfc3339), t)
		}
		pt.Interval.StartTime = nil
	default:
		if iv.GetStartTime() == nil || !iv.GetStartTime().AsTime().Before(end) {
			return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.startTime had an invalid value: The start time must be before the end time (%s) for the non-gauge metric '%s'", i, end.Format(rfc3339), t)
		}
	}
	// Order and rate against the series' latest point.
	ps := s.series[project]
	if ps == nil {
		ps = map[string]*series{}
		s.series[project] = ps
	}
	key := seriesKey(ts)
	sr := ps[key]
	if sr != nil && len(sr.TS.Points) > 0 {
		last := sr.TS.Points[len(sr.TS.Points)-1]
		lastEnd := last.GetInterval().GetEndTime().AsTime()
		if end.Before(lastEnd) {
			return apierror.InvalidArgument("Points must be written in order. One or more of the points specified had an older end time than the most recent point.")
		}
		if end.Sub(lastEnd) < MinWriteInterval {
			return apierror.FailedPrecondition("One or more points were written more frequently than the maximum sampling period configured for the metric.")
		}
		if md.GetMetricKind() == metricpb.MetricDescriptor_CUMULATIVE {
			start := pt.GetInterval().GetStartTime().AsTime()
			lastStart := last.GetInterval().GetStartTime().AsTime()
			if !start.Equal(lastStart) && !start.After(lastEnd) {
				return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.startTime had an invalid value: a cumulative point's start time must equal the series' start time (%s), or be after its last end time (%s) to reset it", i, lastStart.Format(rfc3339), lastEnd.Format(rfc3339))
			}
		}
	}
	if sr == nil {
		stored := &monitoringpb.TimeSeries{Metric: clone(ts.GetMetric()), Resource: clone(ts.GetResource()),
			MetricKind: md.GetMetricKind(), ValueType: md.GetValueType(), Unit: md.GetUnit()}
		if stored.Metric.Labels == nil {
			stored.Metric.Labels = map[string]string{}
		}
		sr = &series{TS: stored}
		ps[key] = sr
	}
	sr.TS.Points = append(sr.TS.Points, pt)
	if len(sr.TS.Points) > MaxPointsPerSeries {
		sr.TS.Points = sr.TS.Points[len(sr.TS.Points)-MaxPointsPerSeries:]
	}
	return nil
}

const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

// checkDistribution checks a written distribution as Google does.
func checkDistribution(i int, d *distribution.Distribution) error {
	if d.GetBucketOptions() == nil {
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: Bucket options must be specified for Distribution metric", i)
	}
	if n := len(d.GetBucketCounts()); n > maxDistributionBuckets {
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: Distribution metric has too many buckets", i)
	}
	if len(d.GetBucketCounts()) > 0 {
		var sum int64
		for _, c := range d.GetBucketCounts() {
			sum += c
		}
		if sum != d.GetCount() {
			return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: Distribution value has |bucket_counts| fields that sum to %d which does not equal the |count| field value of %d", i, sum, d.GetCount())
		}
		if want := bucketCount(d.GetBucketOptions()); want > 0 && len(d.GetBucketCounts()) > want {
			return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: Distribution value has %d bucket counts, more than the %d its bucket options define", i, len(d.GetBucketCounts()), want)
		}
	}
	return nil
}

// bucketCount is how many buckets (with the underflow and overflow ones)
// bucket options define.
func bucketCount(b *distribution.Distribution_BucketOptions) int {
	switch o := b.GetOptions().(type) {
	case *distribution.Distribution_BucketOptions_LinearBuckets:
		return int(o.LinearBuckets.GetNumFiniteBuckets()) + 2
	case *distribution.Distribution_BucketOptions_ExponentialBuckets:
		return int(o.ExponentialBuckets.GetNumFiniteBuckets()) + 2
	case *distribution.Distribution_BucketOptions_ExplicitBuckets:
		return len(o.ExplicitBuckets.GetBounds()) + 1
	}
	return 0
}

// writeProm records a Managed Service for Prometheus point, as Google's
// collectors write them (prombuiltin.go): a prometheus.googleapis.com type
// of a known kind, on prometheus_target, of the kind's value type.
func (s *Server) writeProm(i int, ts *monitoringpb.TimeSeries) error {
	t := ts.GetMetric().GetType()
	name, kind, ok := promKindOf(t)
	if !ok {
		return apierror.InvalidArgument("Field timeSeries[%d].metric.type had an invalid value of %q: a Prometheus metric type is prometheus.googleapis.com/NAME/KIND", i, t)
	}
	r := ts.GetResource()
	if r.GetType() != PromTarget {
		return apierror.InvalidArgument("Field timeSeries[%d].resource.type had an invalid value of %q: Prometheus metrics are written on %s", i, r.GetType(), PromTarget)
	}
	for k := range r.GetLabels() {
		if !hasResourceLabel(PromTarget, k) {
			return apierror.InvalidArgument("Field timeSeries[%d].resource.labels had an invalid value of %q: unrecognized resource label %q", i, k, k)
		}
	}
	p := ts.GetPoints()[0]
	want := promKindFor(name, kind)
	if vt := pointValueType(p.GetValue()); vt != want.value {
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].value had an invalid value: value type for metric %s must be %s, but is %s", i, t, want.value, vt)
	}
	if p.GetInterval().GetEndTime() == nil {
		return apierror.InvalidArgument("Field timeSeries[%d].points[0].interval.endTime had an invalid value: Request was missing field interval.endTime", i)
	}
	pt := PromPoint{Type: t, Labels: ts.GetMetric().GetLabels(), Resource: r.GetLabels(), End: p.GetInterval().GetEndTime().AsTime()}
	if st := p.GetInterval().GetStartTime(); st != nil {
		pt.Start = st.AsTime()
	}
	if d := p.GetValue().GetDistributionValue(); d != nil {
		pt.Histogram = &PromHistogram{Bounds: d.GetBucketOptions().GetExplicitBuckets().GetBounds(), Counts: d.GetBucketCounts(),
			Count: d.GetCount(), Sum: d.GetMean() * float64(d.GetCount())}
	} else {
		pt.Value = p.GetValue().GetDoubleValue()
	}
	s.prom.Record(pt)
	return nil
}
