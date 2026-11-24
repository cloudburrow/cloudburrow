package monitoring

// ---- Compute Engine: compute.googleapis.com/instance/cpu/utilization (#2139) ----
//
// Google's metric (docs.cloud.google.com/monitoring/api/metrics_gcp_c,
// the compute section): "The fraction of the allocated CPU that is
// currently in use on the instance", a DOUBLE GAUGE on the gce_instance
// resource (project_id, instance_id, zone) with the instance_name metric
// label, sampled every 60 seconds. It is read from a ComputeSource, the
// Compute Engine API's own CPU readings of its VMs (the ones its
// autoscalers scale on), one point per VM per sample.

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

// ComputeCPUUtilization is the VM CPU utilization metric type.
const ComputeCPUUtilization = "compute.googleapis.com/instance/cpu/utilization"

// InstanceCPU is one VM's CPU utilization over [Start, End].
type InstanceCPU struct {
	Project, Zone, InstanceID, InstanceName string
	Utilization                             float64
	Start, End                              time.Time
}

// ComputeSource reads the running VMs' last CPU utilizations.
type ComputeSource interface {
	InstanceCPUs() []InstanceCPU
}

type cpuSeries struct {
	cpu    InstanceCPU
	points []runPoint
}

// ComputeBuiltin serves compute.googleapis.com/instance/cpu/utilization.
type ComputeBuiltin struct {
	src ComputeSource

	mu     sync.Mutex
	series map[string]*cpuSeries
	// last is each instance's last point's end, so a reading not yet
	// refreshed is not added twice.
	last map[string]time.Time
}

// NewComputeBuiltin returns Compute Engine's CPU metric over src.
func NewComputeBuiltin(src ComputeSource) *ComputeBuiltin {
	return &ComputeBuiltin{src: src, series: map[string]*cpuSeries{}, last: map[string]time.Time{}}
}

// Sample adds each VM's newest reading as a point.
func (c *ComputeBuiltin) Sample() {
	cpus := c.src.InstanceCPUs()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range cpus {
		k := u.Project + "\x00" + u.Zone + "\x00" + u.InstanceID
		if !u.End.After(c.last[k]) {
			continue
		}
		c.last[k] = u.End
		s := c.series[k]
		if s == nil {
			s = &cpuSeries{}
			c.series[k] = s
		}
		s.cpu = u
		// A GAUGE point is at an instant: the end of the reading.
		s.points = append(s.points, runPoint{start: u.End, end: u.End,
			value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: u.Utilization}}})
		if len(s.points) > requestRing {
			s.points = s.points[len(s.points)-requestRing:]
		}
	}
}

func (c *ComputeBuiltin) Descriptors(project string) []*metricpb.MetricDescriptor {
	return []*metricpb.MetricDescriptor{{Name: descriptorName(project, ComputeCPUUtilization), Type: ComputeCPUUtilization,
		MetricKind: gauge, ValueType: tDouble, Unit: "10^2.%", DisplayName: "CPU utilization",
		MonitoredResourceTypes: []string{"gce_instance"},
		Labels:                 []*labelpb.LabelDescriptor{{Key: "instance_name", Description: "The name of the VM instance."}},
		Description: "Fractional utilization of allocated CPU on this instance. Values are typically numbers between 0.0 and 1.0 " +
			"(but some machine types allow bursting above 1.0). Charts display the values as a percentage between 0% and 100% " +
			"(or more). This metric is reported by the hypervisor for the VM and can differ from `agent.googleapis.com/cpu/utilization`, " +
			"which is reported from inside the VM. Sampled every 60 seconds. After sampling, data is not visible for up to 240 seconds."}}
}

func (c *ComputeBuiltin) Series(project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	if metricType != ComputeCPUUtilization {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*monitoringpb.TimeSeries
	for _, k := range keys {
		s := c.series[k]
		if s.cpu.Project != project {
			continue
		}
		ts := &monitoringpb.TimeSeries{
			Metric: &metricpb.Metric{Type: metricType, Labels: map[string]string{"instance_name": s.cpu.InstanceName}},
			Resource: &monitoredres.MonitoredResource{Type: "gce_instance", Labels: map[string]string{
				"project_id": s.cpu.Project, "instance_id": s.cpu.InstanceID, "zone": s.cpu.Zone}},
			MetricKind: gauge, ValueType: tDouble, Unit: "10^2.%"}
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
