package monitoring

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---- Cloud Logging: log-based metrics and log entry counts ----

// LogMetric is a log-based metric as Cloud Logging holds it.
type LogMetric struct {
	// Name is the metric's ID, as in projects/P/metrics/NAME.
	Name, Description, Unit string
	// Distribution is a distribution metric; otherwise a counter.
	Distribution bool
	Buckets      *distribution.Distribution_BucketOptions
	Labels       []*labelpb.LabelDescriptor
}

// LogSample is one log entry a log-based metric matched: its time, its
// monitored resource, its labels' extracted values and, for a
// distribution, its extracted value (OK false when none could be read).
type LogSample struct {
	Time     time.Time
	Resource *monitoredres.MonitoredResource
	Labels   map[string]string
	Value    float64
	OK       bool
}

// LogEntry is one stored log entry, for logging.googleapis.com/log_entry_count.
type LogEntry struct {
	Time     time.Time
	Log      string // the log ID, decoded
	Severity string
	Resource *monitoredres.MonitoredResource
}

// LogSource is the instance's Cloud Logging.
type LogSource interface {
	// LogMetrics are a project's enabled log-based metrics.
	LogMetrics(project string) []LogMetric
	// LogMetricSamples are the entries metric matched, received after it
	// was created, with timestamps in [from, to].
	LogMetricSamples(project, metric string, from, to time.Time) ([]LogSample, error)
	// LogEntries are a project's stored entries with timestamps in [from, to].
	LogEntries(project string, from, to time.Time) []LogEntry
}

// logBuiltin serves logging.googleapis.com metric types from a LogSource.
// Points are one-minute DELTA intervals, as Cloud Logging reports them.
type logBuiltin struct{ src LogSource }

// NewLogBuiltin is the built-in metrics of a Cloud Logging.
func NewLogBuiltin(src LogSource) Builtin { return logBuiltin{src} }

const (
	logUserPrefix  = "logging.googleapis.com/user/"
	logEntryCount  = "logging.googleapis.com/log_entry_count"
	logPointPeriod = time.Minute
)

func (b logBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	out := []*metricpb.MetricDescriptor{{
		Name: descriptorName(project, logEntryCount), Type: logEntryCount,
		MetricKind: delta, ValueType: tInt, Unit: "1", DisplayName: "Log entries",
		Description: "Number of log entries that contribute to user-defined metrics. Sampled every 60 seconds.",
		Labels: []*labelpb.LabelDescriptor{
			{Key: "log", ValueType: labelpb.LabelDescriptor_STRING, Description: "The name of the log."},
			{Key: "severity", ValueType: labelpb.LabelDescriptor_STRING, Description: "The severity of the log entry."},
		},
		MonitoredResourceTypes: []string{"global", "k8s_container", "cloud_run_revision", "gce_instance", "generic_task", "generic_node"},
	}}
	for _, m := range b.src.LogMetrics(project) {
		md := &metricpb.MetricDescriptor{Name: descriptorName(project, logUserPrefix+m.Name), Type: logUserPrefix + m.Name,
			MetricKind: delta, ValueType: tInt, Unit: m.Unit, Description: m.Description, DisplayName: m.Name}
		if md.Unit == "" {
			md.Unit = "1"
		}
		if m.Distribution {
			md.ValueType = tDist
		}
		for _, l := range m.Labels {
			md.Labels = append(md.Labels, clone(l))
		}
		out = append(out, md)
	}
	return out
}

// minute is the end of the one-minute interval t falls in, (end-1m, end].
func minute(t time.Time) time.Time {
	m := t.Truncate(logPointPeriod)
	if m.Equal(t) {
		return m
	}
	return m.Add(logPointPeriod)
}

func (b logBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	from := start.Add(-logPointPeriod)
	if metricType == logEntryCount {
		groups := map[string]*monitoringpb.TimeSeries{}
		counts := map[string]map[time.Time]int64{}
		for _, e := range b.src.LogEntries(project, from, end) {
			ts := &monitoringpb.TimeSeries{
				Metric:   &metricpb.Metric{Type: logEntryCount, Labels: map[string]string{"log": e.Log, "severity": e.Severity}},
				Resource: resourceOf(e.Resource, project), MetricKind: delta, ValueType: tInt, Unit: "1"}
			k := seriesKey(ts)
			if groups[k] == nil {
				groups[k], counts[k] = ts, map[time.Time]int64{}
			}
			counts[k][minute(e.Time)]++
		}
		return countSeries(groups, counts), nil
	}
	name, ok := strings.CutPrefix(metricType, logUserPrefix)
	if !ok {
		return nil, nil
	}
	var metric *LogMetric
	for _, m := range b.src.LogMetrics(project) {
		if m.Name == name {
			metric = &m
			break
		}
	}
	if metric == nil {
		return nil, nil
	}
	samples, err := b.src.LogMetricSamples(project, name, from, end)
	if err != nil {
		return nil, err
	}
	groups := map[string]*monitoringpb.TimeSeries{}
	if !metric.Distribution {
		counts := map[string]map[time.Time]int64{}
		for _, s := range samples {
			ts := logSeries(metricType, s, project, tInt, metric.Unit)
			k := seriesKey(ts)
			if groups[k] == nil {
				groups[k], counts[k] = ts, map[time.Time]int64{}
			}
			counts[k][minute(s.Time)]++
		}
		return countSeries(groups, counts), nil
	}
	values := map[string]map[time.Time][]float64{}
	for _, s := range samples {
		if !s.OK {
			continue
		}
		ts := logSeries(metricType, s, project, tDist, metric.Unit)
		k := seriesKey(ts)
		if groups[k] == nil {
			groups[k], values[k] = ts, map[time.Time][]float64{}
		}
		values[k][minute(s.Time)] = append(values[k][minute(s.Time)], s.Value)
	}
	var out []*monitoringpb.TimeSeries
	for k, ts := range groups {
		for _, t := range sortedTimes(values[k]) {
			ts.Points = append(ts.Points, minutePoint(t, distValue(newDistribution(metric.Buckets, values[k][t]))))
		}
		out = append(out, ts)
	}
	return out, nil
}

func logSeries(metricType string, s LogSample, project string, vt metricpb.MetricDescriptor_ValueType, unit string) *monitoringpb.TimeSeries {
	labels := map[string]string{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	if unit == "" {
		unit = "1"
	}
	return &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: labels},
		Resource: resourceOf(s.Resource, project), MetricKind: delta, ValueType: vt, Unit: unit}
}

// resourceOf is an entry's monitored resource, global when it has none,
// with its project_id filled in.
func resourceOf(r *monitoredres.MonitoredResource, project string) *monitoredres.MonitoredResource {
	out := &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{}}
	if r.GetType() != "" {
		out.Type = r.GetType()
		for k, v := range r.GetLabels() {
			out.Labels[k] = v
		}
	}
	if out.Labels["project_id"] == "" {
		out.Labels["project_id"] = project
	}
	return out
}

func minutePoint(end time.Time, v *monitoringpb.TypedValue) *monitoringpb.Point {
	return &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{
		StartTime: timestamppb.New(end.Add(-logPointPeriod)), EndTime: timestamppb.New(end)}, Value: v}
}

func sortedTimes[V any](m map[time.Time]V) []time.Time {
	out := make([]time.Time, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func countSeries(groups map[string]*monitoringpb.TimeSeries, counts map[string]map[time.Time]int64) []*monitoringpb.TimeSeries {
	var out []*monitoringpb.TimeSeries
	for k, ts := range groups {
		for _, t := range sortedTimes(counts[k]) {
			ts.Points = append(ts.Points, minutePoint(t, &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: counts[k][t]}}))
		}
		out = append(out, ts)
	}
	return out
}

// newDistribution builds a distribution of values over bucket options
// (one bucket, everything, when there are none).
func newDistribution(opts *distribution.Distribution_BucketOptions, values []float64) *distribution.Distribution {
	if opts == nil {
		opts = &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
			ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{}}}
	}
	d := &distribution.Distribution{BucketOptions: opts, Count: int64(len(values))}
	if len(values) == 0 {
		return d
	}
	n := bucketCount(opts)
	if n <= 0 {
		n = 1
	}
	counts := make([]int64, n)
	lo, hi := math.Inf(1), math.Inf(-1)
	sum := 0.0
	for _, v := range values {
		sum += v
		lo, hi = math.Min(lo, v), math.Max(hi, v)
		counts[bucketIndex(opts, n, v)]++
	}
	d.Mean = sum / float64(len(values))
	for _, v := range values {
		d.SumOfSquaredDeviation += (v - d.Mean) * (v - d.Mean)
	}
	d.Range = &distribution.Distribution_Range{Min: lo, Max: hi}
	d.BucketCounts = counts
	return d
}

func bucketIndex(opts *distribution.Distribution_BucketOptions, n int, v float64) int {
	for i := 0; i < n; i++ {
		lo, hi := bucketBounds(opts, i)
		if v >= lo && v < hi {
			return i
		}
	}
	return n - 1
}

// ---- API request counts and latencies ----

// RequestCount is one counter series of the requests the instance served:
// a service (its short name), a method, a result code (a gRPC code name
// or an HTTP status), the calls, their latencies' sum in seconds, and the
// cumulative latency histogram over Bounds.
type RequestCount struct {
	Service, Method, Code string
	// Project is the project the calls named; "" is the instance's
	// default project's.
	Project string
	Count   uint64
	Sum     float64
	Buckets []uint64
}

// RequestSource is the instance's request counters.
type RequestSource interface {
	Snapshot() []RequestCount
	// Bounds are the latency histogram's upper bounds, in seconds.
	Bounds() []float64
	// API is the googleapis service name of a service's short name, such as
	// pubsub.googleapis.com for pubsub.
	API(service string) string
}

const (
	requestCount     = "serviceruntime.googleapis.com/api/request_count"
	requestLatencies = "serviceruntime.googleapis.com/api/request_latencies"
	// requestRing bounds each request series: a week of one-minute samples.
	requestRing = 7 * 24 * 60
)

type requestDelta struct {
	start, end time.Time
	count      uint64
	sum        float64
	buckets    []uint64
}

// RequestBuiltin serves serviceruntime.googleapis.com/api/* per project
// (the project each call named; a call that named none is the instance's
// default project's) from periodic samples of the request counters: each
// sample's change since the last is one DELTA point. A read takes a sample
// first, so it sees the calls made up to it.
type RequestBuiltin struct {
	src     RequestSource
	project string
	now     func() time.Time

	mu      sync.Mutex
	last    map[string]RequestCount
	lastAt  time.Time
	deltas  map[string][]requestDelta
	headers map[string]RequestCount
}

// NewRequestBuiltin returns the request metrics of project, sampled from
// src, the counters' totals at started being where they start.
func NewRequestBuiltin(src RequestSource, project string, now func() time.Time) *RequestBuiltin {
	r := &RequestBuiltin{src: src, project: project, now: now, last: map[string]RequestCount{},
		deltas: map[string][]requestDelta{}, headers: map[string]RequestCount{}}
	r.lastAt = now()
	for _, c := range src.Snapshot() {
		r.last[requestKey(c)] = c
	}
	return r
}

func requestKey(c RequestCount) string {
	return c.Service + "\x00" + c.Method + "\x00" + c.Code + "\x00" + c.Project
}

// Sample records the counters' change since the last sample.
func (r *RequestBuiltin) Sample() {
	now := r.now()
	snap := r.src.Snapshot()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !now.After(r.lastAt) {
		return
	}
	for _, c := range snap {
		k := requestKey(c)
		prev := r.last[k]
		r.last[k] = c
		if c.Count <= prev.Count {
			continue
		}
		d := requestDelta{start: r.lastAt, end: now, count: c.Count - prev.Count, sum: c.Sum - prev.Sum, buckets: make([]uint64, len(c.Buckets))}
		for i := range c.Buckets {
			d.buckets[i] = c.Buckets[i]
			if i < len(prev.Buckets) {
				d.buckets[i] -= prev.Buckets[i]
			}
		}
		r.headers[k] = c
		r.deltas[k] = append(r.deltas[k], d)
		if len(r.deltas[k]) > requestRing {
			r.deltas[k] = r.deltas[k][len(r.deltas[k])-requestRing:]
		}
	}
	r.lastAt = now
}

func (r *RequestBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	labels := []*labelpb.LabelDescriptor{
		{Key: "protocol", ValueType: labelpb.LabelDescriptor_STRING, Description: "The protocol of the request, e.g. \"http\", \"grpc\"."},
		{Key: "response_code", ValueType: labelpb.LabelDescriptor_INT64, Description: "The HTTP response code for HTTP requests, or HTTP equivalent code for gRPC requests."},
		{Key: "response_code_class", ValueType: labelpb.LabelDescriptor_STRING, Description: "The response code class for HTTP requests, or HTTP equivalent class for gRPC requests."},
		{Key: "grpc_status_code", ValueType: labelpb.LabelDescriptor_STRING, Description: "The numeric gRPC response code for gRPC requests, or gRPC equivalent code for HTTP requests."},
	}
	return []*metricpb.MetricDescriptor{
		{Name: descriptorName(project, requestCount), Type: requestCount, MetricKind: delta, ValueType: tInt, Unit: "1",
			DisplayName: "Request count", Description: "The count of completed requests. Sampled every 60 seconds.",
			Labels: labels, MonitoredResourceTypes: []string{"consumed_api"}},
		{Name: descriptorName(project, requestLatencies), Type: requestLatencies, MetricKind: delta, ValueType: tDist, Unit: "s",
			DisplayName: "Request latencies", Description: "Distribution of latencies in seconds for non-streaming requests. Sampled every 60 seconds.",
			Labels: labels, MonitoredResourceTypes: []string{"consumed_api"}},
	}
}

func (r *RequestBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if metricType != requestCount && metricType != requestLatencies {
		return nil, nil
	}
	r.Sample()
	bounds := r.src.Bounds()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*monitoringpb.TimeSeries
	for k, ds := range r.deltas {
		c := r.headers[k]
		if owner := c.Project; owner != project && (owner != "" || project != r.project) {
			continue
		}
		ts := &monitoringpb.TimeSeries{
			Metric: &metricpb.Metric{Type: metricType, Labels: codeLabels(c.Code)},
			Resource: &monitoredres.MonitoredResource{Type: "consumed_api", Labels: map[string]string{
				"project_id": project, "service": r.src.API(c.Service), "method": c.Method,
				"version": "", "location": "global", "credential_id": ""}},
			MetricKind: delta, ValueType: tInt, Unit: "1"}
		if metricType == requestLatencies {
			ts.ValueType, ts.Unit = tDist, "s"
		}
		for _, d := range ds {
			if d.end.Before(start) || d.end.After(end) {
				continue
			}
			v := &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(d.count)}}
			if metricType == requestLatencies {
				v = distValue(latencyDistribution(bounds, d))
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(d.start), EndTime: timestamppb.New(d.end)}, Value: v})
		}
		if len(ts.Points) > 0 {
			out = append(out, ts)
		}
	}
	return out, nil
}

// latencyDistribution turns a cumulative histogram delta into explicit
// buckets: (-inf, b0), [b0, b1), ..., [bn-1, inf).
func latencyDistribution(bounds []float64, d requestDelta) *distribution.Distribution {
	counts := make([]int64, len(bounds)+1)
	var prev uint64
	for i := range bounds {
		var c uint64
		if i < len(d.buckets) {
			c = d.buckets[i]
		}
		counts[i] = int64(c - prev)
		prev = c
	}
	counts[len(bounds)] = int64(d.count - prev)
	return &distribution.Distribution{Count: int64(d.count), Mean: d.sum / float64(d.count), BucketCounts: counts,
		BucketOptions: &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
			ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{Bounds: bounds}}}}
}

// codeLabels are the request_count labels of a result code: a gRPC code
// name, or an HTTP status.
func codeLabels(code string) map[string]string {
	if n, err := strconv.Atoi(code); err == nil {
		return map[string]string{"protocol": "http", "response_code": code,
			"response_code_class": fmt.Sprintf("%dxx", n/100), "grpc_status_code": strconv.Itoa(int(httpToGRPC(n)))}
	}
	c := codes.Unknown
	for i := codes.OK; i <= codes.Unauthenticated; i++ {
		if strings.EqualFold(i.String(), code) || strings.EqualFold(strings.ReplaceAll(i.String(), "_", ""), strings.ReplaceAll(code, "_", "")) {
			c = i
		}
	}
	h := grpcToHTTP(c)
	return map[string]string{"protocol": "grpc", "response_code": strconv.Itoa(h),
		"response_code_class": fmt.Sprintf("%dxx", h/100), "grpc_status_code": strconv.Itoa(int(c))}
}

// grpcToHTTP is google.rpc.Code's documented HTTP mapping.
func grpcToHTTP(c codes.Code) int {
	switch c {
	case codes.OK:
		return 200
	case codes.Canceled:
		return 499
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return 400
	case codes.DeadlineExceeded:
		return 504
	case codes.NotFound:
		return 404
	case codes.AlreadyExists, codes.Aborted:
		return 409
	case codes.PermissionDenied:
		return 403
	case codes.Unauthenticated:
		return 401
	case codes.ResourceExhausted:
		return 429
	case codes.Unimplemented:
		return 501
	case codes.Unavailable:
		return 503
	}
	return 500
}

func httpToGRPC(n int) codes.Code {
	switch {
	case n < 400:
		return codes.OK
	case n == 400:
		return codes.InvalidArgument
	case n == 401:
		return codes.Unauthenticated
	case n == 403:
		return codes.PermissionDenied
	case n == 404:
		return codes.NotFound
	case n == 409:
		return codes.Aborted
	case n == 429:
		return codes.ResourceExhausted
	case n == 499:
		return codes.Canceled
	case n == 501:
		return codes.Unimplemented
	case n == 503:
		return codes.Unavailable
	case n == 504:
		return codes.DeadlineExceeded
	case n < 500:
		return codes.FailedPrecondition
	}
	return codes.Internal
}
