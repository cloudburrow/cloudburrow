package monitoring

// ---- Cloud Load Balancing: network.googleapis.com/loadbalancer/backend/lb_custom_metric (#2160) ----
//
// The custom metrics of the Application Load Balancers
// (load-balancing/docs/https/applb-custom-metrics, read 2026-11-22): "when
// a metric is configured with dryRun set to true, the metric is reported
// to Monitoring but isn't actually used by the load balancer". The
// load balancers' monitoring pages (https/https-reg-logging-monitoring,
// cdn/docs/cdn-logging-monitoring) name the metric: "Backend load
// balancing custom metrics", network.googleapis.com/loadbalancer/backend/
// lb_custom_metric, "the current utilization by each backend group, based
// on your defined custom metrics", exported in one-minute batches. Google
// documents neither its monitored resource nor its labels: here it is a
// DOUBLE GAUGE on gce_backend_service (project_id, backend_service_id,
// location) with the load balancing pages' filtering dimensions for the
// backend group (backend_name, backend_type, backend_scope,
// backend_scope_type, backend_target_name) and custom_metric_name and
// dry_run for the metric (docs/gaps.md).

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// LBCustomMetric is the load balancers' custom metric type.
const LBCustomMetric = "network.googleapis.com/loadbalancer/backend/lb_custom_metric"

// LBCustomMetricValue is one backend group's current value of one custom
// metric of a backend service.
type LBCustomMetricValue struct {
	Project, BackendServiceID, BackendServiceName, Location string
	GroupName, GroupType, Scope, ScopeType                  string
	Metric                                                  string
	DryRun                                                  bool
	Value                                                   float64
	At                                                      time.Time
}

// LBSource reads the backend groups' custom metric values.
type LBSource interface {
	CustomMetricValues() []LBCustomMetricValue
}

type lbSeries struct {
	v      LBCustomMetricValue
	points []runPoint
}

// LBBuiltin serves network.googleapis.com/loadbalancer/backend/lb_custom_metric.
type LBBuiltin struct {
	src LBSource

	mu     sync.Mutex
	series map[string]*lbSeries
}

// NewLBBuiltin returns the load balancers' custom metric over src.
func NewLBBuiltin(src LBSource) *LBBuiltin {
	return &LBBuiltin{src: src, series: map[string]*lbSeries{}}
}

func lbLabels(v LBCustomMetricValue) map[string]string {
	return map[string]string{"backend_name": v.GroupName, "backend_type": v.GroupType, "backend_scope": v.Scope,
		"backend_scope_type": v.ScopeType, "backend_target_name": v.BackendServiceName, "custom_metric_name": v.Metric,
		"dry_run": strconv.FormatBool(v.DryRun)}
}

// Sample adds each value as a point.
func (b *LBBuiltin) Sample() {
	vals := b.src.CustomMetricValues()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, v := range vals {
		l := lbLabels(v)
		k := v.Project + "\x00" + v.BackendServiceID + "\x00" + v.Location
		for _, n := range []string{"backend_name", "backend_scope", "custom_metric_name", "dry_run"} {
			k += "\x00" + l[n]
		}
		s := b.series[k]
		if s == nil {
			s = &lbSeries{}
			b.series[k] = s
		}
		s.v = v
		if n := len(s.points); n > 0 && !v.At.After(s.points[n-1].end) {
			continue
		}
		s.points = append(s.points, runPoint{start: v.At, end: v.At,
			value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v.Value}}})
		if len(s.points) > requestRing {
			s.points = s.points[len(s.points)-requestRing:]
		}
	}
}

func (b *LBBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	return []*metricpb.MetricDescriptor{{Name: descriptorName(project, LBCustomMetric), Type: LBCustomMetric,
		MetricKind: gauge, ValueType: tDouble, Unit: "1", DisplayName: "Backend load balancing custom metrics",
		MonitoredResourceTypes: []string{"gce_backend_service"},
		Labels: []*labelpb.LabelDescriptor{
			{Key: "backend_name", Description: "The name of the backend instance group, bucket, or NEG."},
			{Key: "backend_type", Description: "The name of the backend group that served the client's request."},
			{Key: "backend_scope", Description: "The Google Cloud scope (region or zone) of the backend service instance group that served the connection."},
			{Key: "backend_scope_type", Description: "The type of the scope of the backend group."},
			{Key: "backend_target_name", Description: "The name of the backend target."},
			{Key: "custom_metric_name", Description: "The name of the custom metric, as in the backend's or backend service's customMetrics."},
			{Key: "dry_run", ValueType: labelpb.LabelDescriptor_BOOL, Description: "Whether the custom metric is a dry-run metric, reported but not used for load balancing."},
		},
		Description: "The current utilization by each backend group, based on your defined custom metrics."}}
}

func (b *LBBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if metricType != LBCustomMetric {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.series))
	for k := range b.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*monitoringpb.TimeSeries
	for _, k := range keys {
		s := b.series[k]
		if s.v.Project != project {
			continue
		}
		ts := &monitoringpb.TimeSeries{
			Metric: &metricpb.Metric{Type: metricType, Labels: lbLabels(s.v)},
			Resource: &monitoredres.MonitoredResource{Type: "gce_backend_service", Labels: map[string]string{
				"project_id": s.v.Project, "backend_service_id": s.v.BackendServiceID, "location": s.v.Location}},
			MetricKind: gauge, ValueType: tDouble, Unit: "1"}
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
