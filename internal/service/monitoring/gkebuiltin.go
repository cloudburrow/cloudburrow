package monitoring

// ---- Kubernetes Engine: kubernetes.io/* system metrics (#2100) ----
//
// GKE's system metrics (re-read 2026-11-21:
// https://docs.cloud.google.com/monitoring/api/metrics_kubernetes), on the
// k8s_container, k8s_pod and k8s_node resources. They are recorded by
// cmd/cloudburrow's GKE metrics sampler from each node's kubelet summary,
// once a minute while a cluster's system metrics are enabled, and served
// here as every other built-in type is: read, never written.

import (
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// gkeMetric is one kubernetes.io metric type GKE writes.
type gkeMetric struct {
	kind     metricpb.MetricDescriptor_MetricKind
	value    metricpb.MetricDescriptor_ValueType
	unit     string
	resource string
	display  string
	desc     string
	labels   []*labelpb.LabelDescriptor
}

var (
	memoryTypeLabel = &labelpb.LabelDescriptor{Key: "memory_type",
		Description: "Either `evictable` or `non-evictable`. Evictable memory is memory that can be easily reclaimed by the kernel, while non-evictable memory cannot."}
	interfaceLabel = &labelpb.LabelDescriptor{Key: "interface", Description: "The name of the network interface."}
)

// GKEMetrics are the kubernetes.io metric types served, by type.
var GKEMetrics = map[string]gkeMetric{
	"kubernetes.io/container/cpu/core_usage_time": {cumulative, tDouble, "s{CPU}", "k8s_container", "CPU usage time",
		"Cumulative CPU usage on all cores used by the container in seconds. Sampled every 60 seconds.", nil},
	"kubernetes.io/container/memory/used_bytes": {gauge, tInt, "By", "k8s_container", "Memory usage",
		"Memory usage in bytes. Sampled every 60 seconds.", []*labelpb.LabelDescriptor{memoryTypeLabel}},
	"kubernetes.io/container/ephemeral_storage/used_bytes": {gauge, tInt, "By", "k8s_container", "Ephemeral storage usage",
		"Local ephemeral storage usage in bytes. Sampled every 60 seconds.", nil},
	"kubernetes.io/pod/network/received_bytes_count": {cumulative, tInt, "By", "k8s_pod", "Bytes received",
		"Cumulative number of bytes received by the pod over the network. Sampled every 60 seconds.", []*labelpb.LabelDescriptor{interfaceLabel}},
	"kubernetes.io/pod/network/sent_bytes_count": {cumulative, tInt, "By", "k8s_pod", "Bytes transmitted",
		"Cumulative number of bytes transmitted by the pod over the network. Sampled every 60 seconds.", []*labelpb.LabelDescriptor{interfaceLabel}},
	"kubernetes.io/pod/ephemeral_storage/used_bytes": {gauge, tInt, "By", "k8s_pod", "Ephemeral storage usage",
		"Local ephemeral storage usage in bytes by the pod. Sampled every 60 seconds.", nil},
	"kubernetes.io/node/cpu/core_usage_time": {cumulative, tDouble, "s{CPU}", "k8s_node", "CPU usage time",
		"Cumulative CPU usage on all cores used on the node in seconds. Sampled every 60 seconds.", nil},
	"kubernetes.io/node/memory/used_bytes": {gauge, tInt, "By", "k8s_node", "Memory usage",
		"Memory usage in bytes. Sampled every 60 seconds.", []*labelpb.LabelDescriptor{memoryTypeLabel}},
	"kubernetes.io/node/network/received_bytes_count": {cumulative, tInt, "By", "k8s_node", "Bytes received",
		"Cumulative number of bytes received by the node over the network. Sampled every 60 seconds.", nil},
	"kubernetes.io/node/network/sent_bytes_count": {cumulative, tInt, "By", "k8s_node", "Bytes transmitted",
		"Cumulative number of bytes transmitted by the node over the network. Sampled every 60 seconds.", nil},
	"kubernetes.io/node/ephemeral_storage/used_bytes": {gauge, tInt, "By", "k8s_node", "Ephemeral storage usage",
		"Local ephemeral storage bytes used by the node. Sampled every 60 seconds.", nil},
	"kubernetes.io/node/ephemeral_storage/total_bytes": {gauge, tInt, "By", "k8s_node", "Total ephemeral storage",
		"Total ephemeral storage bytes on the node. Sampled every 60 seconds.", nil},
	"kubernetes.io/node/ephemeral_storage/allocatable_bytes": {gauge, tInt, "By", "k8s_node", "Allocatable ephemeral storage",
		"Local ephemeral storage bytes allocatable on the node. Sampled every 60 seconds.", nil},
}

// GKEPoint is one reading: a metric of a resource at End, cumulative from
// Start (Start is End for a gauge). Int is the value of an INT64 metric,
// Double of a DOUBLE one.
type GKEPoint struct {
	Metric       string
	MetricLabels map[string]string
	// Resource are the resource's labels; its type is the metric's.
	Resource   map[string]string
	Start, End time.Time
	Int        int64
	Double     float64
}

// gkeRing bounds each series: a day of one-minute readings.
const gkeRing = 24 * 60

// GKEBuiltin serves the kubernetes.io metrics recorded into it.
type GKEBuiltin struct {
	mu     sync.Mutex
	series map[string]*gkeSeries
}

type gkeSeries struct {
	metric   string
	labels   map[string]string
	resource map[string]string
	points   []GKEPoint
}

// NewGKEBuiltin returns an empty GKEBuiltin.
func NewGKEBuiltin() *GKEBuiltin { return &GKEBuiltin{series: map[string]*gkeSeries{}} }

func sortedKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for _, k := range keys {
		out += "\x00" + k + "=" + m[k]
	}
	return out
}

// Record adds readings; one of a metric type not served is ignored.
func (g *GKEBuiltin) Record(points ...GKEPoint) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range points {
		if _, ok := GKEMetrics[p.Metric]; !ok {
			continue
		}
		key := p.Metric + sortedKey(p.MetricLabels) + "\x01" + sortedKey(p.Resource)
		s := g.series[key]
		if s == nil {
			s = &gkeSeries{metric: p.Metric, labels: p.MetricLabels, resource: p.Resource}
			g.series[key] = s
		}
		if n := len(s.points); n > 0 && !p.End.After(s.points[n-1].End) {
			continue
		}
		s.points = append(s.points, p)
		if len(s.points) > gkeRing {
			s.points = s.points[len(s.points)-gkeRing:]
		}
	}
}

func (g *GKEBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	types := make([]string, 0, len(GKEMetrics))
	for t := range GKEMetrics {
		types = append(types, t)
	}
	sort.Strings(types)
	out := make([]*metricpb.MetricDescriptor, 0, len(types))
	for _, t := range types {
		m := GKEMetrics[t]
		out = append(out, &metricpb.MetricDescriptor{Name: descriptorName(project, t), Type: t, MetricKind: m.kind,
			ValueType: m.value, Unit: m.unit, DisplayName: m.display, Description: m.desc, Labels: m.labels,
			MonitoredResourceTypes: []string{m.resource}})
	}
	return out
}

func (g *GKEBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	m, ok := GKEMetrics[metricType]
	if !ok {
		return nil, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := make([]string, 0, len(g.series))
	for k, s := range g.series {
		if s.metric == metricType && s.resource["project_id"] == project {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []*monitoringpb.TimeSeries
	for _, k := range keys {
		s := g.series[k]
		labels := map[string]string{}
		for k, v := range s.labels {
			labels[k] = v
		}
		res := map[string]string{}
		for k, v := range s.resource {
			res[k] = v
		}
		ts := &monitoringpb.TimeSeries{Metric: &metricpb.Metric{Type: metricType, Labels: labels},
			Resource: &monitoredres.MonitoredResource{Type: m.resource, Labels: res}, MetricKind: m.kind, ValueType: m.value, Unit: m.unit}
		for _, p := range s.points {
			if p.End.Before(start) || p.End.After(end) {
				continue
			}
			v := &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: p.Int}}
			if m.value == tDouble {
				v = &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: p.Double}}
			}
			st := p.Start
			if m.kind == gauge || st.IsZero() {
				st = p.End
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
