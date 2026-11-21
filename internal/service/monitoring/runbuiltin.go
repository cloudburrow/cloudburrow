package monitoring

// ---- Cloud Run: run.googleapis.com/* (#2049) ----
//
// Google's Cloud Run metrics (docs.cloud.google.com/monitoring/api/
// metrics_gcp_p_z, the run section): request_count and request_latencies,
// DELTA and sampled every 60 seconds, and container/instance_count, a
// GAUGE by state, each on the cloud_run_revision resource. They are read
// from a RunSource, the cluster's own counters for each revision, sampled
// once a minute as RequestBuiltin samples the API counters: each sample's
// change since the last is one DELTA point, and each sample's instance
// count one GAUGE point.

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	RunRequestCount     = "run.googleapis.com/request_count"
	RunRequestLatencies = "run.googleapis.com/request_latencies"
	RunInstanceCount    = "run.googleapis.com/container/instance_count"
)

// RunRevision is one revision's readings: its counters since they started
// (they restart from zero when the component counting them restarts) and
// its instances now.
type RunRevision struct {
	// Project, Location, Service, Revision and Configuration are the
	// cloud_run_revision resource's labels.
	Project, Location, Service, Revision, Configuration string
	// Requests are the requests answered, by HTTP response code.
	Requests map[int]uint64
	// LatencyBounds are the latency histogram's upper bounds in
	// milliseconds; LatencyBuckets the cumulative count at or under each
	// (one per bound); LatencyCount and LatencySum the requests timed and
	// their total milliseconds.
	LatencyBounds  []float64
	LatencyBuckets []uint64
	LatencyCount   uint64
	LatencySum     float64
	// Active and Idle are the revision's container instances by state.
	Active, Idle int64
}

// RunSource reads every revision's counters.
type RunSource interface {
	RunRevisions() ([]RunRevision, error)
}

type runPoint struct {
	start, end time.Time
	value      *monitoringpb.TypedValue
}

type runSeries struct {
	metric   string
	labels   map[string]string
	resource map[string]string
	points   []runPoint
}

// RunBuiltin serves run.googleapis.com/* from a RunSource.
type RunBuiltin struct {
	src RunSource
	now func() time.Time
	// minGap is the shortest gap between two samples: a read samples first,
	// unless the last sample is this recent.
	minGap time.Duration

	mu     sync.Mutex
	last   map[string]RunRevision
	lastAt time.Time
	series map[string]*runSeries
}

// NewRunBuiltin returns Cloud Run's metrics, sampled from src; the
// counters' values when it starts are where its points start.
func NewRunBuiltin(src RunSource, now func() time.Time) *RunBuiltin {
	r := &RunBuiltin{src: src, now: now, minGap: 10 * time.Second, last: map[string]RunRevision{}, series: map[string]*runSeries{}}
	r.lastAt = now()
	if revs, err := src.RunRevisions(); err == nil {
		for _, rv := range revs {
			r.last[runKey(rv)] = rv
		}
	}
	return r
}

func runKey(rv RunRevision) string {
	return rv.Project + "\x00" + rv.Location + "\x00" + rv.Service + "\x00" + rv.Revision
}

func runResource(rv RunRevision) map[string]string {
	return map[string]string{"project_id": rv.Project, "location": rv.Location, "service_name": rv.Service,
		"revision_name": rv.Revision, "configuration_name": rv.Configuration}
}

func (r *RunBuiltin) add(metric string, labels map[string]string, rv RunRevision, p runPoint) {
	key := metric + "\x00" + runKey(rv)
	lk := make([]string, 0, len(labels))
	for k := range labels {
		lk = append(lk, k)
	}
	sort.Strings(lk)
	for _, k := range lk {
		key += "\x00" + k + "=" + labels[k]
	}
	s := r.series[key]
	if s == nil {
		s = &runSeries{metric: metric, labels: labels, resource: runResource(rv)}
		r.series[key] = s
	}
	s.points = append(s.points, p)
	if len(s.points) > requestRing {
		s.points = s.points[len(s.points)-requestRing:]
	}
}

// since is a counter's change from prev to cur: cur itself when it went
// down, the counter having restarted from zero.
func since(cur, prev uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// Sample records the counters' change since the last sample, and the
// instances now.
func (r *RunBuiltin) Sample() {
	r.sample(false)
}

func (r *RunBuiltin) sample(onRead bool) {
	now := r.now()
	r.mu.Lock()
	recent := onRead && now.Sub(r.lastAt) < r.minGap
	r.mu.Unlock()
	if recent {
		return
	}
	revs, err := r.src.RunRevisions()
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !now.After(r.lastAt) {
		return
	}
	for _, rv := range revs {
		k := runKey(rv)
		prev, seen := r.last[k]
		r.last[k] = rv
		r.add(RunInstanceCount, map[string]string{"state": "active"}, rv,
			runPoint{start: now, end: now, value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: rv.Active}}})
		r.add(RunInstanceCount, map[string]string{"state": "idle"}, rv,
			runPoint{start: now, end: now, value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: rv.Idle}}})
		if !seen {
			// A revision first seen now is new since the last sample:
			// everything it counted is.
			prev = RunRevision{}
		}
		codes := make([]int, 0, len(rv.Requests))
		for c := range rv.Requests {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			n := since(rv.Requests[c], prev.Requests[c])
			if n == 0 {
				continue
			}
			r.add(RunRequestCount, runCodeLabels(c), rv, runPoint{start: r.lastAt, end: now,
				value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: int64(n)}}})
		}
		count := since(rv.LatencyCount, prev.LatencyCount)
		if count == 0 || len(rv.LatencyBounds) == 0 {
			continue
		}
		restarted := rv.LatencyCount < prev.LatencyCount
		sum := rv.LatencySum
		if !restarted {
			sum -= prev.LatencySum
		}
		counts := make([]int64, len(rv.LatencyBounds)+1)
		var below uint64
		for i := range rv.LatencyBounds {
			var c, p uint64
			if i < len(rv.LatencyBuckets) {
				c = rv.LatencyBuckets[i]
			}
			if !restarted && i < len(prev.LatencyBuckets) {
				p = prev.LatencyBuckets[i]
			}
			cum := since(c, p)
			counts[i] = int64(cum - min(cum, below))
			below = cum
		}
		counts[len(rv.LatencyBounds)] = int64(count - min(count, below))
		d := &distribution.Distribution{Count: int64(count), Mean: sum / float64(count), BucketCounts: counts,
			BucketOptions: &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
				ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{Bounds: rv.LatencyBounds}}}}
		r.add(RunRequestLatencies, map[string]string{}, rv, runPoint{start: r.lastAt, end: now, value: distValue(d)})
	}
	r.lastAt = now
}

func runCodeLabels(code int) map[string]string {
	return map[string]string{"response_code": strconv.Itoa(code), "response_code_class": strconv.Itoa(code/100) + "xx", "route": ""}
}

func (r *RunBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	codes := []*labelpb.LabelDescriptor{
		{Key: "response_code", ValueType: labelpb.LabelDescriptor_INT64, Description: "Response code of a request."},
		{Key: "response_code_class", Description: "Response code class of a request."},
		{Key: "route", Description: "Always empty."},
	}
	resources := []string{"cloud_run_instance", "cloud_run_revision"}
	return []*metricpb.MetricDescriptor{
		{Name: descriptorName(project, RunRequestCount), Type: RunRequestCount, MetricKind: delta, ValueType: tInt, Unit: "1",
			DisplayName: "Request Count", Labels: codes, MonitoredResourceTypes: resources,
			Description: "Number of requests reaching the revision. Excludes requests that are not reaching your container instances " +
				"(e.g. unauthorized requests or when maximum number of instances is reached). Captured at the end of the request " +
				"lifecycle. Sampled every 60 seconds. After sampling, data is not visible for up to 120 seconds."},
		{Name: descriptorName(project, RunRequestLatencies), Type: RunRequestLatencies, MetricKind: delta, ValueType: tDist, Unit: "ms",
			DisplayName: "Request Latency", Labels: codes, MonitoredResourceTypes: resources,
			Description: "Distribution of request latency in milliseconds reaching the revision. Latency is measured from when the " +
				"request reaches the running container to when it exits. Notably, it does not include container startup latency. " +
				"Sampled every 60 seconds. After sampling, data is not visible for up to 120 seconds."},
		{Name: descriptorName(project, RunInstanceCount), Type: RunInstanceCount, MetricKind: gauge, ValueType: tInt, Unit: "1",
			DisplayName: "Instance Count", MonitoredResourceTypes: append(resources, "cloud_run_worker_pool"),
			Labels: []*labelpb.LabelDescriptor{{Key: "state", Description: `Whether a container is "active" or "idle".`}},
			Description: "Number of container instances that exist, broken down by state. Sampled every 60 seconds. " +
				"After sampling, data is not visible for up to 120 seconds."},
	}
}

func (r *RunBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if metricType != RunRequestCount && metricType != RunRequestLatencies && metricType != RunInstanceCount {
		return nil, nil
	}
	r.sample(true)
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(r.series))
	for k := range r.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*monitoringpb.TimeSeries
	for _, k := range keys {
		s := r.series[k]
		if s.metric != metricType || s.resource["project_id"] != project {
			continue
		}
		labels := map[string]string{}
		for k, v := range s.labels {
			labels[k] = v
		}
		res := map[string]string{}
		for k, v := range s.resource {
			res[k] = v
		}
		ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: labels},
			Resource:   &monitoredres.MonitoredResource{Type: "cloud_run_revision", Labels: res},
			MetricKind: delta, ValueType: tInt, Unit: "1"}
		switch metricType {
		case RunRequestLatencies:
			ts.ValueType, ts.Unit = tDist, "ms"
		case RunInstanceCount:
			ts.MetricKind = gauge
		}
		for _, p := range s.points {
			if p.end.Before(start) || p.end.After(end) {
				continue
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(p.start), EndTime: timestamppb.New(p.end)}, Value: p.value})
		}
		if len(ts.Points) > 0 {
			out = append(out, ts)
		}
	}
	return out, nil
}
