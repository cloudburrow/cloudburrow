package run

import (
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
)

// The CPU driver's arithmetic (#1789, #1800): usage over allocated CPUs
// against the target, a 10% tolerance, scale-in only after the window.
func TestCPURecommendation(t *testing.T) {
	for _, tc := range []struct {
		g      cpuGroup
		target float64
		want   int32
		ok     bool
	}{
		{cpuGroup{pods: 1, measured: 1, usage: 1, perPod: 1}, 0.6, 2, true},    // 100% of 1 CPU at 60%: ceil(1/0.6)
		{cpuGroup{pods: 2, measured: 2, usage: 2, perPod: 0.5}, 0.5, 8, true},  // 200% per pod at 50%
		{cpuGroup{pods: 3, measured: 3, usage: 1.75, perPod: 1}, 0.6, 3, true}, // 58%: inside the tolerance
		{cpuGroup{pods: 3, measured: 3, usage: 0.01, perPod: 1}, 0.6, 1, true}, // idle: one, as ceil keeps a pod
		{cpuGroup{pods: 3, measured: 3, usage: 0, perPod: 1}, 0.6, 0, true},    // nothing used
		{cpuGroup{pods: 3, measured: 1, usage: 0.1, perPod: 1}, 0.6, 3, true},  // a new pod not yet measured: no scale-in
		{cpuGroup{pods: 2, measured: 1, usage: 1, perPod: 1}, 0.5, 2, true},    // one measured at 100%: 2
		{cpuGroup{pods: 2, measured: 0, perPod: 1}, 0.6, 0, false},             // no sample at all
		{cpuGroup{}, 0.6, 0, true}, // no pods: no CPU demand
	} {
		got, ok := cpuRecommendation(tc.g, tc.target)
		if got != tc.want || ok != tc.ok {
			t.Errorf("cpuRecommendation(%+v, %v) = %d, %v; want %d, %v", tc.g, tc.target, got, ok, tc.want, tc.ok)
		}
	}

	now := time.Unix(1000, 0)
	c := newCPUScaler()
	c.now = func() time.Time { return now }
	if n := c.stabilized("k", 4); n != 4 {
		t.Fatalf("first = %d", n)
	}
	now = now.Add(30 * time.Second)
	if n := c.stabilized("k", 1); n != 4 {
		t.Errorf("scaled in to %d inside the window", n)
	}
	now = now.Add(31 * time.Second)
	if n := c.stabilized("k", 1); n != 1 {
		t.Errorf("after the window = %d, want 1", n)
	}
	if n := c.stabilized("k", 6); n != 6 {
		t.Errorf("scale-out = %d, want 6 at once", n)
	}

	for q, want := range map[string]float64{"250m": 0.25, "1": 1, "1500000000n": 1.5, "2000u": 0.002} {
		if got, err := parseCPU(q); err != nil || got != want {
			t.Errorf("parseCPU(%q) = %v, %v", q, got, err)
		}
	}
	if !c.setFloor("r", 2) || c.floor("r") != 2 || c.setFloor("r", 2) || !c.setFloor("r", 0) || c.floor("r") != 0 {
		t.Error("floors do not record what was set")
	}
	if revisionCPUTarget(nil) != 0.6 || revisionCPUTarget(&runpb.RevisionScaling{}) != 0.6 ||
		revisionCPUTarget(&runpb.RevisionScaling{CpuUtilization: proto.Float32(0)}) != 0 {
		t.Error("a revision's CPU target is not Cloud Run's default, or 0 does not turn it off")
	}
	var none *cpuScaler
	if none.floor("r") != 0 {
		t.Error("a server without the driver has a floor")
	}
}

// An instance's CPU limit is rendered with a small request, so a local node
// can run as many instances as the CPU driver asks for (main run
// 44326182935: each CPU test's service "grew to 1" on a 4-CPU runner).
func TestCPULimitIsRenderedWithASmallRequest(t *testing.T) {
	var b strings.Builder
	cs := []*runpb.Container{{Image: "dev.local/x:1", Resources: &runpb.ResourceRequirements{Limits: map[string]string{"cpu": "1", "memory": "256Mi"}}}}
	if err := renderContainers(&b, cs, containerRender{}); err != nil {
		t.Fatal(err)
	}
	want := "          resources:\n            limits:\n              cpu: \"1\"\n              memory: \"256Mi\"\n            requests:\n              cpu: \"10m\"\n"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("rendered:\n%s\nwant it to hold:\n%s", b.String(), want)
	}
	for limit, want := range map[string]string{"2": "10m", "500m": "10m", "5m": "5m"} {
		if got, ok := cpuRequest(limit); !ok || got != want {
			t.Errorf("cpuRequest(%q) = %q, %v; want %q", limit, got, ok, want)
		}
	}
	if _, ok := cpuRequest(""); ok {
		t.Error("a request was rendered with no CPU limit")
	}
}
