package main

import (
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// monitoringCompute is monitoring.ComputeSource over the Compute Engine
// API's CPU readings of its VMs (#2139), which its autoscalers scale on.
type monitoringCompute struct {
	api func() *compute.Server
}

// InstanceCPUs implements monitoring.ComputeSource.
func (m monitoringCompute) InstanceCPUs() []monitoring.InstanceCPU {
	api := m.api()
	if api == nil {
		return nil
	}
	var out []monitoring.InstanceCPU
	for _, u := range api.CPUSamples() {
		out = append(out, monitoring.InstanceCPU{Project: u.Project, Zone: u.Zone, InstanceID: u.InstanceID,
			InstanceName: u.Name, Utilization: u.Utilization, Start: u.Start, End: u.End})
	}
	return out
}

// sampleComputeMetrics adds compute.googleapis.com/instance/cpu/utilization
// to api and samples it every requestSampleInterval (60 seconds, as Google
// samples it) until stop.
func sampleComputeMetrics(api *monitoring.Server, src monitoring.ComputeSource, stop chan struct{}) {
	b := monitoring.NewComputeBuiltin(src)
	api.AddBuiltin(b)
	t := time.NewTicker(requestSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.Sample()
		case <-stop:
			return
		}
	}
}

// monitoringLB is monitoring.LBSource over the load balancers' custom
// metrics (#2160): every backend group's current value of each custom
// metric, dry-run metrics included, from its endpoints' ORCA reports.
type monitoringLB struct {
	api func() *compute.Server
}

// CustomMetricValues implements monitoring.LBSource.
func (m monitoringLB) CustomMetricValues() []monitoring.LBCustomMetricValue {
	api := m.api()
	if api == nil {
		return nil
	}
	var out []monitoring.LBCustomMetricValue
	for _, v := range api.CustomMetricSamples() {
		out = append(out, monitoring.LBCustomMetricValue{Project: v.Project, BackendServiceID: v.BackendServiceID,
			BackendServiceName: v.BackendService, Location: v.Region, GroupName: v.GroupName, GroupType: v.GroupType,
			Scope: v.Scope, ScopeType: v.ScopeType, Metric: v.Metric, DryRun: v.DryRun, Value: v.Value, At: v.At})
	}
	return out
}

// sampleLBMetrics adds network.googleapis.com/loadbalancer/backend/lb_custom_metric
// to api and samples it every requestSampleInterval (Google exports the
// load balancers' metrics in one-minute batches) until stop.
func sampleLBMetrics(api *monitoring.Server, src monitoring.LBSource, stop chan struct{}) {
	b := monitoring.NewLBBuiltin(src)
	api.AddBuiltin(b)
	t := time.NewTicker(requestSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.Sample()
		case <-stop:
			return
		}
	}
}
