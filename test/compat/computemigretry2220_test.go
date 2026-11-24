//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// retryNetwork makes an auto mode network, a template of VMs on it, and
// deletes the network again, so the group's VM creations fail until the
// network is made again (Google lets a template outlive its network).
func retryNetwork(t *testing.T, ctx context.Context, nets *computeapi.NetworksClient, tpls *computeapi.InstanceTemplatesClient, project, net, tpl string) func() {
	t.Helper()
	mkNet := func() {
		op, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project,
			NetworkResource: &computepb.Network{Name: proto.String(net), AutoCreateSubnetworks: proto.Bool(true)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mkNet()
	op, err := tpls.Insert(ctx, &computepb.InsertInstanceTemplateRequest{Project: project,
		InstanceTemplateResource: &computepb.InstanceTemplate{Name: proto.String(tpl), Properties: &computepb.InstanceProperties{MachineType: proto.String("e2-small"),
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
			NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/" + net)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := tpls.Delete(c, &computepb.DeleteInstanceTemplateRequest{Project: project, InstanceTemplate: tpl}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := nets.Delete(c, &computepb.DeleteNetworkRequest{Project: project, Network: net}); err == nil {
			_ = op.Wait(c)
		}
	})
	op, err = nets.Delete(ctx, &computepb.DeleteNetworkRequest{Project: project, Network: net})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return mkNet
}

// TestComputeMIGCreationRetries2220 (#2220), the official
// compute/apiv1 REST clients: a zonal group whose template's network is
// gone succeeds in its insert, and keeps its two VMs pending, listed
// CREATING with their instance URLs, no id or status and lastAttempt
// errors (RESOURCE_NOT_FOUND), the group's currentActions.creating 2 and
// status.isStable false; the failures after the immediate retry are in
// listErrors (instanceActionDetails naming the VM, CREATING and the
// template); once the network is back the group makes both VMs under the
// same names and is stable. A regional group records its failures for
// regionInstanceGroupManagers.listErrors alike, and a resize to 0 drops
// its pending VM.
// covers: compute.instanceGroupManagers.listErrors, compute.regionInstanceGroupManagers.listErrors, compute.instanceGroupManagers.listManagedInstances, compute.instanceGroupManagers.resize
func TestComputeMIGCreationRetries2220(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	project, zone, region := h.Project(), "us-central1-a", "us-central1"
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	tpls, _ := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	igm, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	rigm, _ := computeapi.NewRegionInstanceGroupManagersRESTClient(ctx, opts...)
	sfx := time.Now().UnixNano() % 1e6
	net, tpl, name, rname := fmt.Sprintf("mr-net-%d", sfx), fmt.Sprintf("mr-t-%d", sfx), fmt.Sprintf("mr-%d", sfx), fmt.Sprintf("mrr-%d", sfx)
	remake := retryNetwork(t, ctx, nets, tpls, project, net, tpl)
	t.Cleanup(func() {
		c := context.Background()
		if op, err := igm.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := rigm.Delete(c, &computepb.DeleteRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname}); err == nil {
			_ = op.Wait(c)
		}
	})

	op, err := igm.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(name), BaseInstanceName: proto.String(name),
			InstanceTemplate: proto.String("global/instanceTemplates/" + tpl), TargetSize: proto.Int32(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("insert failed although the group retries its VMs: %v", err)
	}
	list := func() []*computepb.ManagedInstance {
		var out []*computepb.ManagedInstance
		it := igm.ListManagedInstances(ctx, &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: name})
		for {
			mi, err := it.Next()
			if err != nil {
				return out
			}
			out = append(out, mi)
		}
	}
	listErrs := func() []*computepb.InstanceManagedByIgmError {
		var out []*computepb.InstanceManagedByIgmError
		it := igm.ListErrors(ctx, &computepb.ListErrorsInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: name})
		for {
			e, err := it.Next()
			if err != nil {
				return out
			}
			out = append(out, e)
		}
	}
	get := func() *computepb.InstanceGroupManager {
		m, err := igm.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: name})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	mis := list()
	if len(mis) != 2 {
		t.Fatalf("managed instances = %v", mis)
	}
	names := map[string]bool{}
	for _, mi := range mis {
		names[mi.GetInstance()] = true
		errs := mi.GetLastAttempt().GetErrors().GetErrors()
		if mi.GetCurrentAction() != "CREATING" || mi.Id != nil || mi.GetInstanceStatus() != "" || len(errs) != 1 ||
			errs[0].GetCode() != "RESOURCE_NOT_FOUND" || !strings.Contains(errs[0].GetMessage(), "networks/"+net) ||
			!strings.Contains(mi.GetInstance(), "/projects/"+project+"/zones/"+zone+"/instances/"+name+"-") {
			t.Fatalf("pending instance = %v", mi)
		}
	}
	if m := get(); m.GetStatus().GetIsStable() || m.GetCurrentActions().GetCreating() != 2 || m.GetTargetSize() != 2 {
		t.Fatalf("group while its VMs fail = %v", m)
	}
	waitFor(t, "the failures in listErrors", 2*time.Minute, func() bool { return len(listErrs()) >= 2 })
	for _, e := range listErrs() {
		d := e.GetInstanceActionDetails()
		if e.GetError().GetCode() != "RESOURCE_NOT_FOUND" || !strings.Contains(e.GetError().GetMessage(), "creation failed") ||
			d.GetAction() != "CREATING" || !names[d.GetInstance()] || !strings.HasSuffix(d.GetVersion().GetInstanceTemplate(), "/instanceTemplates/"+tpl) || e.GetTimestamp() == "" {
			t.Fatalf("listErrors item = %v", e)
		}
	}

	// A regional group's failures, then a resize to 0 dropping its VM.
	op, err = rigm.Insert(ctx, &computepb.InsertRegionInstanceGroupManagerRequest{Project: project, Region: region,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(rname), BaseInstanceName: proto.String(rname),
			InstanceTemplate: proto.String("global/instanceTemplates/" + tpl), TargetSize: proto.Int32(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the regional group's listErrors", 2*time.Minute, func() bool {
		it := rigm.ListErrors(ctx, &computepb.ListErrorsRegionInstanceGroupManagersRequest{Project: project, Region: region, InstanceGroupManager: rname})
		e, err := it.Next()
		return err == nil && e.GetError().GetCode() == "RESOURCE_NOT_FOUND" && e.GetInstanceActionDetails().GetAction() == "CREATING"
	})
	op, err = rigm.Resize(ctx, &computepb.ResizeRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname, Size: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rm, err := rigm.Get(ctx, &computepb.GetRegionInstanceGroupManagerRequest{Project: project, Region: region, InstanceGroupManager: rname})
	if err != nil || !rm.GetStatus().GetIsStable() || rm.GetCurrentActions().GetCreating() != 0 {
		t.Fatalf("regional group after resize to 0 = %v, %v", rm, err)
	}

	// The network back: the zonal group makes its VMs, by the same names.
	remake()
	waitFor(t, "the group stable", 12*time.Minute, func() bool { return get().GetStatus().GetIsStable() })
	mis = list()
	if len(mis) != 2 {
		t.Fatalf("after the network came back: %v", mis)
	}
	for _, mi := range mis {
		if !names[mi.GetInstance()] || mi.GetInstanceStatus() != "RUNNING" || mi.GetCurrentAction() != "NONE" || mi.Id == nil ||
			len(mi.GetLastAttempt().GetErrors().GetErrors()) != 0 {
			t.Fatalf("made instance = %v", mi)
		}
	}
	if m := get(); m.GetCurrentActions().GetNone() != 2 || m.GetCurrentActions().GetCreating() != 0 {
		t.Fatalf("group once made = %v", m)
	}
}

// TestGcloudComputeMIGCreationRetries2220 (#2220): the pinned gcloud
// through gcloud-setup, egress to Google blocked: instance-groups managed
// create from a template whose network is gone, list-instances (the VM
// CREATING, its last error), describe (status.isStable False),
// list-errors (the failure, CREATING), then, the network made again,
// wait-until --stable, list-instances (RUNNING, NONE) and delete.
func TestGcloudComputeMIGCreationRetries2220(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	gdir, home := t.TempDir(), t.TempDir()
	cfg := gcloudSetup(t, cli, gdir)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfg, "HOME="+home)
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
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	tpls, _ := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	const zone = "us-central1-a"
	sfx := time.Now().UnixNano() % 1e6
	net, tpl, name := fmt.Sprintf("gmr-net-%d", sfx), fmt.Sprintf("gmr-t-%d", sfx), fmt.Sprintf("gmr-%d", sfx)
	remake := retryNetwork(t, ctx, nets, tpls, h.Project(), net, tpl)
	t.Cleanup(func() { _, _ = run("compute", "instance-groups", "managed", "delete", name, "--zone", zone) })

	gc("compute", "instance-groups", "managed", "create", name, "--zone", zone, "--template", tpl, "--size", "1")
	out := gc("compute", "instance-groups", "managed", "list-instances", name, "--zone", zone,
		"--format=value(instance.basename(),currentAction,lastAttempt.errors.errors[0].code)")
	if !strings.Contains(out, name+"-") || !strings.Contains(out, "CREATING") || !strings.Contains(out, "RESOURCE_NOT_FOUND") {
		t.Fatalf("list-instances while failing: %q", out)
	}
	if got := strings.TrimSpace(gc("compute", "instance-groups", "managed", "describe", name, "--zone", zone, "--format=value(status.isStable)")); got != "False" {
		t.Fatalf("status.isStable while failing = %q", got)
	}
	waitFor(t, "gcloud list-errors", 2*time.Minute, func() bool {
		out, err := run("compute", "instance-groups", "managed", "list-errors", name, "--zone", zone,
			"--format=value(error.code,instanceActionDetails.action)")
		return err == nil && strings.Contains(out, "RESOURCE_NOT_FOUND") && strings.Contains(out, "CREATING")
	})
	remake()
	gc("compute", "instance-groups", "managed", "wait-until", name, "--zone", zone, "--stable", "--timeout", "720")
	out = gc("compute", "instance-groups", "managed", "list-instances", name, "--zone", zone, "--format=value(instanceStatus,currentAction)")
	if !strings.Contains(out, "RUNNING") || !strings.Contains(out, "NONE") {
		t.Fatalf("list-instances once made: %q", out)
	}
	gc("compute", "instance-groups", "managed", "delete", name, "--zone", zone)
}
