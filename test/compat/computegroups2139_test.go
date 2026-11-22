//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// groupTemplate makes a global instance template of e2-small debian-12
// VMs, with a startup script when script is not empty, deleted at cleanup.
func groupTemplate(t *testing.T, ctx context.Context, tpls *computeapi.InstanceTemplatesClient, project, name, script string) {
	t.Helper()
	props := &computepb.InstanceProperties{MachineType: proto.String("e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}}}
	if script != "" {
		props.Metadata = &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(script)}}}
	}
	op, err := tpls.Insert(ctx, &computepb.InsertInstanceTemplateRequest{Project: project,
		InstanceTemplateResource: &computepb.InstanceTemplate{Name: proto.String(name), Properties: props}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := tpls.Delete(c, &computepb.DeleteInstanceTemplateRequest{Project: project, InstanceTemplate: name}); err == nil {
			_ = op.Wait(c)
		}
	})
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not after %s", what, d)
		}
		time.Sleep(5 * time.Second)
	}
}

// TestComputeRegionalGroupsThroughTheGoClient (#2139), the official
// compute/apiv1 REST clients: a regional managed group of three spread
// EVEN over the region's zones (each VM a zonal instance), its
// regionInstanceGroup's get, list, listInstances and setNamedPorts (a
// stale fingerprint refused), setInstanceTemplate (new VMs only) then
// resize, per-VM stop, start, suspend, resume, recreate (re-made from the
// new template) and delete; an autoscaler of it (insert, get, list, patch,
// update) refusing a resize; a zonal unmanaged group's setNamedPorts; and
// deletion of all of it.
// covers: compute.regionInstanceGroupManagers.insert, compute.regionInstanceGroupManagers.get, compute.regionInstanceGroupManagers.list, compute.regionInstanceGroupManagers.delete, compute.regionInstanceGroupManagers.resize, compute.regionInstanceGroupManagers.setInstanceTemplate, compute.regionInstanceGroupManagers.listManagedInstances, compute.regionInstanceGroupManagers.recreateInstances, compute.regionInstanceGroupManagers.deleteInstances, compute.regionInstanceGroupManagers.stopInstances, compute.regionInstanceGroupManagers.startInstances, compute.regionInstanceGroupManagers.suspendInstances, compute.regionInstanceGroupManagers.resumeInstances, compute.regionInstanceGroups.get, compute.regionInstanceGroups.list, compute.regionInstanceGroups.listInstances, compute.regionInstanceGroups.setNamedPorts, compute.regionAutoscalers.insert, compute.regionAutoscalers.get, compute.regionAutoscalers.list, compute.regionAutoscalers.patch, compute.regionAutoscalers.update, compute.regionAutoscalers.delete, compute.instanceGroups.setNamedPorts, compute.instanceGroupManagers.setInstanceTemplate
func TestComputeRegionalGroupsThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project, region := h.Project(), "us-central1"
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rigm, _ := computeapi.NewRegionInstanceGroupManagersRESTClient(ctx, opts...)
	rig, _ := computeapi.NewRegionInstanceGroupsRESTClient(ctx, opts...)
	ras, _ := computeapi.NewRegionAutoscalersRESTClient(ctx, opts...)
	igs, _ := computeapi.NewInstanceGroupsRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	sfx := time.Now().UnixNano() % 1e6
	t1, t2, name := fmt.Sprintf("rg-a-%d", sfx), fmt.Sprintf("rg-b-%d", sfx), fmt.Sprintf("rg-%d", sfx)
	groupTemplate(t, ctx, tpls, project, t1, "")
	groupTemplate(t, ctx, tpls, project, t2, "")
	t.Cleanup(func() {
		c := context.Background()
		if op, err := ras.Delete(c, &computepb.DeleteRegionAutoscalerRequest{Project: project, Region: region, Autoscaler: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := rigm.Delete(c, &computepb.DeleteRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name}); err == nil {
			_ = op.Wait(c)
		}
	})
	op, err := rigm.Insert(ctx, &computepb.InsertRegionInstanceGroupManagerRequest{Project: project, Region: region,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(name), BaseInstanceName: proto.String(name),
			InstanceTemplate: proto.String("global/instanceTemplates/" + t1), TargetSize: proto.Int32(3),
			DistributionPolicy: &computepb.DistributionPolicy{TargetShape: proto.String("EVEN"), Zones: []*computepb.DistributionPolicyZoneConfiguration{
				{Zone: proto.String("zones/us-central1-a")}, {Zone: proto.String("zones/us-central1-b")}, {Zone: proto.String("zones/us-central1-c")}}},
			NamedPorts: []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(80)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	managed := func() []*computepb.ManagedInstance {
		var out []*computepb.ManagedInstance
		it := rigm.ListManagedInstances(ctx, &computepb.ListManagedInstancesRegionInstanceGroupManagersRequest{Project: project, Region: region, InstanceGroupManager: name})
		for {
			mi, err := it.Next()
			if err != nil {
				return out
			}
			out = append(out, mi)
		}
	}
	waitFor(t, "the group stable", 15*time.Minute, func() bool {
		m, err := rigm.Get(ctx, &computepb.GetRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name})
		return err == nil && m.GetStatus().GetIsStable()
	})
	zones := map[string]bool{}
	for _, mi := range managed() {
		parts := strings.Split(mi.GetInstance(), "/")
		zones[parts[len(parts)-3]] = true
		if mi.GetInstanceStatus() != "RUNNING" {
			t.Errorf("%s is %s", mi.GetInstance(), mi.GetInstanceStatus())
		}
	}
	if len(zones) != 3 {
		t.Fatalf("EVEN over three zones: %v", zones)
	}
	found := false
	for it := rigm.List(ctx, &computepb.ListRegionInstanceGroupManagersRequest{Project: project, Region: region}); ; {
		m, err := it.Next()
		if err != nil {
			break
		}
		found = found || m.GetName() == name
	}
	if !found {
		t.Error("regionInstanceGroupManagers.list without the group")
	}

	// The group, its VMs and named ports.
	g, err := rig.Get(ctx, &computepb.GetRegionInstanceGroupRequest{Project: project, Region: region, InstanceGroup: name})
	if err != nil || g.GetSize() != 3 || len(g.GetNamedPorts()) != 1 {
		t.Fatalf("regionInstanceGroups.get = %v, %v", g, err)
	}
	if it := rig.List(ctx, &computepb.ListRegionInstanceGroupsRequest{Project: project, Region: region}); true {
		if _, err := it.Next(); err != nil {
			t.Errorf("regionInstanceGroups.list: %v", err)
		}
	}
	n := 0
	for it := rig.ListInstances(ctx, &computepb.ListInstancesRegionInstanceGroupsRequest{Project: project, Region: region, InstanceGroup: name,
		RegionInstanceGroupsListInstancesRequestResource: &computepb.RegionInstanceGroupsListInstancesRequest{InstanceState: proto.String("ALL")}}); ; n++ {
		if _, err := it.Next(); err != nil {
			break
		}
	}
	if n != 3 {
		t.Errorf("listInstances = %d", n)
	}
	if _, err := rig.SetNamedPorts(ctx, &computepb.SetNamedPortsRegionInstanceGroupRequest{Project: project, Region: region, InstanceGroup: name,
		RegionInstanceGroupsSetNamedPortsRequestResource: &computepb.RegionInstanceGroupsSetNamedPortsRequest{Fingerprint: proto.String("c3RhbGU="),
			NamedPorts: []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(8080)}}}}); computeCode(err) != 412 {
		t.Errorf("a stale fingerprint: %v", err)
	}
	op, err = rig.SetNamedPorts(ctx, &computepb.SetNamedPortsRegionInstanceGroupRequest{Project: project, Region: region, InstanceGroup: name,
		RegionInstanceGroupsSetNamedPortsRequestResource: &computepb.RegionInstanceGroupsSetNamedPortsRequest{Fingerprint: proto.String(g.GetFingerprint()),
			NamedPorts: []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(8080)}, {Name: proto.String("https"), Port: proto.Int32(8443)}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)

	// setInstanceTemplate, then resize: the new VM is of the new template.
	op, err = rigm.SetInstanceTemplate(ctx, &computepb.SetInstanceTemplateRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersSetTemplateRequestResource: &computepb.RegionInstanceGroupManagersSetTemplateRequest{InstanceTemplate: proto.String("global/instanceTemplates/" + t2)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	op, err = rigm.Resize(ctx, &computepb.ResizeRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name, Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	var oldVM, newVM string
	waitFor(t, "four VMs", 10*time.Minute, func() bool {
		oldVM, newVM = "", ""
		l := managed()
		for _, mi := range l {
			if strings.HasSuffix(mi.GetVersion().GetInstanceTemplate(), "/"+t2) {
				newVM = mi.GetInstance()
			} else {
				oldVM = mi.GetInstance()
			}
		}
		return len(l) == 4 && newVM != "" && oldVM != ""
	})

	// Per-VM actions on one of the old VMs.
	vmReq := func(vm string) (string, string) {
		p := strings.Split(vm, "/")
		return p[len(p)-3], p[len(p)-1]
	}
	status := func(vm string) string {
		z, n := vmReq(vm)
		o, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: project, Zone: z, Instance: n})
		if err != nil {
			return err.Error()
		}
		return o.GetStatus()
	}
	rel := []string{oldVM}
	wait := func(what string, op *computeapi.Operation, err error, want string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		_ = op.Wait(ctx)
		if want != "" {
			waitFor(t, what, 5*time.Minute, func() bool { return status(oldVM) == want })
		}
	}
	o1, err := rigm.StopInstances(ctx, &computepb.StopInstancesRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersStopInstancesRequestResource: &computepb.RegionInstanceGroupManagersStopInstancesRequest{Instances: rel}})
	wait("stopInstances", o1, err, "TERMINATED")
	o1, err = rigm.StartInstances(ctx, &computepb.StartInstancesRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersStartInstancesRequestResource: &computepb.RegionInstanceGroupManagersStartInstancesRequest{Instances: rel}})
	wait("startInstances", o1, err, "RUNNING")
	o1, err = rigm.SuspendInstances(ctx, &computepb.SuspendInstancesRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersSuspendInstancesRequestResource: &computepb.RegionInstanceGroupManagersSuspendInstancesRequest{Instances: rel}})
	wait("suspendInstances", o1, err, "SUSPENDED")
	o1, err = rigm.ResumeInstances(ctx, &computepb.ResumeInstancesRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersResumeInstancesRequestResource: &computepb.RegionInstanceGroupManagersResumeInstancesRequest{Instances: rel}})
	wait("resumeInstances", o1, err, "RUNNING")
	o1, err = rigm.RecreateInstances(ctx, &computepb.RecreateInstancesRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersRecreateRequestResource: &computepb.RegionInstanceGroupManagersRecreateRequest{Instances: rel}})
	wait("recreateInstances", o1, err, "")
	waitFor(t, "the old VM re-made from the new template", 10*time.Minute, func() bool {
		for _, mi := range managed() {
			if mi.GetInstance() == oldVM {
				return strings.HasSuffix(mi.GetVersion().GetInstanceTemplate(), "/"+t2) && mi.GetInstanceStatus() == "RUNNING" && mi.GetCurrentAction() == "NONE"
			}
		}
		return false
	})
	o1, err = rigm.DeleteInstances(ctx, &computepb.DeleteInstancesRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name,
		RegionInstanceGroupManagersDeleteInstancesRequestResource: &computepb.RegionInstanceGroupManagersDeleteInstancesRequest{Instances: []string{newVM}}})
	wait("deleteInstances", o1, err, "")
	if m, err := rigm.Get(ctx, &computepb.GetRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name}); err != nil || m.GetTargetSize() != 3 {
		t.Fatalf("after deleteInstances: %v, %v", m.GetTargetSize(), err)
	}

	// An autoscaler of the group: a resize is then refused.
	op, err = ras.Insert(ctx, &computepb.InsertRegionAutoscalerRequest{Project: project, Region: region, AutoscalerResource: &computepb.Autoscaler{
		Name: proto.String(name), Target: proto.String(fmt.Sprintf("projects/%s/regions/%s/instanceGroupManagers/%s", project, region, name)),
		AutoscalingPolicy: &computepb.AutoscalingPolicy{MinNumReplicas: proto.Int32(3), MaxNumReplicas: proto.Int32(5), Mode: proto.String("ON"),
			CpuUtilization: &computepb.AutoscalingPolicyCpuUtilization{UtilizationTarget: proto.Float64(0.6)}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	a, err := ras.Get(ctx, &computepb.GetRegionAutoscalerRequest{Project: project, Region: region, Autoscaler: name})
	if err != nil || a.GetStatus() != "ACTIVE" || a.GetAutoscalingPolicy().GetCoolDownPeriodSec() != 60 {
		t.Fatalf("regionAutoscalers.get = %v, %v", a, err)
	}
	if it := ras.List(ctx, &computepb.ListRegionAutoscalersRequest{Project: project, Region: region}); true {
		if _, err := it.Next(); err != nil {
			t.Errorf("regionAutoscalers.list: %v", err)
		}
	}
	if _, err := rigm.Resize(ctx, &computepb.ResizeRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name, Size: 1}); computeCode(err) != 400 {
		t.Errorf("resize of an autoscaled regional group: %v", err)
	}
	op, err = ras.Patch(ctx, &computepb.PatchRegionAutoscalerRequest{Project: project, Region: region, Autoscaler: proto.String(name),
		AutoscalerResource: &computepb.Autoscaler{Name: proto.String(name), AutoscalingPolicy: &computepb.AutoscalingPolicy{Mode: proto.String("OFF")}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	a, _ = ras.Get(ctx, &computepb.GetRegionAutoscalerRequest{Project: project, Region: region, Autoscaler: name})
	if a.GetAutoscalingPolicy().GetMode() != "OFF" || a.GetAutoscalingPolicy().GetMaxNumReplicas() != 5 {
		t.Errorf("patched = %v", a.GetAutoscalingPolicy())
	}
	op, err = ras.Update(ctx, &computepb.UpdateRegionAutoscalerRequest{Project: project, Region: region, Autoscaler: proto.String(name),
		AutoscalerResource: &computepb.Autoscaler{Name: proto.String(name), Target: a.Target,
			AutoscalingPolicy: &computepb.AutoscalingPolicy{MinNumReplicas: proto.Int32(1), MaxNumReplicas: proto.Int32(3), Mode: proto.String("OFF")}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	op, err = ras.Delete(ctx, &computepb.DeleteRegionAutoscalerRequest{Project: project, Region: region, Autoscaler: name})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)

	// A zonal unmanaged group's named ports.
	ug := name + "-u"
	op, err = igs.Insert(ctx, &computepb.InsertInstanceGroupRequest{Project: project, Zone: "us-central1-a", InstanceGroupResource: &computepb.InstanceGroup{Name: proto.String(ug)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	t.Cleanup(func() {
		if op, err := igs.Delete(context.Background(), &computepb.DeleteInstanceGroupRequest{Project: project, Zone: "us-central1-a", InstanceGroup: ug}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if _, err := igs.SetNamedPorts(ctx, &computepb.SetNamedPortsInstanceGroupRequest{Project: project, Zone: "us-central1-a", InstanceGroup: ug,
		InstanceGroupsSetNamedPortsRequestResource: &computepb.InstanceGroupsSetNamedPortsRequest{NamedPorts: []*computepb.NamedPort{{Name: proto.String("web"), Port: proto.Int32(0)}}}}); computeCode(err) != 400 {
		t.Errorf("port 0: %v", err)
	}
	op, err = igs.SetNamedPorts(ctx, &computepb.SetNamedPortsInstanceGroupRequest{Project: project, Zone: "us-central1-a", InstanceGroup: ug,
		InstanceGroupsSetNamedPortsRequestResource: &computepb.InstanceGroupsSetNamedPortsRequest{NamedPorts: []*computepb.NamedPort{{Name: proto.String("web"), Port: proto.Int32(8080)}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	if g, err := igs.Get(ctx, &computepb.GetInstanceGroupRequest{Project: project, Zone: "us-central1-a", InstanceGroup: ug}); err != nil || g.GetNamedPorts()[0].GetPort() != 8080 {
		t.Errorf("named ports = %v, %v", g.GetNamedPorts(), err)
	}

	op, err = rigm.Delete(ctx, &computepb.DeleteRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: name})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
}

// TestComputeAutoscalerScalesOnRealCPU (#2139): a zonal group of one VM
// whose startup script keeps one of its two vCPUs busy (50%), and an
// autoscaler with a 20% target: the VM's real CPU, read from its pod's
// cgroup, makes the autoscaler add VMs (recommendedSize 3, capped at
// maxNumReplicas 3); the VMs' utilization is
// compute.googleapis.com/instance/cpu/utilization in Cloud Monitoring when
// the instance serves it; mode OFF keeps the size; then everything is
// deleted.
// covers: compute.autoscalers.insert, compute.autoscalers.get, compute.autoscalers.list, compute.autoscalers.patch, compute.autoscalers.update, compute.autoscalers.delete, compute.instanceGroupManagers.insert, compute.instanceGroupManagers.get, compute.instanceGroupManagers.delete
func TestComputeAutoscalerScalesOnRealCPU(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project, zone := h.Project(), "us-central1-a"
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	igms, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	as, _ := computeapi.NewAutoscalersRESTClient(ctx, opts...)
	sfx := time.Now().UnixNano() % 1e6
	tpl, name := fmt.Sprintf("burn-%d", sfx), fmt.Sprintf("burn-%d", sfx)
	groupTemplate(t, ctx, tpls, project, tpl, "#!/bin/bash\nnohup sh -c 'while :; do :; done' >/dev/null 2>&1 &\n")
	t.Cleanup(func() {
		c := context.Background()
		if op, err := as.Delete(c, &computepb.DeleteAutoscalerRequest{Project: project, Zone: zone, Autoscaler: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := igms.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name}); err == nil {
			_ = op.Wait(c)
		}
	})
	op, err := igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(name), BaseInstanceName: proto.String(name),
			InstanceTemplate: proto.String("global/instanceTemplates/" + tpl), TargetSize: proto.Int32(1)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	op, err = as.Insert(ctx, &computepb.InsertAutoscalerRequest{Project: project, Zone: zone, AutoscalerResource: &computepb.Autoscaler{
		Name: proto.String(name), Target: proto.String(fmt.Sprintf("projects/%s/zones/%s/instanceGroupManagers/%s", project, zone, name)),
		AutoscalingPolicy: &computepb.AutoscalingPolicy{MinNumReplicas: proto.Int32(1), MaxNumReplicas: proto.Int32(3), CoolDownPeriodSec: proto.Int32(30),
			CpuUtilization: &computepb.AutoscalingPolicyCpuUtilization{UtilizationTarget: proto.Float64(0.2), PredictiveMethod: proto.String("NONE")}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	waitFor(t, "the autoscaler adding VMs on the VM's CPU", 15*time.Minute, func() bool {
		m, err := igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name})
		return err == nil && m.GetTargetSize() >= 2 && strings.HasSuffix(m.GetStatus().GetAutoscaler(), "/autoscalers/"+name)
	})
	a, err := as.Get(ctx, &computepb.GetAutoscalerRequest{Project: project, Zone: zone, Autoscaler: name})
	if err != nil || a.GetRecommendedSize() < 2 || a.GetStatus() != "ACTIVE" {
		t.Fatalf("autoscaler = %v, %v", a, err)
	}
	found := false
	for it := as.List(ctx, &computepb.ListAutoscalersRequest{Project: project, Zone: zone}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		found = found || x.GetName() == name
	}
	if !found {
		t.Error("autoscalers.list without the autoscaler")
	}

	if os.Getenv(EnvMonitoring) != "" {
		mc := monitoringClient(t, h)
		waitFor(t, "instance/cpu/utilization points", 5*time.Minute, func() bool {
			it := mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project,
				Filter:   `metric.type = "compute.googleapis.com/instance/cpu/utilization" AND resource.type = "gce_instance"`,
				Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(time.Now().Add(-time.Hour)), EndTime: timestamppb.Now()}})
			for {
				ts, err := it.Next()
				if err == iterator.Done || err != nil {
					return false
				}
				if strings.HasPrefix(ts.GetMetric().GetLabels()["instance_name"], name+"-") && ts.GetResource().GetLabels()["zone"] == zone &&
					len(ts.GetPoints()) > 0 && ts.GetPoints()[0].GetValue().GetDoubleValue() > 0.2 {
					return true
				}
			}
		})
	}

	// OFF: the recommendation is kept, the size no longer follows it.
	op, err = as.Patch(ctx, &computepb.PatchAutoscalerRequest{Project: project, Zone: zone, Autoscaler: proto.String(name),
		AutoscalerResource: &computepb.Autoscaler{Name: proto.String(name), AutoscalingPolicy: &computepb.AutoscalingPolicy{Mode: proto.String("OFF")}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	op, err = as.Update(ctx, &computepb.UpdateAutoscalerRequest{Project: project, Zone: zone, Autoscaler: proto.String(name),
		AutoscalerResource: &computepb.Autoscaler{Name: proto.String(name), Target: a.Target,
			AutoscalingPolicy: &computepb.AutoscalingPolicy{MaxNumReplicas: proto.Int32(3), Mode: proto.String("OFF")}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	a, _ = as.Get(ctx, &computepb.GetAutoscalerRequest{Project: project, Zone: zone, Autoscaler: name})
	if a.GetAutoscalingPolicy().GetMode() != "OFF" || a.GetAutoscalingPolicy().GetCpuUtilization().GetUtilizationTarget() != 0.6 {
		t.Errorf("updated policy = %v", a.GetAutoscalingPolicy())
	}
	hasOff := false
	for _, d := range a.GetStatusDetails() {
		hasOff = hasOff || d.GetType() == "MODE_OFF"
	}
	if !hasOff {
		t.Errorf("statusDetails = %v", a.GetStatusDetails())
	}
	op, err = as.Delete(ctx, &computepb.DeleteAutoscalerRequest{Project: project, Zone: zone, Autoscaler: name})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	op, err = igms.Delete(ctx, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
}
