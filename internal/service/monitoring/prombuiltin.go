package monitoring

// ---- Managed Service for Prometheus: prometheus.googleapis.com/* (#2100) ----
//
// The metrics Managed Service for Prometheus collects, and GKE's metric
// packages collected through it (the control plane's, kube state metrics,
// cAdvisor and the kubelet's), are written to Cloud Monitoring as
// prometheus.googleapis.com/NAME/KIND on the prometheus_target resource
// (re-read 2026-11-22:
// https://docs.cloud.google.com/stackdriver/docs/managed-prometheus/setup-managed,
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/control-plane-metrics):
// a gauge is NAME/gauge (GAUGE, DOUBLE), a counter NAME/counter
// (CUMULATIVE, DOUBLE), a histogram NAME/histogram (CUMULATIVE,
// DISTRIBUTION), a summary's quantiles NAME/summary (GAUGE), its sum
// NAME_sum/summary:counter and count NAME_count/summary (CUMULATIVE), and
// an untyped metric NAME/unknown (GAUGE) and NAME/unknown:counter
// (CUMULATIVE). The resource's labels are project_id, location, cluster,
// namespace, job and instance. They are recorded here by cmd/cloudburrow's
// collector, or written with CreateTimeSeries as Google's collectors write
// them, and read as every other built-in type is: ListTimeSeries, MQL,
// PromQL and the Prometheus HTTP API.

import (
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PromPrefix is the domain of Managed Service for Prometheus' metrics.
const PromPrefix = "prometheus.googleapis.com/"

// PromTarget is the monitored resource Prometheus metrics are written on.
const PromTarget = "prometheus_target"

// promKinds are the kinds of prometheus.googleapis.com metric types.
var promKinds = map[string]struct {
	kind  metricpb.MetricDescriptor_MetricKind
	value metricpb.MetricDescriptor_ValueType
}{
	"gauge": {gauge, tDouble}, "counter": {cumulative, tDouble}, "histogram": {cumulative, tDist},
	"summary": {gauge, tDouble}, "summary:counter": {cumulative, tDouble},
	"unknown": {gauge, tDouble}, "unknown:counter": {cumulative, tDouble},
}

// PromMetricType is the metric type of a Prometheus metric name and kind
// (gauge, counter, histogram, summary, summary:counter, unknown,
// unknown:counter).
func PromMetricType(name, kind string) string { return PromPrefix + name + "/" + kind }

// promKindOf is a prometheus.googleapis.com type's name and kind.
func promKindOf(metricType string) (name, kind string, ok bool) {
	rest, ok := strings.CutPrefix(metricType, PromPrefix)
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		return "", "", false
	}
	name, kind = rest[:i], rest[i+1:]
	_, ok = promKinds[kind]
	return name, kind, ok
}

// promKindFor is a type's kind and value type: a summary's NAME_count is
// cumulative, its quantiles gauges.
func promKindFor(name, kind string) struct {
	kind  metricpb.MetricDescriptor_MetricKind
	value metricpb.MetricDescriptor_ValueType
} {
	k := promKinds[kind]
	if kind == "summary" && strings.HasSuffix(name, "_count") {
		k.kind = cumulative
	}
	return k
}

// PromHistogram is a histogram's reading: Bounds are its finite upper
// bounds, Counts the count in each bucket (len(Bounds)+1, the last the
// overflow), not cumulative.
type PromHistogram struct {
	Bounds []float64
	Counts []int64
	Count  int64
	Sum    float64
}

// PromPoint is one reading of a Prometheus series.
type PromPoint struct {
	// Type is the metric type, PromMetricType's.
	Type   string
	Labels map[string]string
	// Resource are the prometheus_target labels.
	Resource   map[string]string
	Start, End time.Time
	Value      float64
	Histogram  *PromHistogram
}

// promRing bounds each series: six hours of one-minute readings, for the
// many series a cluster's packages hold.
const promRing = 6 * 60

// PromBuiltin serves the prometheus.googleapis.com metrics recorded into it.
type PromBuiltin struct {
	mu     sync.Mutex
	series map[string]*promStored
	// types are the metric types recorded, by project.
	types map[string]map[string]bool
}

type promStored struct {
	typ      string
	labels   map[string]string
	resource map[string]string
	points   []PromPoint
}

// NewPromBuiltin returns an empty PromBuiltin.
func NewPromBuiltin() *PromBuiltin {
	return &PromBuiltin{series: map[string]*promStored{}, types: map[string]map[string]bool{}}
}

// Record adds readings; one whose type is not a Prometheus one is ignored,
// as is one no newer than its series' last.
func (g *PromBuiltin) Record(points ...PromPoint) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range points {
		if _, kind, ok := promKindOf(p.Type); !ok || (kind == "histogram") != (p.Histogram != nil) {
			continue
		}
		project := p.Resource["project_id"]
		key := p.Type + sortedKey(p.Labels) + "\x01" + sortedKey(p.Resource)
		s := g.series[key]
		if s == nil {
			s = &promStored{typ: p.Type, labels: p.Labels, resource: p.Resource}
			g.series[key] = s
			if g.types[project] == nil {
				g.types[project] = map[string]bool{}
			}
			g.types[project][p.Type] = true
		}
		if n := len(s.points); n > 0 && !p.End.After(s.points[n-1].End) {
			continue
		}
		s.points = append(s.points, p)
		if len(s.points) > promRing {
			s.points = s.points[len(s.points)-promRing:]
		}
	}
}

func (g *PromBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	g.mu.Lock()
	types := make([]string, 0, len(g.types[project]))
	for t := range g.types[project] {
		types = append(types, t)
	}
	// The labels each type's series carry.
	labelKeys := map[string]map[string]bool{}
	for _, s := range g.series {
		if s.resource["project_id"] != project {
			continue
		}
		if labelKeys[s.typ] == nil {
			labelKeys[s.typ] = map[string]bool{}
		}
		for k := range s.labels {
			labelKeys[s.typ][k] = true
		}
	}
	g.mu.Unlock()
	sort.Strings(types)
	out := make([]*metricpb.MetricDescriptor, 0, len(types))
	for _, t := range types {
		name, kind, _ := promKindOf(t)
		k := promKindFor(name, kind)
		var ls []*labelpb.LabelDescriptor
		keys := make([]string, 0, len(labelKeys[t]))
		for key := range labelKeys[t] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			ls = append(ls, &labelpb.LabelDescriptor{Key: key, ValueType: labelpb.LabelDescriptor_STRING})
		}
		out = append(out, &metricpb.MetricDescriptor{Name: descriptorName(project, t), Type: t, MetricKind: k.kind,
			ValueType: k.value, Unit: "1", DisplayName: "Prometheus/" + name + "/" + kind, Description: name,
			Labels: ls, MonitoredResourceTypes: []string{PromTarget}})
	}
	return out
}

func (g *PromBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	name, kind, ok := promKindOf(metricType)
	if !ok {
		return nil, nil
	}
	k := promKindFor(name, kind)
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := make([]string, 0)
	for key, s := range g.series {
		if s.typ == metricType && s.resource["project_id"] == project {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var out []*monitoringpb.TimeSeries
	for _, key := range keys {
		s := g.series[key]
		ls := map[string]string{}
		for k, v := range s.labels {
			ls[k] = v
		}
		res := map[string]string{}
		for k, v := range s.resource {
			res[k] = v
		}
		ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: ls},
			Resource: &monitoredres.MonitoredResource{Type: PromTarget, Labels: res}, MetricKind: k.kind, ValueType: k.value, Unit: "1"}
		for _, p := range s.points {
			if p.End.Before(start) || p.End.After(end) {
				continue
			}
			st := p.Start
			if k.kind == gauge || st.IsZero() {
				st = p.End
			}
			v := &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: p.Value}}
			if h := p.Histogram; h != nil {
				d := &distribution.Distribution{Count: h.Count, BucketCounts: h.Counts,
					BucketOptions: &distribution.Distribution_BucketOptions{Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
						ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{Bounds: h.Bounds}}}}
				if h.Count > 0 {
					d.Mean = h.Sum / float64(h.Count)
				}
				v = &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{DistributionValue: d}}
			}
			ts.Points = append(ts.Points, &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(st), EndTime: timestamppb.New(p.End)}, Value: v})
		}
		if len(ts.Points) > 0 {
			out = append(out, ts)
		}
	}
	return out, nil
}
