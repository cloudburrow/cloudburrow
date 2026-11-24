package monitoring

import (
	"testing"
	"time"
)

type fakeComputeSource struct{ cpus []InstanceCPU }

func (f *fakeComputeSource) InstanceCPUs() []InstanceCPU { return f.cpus }

// TestComputeBuiltinServesCPUUtilization (#2139): each VM's reading is
// one GAUGE point on gce_instance, taken once however often it is sampled.
func TestComputeBuiltinServesCPUUtilization(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	src := &fakeComputeSource{cpus: []InstanceCPU{{Project: "p", Zone: "us-central1-a", InstanceID: "123", InstanceName: "vm1",
		Utilization: 0.25, Start: t0.Add(-time.Minute), End: t0}, {Project: "other", Zone: "us-central1-a", InstanceID: "9", InstanceName: "x", End: t0}}}
	b := NewComputeBuiltin(src)
	if ds := b.Descriptors("p"); len(ds) != 1 || ds[0].GetType() != ComputeCPUUtilization || ds[0].GetMetricKind() != gauge || ds[0].GetValueType() != tDouble ||
		ds[0].GetMonitoredResourceTypes()[0] != "gce_instance" {
		t.Fatalf("descriptors = %v", ds)
	}
	b.Sample()
	b.Sample()
	src.cpus[0].Utilization, src.cpus[0].End = 0.75, t0.Add(time.Minute)
	b.Sample()
	ts, err := b.Series("p", ComputeCPUUtilization, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil || len(ts) != 1 {
		t.Fatalf("series = %v, %v", ts, err)
	}
	s := ts[0]
	r := s.GetResource()
	if r.GetType() != "gce_instance" || r.GetLabels()["instance_id"] != "123" || r.GetLabels()["zone"] != "us-central1-a" || r.GetLabels()["project_id"] != "p" ||
		s.GetMetric().GetLabels()["instance_name"] != "vm1" || len(s.GetPoints()) != 2 || s.GetPoints()[1].GetValue().GetDoubleValue() != 0.75 {
		t.Fatalf("series = %v", s)
	}
}
