package run

import (
	"encoding/json"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
)

func poolFromJSON(t *testing.T, body string) *runpb.WorkerPool {
	t.Helper()
	var wp runpb.WorkerPool
	if err := runOverlay.Unmarshal([]byte(body), &wp); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return &wp
}

func TestWorkerPoolScalingFieldsRoundTripOverJSON(t *testing.T) {
	body := `{"scaling":{"scalingMode":"AUTOMATIC","minInstanceCount":2,"maxInstanceCount":5,"cpuScaling":{"cpuUtilization":0.7}}}`
	wp := poolFromJSON(t, body)
	p := scalingOf(wp.GetScaling())
	if !p.automatic() || p.min != 2 || p.max != 5 || p.cpu == nil || *p.cpu != float32(0.7) {
		t.Fatalf("scalingOf = %+v", p)
	}
	if got := poolInstances(wp); got != 2 {
		t.Errorf("poolInstances = %d, want the minimum, 2", got)
	}
	// The generated message is untouched: the fields are unknown to it.
	if wp.GetScaling().ManualInstanceCount != nil || len(wp.GetScaling().ProtoReflect().GetUnknown()) == 0 {
		t.Errorf("scaling = %v, want only unknown fields", wp.GetScaling())
	}
	out, err := runOverlay.Marshal(wp)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(out, &got)
	_ = json.Unmarshal([]byte(body), &want)
	if g, w := mustJSON(got["scaling"]), mustJSON(want["scaling"]); g != w {
		t.Errorf("scaling = %s, want %s", g, w)
	}
	// Over gRPC the message decodes as the proto defines it.
	raw, _ := proto.Marshal(wp)
	var grpcSide runpb.WorkerPool
	if err := proto.Unmarshal(raw, &grpcSide); err != nil || grpcSide.GetScaling().ManualInstanceCount != nil {
		t.Errorf("gRPC decode = %v, %v", &grpcSide, err)
	}
	// cpuUtilization 0.0 is set (it disables the threshold), not absent.
	if p := scalingOf(poolFromJSON(t, `{"scaling":{"cpuScaling":{"cpuUtilization":0}}}`).GetScaling()); p.cpu == nil || *p.cpu != 0 {
		t.Errorf("cpu 0 read as %v", p.cpu)
	}
	if err := runOverlay.Unmarshal([]byte(`{"scaling":{"scalingMode":"SOMETIMES"}}`), &runpb.WorkerPool{}); err == nil {
		t.Error("an unknown scalingMode decoded")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestWorkerPoolScalingLimits(t *testing.T) {
	for _, tc := range []struct {
		scaling, err string
	}{
		{`{"scalingMode":"MANUAL","manualInstanceCount":3}`, ""},
		{`{"scalingMode":"AUTOMATIC","minInstanceCount":1,"maxInstanceCount":1}`, ""},
		{`{"scalingMode":"AUTOMATIC","minInstanceCount":3}`, ""},
		{`{"cpuScaling":{"cpuUtilization":0.1}}`, ""},
		{`{"cpuScaling":{"cpuUtilization":0.9}}`, ""},
		{`{"cpuScaling":{"cpuUtilization":0}}`, ""},
		{`{"cpuScaling":{"cpuUtilization":0.09}}`, "cpuUtilization"},
		{`{"cpuScaling":{"cpuUtilization":0.95}}`, "cpuUtilization"},
		{`{"cpuScaling":{"cpuUtilization":-0.5}}`, "cpuUtilization"},
		{`{"minInstanceCount":-1}`, "minInstanceCount"},
		{`{"maxInstanceCount":-1}`, "maxInstanceCount"},
		{`{"minInstanceCount":4,"maxInstanceCount":3}`, "must not exceed"},
		{`{"manualInstanceCount":-1}`, "manualInstanceCount"},
	} {
		err := checkPoolScaling(poolFromJSON(t, `{"scaling":`+tc.scaling+`}`).GetScaling())
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.scaling, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: err = %v, want one naming %s", tc.scaling, err, tc.err)
		}
	}
}

func TestWorkerPoolScalingSplitsAndDefaults(t *testing.T) {
	wp := poolFromJSON(t, `{"name":"projects/p/locations/l/workerPools/w","scaling":{"scalingMode":"AUTOMATIC","minInstanceCount":4,"maxInstanceCount":9},
		"instanceSplits":[{"type":"INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST","percent":50},{"revision":"w-00001-abc","percent":50}]}`)
	spec := workerPoolSpec(wp, 2, "projects/p/locations/l/workerPools/w/revisions/w-00002-def")
	if spec.GetScaling().ManualInstanceCount != nil {
		t.Errorf("an AUTOMATIC pool was given manualInstanceCount %d", spec.GetScaling().GetManualInstanceCount())
	}
	if got := RevisionInstances(spec); got["projects/p/locations/l/workerPools/w/revisions/w-00002-def"] != 2 || got["projects/p/locations/l/workerPools/w/revisions/w-00001-abc"] != 2 {
		t.Errorf("RevisionInstances = %v, want the minimum split 2/2", got)
	}
	if p := scalingOf(spec.GetScaling()); p.max != 9 {
		t.Errorf("the stored spec lost maxInstanceCount: %+v", p)
	}
	manual := workerPoolSpec(poolFromJSON(t, `{"scaling":{"scalingMode":"MANUAL"}}`), 1, "r")
	if manual.GetScaling().GetManualInstanceCount() != defaultWorkerPoolInstances {
		t.Errorf("a MANUAL pool with no count got %d", manual.GetScaling().GetManualInstanceCount())
	}
}

func TestWorkerPoolMaskNamesScalingFields(t *testing.T) {
	current := poolFromJSON(t, `{"name":"n","description":"keep","scaling":{"scalingMode":"MANUAL","manualInstanceCount":2}}`)
	in := poolFromJSON(t, `{"name":"n","description":"drop","scaling":{"scalingMode":"AUTOMATIC","minInstanceCount":1,"maxInstanceCount":3}}`)
	out, err := mergeWorkerPoolMask(current, in, []string{"scaling.scaling_mode", "scaling.max_instance_count"})
	if err != nil {
		t.Fatal(err)
	}
	p := scalingOf(out.GetScaling())
	if !p.automatic() || p.max != 3 || p.min != 0 || out.GetDescription() != "keep" || out.GetScaling().GetManualInstanceCount() != 2 {
		t.Errorf("merged = %v (%+v)", out, p)
	}
	if _, err := mergeWorkerPoolMask(current, in, []string{"scaling.surge"}); err == nil {
		t.Error("an unknown scaling path merged")
	}
}

func TestWorkerPoolScalingMessageRoundTrip(t *testing.T) {
	cpu := float32(0.5)
	in := WorkerPoolScaling{Mode: "AUTOMATIC", Min: 1, Max: 3, CPU: &cpu}
	sc, err := in.Message()
	if err != nil {
		t.Fatal(err)
	}
	// Over the gRPC wire and back, as the console sends it.
	raw, _ := proto.Marshal(sc)
	var back runpb.WorkerPoolScaling
	_ = proto.Unmarshal(raw, &back)
	got := ReadWorkerPoolScaling(&back)
	if got.Mode != "AUTOMATIC" || got.Min != 1 || got.Max != 3 || got.CPU == nil || *got.CPU != cpu || got.Manual != nil {
		t.Errorf("read back %+v", got)
	}
	if _, err := (WorkerPoolScaling{Mode: "AUTOMATIC", Min: 4, Max: 2}).Message(); err == nil {
		t.Error("a minimum above the maximum was accepted")
	}
	if _, err := (WorkerPoolScaling{Mode: "SOMETIMES"}).Message(); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
