//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestComputeWorkloadPoliciesOnGroupsThroughTheGoClient (#2206), the
// official compute/apiv1 REST client: workload resource policies
// (HIGH_AVAILABILITY; HIGH_THROUGHPUT with maxTopologyDistance, or with
// acceleratorTopology 1x72 and acceleratorTopologyMode) and their
// documented refusals; a zonal managed instance group made with one
// (resourcePolicies.workloadPolicy), its policy changed by patch while it
// has no VMs, a second template and a size above zero refused (the series
// workload policies support need GPUs or TPUs); a group with a VM refused
// a policy; a regional group taking one only with the ANY_SINGLE_ZONE or
// ANY shape; and a policy in use refused deletion.
// covers: compute.resourcePolicies.insert, compute.resourcePolicies.get, compute.resourcePolicies.delete, compute.instanceGroupManagers.insert, compute.instanceGroupManagers.get, compute.instanceGroupManagers.patch, compute.instanceGroupManagers.delete, compute.regionInstanceGroupManagers.insert, compute.regionInstanceGroupManagers.get, compute.regionInstanceGroupManagers.delete
func TestComputeWorkloadPoliciesOnGroupsThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	pols, err := computeapi.NewResourcePoliciesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	tpls, _ := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	igm, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	rigm, _ := computeapi.NewRegionInstanceGroupManagersRESTClient(ctx, opts...)
	p := h.Project()
	const region, zone = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	ha, ht, acc, tpl, tpl2 := "wl-ha-"+sfx, "wl-ht-"+sfx, "wl-acc-"+sfx, "wl-t-"+sfx, "wl-t2-"+sfx
	group, busy, regional := "wl-g-"+sfx, "wl-busy-"+sfx, "wl-r-"+sfx
	groupTemplate(t, ctx, tpls, p, tpl, "")
	groupTemplate(t, ctx, tpls, p, tpl2, "")
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, g := range []string{group, busy} {
			if op, err := igm.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: g}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := rigm.Delete(c, &computepb.DeleteRegionInstanceGroupManagerRequest{Project: p, Region: region, InstanceGroupManager: regional}); err == nil {
			_ = op.Wait(c)
		}
		for _, n := range []string{ha, ht, acc} {
			if op, err := pols.Delete(c, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: n}); err == nil {
				_ = op.Wait(c)
			}
		}
	})
	insertPol := func(name string, wp *computepb.ResourcePolicyWorkloadPolicy) error {
		op, err := pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
			Name: proto.String(name), WorkloadPolicy: wp}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	for _, bad := range []*computepb.ResourcePolicyWorkloadPolicy{
		{Type: proto.String("HIGH_AVAILABILITY"), MaxTopologyDistance: proto.String("BLOCK")},
		{Type: proto.String("HIGH_THROUGHPUT"), AcceleratorTopology: proto.String("1x72"), MaxTopologyDistance: proto.String("BLOCK")},
		{Type: proto.String("HIGH_THROUGHPUT"), AcceleratorTopologyMode: proto.String("SOMETIMES")},
	} {
		if err := insertPol("wl-bad-"+sfx, bad); computeCode(err) != http.StatusBadRequest {
			t.Errorf("workload policy %v: %v, want 400", bad, err)
		}
	}
	if err := insertPol(ha, &computepb.ResourcePolicyWorkloadPolicy{Type: proto.String("HIGH_AVAILABILITY")}); err != nil {
		t.Fatal(err)
	}
	if err := insertPol(ht, &computepb.ResourcePolicyWorkloadPolicy{Type: proto.String("HIGH_THROUGHPUT"), MaxTopologyDistance: proto.String("BLOCK")}); err != nil {
		t.Fatal(err)
	}
	if err := insertPol(acc, &computepb.ResourcePolicyWorkloadPolicy{Type: proto.String("HIGH_THROUGHPUT"),
		AcceleratorTopology: proto.String("1x72"), AcceleratorTopologyMode: proto.String("AUTO_CONNECT")}); err != nil {
		t.Fatal(err)
	}
	if po, err := pols.Get(ctx, &computepb.GetResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: acc}); err != nil ||
		po.GetWorkloadPolicy().GetAcceleratorTopology() != "1x72" || po.GetWorkloadPolicy().GetAcceleratorTopologyMode() != "AUTO_CONNECT" {
		t.Errorf("the accelerator topology policy: %v %v", po.GetWorkloadPolicy(), err)
	}
	polURL := func(n string) string { return "projects/" + p + "/regions/" + region + "/resourcePolicies/" + n }

	// A zonal group made with a workload policy.
	insertGroup := func(name string, size int32, wp string) error {
		g := &computepb.InstanceGroupManager{Name: proto.String(name), BaseInstanceName: proto.String(name),
			InstanceTemplate: proto.String("global/instanceTemplates/" + tpl), TargetSize: proto.Int32(size)}
		if wp != "" {
			g.ResourcePolicies = &computepb.InstanceGroupManagerResourcePolicies{WorkloadPolicy: proto.String(wp)}
		}
		op, err := igm.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManagerResource: g})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	if err := insertGroup(group, 2, polURL(ht)); computeCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "A4X") {
		t.Errorf("a group of e2-small VMs with a workload policy: %v, want 400", err)
	}
	if err := insertGroup(group, 0, "regions/"+region+"/resourcePolicies/"+ht); err != nil {
		t.Fatal(err)
	}
	g, err := igm.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: group})
	if err != nil || !strings.HasSuffix(g.GetResourcePolicies().GetWorkloadPolicy(), polURL(ht)) {
		t.Fatalf("the group's workload policy: %v %v", g.GetResourcePolicies(), err)
	}
	patch := func(name string, m *computepb.InstanceGroupManager) error {
		op, err := igm.Patch(ctx, &computepb.PatchInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: name, InstanceGroupManagerResource: m})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	wait(igm.Patch(ctx, &computepb.PatchInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: group,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{ResourcePolicies: &computepb.InstanceGroupManagerResourcePolicies{WorkloadPolicy: proto.String(polURL(ha))}}}))
	if g, err := igm.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: group}); err != nil ||
		!strings.HasSuffix(g.GetResourcePolicies().GetWorkloadPolicy(), polURL(ha)) {
		t.Errorf("the group's workload policy after the patch: %v %v", g.GetResourcePolicies(), err)
	}
	if err := patch(group, &computepb.InstanceGroupManager{Versions: []*computepb.InstanceGroupManagerVersion{
		{InstanceTemplate: proto.String("global/instanceTemplates/" + tpl)},
		{Name: proto.String("canary"), InstanceTemplate: proto.String("global/instanceTemplates/" + tpl2), TargetSize: &computepb.FixedOrPercent{Fixed: proto.Int32(1)}}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a second instance template: %v, want 400", err)
	}
	if op, err := igm.Resize(ctx, &computepb.ResizeInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: group, Size: 1}); err == nil {
		if err = op.Wait(ctx); computeCode(err) != http.StatusBadRequest {
			t.Errorf("resize to 1: %v, want 400", err)
		}
	} else if computeCode(err) != http.StatusBadRequest {
		t.Errorf("resize to 1: %v, want 400", err)
	}
	if _, err := pols.Delete(ctx, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: ha}); computeCode(err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "is already being used by") {
		t.Errorf("deleting the policy a group uses: %v, want 400", err)
	}

	// A group with a VM can't take one.
	if err := insertGroup(busy, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := patch(busy, &computepb.InstanceGroupManager{ResourcePolicies: &computepb.InstanceGroupManagerResourcePolicies{WorkloadPolicy: proto.String(polURL(ha))}}); computeCode(err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "Workload policy update is not allowed when the MIG has instances.") {
		t.Errorf("a workload policy on a group with a VM: %v, want 400", err)
	}

	// A regional group: ANY_SINGLE_ZONE, not EVEN.
	insertRegional := func(shape string) error {
		op, err := rigm.Insert(ctx, &computepb.InsertRegionInstanceGroupManagerRequest{Project: p, Region: region,
			InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(regional), BaseInstanceName: proto.String(regional),
				InstanceTemplate: proto.String("global/instanceTemplates/" + tpl), TargetSize: proto.Int32(0),
				DistributionPolicy: &computepb.DistributionPolicy{TargetShape: proto.String(shape)},
				ResourcePolicies:   &computepb.InstanceGroupManagerResourcePolicies{WorkloadPolicy: proto.String(polURL(ht))}}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	if err := insertRegional("EVEN"); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a regional EVEN group with a workload policy: %v, want 400", err)
	}
	if err := insertRegional("ANY_SINGLE_ZONE"); err != nil {
		t.Fatal(err)
	}
	if r, err := rigm.Get(ctx, &computepb.GetRegionInstanceGroupManagerRequest{Project: p, Region: region, InstanceGroupManager: regional}); err != nil ||
		!strings.HasSuffix(r.GetResourcePolicies().GetWorkloadPolicy(), polURL(ht)) {
		t.Errorf("the regional group's workload policy: %v %v", r.GetResourcePolicies(), err)
	}
	wait(rigm.Delete(ctx, &computepb.DeleteRegionInstanceGroupManagerRequest{Project: p, Region: region, InstanceGroupManager: regional}))
	wait(igm.Delete(ctx, &computepb.DeleteInstanceGroupManagerRequest{Project: p, Zone: zone, InstanceGroupManager: group}))
	wait(pols.Delete(ctx, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: ha}))
}

// TestGcloudComputeWorkloadPolicies (#2206): gcloud compute
// resource-policies create workload-policy, instance-groups managed create
// --workload-policy and update --workload-policy, with egress to Google
// blocked and every command against Compute Engine here.
func TestGcloudComputeWorkloadPolicies(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	const region, zone = "us-central1", "us-central1-a"
	ht, ha, tpl, group := "gw-ht-"+sfx, "gw-ha-"+sfx, "gw-t-"+sfx, "gw-g-"+sfx
	groupTemplate(t, ctx, tpls, h.Project(), tpl, "")
	gdir, home := t.TempDir(), t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	run := func(args ...string) (string, error) {
		args = append(args, "--project", h.Project(), "--quiet")
		cmd := exec.Command(gcloud, args...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name, "HOME="+home)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	t.Cleanup(func() {
		_, _ = run("compute", "instance-groups", "managed", "delete", group, "--zone", zone)
		_, _ = run("compute", "resource-policies", "delete", ht, "--region", region)
		_, _ = run("compute", "resource-policies", "delete", ha, "--region", region)
	})
	gc("compute", "resource-policies", "create", "workload-policy", ht, "--region", region, "--type=high-throughput", "--max-topology-distance=block")
	gc("compute", "resource-policies", "create", "workload-policy", ha, "--region", region, "--type=high-availability")
	if got := gc("compute", "resource-policies", "describe", ht, "--region", region,
		"--format=value(workloadPolicy.type,workloadPolicy.maxTopologyDistance)"); !strings.Contains(got, "HIGH_THROUGHPUT") || !strings.Contains(got, "BLOCK") {
		t.Errorf("describe the workload policy: %s", got)
	}
	polURL := func(n string) string {
		return "projects/" + h.Project() + "/regions/" + region + "/resourcePolicies/" + n
	}
	gc("compute", "instance-groups", "managed", "create", group, "--zone", zone, "--template", tpl, "--size", "0", "--workload-policy", polURL(ht))
	if got := gc("compute", "instance-groups", "managed", "describe", group, "--zone", zone, "--format=value(resourcePolicies.workloadPolicy)"); !strings.Contains(got, polURL(ht)) {
		t.Errorf("the group's workload policy: %s", got)
	}
	gc("compute", "instance-groups", "managed", "update", group, "--zone", zone, "--workload-policy", polURL(ha))
	if got := gc("compute", "instance-groups", "managed", "describe", group, "--zone", zone, "--format=value(resourcePolicies.workloadPolicy)"); !strings.Contains(got, polURL(ha)) {
		t.Errorf("the group's workload policy after update: %s", got)
	}
	if out, err := run("compute", "resource-policies", "delete", ha, "--region", region); err == nil || !strings.Contains(out, "is already being used by") {
		t.Errorf("deleted the policy a group uses: %v %s", err, out)
	}
	gc("compute", "instance-groups", "managed", "delete", group, "--zone", zone)
	gc("compute", "resource-policies", "delete", ha, "--region", region)
}

// TestTerraformComputeWorkloadPolicyGroup (#2206): a
// google_compute_resource_policy workload_policy and a
// google_compute_instance_group_manager with resource_policies {
// workload_policy }, changed to a second policy in place, through
// `cloudburrow terraform` with egress to Google blocked.
func TestTerraformComputeWorkloadPolicyGroup(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	igm, err := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	m := newTFModule(t)
	const region, zone = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	name := "tfw-" + sfx
	config := func(policy string) string {
		return fmt.Sprintf(`
resource "google_compute_resource_policy" "ht" {
  project = %[1]q
  name    = "%[2]s-ht"
  region  = %[4]q
  workload_policy {
    type                  = "HIGH_THROUGHPUT"
    max_topology_distance = "BLOCK"
  }
}

resource "google_compute_resource_policy" "ha" {
  project = %[1]q
  name    = "%[2]s-ha"
  region  = %[4]q
  workload_policy {
    type = "HIGH_AVAILABILITY"
  }
}

resource "google_compute_instance_template" "t" {
  project      = %[1]q
  name         = "%[2]s-t"
  machine_type = "e2-small"
  disk {
    source_image = "debian-cloud/debian-12"
    boot         = true
    auto_delete  = true
  }
  network_interface {
    network = "default"
  }
}

resource "google_compute_instance_group_manager" "g" {
  project            = %[1]q
  name               = %[2]q
  zone               = %[3]q
  base_instance_name = %[2]q
  target_size        = 0
  version {
    instance_template = google_compute_instance_template.t.self_link
  }
  resource_policies {
    workload_policy = google_compute_resource_policy.%[5]s.self_link
  }
}
`, m.project, name, zone, region, policy)
	}
	m.write(config("ht"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	t.Cleanup(func() { _, _ = m.tf("destroy", "-auto-approve", "-input=false", "-no-color") })
	check := func(policy string) {
		t.Helper()
		g, err := igm.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: m.project, Zone: zone, InstanceGroupManager: name})
		if err != nil || !strings.HasSuffix(g.GetResourcePolicies().GetWorkloadPolicy(), "/regions/"+region+"/resourcePolicies/"+name+"-"+policy) {
			t.Fatalf("the group Terraform made: %v, %v", g.GetResourcePolicies(), err)
		}
	}
	check("ht")
	m.planClean()
	m.write(config("ha"))
	m.apply()
	check("ha")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := igm.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: m.project, Zone: zone, InstanceGroupManager: name}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the group after destroy: %v", err)
	}
}
