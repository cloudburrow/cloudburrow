package main

import (
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
)

// The deploy and edit forms' service-level scaling and utilization targets
// (#1800) build the ServiceScaling and RevisionScaling run/v2 defines, and
// the fields prefill from them.
func TestRunServiceScalingFieldsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		values map[string]string
		want   *runpb.ServiceScaling
	}{
		{map[string]string{"serviceScalingMode": "AUTOMATIC", "serviceMinInstances": "1", "serviceMaxInstances": "3"},
			&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_AUTOMATIC, MinInstanceCount: 1, MaxInstanceCount: 3}},
		{map[string]string{"serviceScalingMode": "AUTOMATIC"}, &runpb.ServiceScaling{}},
		{map[string]string{"serviceScalingMode": "MANUAL", "manualInstances": "2"},
			&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(2)}},
		{map[string]string{"serviceScalingMode": "MANUAL", "manualInstances": "0"},
			&runpb.ServiceScaling{ScalingMode: runpb.ServiceScaling_MANUAL, ManualInstanceCount: proto.Int32(0)}},
	} {
		got, ok, err := runFormServiceScaling(tc.values)
		if err != nil || !ok || !proto.Equal(got, tc.want) {
			t.Errorf("%v = %v, %v, %v; want %v", tc.values, got, ok, err, tc.want)
			continue
		}
		back := map[string]string{}
		for _, f := range runServiceScalingFields(got) {
			back[f.Name] = f.Default
		}
		for k, v := range tc.values {
			if back[k] != v {
				t.Errorf("%v prefills %s as %q", tc.values, k, back[k])
			}
		}
	}
	if _, ok, err := runFormServiceScaling(map[string]string{}); ok || err != nil {
		t.Errorf("a request without the fields changed the scaling (%v)", err)
	}
	for _, bad := range []map[string]string{
		{"serviceScalingMode": "MANUAL"},
		{"serviceScalingMode": "MANUAL", "manualInstances": "1", "serviceMaxInstances": "2"},
		{"serviceScalingMode": "AUTOMATIC", "manualInstances": "1"},
		{"serviceScalingMode": "SOMETIMES"},
		{"serviceScalingMode": "AUTOMATIC", "serviceMinInstances": "-1"},
	} {
		if _, _, err := runFormServiceScaling(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}

	tmpl, err := runFormTemplate(map[string]string{"image": "i:1", "cpuUtilization": "0.5", "concurrencyUtilization": "0"})
	if err != nil {
		t.Fatal(err)
	}
	if s := tmpl.GetScaling(); s.CpuUtilization == nil || s.GetCpuUtilization() != 0.5 ||
		s.ConcurrencyUtilization == nil || s.GetConcurrencyUtilization() != 0 {
		t.Errorf("template scaling = %v", s)
	}
	if tmpl, _ := runFormTemplate(map[string]string{"image": "i:1"}); tmpl.GetScaling() != nil {
		t.Errorf("blank targets set scaling %v", tmpl.GetScaling())
	}
	if _, err := runFormTemplate(map[string]string{"image": "i:1", "cpuUtilization": "half"}); err == nil {
		t.Error("a CPU target that is not a number was accepted")
	}
}
