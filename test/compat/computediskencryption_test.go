//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// diskStorage is what a disk's storage holds on the cluster's node: its
// file names, and whether marker is anywhere in its bytes. ok is false
// when the test has no cluster to look at (CLOUDBURROW_TEST_KUBECONFIG and
// CLOUDBURROW_TEST_CLUSTER).
func diskStorage(t *testing.T, diskID uint64, marker string) (names string, found, ok bool) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if kubeconfig == "" || cluster == "" {
		t.Logf("%s and CLOUDBURROW_TEST_CLUSTER are not set: the disk's storage is not inspected", envKubeconfig)
		return "", false, false
	}
	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", "cloudburrow"}, args...)...).Output()
		if err != nil {
			t.Fatalf("kubectl %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	pv := kubectl("get", "pvc", fmt.Sprintf("disk-%d", diskID), "-o", "jsonpath={.spec.volumeName}")
	path := kubectl("get", "pv", pv, "-o", "jsonpath={.spec.local.path}{.spec.hostPath.path}")
	if path == "" || strings.ContainsAny(path, "'\"$`;") {
		t.Fatalf("the disk's volume path: %q", path)
	}
	out, err := exec.Command("docker", "exec", cluster+"-control-plane", "ls", "-A", path).Output()
	if err != nil {
		t.Fatalf("list %s: %v", path, err)
	}
	g := exec.Command("docker", "exec", cluster+"-control-plane", "grep", "-r", "-l", "-F", marker, path)
	var gout bytes.Buffer
	g.Stdout = &gout
	_ = g.Run() // grep exits 1 when it finds nothing
	return strings.Join(strings.Fields(string(out)), " "), gout.Len() > 0, true
}

// TestComputeDiskEncryptionAndInstancePoliciesThroughTheGoClient (#2206),
// the official compute/apiv1 REST clients, on one VM:
//   - a disk with a Cloud KMS key, attached and written over SSH: once
//     the VM stops, its storage holds only the sealed file, without the
//     data; with the key version disabled the VM does not start, and with
//     it enabled again it starts and reads the data back;
//   - a disk with a customer-supplied key (rawKey): only its sha256 is
//     answered; attach needs the key; start is refused, and
//     startWithEncryptionKey with a wrong key too, and with the key the
//     data is back;
//   - placement policies: a spread policy gives the VM an availability
//     domain; a compact one, added to the stopped VM, its physical host
//     topology; an instance schedule starts the stopped VM when it falls
//     due;
//   - zonal Asynchronous Replication from us-central1-a to us-east1-b: the
//     secondary's data, read back through a disk made from its snapshot.
//
// covers: compute.instances.addResourcePolicies, compute.instances.removeResourcePolicies, compute.instances.startWithEncryptionKey, compute.disks.startAsyncReplication, compute.disks.stopAsyncReplication, compute.disks.createSnapshot, compute.resourcePolicies.insert, compute.resourcePolicies.get, compute.resourcePolicies.delete, compute.instances.start, compute.instances.stop
func TestComputeDiskEncryptionAndInstancePoliciesThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	if os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services compute,kms")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	pols, _ := computeapi.NewResourcePoliciesRESTClient(ctx, opts...)
	snaps, _ := computeapi.NewSnapshotsRESTClient(ctx, opts...)
	p := h.Project()
	const region, za, zs = "us-central1", "us-central1-a", "us-east1-b"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	vm := "enc-vm-" + sfx
	cmek, csek, prim, sec, check := "enc-cmek-"+sfx, "enc-csek-"+sfx, "enc-prim-"+sfx, "enc-sec-"+sfx, "enc-check-"+sfx
	spread, compact, sched, snap := "enc-spread-"+sfx, "enc-compact-"+sfx, "enc-sched-"+sfx, "enc-snap-"+sfx
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
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: za, Instance: vm}); err == nil {
			_ = op.Wait(c)
		}
		for _, d := range [][2]string{{za, cmek}, {za, csek}, {zs, sec}, {za, prim}, {za, check}} {
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: d[0], Disk: d[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := snaps.Delete(c, &computepb.DeleteSnapshotRequest{Project: p, Snapshot: snap}); err == nil {
			_ = op.Wait(c)
		}
		for _, n := range []string{spread, compact, sched} {
			if op, err := pols.Delete(c, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: n}); err == nil {
				_ = op.Wait(c)
			}
		}
	})
	keys, _ := diskKeys(t, h, "enc-"+sfx, region)
	kc := kmsClients(t, h)["grpc"]
	setState := func(version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %s %v: %v", version, st, err)
		}
	}
	sh := newVMShell(t, addr, p)
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: za, InstanceResource: &computepb.Instance{
		Name: proto.String(vm), MachineType: proto.String("zones/" + za + "/machineTypes/n2-standard-2"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(sh.keyLine)}}},
	}}))
	stop := func() {
		t.Helper()
		wait(insts.Stop(ctx, &computepb.StopInstanceRequest{Project: p, Zone: za, Instance: vm}))
	}
	start := func() error {
		op, err := insts.Start(ctx, &computepb.StartInstanceRequest{Project: p, Zone: za, Instance: vm})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	attach := func(disk, dev string, key *computepb.CustomerEncryptionKey) error {
		op, err := insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: za, Instance: vm,
			AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + za + "/disks/" + disk), DeviceName: proto.String(dev),
				DiskEncryptionKey: key}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	write := func(dev, text string) {
		t.Helper()
		if out, err := sh.run(za, vm, "echo "+text+" | sudo tee /mnt/disks/"+dev+"/f.txt && sync"); err != nil {
			t.Fatalf("write to %s: %v %s", dev, err, out)
		}
	}
	read := func(dev string) string {
		t.Helper()
		out, err := sh.run(za, vm, "cat /mnt/disks/"+dev+"/f.txt")
		if err != nil {
			t.Fatalf("read %s: %v %s", dev, err, out)
		}
		return strings.TrimSpace(out)
	}

	// A Cloud KMS key: sealed at rest, no start while its version is
	// disabled, the data back once it is enabled.
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(cmek), SizeGb: proto.Int64(10), DiskEncryptionKey: &computepb.CustomerEncryptionKey{KmsKeyName: proto.String(keys[region])}}}))
	if err := attach(cmek, "cmek", nil); err != nil {
		t.Fatal(err)
	}
	marker := "cmek-secret-" + sfx
	write("cmek", marker)
	stop()
	cd, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: cmek})
	if err != nil {
		t.Fatal(err)
	}
	if names, found, ok := diskStorage(t, cd.GetId(), marker); ok && (names != ".cloudburrow-sealed" || found) {
		t.Errorf("the stopped VM's CMEK disk holds %q (the data in the clear: %v), want only the sealed file", names, found)
	}
	v1 := keys[region] + "/cryptoKeyVersions/1"
	setState(v1, kmspb.CryptoKeyVersion_DISABLED)
	if err := start(); computeCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "Cloud KMS error") {
		setState(v1, kmspb.CryptoKeyVersion_ENABLED)
		t.Fatalf("start with the key version disabled: %v, want 400", err)
	}
	setState(v1, kmspb.CryptoKeyVersion_ENABLED)
	if err := start(); err != nil {
		t.Fatalf("start with the key version enabled again: %v", err)
	}
	if got := read("cmek"); got != marker {
		t.Errorf("the CMEK disk after the restart: %q, want %q", got, marker)
	}

	// A customer-supplied key.
	raw := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	wrong := base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(csek), SizeGb: proto.Int64(10), DiskEncryptionKey: &computepb.CustomerEncryptionKey{RawKey: proto.String(raw)}}}))
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: csek}); err != nil ||
		d.GetDiskEncryptionKey().GetSha256() == "" || d.GetDiskEncryptionKey().GetRawKey() != "" {
		t.Errorf("the CSEK disk's key: %v %v", d.GetDiskEncryptionKey(), err)
	}
	if err := attach(csek, "csek", nil); computeCode(err) != http.StatusBadRequest {
		t.Errorf("attach without the key: %v, want 400", err)
	}
	if err := attach(csek, "csek", &computepb.CustomerEncryptionKey{RawKey: proto.String(raw)}); err != nil {
		t.Fatal(err)
	}
	write("csek", "csek-secret-"+sfx)
	stop()
	if err := start(); computeCode(err) != http.StatusBadRequest {
		t.Errorf("start without the CSEK: %v, want 400", err)
	}
	startWith := func(k string) error {
		op, err := insts.StartWithEncryptionKey(ctx, &computepb.StartWithEncryptionKeyInstanceRequest{Project: p, Zone: za, Instance: vm,
			InstancesStartWithEncryptionKeyRequestResource: &computepb.InstancesStartWithEncryptionKeyRequest{Disks: []*computepb.CustomerEncryptionKeyProtectedDisk{{
				Source:            proto.String("projects/" + p + "/zones/" + za + "/disks/" + csek),
				DiskEncryptionKey: &computepb.CustomerEncryptionKey{RawKey: proto.String(k)}}}}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	if err := startWith(wrong); computeCode(err) != http.StatusBadRequest {
		t.Errorf("startWithEncryptionKey with a wrong key: %v, want 400", err)
	}
	if err := startWith(raw); err != nil {
		t.Fatal(err)
	}
	if got := read("csek"); got != "csek-secret-"+sfx {
		t.Errorf("the CSEK disk after startWithEncryptionKey: %q", got)
	}
	wait(insts.DetachDisk(ctx, &computepb.DetachDiskInstanceRequest{Project: p, Zone: za, Instance: vm, DeviceName: "csek"}))

	// Zonal Asynchronous Replication to us-east1-b.
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(prim), SizeGb: proto.Int64(10), Type: proto.String("zones/" + za + "/diskTypes/pd-balanced")}}))
	if err := attach(prim, "prim", nil); err != nil {
		t.Fatal(err)
	}
	write("prim", "replicated-"+sfx)
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zs, DiskResource: &computepb.Disk{
		Name: proto.String(sec), SizeGb: proto.Int64(10), Type: proto.String("zones/" + zs + "/diskTypes/pd-balanced"),
		AsyncPrimaryDisk: &computepb.DiskAsyncReplication{Disk: proto.String("projects/" + p + "/zones/" + za + "/disks/" + prim)}}}))
	wait(disks.StartAsyncReplication(ctx, &computepb.StartAsyncReplicationDiskRequest{Project: p, Zone: za, Disk: prim,
		DisksStartAsyncReplicationRequestResource: &computepb.DisksStartAsyncReplicationRequest{AsyncSecondaryDisk: proto.String("zones/" + zs + "/disks/" + sec)}}))
	if s, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zs, Disk: sec}); err != nil || s.GetResourceStatus().GetAsyncPrimaryDisk().GetState() != "ACTIVE" {
		t.Errorf("the secondary while replicating: %v %v", s.GetResourceStatus(), err)
	}
	wait(disks.StopAsyncReplication(ctx, &computepb.StopAsyncReplicationDiskRequest{Project: p, Zone: za, Disk: prim}))
	if s, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zs, Disk: sec}); err != nil || s.GetResourceStatus().GetAsyncPrimaryDisk().GetState() != "STOPPED" {
		t.Errorf("the secondary after stopAsyncReplication: %v %v", s.GetResourceStatus(), err)
	}
	wait(disks.CreateSnapshot(ctx, &computepb.CreateSnapshotDiskRequest{Project: p, Zone: zs, Disk: sec,
		SnapshotResource: &computepb.Snapshot{Name: proto.String(snap)}}))
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(check), SourceSnapshot: proto.String("global/snapshots/" + snap)}}))
	if err := attach(check, "check", nil); err != nil {
		t.Fatal(err)
	}
	if got := read("check"); got != "replicated-"+sfx {
		t.Errorf("the secondary's data: %q", got)
	}

	// Placement policies.
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(spread), GroupPlacementPolicy: &computepb.ResourcePolicyGroupPlacementPolicy{AvailabilityDomainCount: proto.Int32(2)}}}))
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(compact), GroupPlacementPolicy: &computepb.ResourcePolicyGroupPlacementPolicy{Collocation: proto.String("COLLOCATED")}}}))
	if _, err := pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(spread + "-x"), GroupPlacementPolicy: &computepb.ResourcePolicyGroupPlacementPolicy{AvailabilityDomainCount: proto.Int32(9)}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("availabilityDomainCount 9: %v, want 400", err)
	}
	policy := func(name string) string { return "projects/" + p + "/regions/" + region + "/resourcePolicies/" + name }
	addPol := func(name string) error {
		op, err := insts.AddResourcePolicies(ctx, &computepb.AddResourcePoliciesInstanceRequest{Project: p, Zone: za, Instance: vm,
			InstancesAddResourcePoliciesRequestResource: &computepb.InstancesAddResourcePoliciesRequest{ResourcePolicies: []string{policy(name)}}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	removePol := func(name string) {
		t.Helper()
		wait(insts.RemoveResourcePolicies(ctx, &computepb.RemoveResourcePoliciesInstanceRequest{Project: p, Zone: za, Instance: vm,
			InstancesRemoveResourcePoliciesRequestResource: &computepb.InstancesRemoveResourcePoliciesRequest{ResourcePolicies: []string{policy(name)}}}))
	}
	if err := addPol(spread); err != nil {
		t.Fatal(err)
	}
	if err := addPol(compact); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a second placement policy: %v, want 400", err)
	}
	if v, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: za, Instance: vm}); err != nil || v.GetScheduling().GetAvailabilityDomain() != 1 {
		t.Errorf("the spread VM's availability domain: %v %v", v.GetScheduling(), err)
	}
	removePol(spread)
	if err := addPol(compact); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a compact policy on a running VM: %v, want 400", err)
	}
	stop()
	if err := addPol(compact); err != nil {
		t.Fatal(err)
	}
	if v, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: za, Instance: vm}); err != nil || v.GetResourceStatus().GetPhysicalHostTopology().GetCluster() == "" {
		t.Errorf("the compact VM's topology: %v %v", v.GetResourceStatus(), err)
	}
	if _, err := pols.Delete(ctx, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: compact}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting a policy a VM has: %v, want 400", err)
	}
	removePol(compact)

	// An instance schedule starts the stopped VM when it falls due.
	due := time.Now().UTC().Add(2 * time.Minute)
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(sched), InstanceSchedulePolicy: &computepb.ResourcePolicyInstanceSchedulePolicy{
			VmStartSchedule: &computepb.ResourcePolicyInstanceSchedulePolicySchedule{Schedule: proto.String(fmt.Sprintf("%d %d * * *", due.Minute(), due.Hour()))},
			VmStopSchedule:  &computepb.ResourcePolicyInstanceSchedulePolicySchedule{Schedule: proto.String(fmt.Sprintf("%d %d * * *", due.Minute(), (due.Hour()+12)%24))},
			TimeZone:        proto.String("UTC")}}}))
	if err := addPol(sched); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		v, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: za, Instance: vm})
		if err != nil {
			t.Fatal(err)
		}
		if v.GetStatus() == "RUNNING" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance schedule did not start the VM: %s", v.GetStatus())
		}
		time.Sleep(10 * time.Second)
	}
	if po, err := pols.Get(ctx, &computepb.GetResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: sched}); err != nil ||
		po.GetResourceStatus().GetInstanceSchedulePolicy().GetLastRunStartTime() == "" {
		t.Errorf("the schedule's status: %v %v", po.GetResourceStatus(), err)
	}
	removePol(sched)
}

// TestGcloudComputeInstancePolicies (#2206): gcloud compute
// resource-policies create instance-schedule and group-placement, and
// instances add-resource-policies and remove-resource-policies, with
// egress to Google blocked and every command against Compute Engine here.
func TestGcloudComputeInstancePolicies(t *testing.T) {
	h := New(t)
	_ = computeOpts(t, h)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	const region, zone = "us-central1", "us-central1-a"
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
	vm, sched, spread := "gp-vm-"+sfx, "gp-sched-"+sfx, "gp-spread-"+sfx
	t.Cleanup(func() {
		_, _ = run("compute", "instances", "delete", vm, "--zone", zone)
		_, _ = run("compute", "resource-policies", "delete", sched, "--region", region)
		_, _ = run("compute", "resource-policies", "delete", spread, "--region", region)
	})
	if out, err := run("compute", "resource-policies", "create", "instance-schedule", sched, "--region", region,
		"--vm-start-schedule", "0,30 8 * * *", "--timezone", "UTC"); err == nil {
		t.Errorf("two starts an hour: %s", out)
	}
	gc("compute", "resource-policies", "create", "instance-schedule", sched, "--region", region,
		"--vm-start-schedule", "0 8 * * 1-5", "--vm-stop-schedule", "0 18 * * 1-5", "--timezone", "Europe/Paris")
	if got := gc("compute", "resource-policies", "describe", sched, "--region", region,
		"--format=value(instanceSchedulePolicy.vmStartSchedule.schedule,instanceSchedulePolicy.timeZone)"); !strings.Contains(got, "0 8 * * 1-5") || !strings.Contains(got, "Europe/Paris") {
		t.Errorf("describe the schedule: %s", got)
	}
	gc("compute", "resource-policies", "create", "group-placement", spread, "--region", region, "--availability-domain-count", "2")
	gc("compute", "instances", "create", vm, "--zone", zone, "--machine-type", "e2-small",
		"--image-family", "debian-12", "--image-project", "debian-cloud", "--resource-policies", sched)
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=value(resourcePolicies)"); !strings.Contains(got, sched) {
		t.Errorf("the VM's policies after create: %s", got)
	}
	if out, err := run("compute", "resource-policies", "delete", sched, "--region", region); err == nil {
		t.Errorf("deleted a schedule a VM has: %s", out)
	}
	gc("compute", "instances", "add-resource-policies", vm, "--zone", zone, "--resource-policies", spread)
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=value(scheduling.availabilityDomain)"); strings.TrimSpace(got) != "1" {
		t.Errorf("the availability domain: %q", got)
	}
	gc("compute", "instances", "remove-resource-policies", vm, "--zone", zone, "--resource-policies", sched+","+spread)
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=value(resourcePolicies)"); strings.TrimSpace(got) != "" {
		t.Errorf("the VM's policies after remove: %s", got)
	}
	gc("compute", "resource-policies", "delete", sched, "--region", region)
}

// TestTerraformComputeInstanceScheduleAndCSEKDisk (#2206): a
// google_compute_resource_policy instance schedule and a spread placement
// policy on a google_compute_instance (resource_policies), and a
// google_compute_disk with a customer-supplied raw_key, through
// `cloudburrow terraform` with egress to Google blocked.
func TestTerraformComputeInstanceScheduleAndCSEKDisk(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	m := newTFModule(t)
	const region, zone = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	name := "tfs-" + sfx
	raw := base64.StdEncoding.EncodeToString([]byte("terraform-csek-key-0123456789abc"))
	t.Cleanup(func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: m.project, Zone: zone, Instance: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: m.project, Zone: zone, Disk: name + "-csek"}); err == nil {
			_ = op.Wait(c)
		}
	})
	m.write(fmt.Sprintf(`
resource "google_compute_resource_policy" "sched" {
  project = %[1]q
  name    = "%[2]s-sched"
  region  = %[4]q
  instance_schedule_policy {
    vm_start_schedule {
      schedule = "0 8 * * *"
    }
    vm_stop_schedule {
      schedule = "0 18 * * *"
    }
    time_zone = "UTC"
  }
}

resource "google_compute_instance" "vm" {
  project           = %[1]q
  name              = %[2]q
  zone              = %[3]q
  machine_type      = "e2-small"
  resource_policies = [google_compute_resource_policy.sched.self_link]
  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
    }
  }
  network_interface {
    network = "default"
  }
}

resource "google_compute_disk" "csek" {
  project = %[1]q
  name    = "%[2]s-csek"
  zone    = %[3]q
  size    = 10
  disk_encryption_key {
    raw_key = %[5]q
  }
}
`, m.project, name, zone, region, raw))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	v, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: m.project, Zone: zone, Instance: name})
	if err != nil || len(v.GetResourcePolicies()) != 1 || !strings.HasSuffix(v.GetResourcePolicies()[0], "/resourcePolicies/"+name+"-sched") {
		t.Fatalf("the VM Terraform made: %v, %v", v.GetResourcePolicies(), err)
	}
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: m.project, Zone: zone, Disk: name + "-csek"})
	if err != nil || d.GetDiskEncryptionKey().GetSha256() == "" {
		t.Fatalf("the CSEK disk Terraform made: %v, %v", d.GetDiskEncryptionKey(), err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: m.project, Zone: zone, Instance: name}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the VM after destroy: %v", err)
	}
}
