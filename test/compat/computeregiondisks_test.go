//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// vmShell is SSH into the VMs of a test: a key, the ssh-keys metadata
// line that installs it, and run, which runs a command on a running VM
// through its forwarded port, retrying while it boots.
type vmShell struct {
	keyLine string
	run     func(zone, vm, cmd string) (string, error)
}

func newVMShell(t *testing.T, computeAddr, project string) vmShell {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	sh := vmShell{keyLine: "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"}
	sh.run = func(zone, vm, cmd string) (string, error) {
		t.Helper()
		var out string
		var err error
		deadline := time.Now().Add(3 * time.Minute)
		for {
			var c *ssh.Client
			c, err = ssh.Dial("tcp", vmSSHAddress(t, computeAddr, project, zone, vm), &ssh.ClientConfig{User: "tester",
				Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				s, err2 := c.NewSession()
				if err2 == nil {
					b, err3 := s.CombinedOutput(cmd)
					c.Close()
					// The command ran: its own failure is its answer.
					return string(b), err3
				}
				err = err2
				c.Close()
			}
			if time.Now().After(deadline) {
				return out, fmt.Errorf("ssh %s/%s: %w", zone, vm, err)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return sh
}

// TestComputeRegionalDisksThroughTheGoClient (#2206), the official
// compute/apiv1 REST client: regionDiskTypes and diskTypes.aggregatedList; a regional disk's documented
// refusals (one replica zone, the same zone twice, regional pd-standard
// under 200 GB); a regional disk attached to a VM in us-central1-a, a file
// written to it over SSH; the attach refused from a VM in us-central1-b
// while it is attached, then the failover by forceAttach, the zone-b VM
// reading the file zone a's wrote and zone a's VM no longer able to
// write; resize, setLabels, IAM, a snapshot; a consistency group and
// Asynchronous Replication to a secondary disk in us-east1 (start, the
// secondary's clone by bulkInsert, stopGroupAsyncReplication), the
// secondary's data read back in a disk made from its snapshot; and the
// deletes.
// covers: compute.diskTypes.aggregatedList, compute.regionDiskTypes.list, compute.regionDiskTypes.get, compute.regionDisks.insert, compute.regionDisks.get, compute.regionDisks.list, compute.regionDisks.delete, compute.regionDisks.resize, compute.regionDisks.setLabels, compute.regionDisks.createSnapshot, compute.regionDisks.getIamPolicy, compute.regionDisks.setIamPolicy, compute.regionDisks.testIamPermissions, compute.regionDisks.addResourcePolicies, compute.regionDisks.removeResourcePolicies, compute.regionDisks.startAsyncReplication, compute.regionDisks.stopAsyncReplication, compute.regionDisks.stopGroupAsyncReplication, compute.regionDisks.bulkInsert, compute.resourcePolicies.insert, compute.resourcePolicies.get, compute.resourcePolicies.list, compute.resourcePolicies.delete, compute.resourcePolicies.aggregatedList, compute.instances.attachDisk, compute.regionOperations.get
func TestComputeRegionalDisksThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	rdisks, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rtypes, _ := computeapi.NewRegionDiskTypesRESTClient(ctx, opts...)
	pols, _ := computeapi.NewResourcePoliciesRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	snaps, _ := computeapi.NewSnapshotsRESTClient(ctx, opts...)
	p := h.Project()
	const region, region2, za, zb = "us-central1", "us-east1", "us-central1-a", "us-central1-b"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	rd, sec, vma, vmb := "rd-"+sfx, "rd-sec-"+sfx, "rd-vma-"+sfx, "rd-vmb-"+sfx
	cg, cg2, snap, check := "rd-cg-"+sfx, "rd-cg2-"+sfx, "rd-snap-"+sfx, "rd-check-"+sfx
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
		for _, v := range [][2]string{{za, vma}, {zb, vmb}} {
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: v[0], Instance: v[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, d := range [][2]string{{region, rd}, {region2, sec}, {region2, sec + "-clone"}} {
			if op, err := rdisks.Delete(c, &computepb.DeleteRegionDiskRequest{Project: p, Region: d[0], Disk: d[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: zb, Disk: check}); err == nil {
			_ = op.Wait(c)
		}
		for _, s := range []string{snap, snap + "-sec"} {
			if op, err := snaps.Delete(c, &computepb.DeleteSnapshotRequest{Project: p, Snapshot: s}); err == nil {
				_ = op.Wait(c)
			}
		}
		for _, pl := range [][2]string{{region, cg}, {region2, cg2}} {
			if op, err := pols.Delete(c, &computepb.DeleteResourcePolicyRequest{Project: p, Region: pl[0], ResourcePolicy: pl[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
	})

	found := map[string]string{}
	for it := rtypes.List(ctx, &computepb.ListRegionDiskTypesRequest{Project: p, Region: region}); ; {
		dt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[dt.GetName()] = dt.GetValidDiskSize()
	}
	if found["pd-standard"] != "200GB-65536GB" || found["hyperdisk-balanced-high-availability"] == "" || found["pd-balanced"] == "" {
		t.Fatalf("regionDiskTypes: %v", found)
	}
	ztypes, err := computeapi.NewDiskTypesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]int{}
	for it := ztypes.AggregatedList(ctx, &computepb.AggregatedListDiskTypesRequest{Project: p}); ; {
		pair, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		scopes[pair.Key] = len(pair.Value.GetDiskTypes())
	}
	if scopes["regions/"+region] != len(found) || scopes["zones/"+za] == 0 {
		t.Errorf("diskTypes.aggregatedList: %v", scopes)
	}
	if dt, err := rtypes.Get(ctx, &computepb.GetRegionDiskTypeRequest{Project: p, Region: region, DiskType: "pd-ssd"}); err != nil || dt.GetValidDiskSize() != "10GB-65536GB" {
		t.Fatalf("regionDiskTypes.get pd-ssd: %v %v", dt, err)
	}
	zones := []string{"zones/" + za, "zones/" + zb}
	for name, d := range map[string]*computepb.Disk{
		"one replica zone": {Name: proto.String(rd), SizeGb: proto.Int64(10), ReplicaZones: zones[:1]},
		"the same zone":    {Name: proto.String(rd), SizeGb: proto.Int64(10), ReplicaZones: []string{"zones/" + za, "zones/" + za}},
		"a small standard": {Name: proto.String(rd), SizeGb: proto.Int64(100), ReplicaZones: zones, Type: proto.String("regions/" + region + "/diskTypes/pd-standard")},
	} {
		if _, err := rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: d}); computeCode(err) != http.StatusBadRequest {
			t.Errorf("a regional disk with %s: %v, want 400", name, err)
		}
	}
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(cg), DiskConsistencyGroupPolicy: &computepb.ResourcePolicyDiskConsistencyGroupPolicy{}}}))
	if pol, err := pols.Get(ctx, &computepb.GetResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: cg}); err != nil || pol.GetDiskConsistencyGroupPolicy() == nil {
		t.Fatalf("resourcePolicies.get: %v %v", pol, err)
	}
	listedPolicy := false
	for it := pols.List(ctx, &computepb.ListResourcePoliciesRequest{Project: p, Region: region}); ; {
		pol, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		listedPolicy = listedPolicy || pol.GetName() == cg
	}
	if !listedPolicy {
		t.Error("the consistency group is not listed")
	}
	if it := pols.AggregatedList(ctx, &computepb.AggregatedListResourcePoliciesRequest{Project: p}); true {
		pair, err := it.Next()
		if err != nil || pair.Key != "regions/"+region {
			t.Errorf("resourcePolicies.aggregatedList: %v %v", pair.Key, err)
		}
	}
	wait(rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: &computepb.Disk{
		Name: proto.String(rd), SizeGb: proto.Int64(10), ReplicaZones: zones, Labels: map[string]string{"env": "test"}}}))
	d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd})
	if err != nil || d.GetStatus() != "READY" || len(d.GetReplicaZones()) != 2 || !strings.HasSuffix(d.GetType(), "/regions/"+region+"/diskTypes/pd-balanced") {
		t.Fatalf("the regional disk: %v %v", d, err)
	}
	wait(rdisks.AddResourcePolicies(ctx, &computepb.AddResourcePoliciesRegionDiskRequest{Project: p, Region: region, Disk: rd,
		RegionDisksAddResourcePoliciesRequestResource: &computepb.RegionDisksAddResourcePoliciesRequest{ResourcePolicies: []string{"regions/" + region + "/resourcePolicies/" + cg}}}))
	listed := false
	for it := rdisks.List(ctx, &computepb.ListRegionDisksRequest{Project: p, Region: region}); ; {
		dd, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		listed = listed || dd.GetName() == rd && len(dd.GetResourcePolicies()) == 1
	}
	if !listed {
		t.Error("the regional disk is not listed with its consistency group")
	}

	// IAM on the regional disk.
	pol, err := rdisks.SetIamPolicy(ctx, &computepb.SetIamPolicyRegionDiskRequest{Project: p, Region: region, Resource: rd,
		RegionSetPolicyRequestResource: &computepb.RegionSetPolicyRequest{Policy: &computepb.Policy{Bindings: []*computepb.Binding{
			{Role: proto.String("roles/compute.storageAdmin"), Members: []string{"user:ops@example.com"}}}}}})
	if err != nil || len(pol.GetBindings()) != 1 {
		t.Fatalf("setIamPolicy: %v %v", pol, err)
	}
	if got, err := rdisks.GetIamPolicy(ctx, &computepb.GetIamPolicyRegionDiskRequest{Project: p, Region: region, Resource: rd}); err != nil || got.GetEtag() != pol.GetEtag() {
		t.Errorf("getIamPolicy: %v %v", got, err)
	}
	if tp, err := rdisks.TestIamPermissions(ctx, &computepb.TestIamPermissionsRegionDiskRequest{Project: p, Region: region, Resource: rd,
		TestPermissionsRequestResource: &computepb.TestPermissionsRequest{Permissions: []string{"compute.disks.get"}}}); err != nil || len(tp.GetPermissions()) != 1 {
		t.Errorf("testIamPermissions: %v %v", tp, err)
	}

	// The VMs, one in each replica zone, and the failover.
	sh := newVMShell(t, addr, p)
	newVM := func(zone, name string) {
		t.Helper()
		wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
			Name: proto.String(name), MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
			NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default"),
				AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT")}}}},
			Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(sh.keyLine)}}},
		}}))
	}
	newVM(za, vma)
	newVM(zb, vmb)
	attach := func(zone, vm string, force bool) error {
		req := &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: vm,
			AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("regions/" + region + "/disks/" + rd), DeviceName: proto.String("shared")}}
		if force {
			req.ForceAttach = proto.Bool(true)
		}
		op, err := insts.AttachDisk(ctx, req)
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	if err := attach(za, vma, false); err != nil {
		t.Fatal(err)
	}
	if out, err := sh.run(za, vma, "echo written-in-zone-a | sudo tee /mnt/disks/shared/failover.txt && sync"); err != nil {
		t.Fatalf("writing in zone a: %v %s", err, out)
	}
	if err := attach(zb, vmb, false); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("attaching in zone b while zone a's VM has it: %v, want 400", err)
	}
	if err := attach(zb, vmb, true); err != nil {
		t.Fatalf("force-attach in zone b: %v", err)
	}
	if out, err := sh.run(zb, vmb, "cat /mnt/disks/shared/failover.txt"); err != nil || strings.TrimSpace(out) != "written-in-zone-a" {
		t.Fatalf("zone b's VM reads %q (%v); want what zone a's VM wrote", out, err)
	}
	// Zone a's VM, restarted fenced, can no longer write the disk.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := sh.run(za, vma, "sudo touch /mnt/disks/shared/after-failover && echo WROTE")
		if !strings.Contains(out, "WROTE") && err != nil && strings.Contains(out, "Read-only") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("zone a's VM still writes the disk after the force-attach: %q %v", out, err)
		}
		time.Sleep(3 * time.Second)
	}

	// Resize, labels, a snapshot.
	wait(rdisks.Resize(ctx, &computepb.ResizeRegionDiskRequest{Project: p, Region: region, Disk: rd,
		RegionDisksResizeRequestResource: &computepb.RegionDisksResizeRequest{SizeGb: proto.Int64(20)}}))
	d, _ = rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd})
	wait(rdisks.SetLabels(ctx, &computepb.SetLabelsRegionDiskRequest{Project: p, Region: region, Resource: rd,
		RegionSetLabelsRequestResource: &computepb.RegionSetLabelsRequest{LabelFingerprint: d.LabelFingerprint, Labels: map[string]string{"env": "prod"}}}))
	if d, _ = rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd}); d.GetSizeGb() != 20 || d.GetLabels()["env"] != "prod" ||
		len(d.GetUsers()) != 2 {
		t.Errorf("after resize and setLabels: %d GB, %v, users %v", d.GetSizeGb(), d.GetLabels(), d.GetUsers())
	}
	wait(rdisks.CreateSnapshot(ctx, &computepb.CreateSnapshotRegionDiskRequest{Project: p, Region: region, Disk: rd,
		SnapshotResource: &computepb.Snapshot{Name: proto.String(snap)}}))
	if s, err := snaps.Get(ctx, &computepb.GetSnapshotRequest{Project: p, Snapshot: snap}); err != nil || s.GetStatus() != "READY" ||
		!strings.HasSuffix(s.GetSourceDisk(), "/regions/"+region+"/disks/"+rd) {
		t.Fatalf("the regional disk's snapshot: %v %v", s, err)
	}

	// Asynchronous Replication to us-east1, and the secondary's group.
	wait(rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region2, DiskResource: &computepb.Disk{
		Name: proto.String(sec), ReplicaZones: []string{"zones/us-east1-b", "zones/us-east1-c"},
		AsyncPrimaryDisk: &computepb.DiskAsyncReplication{Disk: proto.String("projects/" + p + "/regions/" + region + "/disks/" + rd)}}}))
	if s, _ := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region2, Disk: sec}); s.GetSizeGb() != 20 {
		t.Fatalf("the secondary is %d GB, want the primary's 20", s.GetSizeGb())
	}
	if _, err := rdisks.StopAsyncReplication(ctx, &computepb.StopAsyncReplicationRegionDiskRequest{Project: p, Region: region2, Disk: sec}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("stopAsyncReplication before it started: %v, want 400", err)
	}
	wait(rdisks.StartAsyncReplication(ctx, &computepb.StartAsyncReplicationRegionDiskRequest{Project: p, Region: region, Disk: rd,
		RegionDisksStartAsyncReplicationRequestResource: &computepb.RegionDisksStartAsyncReplicationRequest{AsyncSecondaryDisk: proto.String("regions/" + region2 + "/disks/" + sec)}}))
	if s, _ := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region2, Disk: sec}); s.GetResourceStatus().GetAsyncPrimaryDisk().GetState() != "ACTIVE" {
		t.Fatalf("the secondary while replicating: %v", s.GetResourceStatus())
	}
	if _, err := rdisks.RemoveResourcePolicies(ctx, &computepb.RemoveResourcePoliciesRegionDiskRequest{Project: p, Region: region, Disk: rd,
		RegionDisksRemoveResourcePoliciesRequestResource: &computepb.RegionDisksRemoveResourcePoliciesRequest{ResourcePolicies: []string{"regions/" + region + "/resourcePolicies/" + cg}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("removing a replicating primary from its group: %v, want 400", err)
	}
	if out, err := sh.run(zb, vmb, "echo replicated | sudo tee /mnt/disks/shared/replica.txt && sync"); err != nil {
		t.Fatalf("writing in zone b: %v %s", err, out)
	}
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region2, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(cg2), DiskConsistencyGroupPolicy: &computepb.ResourcePolicyDiskConsistencyGroupPolicy{}}}))
	wait(rdisks.AddResourcePolicies(ctx, &computepb.AddResourcePoliciesRegionDiskRequest{Project: p, Region: region2, Disk: sec,
		RegionDisksAddResourcePoliciesRequestResource: &computepb.RegionDisksAddResourcePoliciesRequest{ResourcePolicies: []string{cg2}}}))
	wait(rdisks.BulkInsert(ctx, &computepb.BulkInsertRegionDiskRequest{Project: p, Region: region2,
		BulkInsertDiskResourceResource: &computepb.BulkInsertDiskResource{SourceConsistencyGroupPolicy: proto.String("projects/" + p + "/regions/" + region2 + "/resourcePolicies/" + cg2)}}))
	if c, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region2, Disk: sec + "-clone"}); err != nil || c.GetStatus() != "READY" ||
		!strings.HasSuffix(c.GetSourceConsistencyGroupPolicy(), "/resourcePolicies/"+cg2) {
		t.Fatalf("the clone of the group: %v %v", c, err)
	}
	wait(rdisks.StopGroupAsyncReplication(ctx, &computepb.StopGroupAsyncReplicationRegionDiskRequest{Project: p, Region: region2,
		DisksStopGroupAsyncReplicationResourceResource: &computepb.DisksStopGroupAsyncReplicationResource{ResourcePolicy: proto.String("projects/" + p + "/regions/" + region2 + "/resourcePolicies/" + cg2)}}))
	if s, _ := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region2, Disk: sec}); s.GetResourceStatus().GetAsyncPrimaryDisk().GetState() != "STOPPED" {
		t.Fatalf("the secondary after stopGroupAsyncReplication: %v", s.GetResourceStatus())
	}
	// The secondary has what zone b's VM wrote: read in a disk made from
	// its snapshot, attached to that VM.
	wait(rdisks.CreateSnapshot(ctx, &computepb.CreateSnapshotRegionDiskRequest{Project: p, Region: region2, Disk: sec,
		SnapshotResource: &computepb.Snapshot{Name: proto.String(snap + "-sec")}}))
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zb, DiskResource: &computepb.Disk{
		Name: proto.String(check), SourceSnapshot: proto.String("global/snapshots/" + snap + "-sec")}}))
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zb, Instance: vmb,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + zb + "/disks/" + check), DeviceName: proto.String("check")}}))
	if out, err := sh.run(zb, vmb, "cat /mnt/disks/check/replica.txt /mnt/disks/check/failover.txt"); err != nil ||
		!strings.Contains(out, "replicated") || !strings.Contains(out, "written-in-zone-a") {
		t.Fatalf("the secondary's data: %q %v", out, err)
	}

	// The deletes: the group in use first.
	if _, err := pols.Delete(ctx, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: cg}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting a consistency group in use: %v, want 400", err)
	}
	wait(insts.Delete(ctx, &computepb.DeleteInstanceRequest{Project: p, Zone: za, Instance: vma}))
	wait(insts.Delete(ctx, &computepb.DeleteInstanceRequest{Project: p, Zone: zb, Instance: vmb}))
	wait(rdisks.RemoveResourcePolicies(ctx, &computepb.RemoveResourcePoliciesRegionDiskRequest{Project: p, Region: region, Disk: rd,
		RegionDisksRemoveResourcePoliciesRequestResource: &computepb.RegionDisksRemoveResourcePoliciesRequest{ResourcePolicies: []string{cg}}}))
	wait(rdisks.Delete(ctx, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: rd}))
	if _, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the regional disk after delete: %v", err)
	}
	wait(pols.Delete(ctx, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: cg}))
}

// TestGcloudComputeRegionalDisks (#2206): the pinned gcloud through
// gcloud-setup, egress to Google blocked, every command against Compute
// Engine, which is served and overridden: disks create --region
// --replica-zones, describe, list, resize, add-labels, snapshot, a
// consistency group (resource-policies create disk-consistency-group),
// add-resource-policies, a secondary made with --primary-disk,
// start-async-replication, stop-async-replication, instances attach-disk
// --disk-scope regional (--force-attach for the second VM) and the deletes.
func TestGcloudComputeRegionalDisks(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	rdisks, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	gdir, home := t.TempDir(), t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	run := func(args ...string) (string, error) {
		if !hasStr(args, "--project") {
			args = append(args, "--project", h.Project())
		}
		cmd := exec.Command(gcloud, append(args, "--quiet")...)
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
	const region = "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	rd, sec, cg, snap, vma, vmb := "gr-"+sfx, "gr-sec-"+sfx, "gr-cg-"+sfx, "gr-snap-"+sfx, "gr-vma-"+sfx, "gr-vmb-"+sfx
	t.Cleanup(func() {
		_, _ = run("compute", "instances", "delete", vma, "--zone", region+"-a")
		_, _ = run("compute", "instances", "delete", vmb, "--zone", region+"-b")
		_, _ = run("compute", "disks", "delete", sec, "--region", "us-east1")
		_, _ = run("compute", "disks", "delete", rd, "--region", region)
		_, _ = run("compute", "snapshots", "delete", snap)
		_, _ = run("compute", "resource-policies", "delete", cg, "--region", region)
	})
	if got := gc("compute", "disk-types", "list", "--filter=region:"+region, "--format=value(name)"); !strings.Contains(got, "hyperdisk-balanced-high-availability") {
		t.Errorf("disk-types list --regions: %s", got)
	}
	if out, err := run("compute", "disks", "create", rd, "--region", region, "--replica-zones", region+"-a,"+region+"-a", "--size", "10GB"); err == nil {
		t.Errorf("a regional disk in one zone twice: %s", out)
	}
	gc("compute", "resource-policies", "create", "disk-consistency-group", cg, "--region", region)
	gc("compute", "disks", "create", rd, "--region", region, "--replica-zones", region+"-a,"+region+"-b", "--size", "10GB", "--type", "pd-balanced")
	if got := gc("compute", "disks", "describe", rd, "--region", region, "--format=value(status,sizeGb,replicaZones.len())"); !strings.Contains(got, "READY") || !strings.Contains(got, "10") {
		t.Fatalf("disks describe --region: %s", got)
	}
	if got := gc("compute", "disks", "list", "--format=value(name,region.basename())"); !strings.Contains(got, rd) || !strings.Contains(got, region) {
		t.Errorf("disks list: %s", got)
	}
	gc("compute", "disks", "resize", rd, "--region", region, "--size", "20GB")
	gc("compute", "disks", "add-labels", rd, "--region", region, "--labels", "env=gcloud")
	gc("compute", "disks", "add-resource-policies", rd, "--region", region, "--resource-policies", cg)
	if got := gc("compute", "disks", "describe", rd, "--region", region, "--format=value(sizeGb,labels.env,resourcePolicies[0].basename())"); !strings.Contains(got, "20") ||
		!strings.Contains(got, "gcloud") || !strings.Contains(got, cg) {
		t.Errorf("after resize, add-labels and add-resource-policies: %s", got)
	}
	gc("compute", "disks", "snapshot", rd, "--region", region, "--snapshot-names", snap)
	gc("compute", "disks", "create", sec, "--region", "us-east1", "--replica-zones", "us-east1-b,us-east1-c", "--size", "20GB",
		"--primary-disk", rd, "--primary-disk-region", region)
	gc("compute", "disks", "start-async-replication", rd, "--region", region, "--secondary-disk", sec, "--secondary-disk-region", "us-east1")
	if got := gc("compute", "disks", "describe", sec, "--region", "us-east1", "--format=value(resourceStatus.asyncPrimaryDisk.state)"); strings.TrimSpace(got) != "ACTIVE" {
		t.Errorf("the secondary while replicating: %q", got)
	}
	gc("compute", "disks", "stop-async-replication", rd, "--region", region)
	if got := gc("compute", "disks", "describe", sec, "--region", "us-east1", "--format=value(resourceStatus.asyncPrimaryDisk.state)"); strings.TrimSpace(got) != "STOPPED" {
		t.Errorf("the secondary after stop-async-replication: %q", got)
	}
	for _, v := range [][2]string{{vma, region + "-a"}, {vmb, region + "-b"}} {
		gc("compute", "instances", "create", v[0], "--zone", v[1], "--machine-type", "e2-small", "--image-family", "debian-12", "--image-project", "debian-cloud")
	}
	gc("compute", "instances", "attach-disk", vma, "--zone", region+"-a", "--disk", rd, "--disk-scope", "regional", "--device-name", "shared")
	if out, err := run("compute", "instances", "attach-disk", vmb, "--zone", region+"-b", "--disk", rd, "--disk-scope", "regional"); err == nil {
		t.Errorf("a second read-write attachment without --force-attach: %s", out)
	}
	gc("compute", "instances", "attach-disk", vmb, "--zone", region+"-b", "--disk", rd, "--disk-scope", "regional", "--force-attach", "--device-name", "shared")
	d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: h.Project(), Region: region, Disk: rd})
	if err != nil || len(d.GetUsers()) != 2 {
		t.Fatalf("the disk after the force-attach: %v %v", d.GetUsers(), err)
	}
	gc("compute", "instances", "delete", vma, "--zone", region+"-a")
	gc("compute", "instances", "delete", vmb, "--zone", region+"-b")
	gc("compute", "disks", "remove-resource-policies", rd, "--region", region, "--resource-policies", cg)
	gc("compute", "disks", "delete", sec, "--region", "us-east1")
	gc("compute", "disks", "delete", rd, "--region", region)
	gc("compute", "snapshots", "delete", snap)
	gc("compute", "resource-policies", "delete", cg, "--region", region)
	if _, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: h.Project(), Region: region, Disk: rd}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the disk after gcloud deleted it: %v", err)
	}
}

// TestTerraformComputeRegionDisk (#2206): google_compute_region_disk with
// its replica zones and labels, google_compute_resource_policy as a
// disk consistency group and google_compute_region_disk_resource_policy_attachment
// apply through `cloudburrow terraform` with egress to Google blocked; the
// official client reads the disk back; the plan is clean; a resize and a
// label change apply in place and the plan is clean again; destroy removes
// them.
func TestTerraformComputeRegionDisk(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	rdisks, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	pols, _ := computeapi.NewResourcePoliciesRESTClient(ctx, opts...)
	m := newTFModule(t)
	const region = "us-central1"
	name := fmt.Sprintf("tfrd-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		c := context.Background()
		if op, err := rdisks.Delete(c, &computepb.DeleteRegionDiskRequest{Project: m.project, Region: region, Disk: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := pols.Delete(c, &computepb.DeleteResourcePolicyRequest{Project: m.project, Region: region, ResourcePolicy: name + "-cg"}); err == nil {
			_ = op.Wait(c)
		}
	})
	module := func(size int, env string) string {
		return fmt.Sprintf(`
resource "google_compute_resource_policy" "cg" {
  project = %[1]q
  name    = "%[2]s-cg"
  region  = %[3]q
  disk_consistency_group_policy {
    enabled = true
  }
}
resource "google_compute_region_disk" "rd" {
  project       = %[1]q
  name          = %[2]q
  region        = %[3]q
  type          = "pd-balanced"
  size          = %[4]d
  replica_zones = ["%[3]s-a", "%[3]s-b"]
  labels        = { env = %[5]q }
}
resource "google_compute_region_disk_resource_policy_attachment" "cg" {
  project = %[1]q
  name    = google_compute_resource_policy.cg.name
  disk    = google_compute_region_disk.rd.name
  region  = %[3]q
}
`, m.project, name, region, size, env)
	}
	m.write(module(10, "dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: m.project, Region: region, Disk: name})
	if err != nil || d.GetSizeGb() != 10 || len(d.GetReplicaZones()) != 2 || d.GetLabels()["env"] != "dev" || len(d.GetResourcePolicies()) != 1 {
		t.Fatalf("the regional disk Terraform made: %v, %v", d, err)
	}
	m.planClean()
	m.write(module(20, "prod"))
	m.apply()
	if d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: m.project, Region: region, Disk: name}); err != nil || d.GetSizeGb() != 20 || d.GetLabels()["env"] != "prod" ||
		d.GetId() == 0 {
		t.Fatalf("after the resize and label change: %v, %v", d, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: m.project, Region: region, Disk: name}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the disk after destroy: %v", err)
	}
	if _, err := pols.Get(ctx, &computepb.GetResourcePolicyRequest{Project: m.project, Region: region, ResourcePolicy: name + "-cg"}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the consistency group after destroy: %v", err)
	}
}
