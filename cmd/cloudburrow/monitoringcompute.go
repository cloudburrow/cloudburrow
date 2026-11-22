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
