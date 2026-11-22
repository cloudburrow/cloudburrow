//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestComputeRollingUpdatesThroughTheGoClient2139 (#2139), the official
// compute/apiv1 REST clients: a zonal managed group of two VMs patched to a
// template of another machine type with an Automatic (PROACTIVE) update,
// maxSurge 1 and maxUnavailable 0, every VM replaced by a new one (new
// names), never more than three at once, the group stable at its version
// target; a Selective (OPPORTUNISTIC) patch back that leaves the VMs as
// they are until applyUpdatesToInstances replaces the one named, by the
// same name; RECREATE with a surge refused; a regional group's fixed
// maxSurge below its number of zones refused, its defaults the number of
// zones.
// covers: compute.instanceGroupManagers.patch, compute.instanceGroupManagers.applyUpdatesToInstances, compute.instanceGroupManagers.listManagedInstances, compute.regionInstanceGroupManagers.patch
func TestComputeRollingUpdatesThroughTheGoClient2139(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project, zone, region := h.Project(), "us-central1-a", "us-central1"
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	igm, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	rigm, _ := computeapi.NewRegionInstanceGroupManagersRESTClient(ctx, opts...)
	sfx := time.Now().UnixNano() % 1e6
	t1, t2, name := fmt.Sprintf("ru-a-%d", sfx), fmt.Sprintf("ru-b-%d", sfx), fmt.Sprintf("ru-%d", sfx)
	groupTemplate(t, ctx, tpls, project, t1, "")
	// t2: another machine type, so the update needs REPLACE.
	op, err := tpls.Insert(ctx, &computepb.InsertInstanceTemplateRequest{Project: project, InstanceTemplateResource: &computepb.InstanceTemplate{
		Name: proto.String(t2), Properties: &computepb.InstanceProperties{MachineType: proto.String("e2-medium"),
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
			NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}}}}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("template %s: %v", t2, err)
	}
	t.Cleanup(func() {
		if op, err := tpls.Delete(context.Background(), &computepb.DeleteInstanceTemplateRequest{Project: project, InstanceTemplate: t2}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	t.Cleanup(func() {
		c := context.Background()
		if op, err := igm.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name}); err == nil {
			_ = op.Wait(c)
		}
	})
	op, err = igm.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(name), BaseInstanceName: proto.String(name),
			InstanceTemplate: proto.String("global/instanceTemplates/" + t1), TargetSize: proto.Int32(2)}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("insert: %v", err)
	}
	managed := func() map[string]string {
		out := map[string]string{}
		it := igm.ListManagedInstances(ctx, &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: name})
		for {
			mi, err := it.Next()
			if err != nil {
				return out
			}
			if mi.GetCurrentAction() != "DELETING" {
				out[mi.GetName()] = mi.GetVersion().GetInstanceTemplate()
			}
		}
	}
	get := func() *computepb.InstanceGroupManager {
		m, err := igm.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	waitFor(t, "the group stable", 15*time.Minute, func() bool { return get().GetStatus().GetIsStable() })
	before := managed()

	if _, err := igm.Patch(ctx, &computepb.PatchInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{UpdatePolicy: &computepb.InstanceGroupManagerUpdatePolicy{
			Type: proto.String("PROACTIVE"), ReplacementMethod: proto.String("RECREATE"), MaxSurge: &computepb.FixedOrPercent{Fixed: proto.Int32(1)}}}}); err == nil ||
		!strings.Contains(err.Error(), "maxSurge") {
		t.Errorf("RECREATE with maxSurge 1: %v", err)
	}
	op, err = igm.Patch(ctx, &computepb.PatchInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{
			Versions: []*computepb.InstanceGroupManagerVersion{{InstanceTemplate: proto.String("global/instanceTemplates/" + t2)}},
			UpdatePolicy: &computepb.InstanceGroupManagerUpdatePolicy{Type: proto.String("PROACTIVE"), MinimalAction: proto.String("REPLACE"),
				MaxSurge: &computepb.FixedOrPercent{Fixed: proto.Int32(1)}, MaxUnavailable: &computepb.FixedOrPercent{Fixed: proto.Int32(0)}}}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("Patch PROACTIVE: %v", err)
	}
	waitFor(t, "every VM on "+t2, 20*time.Minute, func() bool {
		cur := managed()
		if len(cur) > 3 {
			t.Errorf("%d VMs during the update, beyond 2 + maxSurge 1: %v", len(cur), cur)
		}
		for _, tpl := range cur {
			if !strings.HasSuffix(tpl, "/"+t2) {
				return false
			}
		}
		st := get().GetStatus()
		return len(cur) == 2 && st.GetIsStable() && st.GetVersionTarget().GetIsReached()
	})
	after := managed()
	for n := range before {
		if _, ok := after[n]; ok {
			t.Errorf("SUBSTITUTE kept %s: %v", n, after)
		}
	}
	up := get().GetUpdatePolicy()
	if up.GetType() != "PROACTIVE" || up.GetMaxSurge().GetCalculated() != 1 || up.GetReplacementMethod() != "SUBSTITUTE" {
		t.Errorf("updatePolicy = %v", up)
	}

	// Selective back to t1: nothing moves until applyUpdatesToInstances.
	op, err = igm.Patch(ctx, &computepb.PatchInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{
			Versions:     []*computepb.InstanceGroupManagerVersion{{InstanceTemplate: proto.String("global/instanceTemplates/" + t1)}},
			UpdatePolicy: &computepb.InstanceGroupManagerUpdatePolicy{Type: proto.String("OPPORTUNISTIC")}}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("Patch OPPORTUNISTIC: %v", err)
	}
	time.Sleep(10 * time.Second)
	cur := managed()
	var one string
	for n, tpl := range cur {
		if !strings.HasSuffix(tpl, "/"+t2) {
			t.Errorf("OPPORTUNISTIC moved %s to %s", n, tpl)
		}
		one = n
	}
	if get().GetStatus().GetVersionTarget().GetIsReached() {
		t.Error("the version target is reached with every VM on the old template")
	}
	op, err = igm.ApplyUpdatesToInstances(ctx, &computepb.ApplyUpdatesToInstancesInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name,
		InstanceGroupManagersApplyUpdatesRequestResource: &computepb.InstanceGroupManagersApplyUpdatesRequest{
			Instances: []string{"zones/" + zone + "/instances/" + one}, MinimalAction: proto.String("REPLACE")}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("ApplyUpdatesToInstances: %v", err)
	}
	waitFor(t, one+" on "+t1, 15*time.Minute, func() bool {
		m := managed()
		return strings.HasSuffix(m[one], "/"+t1) && len(m) == 2
	})
	for n, tpl := range managed() {
		if n != one && !strings.HasSuffix(tpl, "/"+t2) {
			t.Errorf("%s, not named, moved to %s", n, tpl)
		}
	}

	// A regional group of three zones.
	rname := name + "-r"
	op, err = rigm.Insert(ctx, &computepb.InsertRegionInstanceGroupManagerRequest{Project: project, Region: region,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(rname), InstanceTemplate: proto.String("global/instanceTemplates/" + t1), TargetSize: proto.Int32(0)}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("regional insert: %v", err)
	}
	t.Cleanup(func() {
		if op, err := rigm.Delete(context.Background(), &computepb.DeleteRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if _, err := rigm.Patch(ctx, &computepb.PatchRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{UpdatePolicy: &computepb.InstanceGroupManagerUpdatePolicy{
			Type: proto.String("PROACTIVE"), MaxSurge: &computepb.FixedOrPercent{Fixed: proto.Int32(1)}}}}); err == nil {
		t.Error("a regional group of three zones took a fixed maxSurge of 1")
	}
	op, err = rigm.Patch(ctx, &computepb.PatchRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{UpdatePolicy: &computepb.InstanceGroupManagerUpdatePolicy{Type: proto.String("PROACTIVE")}}})
	if err != nil || op.Wait(ctx) != nil {
		t.Fatalf("regional Patch: %v", err)
	}
	r, err := rigm.Get(ctx, &computepb.GetRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname})
	if err != nil || r.GetUpdatePolicy().GetMaxSurge().GetFixed() != 3 || r.GetUpdatePolicy().GetMaxUnavailable().GetFixed() != 3 {
		t.Errorf("a regional group's defaults: %v %v", r.GetUpdatePolicy(), err)
	}
}
