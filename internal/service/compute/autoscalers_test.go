package compute

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// fakeCPU is a CPU source whose load the test sets: each VM uses util of
// its vCPUs from the moment it is set, by the fixture's clock.
type fakeCPU struct {
	mu   sync.Mutex
	now  func() time.Time
	util map[string]float64 // by VM name; unset is idle
	secs map[string]float64
	at   map[string]time.Time
}

func (f *fakeCPU) read(_ context.Context, vm VMSpec) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if last, ok := f.at[vm.Name]; ok {
		f.secs[vm.Name] += f.util[vm.Name] * now.Sub(last).Seconds() * float64(vm.CPUs)
	}
	f.at[vm.Name] = now
	return f.secs[vm.Name], nil
}

// setAll sets every VM's load, those made later included.
func (f *fakeCPU) setAll(s *Server, u float64) {
	s.mu.Lock()
	var names []string
	for k, o := range s.res {
		if strings.Contains(k, "/instances/") {
			names = append(names, str(o, "name"))
		}
	}
	s.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.util["*"] = u
	for _, n := range names {
		f.util[n] = u
	}
}

type scaleFixture struct {
	t    *testing.T
	s    *Server
	now  *time.Time
	cpu  *fakeCPU
	call func(method, path string, q map[string][]string, body obj) obj
}

func newScaleFixture(t *testing.T) *scaleFixture {
	t.Helper()
	now := time.Date(2026, 11, 22, 8, 30, 0, 0, time.UTC)
	f := &scaleFixture{t: t, now: &now}
	f.cpu = &fakeCPU{now: func() time.Time { return *f.now }, util: map[string]float64{}, secs: map[string]float64{}, at: map[string]time.Time{}}
	s, err := New(Options{VMs: fakeVMs{free: 1 << 40}, Now: func() time.Time { return *f.now }, CPU: func(ctx context.Context, vm VMSpec) (float64, error) {
		f.cpu.mu.Lock()
		if _, ok := f.cpu.util[vm.Name]; !ok {
			f.cpu.util[vm.Name] = f.cpu.util["*"]
		}
		f.cpu.mu.Unlock()
		return f.cpu.read(ctx, vm)
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.call = func(method, path string, q map[string][]string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, "p2139", path, q, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		if method != http.MethodGet {
			waitIdle(t, s)
		}
		return out
	}
	f.call("POST", "global/instanceTemplates", nil, obj{"name": "web", "properties": obj{"machineType": "e2-small",
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}}}})
	return f
}

// pass moves the clock on by d and runs the autoscalers.
func (f *scaleFixture) pass(d time.Duration) {
	f.t.Helper()
	*f.now = f.now.Add(d)
	f.s.autoscaleOnce(context.Background())
	waitIdle(f.t, f.s)
}

// passes runs a pass every minute for d.
func (f *scaleFixture) passes(d time.Duration) {
	f.t.Helper()
	for i := time.Duration(0); i < d; i += time.Minute {
		f.pass(time.Minute)
	}
}

func (f *scaleFixture) size(mig string) int {
	f.t.Helper()
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return len(f.s.liveManagedLocked("projects/p2139/zones/us-central1-a/instanceGroupManagers/" + mig))
}

func (f *scaleFixture) mig(name string, size int, policy obj) {
	f.t.Helper()
	f.call("POST", "zones/us-central1-a/instanceGroupManagers", nil, obj{"name": name, "instanceTemplate": "global/instanceTemplates/web", "targetSize": float64(size)})
	f.call("POST", "zones/us-central1-a/autoscalers", nil, obj{"name": name, "target": "zones/us-central1-a/instanceGroupManagers/" + name, "autoscalingPolicy": policy})
}

func (f *scaleFixture) autoscaler(name string) obj {
	f.t.Helper()
	return f.call("GET", "zones/us-central1-a/autoscalers/"+name, nil, nil)
}

func hasDetail(a obj, typ string) bool {
	for _, d := range listOf(a["statusDetails"]) {
		if str(d.(map[string]any), "type") == typ {
			return true
		}
	}
	return false
}

// TestAutoscalerScalesOutAndIn (#2139): CPU above the target adds VMs at
// once (those still initializing left out of the signal), up to
// maxNumReplicas; CPU below it removes them only once the 10-minute
// stabilization period has passed, down to minNumReplicas.
func TestAutoscalerScalesOutAndIn(t *testing.T) {
	f := newScaleFixture(t)
	f.mig("web", 1, obj{"minNumReplicas": 1.0, "maxNumReplicas": 4.0, "coolDownPeriodSec": 60.0,
		"cpuUtilization": obj{"utilizationTarget": 0.5}})
	a := f.autoscaler("web")
	pol := a["autoscalingPolicy"].(map[string]any)
	if a["status"] != "ACTIVE" || pol["mode"] != "ON" || pol["cpuUtilization"].(map[string]any)["predictiveMethod"] != "NONE" {
		t.Fatalf("autoscaler = %v", a)
	}
	if m := f.call("GET", "zones/us-central1-a/instanceGroupManagers/web", nil, nil); !strings.HasSuffix(str(m["status"].(map[string]any), "autoscaler"), "/autoscalers/web") {
		t.Fatalf("manager's status.autoscaler: %v", m["status"])
	}
	// A busy VM still in its initialization period does not count.
	f.cpu.setAll(f.s, 1)
	f.pass(20 * time.Second)
	f.pass(20 * time.Second)
	if f.size("web") != 1 {
		t.Fatalf("scaled on an initializing VM: %d", f.size("web"))
	}
	// Past it: 1 VM at 100% with a 50% target is 2 VMs.
	f.pass(30 * time.Second)
	if f.size("web") != 2 || f.autoscaler("web")["recommendedSize"] != 2.0 {
		t.Fatalf("scale out: size %d, %v", f.size("web"), f.autoscaler("web")["recommendedSize"])
	}
	// Both busy: 4, then capped at 4.
	f.pass(90 * time.Second)
	f.pass(30 * time.Second)
	if f.size("web") != 4 {
		t.Fatalf("second scale out: %d", f.size("web"))
	}
	f.pass(90 * time.Second)
	f.pass(30 * time.Second)
	if a := f.autoscaler("web"); f.size("web") != 4 || !hasDetail(a, "CAPPED_AT_MAX_NUM_REPLICAS") {
		t.Fatalf("capped: %d, %v", f.size("web"), a["statusDetails"])
	}
	// Idle: nothing removed within the stabilization period...
	f.cpu.setAll(f.s, 0.05)
	f.passes(8 * time.Minute)
	if f.size("web") != 4 {
		t.Fatalf("scaled in within the stabilization period: %d", f.size("web"))
	}
	// ...then down to the minimum.
	f.passes(4 * time.Minute)
	if f.size("web") != 1 || f.autoscaler("web")["recommendedSize"] != 1.0 {
		t.Fatalf("scale in: %d, %v", f.size("web"), f.autoscaler("web")["recommendedSize"])
	}
	// A zonal group's resize is overridden at the next pass.
	f.call("POST", "zones/us-central1-a/instanceGroupManagers/web/resize", map[string][]string{"size": {"3"}}, nil)
	if f.size("web") != 3 {
		t.Fatalf("resize: %d", f.size("web"))
	}
	f.passes(12 * time.Minute)
	if f.size("web") != 1 {
		t.Fatalf("resize not overridden: %d", f.size("web"))
	}
	// Deleting the group leaves the autoscaler in ERROR.
	f.call("DELETE", "zones/us-central1-a/instanceGroupManagers/web", nil, nil)
	if a := f.autoscaler("web"); a["status"] != "ERROR" || !hasDetail(a, "SCALING_TARGET_DOES_NOT_EXIST") {
		t.Fatalf("orphan autoscaler: %v", a)
	}
	f.call("DELETE", "zones/us-central1-a/autoscalers/web", nil, nil)
}

// TestAutoscalerMinimumAndScaleInControls (#2139): minNumReplicas raises
// an idle group; scaleInControl removes at most maxScaledInReplicas below
// the peak of its time window.
func TestAutoscalerMinimumAndScaleInControls(t *testing.T) {
	f := newScaleFixture(t)
	f.mig("web", 1, obj{"minNumReplicas": 2.0, "maxNumReplicas": 6.0, "cpuUtilization": obj{"utilizationTarget": 0.5},
		"scaleInControl": obj{"maxScaledInReplicas": obj{"fixed": 1.0}, "timeWindowSec": 1200.0}})
	f.pass(15 * time.Second)
	if f.size("web") != 2 {
		t.Fatalf("raised to the minimum: %d", f.size("web"))
	}
	f.cpu.setAll(f.s, 1)
	f.passes(6 * time.Minute)
	if f.size("web") != 6 {
		t.Fatalf("scale out to the maximum: %d", f.size("web"))
	}
	f.cpu.setAll(f.s, 0)
	f.passes(12 * time.Minute)
	if got := f.size("web"); got != 5 {
		t.Fatalf("scale in past the stabilization period: %d, want 5 (6 less maxScaledInReplicas 1)", got)
	}
	// The window's peak of 6 was last seen when the scale in began: 20
	// minutes on, one more may go.
	f.passes(21 * time.Minute)
	if got := f.size("web"); got >= 5 {
		t.Fatalf("scale in after the time window: %d", got)
	}
	a := f.autoscaler("web")
	sc := a["autoscalingPolicy"].(map[string]any)["scaleInControl"].(map[string]any)
	if sc["maxScaledInReplicas"].(map[string]any)["calculated"] != 1.0 {
		t.Errorf("calculated = %v", sc)
	}
}

// TestAutoscalerModes (#2139): OFF makes recommendations but never acts;
// ONLY_SCALE_OUT adds VMs and never removes them; patch and update.
func TestAutoscalerModes(t *testing.T) {
	f := newScaleFixture(t)
	f.mig("off", 1, obj{"maxNumReplicas": 3.0, "mode": "OFF", "cpuUtilization": obj{"utilizationTarget": 0.5}})
	f.mig("out", 1, obj{"maxNumReplicas": 3.0, "mode": "ONLY_SCALE_OUT", "cpuUtilization": obj{"utilizationTarget": 0.5}})
	f.cpu.setAll(f.s, 1)
	f.pass(90 * time.Second)
	f.pass(30 * time.Second)
	off := f.autoscaler("off")
	if f.size("off") != 1 || off["recommendedSize"] != 2.0 || !hasDetail(off, "MODE_OFF") {
		t.Fatalf("OFF: size %d, %v", f.size("off"), off)
	}
	if f.size("out") != 2 || !hasDetail(f.autoscaler("out"), "MODE_ONLY_SCALE_OUT") {
		t.Fatalf("ONLY_SCALE_OUT out: %d", f.size("out"))
	}
	f.cpu.setAll(f.s, 0)
	f.passes(15 * time.Minute)
	if out := f.autoscaler("out"); f.size("out") != 2 || out["recommendedSize"] != 1.0 {
		t.Fatalf("ONLY_SCALE_OUT in: %d, %v", f.size("out"), out["recommendedSize"])
	}
	// Patch turns "off" ON; it then follows its recommendation.
	f.call("PATCH", "zones/us-central1-a/autoscalers", map[string][]string{"autoscaler": {"off"}}, obj{"autoscalingPolicy": obj{"mode": "ON"}})
	if p := f.autoscaler("off")["autoscalingPolicy"].(map[string]any); p["mode"] != "ON" || p["maxNumReplicas"] != 3.0 || p["cpuUtilization"].(map[string]any)["utilizationTarget"] != 0.5 {
		t.Fatalf("patched policy = %v", p)
	}
	// Update replaces the policy: the target falls back to 0.6.
	f.call("PUT", "zones/us-central1-a/autoscalers", map[string][]string{"autoscaler": {"out"}}, obj{"name": "out",
		"target": "zones/us-central1-a/instanceGroupManagers/out", "autoscalingPolicy": obj{"maxNumReplicas": 2.0}})
	if p := f.autoscaler("out")["autoscalingPolicy"].(map[string]any); p["mode"] != "ON" || p["cpuUtilization"].(map[string]any)["utilizationTarget"] != 0.6 || p["coolDownPeriodSec"] != 60.0 {
		t.Fatalf("updated policy = %v", p)
	}
	f.cpu.setAll(f.s, 0)
	f.passes(12 * time.Minute)
	if f.size("off") != 1 || f.size("out") != 1 {
		t.Fatalf("after ON: off %d, out %d", f.size("off"), f.size("out"))
	}
	// The VMs' readings are the CPU metric's points.
	if smp := f.s.CPUSamples(); len(smp) == 0 || smp[0].Project != "p2139" || smp[0].Zone != "us-central1-a" || smp[0].InstanceID == "" {
		t.Fatalf("CPU samples = %+v", smp)
	}
}

// TestAutoscalerValidation (#2139): the documented limits.
func TestAutoscalerValidation(t *testing.T) {
	f := newScaleFixture(t)
	f.call("POST", "zones/us-central1-a/instanceGroupManagers", nil, obj{"name": "web", "instanceTemplate": "global/instanceTemplates/web", "targetSize": 1.0})
	tgt := "zones/us-central1-a/instanceGroupManagers/web"
	for name, pol := range map[string]obj{
		"no max":      {"minNumReplicas": 1.0},
		"max < min":   {"minNumReplicas": 3.0, "maxNumReplicas": 2.0},
		"negative":    {"minNumReplicas": -1.0, "maxNumReplicas": 2.0},
		"target 1.5":  {"maxNumReplicas": 2.0, "cpuUtilization": obj{"utilizationTarget": 1.5}},
		"target 0":    {"maxNumReplicas": 2.0, "cpuUtilization": obj{"utilizationTarget": 0.0}},
		"bad mode":    {"maxNumReplicas": 2.0, "mode": "SOMETIMES"},
		"fixed 0":     {"maxNumReplicas": 2.0, "scaleInControl": obj{"maxScaledInReplicas": obj{"fixed": 0.0}}},
		"percent 101": {"maxNumReplicas": 2.0, "scaleInControl": obj{"maxScaledInReplicas": obj{"percent": 101.0}}},
	} {
		if _, err := f.s.Call("POST", "p2139", "zones/us-central1-a/autoscalers", nil, obj{"name": "a", "target": tgt, "autoscalingPolicy": pol}); ErrorStatus(err) != http.StatusBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.s.Call("POST", "p2139", "zones/us-central1-a/autoscalers", nil, obj{"name": "a", "target": "zones/us-central1-a/instanceGroupManagers/none", "autoscalingPolicy": obj{"maxNumReplicas": 2.0}}); err == nil {
		t.Error("an autoscaler of no group was made")
	}
	f.call("POST", "zones/us-central1-a/autoscalers", nil, obj{"name": "a", "target": tgt, "autoscalingPolicy": obj{"maxNumReplicas": 2.0}})
	if _, err := f.s.Call("POST", "p2139", "zones/us-central1-a/autoscalers", nil, obj{"name": "b", "target": tgt, "autoscalingPolicy": obj{"maxNumReplicas": 2.0}}); err == nil {
		t.Error("a second autoscaler of the group was made")
	}
	if p := f.autoscaler("a")["autoscalingPolicy"].(map[string]any); p["minNumReplicas"] != 1.0 || p["coolDownPeriodSec"] != 60.0 {
		t.Errorf("defaults = %v", p)
	}
}

// TestRegionalGroups (#2139): a regional group's VMs spread EVEN over its
// zones, ANY_SINGLE_ZONE in one, regionInstanceGroups, named ports with
// their fingerprint, setInstanceTemplate for new VMs only, recreate with
// the new template, and an autoscaled regional group's resize refused;
// over the Go client.
func TestRegionalGroups(t *testing.T) {
	ctx := context.Background()
	f := newScaleFixture(t)
	_, opts := newTestServerFrom(t, f.s)
	rigm, err := computeapi.NewRegionInstanceGroupManagersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	op, err := rigm.Insert(ctx, &computepb.InsertRegionInstanceGroupManagerRequest{Project: "p2139", Region: "us-central1",
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String("rmig"), BaseInstanceName: proto.String("rmig"),
			InstanceTemplate: proto.String("global/instanceTemplates/web"), TargetSize: proto.Int32(5),
			NamedPorts: []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(80)}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	waitIdle(t, f.s)
	m, err := rigm.Get(ctx, &computepb.GetRegionInstanceGroupManagerRequest{Project: "p2139", Region: "us-central1", InstanceGroupManager: "rmig"})
	if err != nil || m.GetDistributionPolicy().GetTargetShape() != "EVEN" || len(m.GetDistributionPolicy().GetZones()) != 3 ||
		m.GetUpdatePolicy().GetInstanceRedistributionType() != "PROACTIVE" || !m.GetStatus().GetIsStable() || !strings.HasSuffix(m.GetRegion(), "/regions/us-central1") {
		t.Fatalf("rmig = %v, %v", m, err)
	}
	byZone := map[string]int{}
	it := rigm.ListManagedInstances(ctx, &computepb.ListManagedInstancesRegionInstanceGroupManagersRequest{Project: "p2139", Region: "us-central1", InstanceGroupManager: "rmig"})
	var first string
	for {
		mi, err := it.Next()
		if err != nil {
			break
		}
		parts := strings.Split(mi.GetInstance(), "/")
		byZone[parts[len(parts)-3]]++
		first = mi.GetInstance()
	}
	if len(byZone) != 3 || byZone["us-central1-a"]+byZone["us-central1-b"]+byZone["us-central1-c"] != 5 || byZone["us-central1-a"] < 1 || byZone["us-central1-c"] < 1 {
		t.Fatalf("EVEN = %v", byZone)
	}
	rig, err := computeapi.NewRegionInstanceGroupsRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	g, err := rig.Get(ctx, &computepb.GetRegionInstanceGroupRequest{Project: "p2139", Region: "us-central1", InstanceGroup: "rmig"})
	if err != nil || g.GetSize() != 5 || len(g.GetNamedPorts()) != 1 {
		t.Fatalf("regional group = %v, %v", g, err)
	}
	li := rig.ListInstances(ctx, &computepb.ListInstancesRegionInstanceGroupsRequest{Project: "p2139", Region: "us-central1", InstanceGroup: "rmig",
		RegionInstanceGroupsListInstancesRequestResource: &computepb.RegionInstanceGroupsListInstancesRequest{InstanceState: proto.String("ALL")}})
	n := 0
	for {
		if _, err := li.Next(); err != nil {
			break
		}
		n++
	}
	if n != 5 {
		t.Fatalf("listInstances = %d", n)
	}
	// Named ports: a stale fingerprint, a port out of range and a bad
	// name are refused.
	set := func(fp string, ports ...*computepb.NamedPort) error {
		op, err := rig.SetNamedPorts(ctx, &computepb.SetNamedPortsRegionInstanceGroupRequest{Project: "p2139", Region: "us-central1", InstanceGroup: "rmig",
			RegionInstanceGroupsSetNamedPortsRequestResource: &computepb.RegionInstanceGroupsSetNamedPortsRequest{Fingerprint: proto.String(fp), NamedPorts: ports}})
		if err == nil {
			_ = op.Wait(ctx)
		}
		return err
	}
	if err := set("stale", &computepb.NamedPort{Name: proto.String("http"), Port: proto.Int32(8080)}); err == nil {
		t.Error("a stale fingerprint was taken")
	}
	if err := set(g.GetFingerprint(), &computepb.NamedPort{Name: proto.String("http"), Port: proto.Int32(70000)}); err == nil {
		t.Error("port 70000 was taken")
	}
	if err := set(g.GetFingerprint(), &computepb.NamedPort{Name: proto.String("Bad_Name"), Port: proto.Int32(80)}); err == nil {
		t.Error("a bad port name was taken")
	}
	if err := set(g.GetFingerprint(), &computepb.NamedPort{Name: proto.String("http"), Port: proto.Int32(8080)}, &computepb.NamedPort{Name: proto.String("https"), Port: proto.Int32(8443)}); err != nil {
		t.Fatal(err)
	}
	if m := f.call("GET", "regions/us-central1/instanceGroupManagers/rmig", nil, nil); len(listOf(m["namedPorts"])) != 2 {
		t.Errorf("manager's named ports = %v", m["namedPorts"])
	}

	// setInstanceTemplate: new VMs are of the new template, old ones keep
	// theirs until recreated.
	f.call("POST", "global/instanceTemplates", nil, obj{"name": "web2", "properties": obj{"machineType": "e2-medium",
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}}}})
	op, err = rigm.SetInstanceTemplate(ctx, &computepb.SetInstanceTemplateRegionInstanceGroupManagerRequest{Project: "p2139", Region: "us-central1", InstanceGroupManager: "rmig",
		RegionInstanceGroupManagersSetTemplateRequestResource: &computepb.RegionInstanceGroupManagersSetTemplateRequest{InstanceTemplate: proto.String("global/instanceTemplates/web2")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	f.call("POST", "regions/us-central1/instanceGroupManagers/rmig/resize", map[string][]string{"size": {"6"}}, nil)
	tpls := map[string]int{}
	mis := f.call("POST", "regions/us-central1/instanceGroupManagers/rmig/listManagedInstances", nil, obj{})
	for _, e := range listOf(mis["managedInstances"]) {
		tpls[lastSeg(str(e.(map[string]any)["version"].(map[string]any), "instanceTemplate"))]++
	}
	if tpls["web"] != 5 || tpls["web2"] != 1 {
		t.Fatalf("templates after setInstanceTemplate and resize = %v", tpls)
	}
	rel := Rel(first, "p2139")
	parts := strings.Split(rel, "/")
	f.call("POST", "regions/us-central1/instanceGroupManagers/rmig/recreateInstances", nil, obj{"instances": []any{rel}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.s.mu.Lock()
		o := f.s.res[rel]
		tpl := ""
		if o != nil && o["_deleting"] != true {
			tpl = instanceTemplateOf(o)
		}
		f.s.mu.Unlock()
		if strings.HasSuffix(tpl, "/web2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not recreated from web2", parts[len(parts)-1])
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitIdle(t, f.s)

	// An autoscaled regional group cannot be resized.
	f.call("POST", "regions/us-central1/autoscalers", nil, obj{"name": "rmig", "target": "regions/us-central1/instanceGroupManagers/rmig",
		"autoscalingPolicy": obj{"maxNumReplicas": 8.0, "minNumReplicas": 6.0}})
	if _, err := f.s.Call("POST", "p2139", "regions/us-central1/instanceGroupManagers/rmig/resize", map[string][]string{"size": {"2"}}, nil); err == nil ||
		!strings.Contains(err.Error(), "Resizing of autoscaled regional managed instance groups is not allowed") {
		t.Fatalf("resize of an autoscaled regional group: %v", err)
	}
	ras, err := computeapi.NewRegionAutoscalersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := ras.Get(ctx, &computepb.GetRegionAutoscalerRequest{Project: "p2139", Region: "us-central1", Autoscaler: "rmig"}); err != nil || a.GetAutoscalingPolicy().GetMaxNumReplicas() != 8 {
		t.Fatalf("regional autoscaler = %v, %v", a, err)
	}

	// ANY_SINGLE_ZONE: every VM in one zone; PROACTIVE redistribution
	// with it is refused.
	if _, err := f.s.Call("POST", "p2139", "regions/us-central1/instanceGroupManagers", nil, obj{"name": "one", "instanceTemplate": "global/instanceTemplates/web", "targetSize": 3.0,
		"distributionPolicy": obj{"targetShape": "ANY_SINGLE_ZONE"}, "updatePolicy": obj{"instanceRedistributionType": "PROACTIVE"}}); err == nil {
		t.Error("PROACTIVE redistribution with ANY_SINGLE_ZONE was taken")
	}
	if _, err := f.s.Call("POST", "p2139", "regions/us-central1/instanceGroupManagers", nil, obj{"name": "far", "instanceTemplate": "global/instanceTemplates/web",
		"distributionPolicy": obj{"zones": []any{obj{"zone": "zones/europe-west1-b"}}}}); err == nil {
		t.Error("a zone of another region was taken")
	}
	f.call("POST", "regions/us-central1/instanceGroupManagers", nil, obj{"name": "one", "instanceTemplate": "global/instanceTemplates/web", "targetSize": 3.0,
		"distributionPolicy": obj{"targetShape": "ANY_SINGLE_ZONE", "zones": []any{obj{"zone": "zones/us-central1-b"}, obj{"zone": "zones/us-central1-c"}}}})
	f.s.mu.Lock()
	zones := map[string]bool{}
	for _, vm := range f.s.liveManagedLocked("projects/p2139/regions/us-central1/instanceGroupManagers/one") {
		zones[lastSeg(str(f.s.res[vm], "zone"))] = true
	}
	f.s.mu.Unlock()
	if len(zones) != 1 || !zones["us-central1-b"] {
		t.Fatalf("ANY_SINGLE_ZONE zones = %v", zones)
	}
	if l := f.call("GET", "aggregated/instanceGroupManagers", nil, nil); l["items"].(map[string]any)["regions/us-central1"] == nil {
		t.Errorf("aggregated list without the regional groups: %v", l["items"])
	}
}

// TestManagedVMActions (#2139): a managed group's per-VM Start, Stop,
// Suspend, Resume and Delete, and their currentAction.
func TestManagedVMActions(t *testing.T) {
	f := newScaleFixture(t)
	f.call("POST", "zones/us-central1-a/instanceGroupManagers", nil, obj{"name": "web", "instanceTemplate": "global/instanceTemplates/web", "targetSize": 2.0})
	mis := f.call("POST", "zones/us-central1-a/instanceGroupManagers/web/listManagedInstances", nil, obj{})
	var vms []any
	for _, e := range listOf(mis["managedInstances"]) {
		vms = append(vms, str(e.(map[string]any), "instance"))
		if a := str(e.(map[string]any), "currentAction"); a != "NONE" {
			t.Errorf("currentAction = %s", a)
		}
	}
	base := "zones/us-central1-a/instanceGroupManagers/web/"
	status := func(i int) string {
		return str(f.call("GET", "zones/us-central1-a/instances/"+lastSeg(vms[i].(string)), nil, nil), "status")
	}
	f.call("POST", base+"stopInstances", nil, obj{"instances": vms[:1]})
	if status(0) != stTerminated || status(1) != stRunning {
		t.Fatalf("stop: %s %s", status(0), status(1))
	}
	f.call("POST", base+"startInstances", nil, obj{"instances": vms[:1]})
	f.call("POST", base+"suspendInstances", nil, obj{"instances": vms[1:]})
	if status(0) != stRunning || status(1) != "SUSPENDED" {
		t.Fatalf("start/suspend: %s %s", status(0), status(1))
	}
	f.call("POST", base+"resumeInstances", nil, obj{"instances": vms[1:]})
	f.call("POST", base+"deleteInstances", nil, obj{"instances": vms[1:]})
	if m := f.call("GET", "zones/us-central1-a/instanceGroupManagers/web", nil, nil); m["targetSize"] != 1.0 || f.size("web") != 1 {
		t.Fatalf("after deleteInstances: %v, %d", m["targetSize"], f.size("web"))
	}
	if _, err := f.s.Call("POST", "p2139", base+"stopInstances", nil, obj{"instances": []any{"zones/us-central1-a/instances/stranger"}}); err == nil {
		t.Error("stopInstances of a VM not the group's was taken")
	}
	if _, err := f.s.Call("POST", "p2139", "zones/us-central1-a/instanceGroups/web/setNamedPorts", nil, obj{"namedPorts": []any{obj{"name": "http", "port": 0.0}}}); err == nil {
		t.Error("port 0 was taken")
	}
}

// TestAutoscaleLoopStops (#2139): Stop ends the loops.
func TestAutoscaleLoopStops(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}, AutoscaleEvery: time.Millisecond, CPU: func(context.Context, VMSpec) (float64, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { _ = s.Stop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}
