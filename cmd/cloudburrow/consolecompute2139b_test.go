package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// settleRegion waits until no operation of the region's zones, or the
// region itself, is running: a regional group's VMs are spread.
func settleRegion(t *testing.T, s computeScreen, project, region string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		busy := false
		for _, scope := range append([]string{"regions/" + region}, func() []string {
			var out []string
			for _, z := range compute.Zones(region) {
				out = append(out, "zones/"+z)
			}
			return out
		}()...) {
			l, err := s.list(project, scope+"/operations")
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range l {
				busy = busy || vpcStr(op, "status") != "DONE"
			}
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("operations never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func c2139bAction(acts []console.Action, id string) *console.Action {
	for i := range acts {
		if acts[i].ID == id {
			return &acts[i]
		}
	}
	return nil
}

// The Instance groups screens (#2139): Create instance group's Location
// (Single zone, Multiple zones with Region, Zones and Target distribution
// shape), Autoscaling and Port mapping sections; the list's Autoscaling
// and Location; a group's page with its Autoscaling, Named ports and VMs;
// Update VMs, Edit autoscaling, Edit port mapping, Resize refused while
// autoscaled, Start, Stop, Suspend, Resume and Delete instance over the
// checked VMs, and Restart/replace VMs on a regional group.
func TestConsoleComputeInstanceGroups2139b(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "c2139b"
	tpls := computeTemplatesProvider{s}
	for _, n := range []string{"tpl", "tpl-2"} {
		if _, err := tpls.Create(ctx, project, map[string]string{"name": n, "location": "Global", "machineType": "e2-small"}); err != nil {
			t.Fatal(err)
		}
	}
	groups := computeGroupsProvider{s}

	// The form.
	gf := groups.CreateFormFor(ctx, project)
	if f := vpcField(gf, "locationType"); f == nil || len(f.UnavailableOptions) != 0 || f.Section != "Location" {
		t.Fatalf("Location = %+v", f)
	}
	if f := vpcField(gf, "targetShape"); f == nil || strings.Join(f.Options, ",") != "EVEN,BALANCED,ANY,ANY_SINGLE_ZONE" ||
		f.OptionLabels["ANY_SINGLE_ZONE"] != "Any single zone" || f.ShowWhen != "locationType="+groupMultipleZones {
		t.Fatalf("Target distribution shape = %+v", f)
	}
	if f := vpcField(gf, "autoscaling"); f == nil || f.Default != autoscaleOn || len(f.DisabledOptions) != 0 ||
		strings.Join(f.Options, "|") != autoscaleOn+"|"+autoscaleScaleOut+"|"+autoscaleOff || f.Section != "Autoscaling" {
		t.Fatalf("Autoscaling mode = %+v", f)
	}
	for _, n := range []string{"minReplicas", "maxReplicas", "cpuTarget", "initPeriod", "scaleIn", "scaleInMax", "scaleInUnit", "scaleInWindow", "zones", "namedPorts"} {
		if vpcField(gf, n) == nil {
			t.Errorf("Create instance group has no %s", n)
		}
	}
	if f := vpcField(gf, "namedPorts"); f.Label != "Port mapping" || f.AddLabel != "Add port" || f.Group[0].Label != "Port name" || f.Group[1].Label != "Port numbers" {
		t.Errorf("Port mapping = %+v", f)
	}

	// Port mapping is checked as the API checks it.
	for _, bad := range []string{`[{"name":"http","ports":"0"}]`, `[{"name":"http","ports":"65536"}]`, `[{"name":"Http","ports":"80"}]`,
		`[{"name":"http-","ports":"80"}]`, `[{"name":"` + strings.Repeat("a", 64) + `","ports":"80"}]`} {
		if _, err := parseNamedPorts(bad); err == nil {
			t.Errorf("Port mapping %s is accepted", bad)
		}
	}
	if np, err := parseNamedPorts(`[{"name":"http","ports":"80, 8080"},{"name":"https","ports":"443"}]`); err != nil || len(np) != 3 {
		t.Fatalf("Port mapping = %v, %v", np, err)
	}

	// A zonal group, autoscaled, with a named port.
	if _, err := groups.Create(ctx, project, map[string]string{"kind": groupManaged, "name": "web", "template": "global/tpl", "locationType": groupSingleZone,
		"zone": "us-central1-a", "autoscaling": autoscaleOn, "minReplicas": "2", "maxReplicas": "5", "cpuTarget": "70", "initPeriod": "90",
		"scaleIn": "true", "scaleInMax": "50", "scaleInUnit": "%", "scaleInWindow": "20",
		"namedPorts": `[{"name":"http","ports":"80"}]`}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	a, err := s.call("GET", project, "zones/us-central1-a/autoscalers/web", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pol := vmMap(a, "autoscalingPolicy")
	if vpcNum(pol, "minNumReplicas") != 2 || vpcNum(pol, "maxNumReplicas") != 5 || vpcNum(pol, "coolDownPeriodSec") != 90 ||
		vpcNum(vmMap(pol, "cpuUtilization"), "utilizationTarget") != 0.7 || vpcStr(pol, "mode") != "ON" ||
		vpcNum(vmMap(vmMap(pol, "scaleInControl"), "maxScaledInReplicas"), "percent") != 50 || vpcNum(vmMap(pol, "scaleInControl"), "timeWindowSec") != 1200 {
		t.Fatalf("web's autoscaler = %v", pol)
	}
	gl, _ := groups.List(ctx, project)
	r := vpcRow(gl, "web")
	if r == nil || r.Fields["Autoscaling"] != "On: Target CPU utilization 70%" || r.Fields["Location"] != "us-central1-a" || r.Fields["Instances"] != "2" {
		t.Fatalf("web's row = %+v", r)
	}
	if rs := c2139bAction(r.Actions, "resize"); rs == nil || rs.Unavailable == "" {
		t.Errorf("Resize is offered while autoscaled: %+v", rs)
	}
	path := []string{"us-central1-a", "web"}
	d, err := groups.Detail(ctx, project, path)
	if err != nil {
		t.Fatal(err)
	}
	props := c2139Props(d)
	for _, want := range []string{"Autoscaling mode=On", "Minimum number of instances=2", "Maximum number of instances=5", "Autoscaling signals=CPU utilization: 70%",
		"Initialization period=90 seconds", "Scale-in controls=Don't scale in by more than 50% over the last 20 minutes", "Named ports=http:80"} {
		if !strings.Contains(props, want) {
			t.Errorf("web's page lacks %s: %s", want, props)
		}
	}
	vms := d.Sections[0].Listing
	if vms.Total != 2 || !vms.Items[0].Selectable || vpcActionIDs(vms.SelectActions) != "startvms,stopvms,suspendvms,resumevms,deletevms" ||
		vms.Items[0].Fields["Template"] != "tpl" {
		t.Fatalf("web's VMs = %+v", vms)
	}
	acts := groups.DetailActions(ctx, project, path)
	if got := vpcActionIDs(acts); got != "resize,restart,updatevms,editautoscaling,editports,delete,startvms,stopvms,suspendvms,resumevms,deletevms" {
		t.Fatalf("web's actions = %s", got)
	}
	if f := vpcField(c2139bAction(acts, "editautoscaling").Fields, "cpuTarget"); f.Default != "70" {
		t.Errorf("Edit autoscaling's target = %+v", f)
	}

	// Update VMs: new VMs use tpl-2; the existing keep tpl.
	if err := groups.ActAt(ctx, project, path, "updatevms", map[string]string{"template": "global/tpl-2"}); err != nil {
		t.Fatal(err)
	}
	mgr, _ := s.call("GET", project, "zones/us-central1-a/instanceGroupManagers/web", nil, nil)
	if !strings.HasSuffix(vpcStr(mgr, "instanceTemplate"), "/tpl-2") {
		t.Fatalf("web's template = %s", vpcStr(mgr, "instanceTemplate"))
	}
	d, _ = groups.Detail(ctx, project, path)
	if d.Sections[0].Listing.Items[0].Fields["Template"] != "tpl" {
		t.Errorf("an existing VM's template changed: %+v", d.Sections[0].Listing.Items[0])
	}

	// Edit port mapping.
	if err := groups.ActAt(ctx, project, path, "editports", map[string]string{"namedPorts": `[{"name":"http","ports":"80,8080"}]`}); err != nil {
		t.Fatal(err)
	}
	if d, _ := groups.Detail(ctx, project, path); !strings.Contains(c2139Props(d), "Named ports=http:80,8080") {
		t.Errorf("web's named ports = %s", c2139Props(d))
	}
	if err := groups.ActAt(ctx, project, path, "editports", map[string]string{"namedPorts": `[{"name":"http","ports":"99999"}]`}); err == nil {
		t.Error("port 99999 is accepted")
	}

	// Edit autoscaling: Off, then Resize works.
	if err := groups.ActAt(ctx, project, path, "editautoscaling", map[string]string{"autoscaling": autoscaleOff, "minReplicas": "2", "maxReplicas": "5"}); err != nil {
		t.Fatal(err)
	}
	gl, _ = groups.List(ctx, project)
	if r := vpcRow(gl, "web"); r.Fields["Autoscaling"] != "Off" || c2139bAction(r.Actions, "resize").Unavailable != "" {
		t.Fatalf("web after Off = %+v", r)
	}
	if err := groups.ActAt(ctx, project, path, "resize", map[string]string{"size": "3"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)

	// The checked VMs.
	d, _ = groups.Detail(ctx, project, path)
	first, second := d.Sections[0].Listing.Items[0].Name, d.Sections[0].Listing.Items[1].Name
	if err := groups.ActAt(ctx, project, path, "stopvms", map[string]string{groupVMsField: first + "\n" + second}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if o, _ := s.call("GET", project, "zones/us-central1-a/instances/"+first, nil, nil); vpcStr(o, "status") != "TERMINATED" {
		t.Errorf("%s after Stop = %s", first, vpcStr(o, "status"))
	}
	if err := groups.ActAt(ctx, project, path, "startvms", map[string]string{groupVMsField: first}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := groups.ActAt(ctx, project, path, "suspendvms", map[string]string{groupVMsField: first}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := groups.ActAt(ctx, project, path, "resumevms", map[string]string{groupVMsField: first}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := groups.ActAt(ctx, project, path, "deletevms", map[string]string{groupVMsField: second}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := groups.ActAt(ctx, project, path, "stopvms", map[string]string{groupVMsField: "not-mine"}); err == nil {
		t.Error("a VM not of the group is acted on")
	}
	mgr, _ = s.call("GET", project, "zones/us-central1-a/instanceGroupManagers/web", nil, nil)
	if vpcNum(mgr, "targetSize") != 2 {
		t.Errorf("web's target size after Delete instance = %v", mgr["targetSize"])
	}

	// A regional group across two zones, Even.
	if _, err := groups.Create(ctx, project, map[string]string{"kind": groupManaged, "name": "reg", "template": "global/tpl", "locationType": groupMultipleZones,
		"region": "us-central1", "zones": "us-central1-b, us-central1-c", "targetShape": "EVEN", "autoscaling": autoscaleOff, "size": "4"}); err != nil {
		t.Fatal(err)
	}
	settleRegion(t, s, project, "us-central1")
	gl, _ = groups.List(ctx, project)
	if r := vpcRow(gl, "reg"); r == nil || r.Fields["Location"] != "us-central1" || r.Fields["Instances"] != "4" || strings.Join(r.Target, "/") != "us-central1/reg" {
		t.Fatalf("reg's row = %+v", r)
	}
	rpath := []string{"us-central1", "reg"}
	d, _ = groups.Detail(ctx, project, rpath)
	if p := c2139Props(d); !strings.Contains(p, "Region=us-central1") || !strings.Contains(p, "Zones=us-central1-b, us-central1-c") ||
		!strings.Contains(p, "Target distribution shape=Even") {
		t.Fatalf("reg's page = %s", p)
	}
	perZone := map[string]int{}
	for _, it := range d.Sections[0].Listing.Items {
		perZone[it.Fields["Zone"]]++
	}
	if perZone["us-central1-b"] != 2 || perZone["us-central1-c"] != 2 {
		t.Fatalf("reg's VMs by zone = %v", perZone)
	}
	// Restart/replace VMs resets each VM in its own zone.
	if err := groups.ActAt(ctx, project, rpath, "restart", map[string]string{"operation": "Restart"}); err != nil {
		t.Fatal(err)
	}
	settleRegion(t, s, project, "us-central1")
	if err := groups.ActAt(ctx, project, rpath, "restart", map[string]string{"operation": "Replace"}); err != nil {
		t.Fatal(err)
	}
	settleRegion(t, s, project, "us-central1")
	// Autoscaling turned on from Edit autoscaling makes the autoscaler.
	if err := groups.ActAt(ctx, project, rpath, "editautoscaling", map[string]string{"autoscaling": autoscaleScaleOut, "minReplicas": "1", "maxReplicas": "6"}); err != nil {
		t.Fatal(err)
	}
	gl, _ = groups.List(ctx, project)
	if r := vpcRow(gl, "reg"); r.Fields["Autoscaling"] != "Only scale out: Target CPU utilization 60%" {
		t.Fatalf("reg's autoscaling = %+v", r.Fields)
	}
	for _, p := range [][]string{rpath, path} {
		if err := groups.ActAt(ctx, project, p, "delete", nil); err != nil {
			t.Fatal(err)
		}
	}
	settleRegion(t, s, project, "us-central1")
	if gl, _ := groups.List(ctx, project); gl.Total != 0 {
		t.Fatalf("groups after Delete: %+v", gl.Items)
	}
}
