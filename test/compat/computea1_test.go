//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// a1Waiter is Insert-and-Wait for the compute clients' operations.
func a1Waiter(ctx context.Context, t *testing.T) func(op *computeapi.Operation, err error) {
	return func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// a1VM is an instance of debian-12 in zone with the NIC and extra disks
// given.
func a1VM(name, zone string, nic *computepb.NetworkInterface, md []*computepb.Items, extra ...*computepb.AttachedDisk) *computepb.Instance {
	disks := append([]*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
		InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}}, extra...)
	return &computepb.Instance{Name: proto.String(name), MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: disks, NetworkInterfaces: []*computepb.NetworkInterface{nic}, Metadata: &computepb.Metadata{Items: md}}
}

// a1SSH runs commands in VMs over their forwarded SSH ports, as tester.
type a1SSH struct {
	t       *testing.T
	addr, p string
	signer  ssh.Signer
	keyLine string
}

func newA1SSH(t *testing.T, addr, project string) *a1SSH {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	return &a1SSH{t: t, addr: addr, p: project, signer: signer,
		keyLine: "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"}
}

func (a *a1SSH) run(zone, vm, cmd string) string {
	a.t.Helper()
	sshAddr := vmSSHAddress(a.t, a.addr, a.p, zone, vm)
	var out string
	var err error
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var c *ssh.Client
		c, err = ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(a.signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
		if err == nil {
			var s *ssh.Session
			if s, err = c.NewSession(); err == nil {
				var b []byte
				b, err = s.CombinedOutput(cmd)
				out = string(b)
			}
			c.Close()
			if err == nil {
				return out
			}
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("ssh %s %q: %v: %s", vm, cmd, err, out)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestComputeLabelsSchedulingAndExternalIPsThroughTheGoClient (#2139,
// #2140), the official compute/apiv1 REST client: setLabels on a custom
// image and a machine image under their label fingerprints (a stale one
// refused); setScheduling's Standard to Spot refused while the VM runs and
// applied once stopped; each VM's ephemeral natIP its own 192.0.2.0/24
// address (SSH still on the forwarded port); addresses.insert with that
// natIP promoting it to a static address IN_USE by the VM; a reserved
// address given to another VM with deleteAccessConfig and
// addAccessConfig; addresses.delete refused while IN_USE; the address
// RESERVED again once its VM is deleted.
// covers-pending (EPF: make this "covers:" once run): compute.images.insert, compute.images.setLabels, compute.images.get, compute.images.delete, compute.machineImages.insert, compute.machineImages.get, compute.machineImages.setLabels, compute.machineImages.delete, compute.instances.setScheduling, compute.instances.addAccessConfig, compute.instances.deleteAccessConfig, compute.addresses.insert, compute.addresses.get, compute.addresses.delete, compute.instances.stop, compute.instances.start
func TestComputeLabelsSchedulingAndExternalIPsThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	images, _ := computeapi.NewImagesRESTClient(ctx, opts...)
	mis, _ := computeapi.NewMachineImagesRESTClient(ctx, opts...)
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vm1, vm2, img, mi, promoted, fixed := "a1-vm1-"+sfx, "a1-vm2-"+sfx, "a1-img-"+sfx, "a1-mi-"+sfx, "a1-ip-"+sfx, "a1-fixed-"+sfx
	wait := a1Waiter(ctx, t)
	t.Cleanup(func() {
		c := context.Background()
		for _, n := range []string{vm1, vm2} {
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: n}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := images.Delete(c, &computepb.DeleteImageRequest{Project: p, Image: img}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := mis.Delete(c, &computepb.DeleteMachineImageRequest{Project: p, MachineImage: mi}); err == nil {
			_ = op.Wait(c)
		}
		for _, a := range []string{promoted, fixed} {
			if op, err := addrs.Delete(c, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: a}); err == nil {
				_ = op.Wait(c)
			}
		}
	})
	nat := func() *computepb.NetworkInterface {
		return &computepb.NetworkInterface{Network: proto.String("global/networks/default"),
			AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT")}}}
	}
	var wg sync.WaitGroup
	for _, n := range []string{vm1, vm2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: a1VM(n, zone, nat(), nil)}))
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	natOf := func(n string) string {
		t.Helper()
		i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: n})
		if err != nil {
			t.Fatal(err)
		}
		acs := i.GetNetworkInterfaces()[0].GetAccessConfigs()
		if len(acs) == 0 {
			return ""
		}
		return acs[0].GetNatIP()
	}
	ip1, ip2 := natOf(vm1), natOf(vm2)
	if !strings.HasPrefix(ip1, "192.0.2.") || !strings.HasPrefix(ip2, "192.0.2.") || ip1 == ip2 {
		t.Fatalf("ephemeral natIPs %q, %q", ip1, ip2)
	}
	if a := vmSSHAddress(t, h.Endpoint(EnvCompute), p, zone, vm1); !strings.HasPrefix(a, "127.0.0.1:") {
		t.Fatalf("cloudburrowSshAddress = %s", a)
	}

	// Labels on a custom image and a machine image.
	wait(images.Insert(ctx, &computepb.InsertImageRequest{Project: p, ForceCreate: proto.Bool(true), ImageResource: &computepb.Image{
		Name: proto.String(img), SourceDisk: proto.String("zones/" + zone + "/disks/" + vm1)}}))
	wait(mis.Insert(ctx, &computepb.InsertMachineImageRequest{Project: p, MachineImageResource: &computepb.MachineImage{
		Name: proto.String(mi), SourceInstance: proto.String("zones/" + zone + "/instances/" + vm1)}}))
	im, err := images.Get(ctx, &computepb.GetImageRequest{Project: p, Image: img})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := images.SetLabels(ctx, &computepb.SetLabelsImageRequest{Project: p, Resource: img, GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{
		Labels: map[string]string{"env": "prod"}, LabelFingerprint: proto.String("c3RhbGU=")}}); computeCode(err) != http.StatusPreconditionFailed {
		t.Fatalf("images.setLabels with a stale fingerprint: %v", err)
	}
	wait(images.SetLabels(ctx, &computepb.SetLabelsImageRequest{Project: p, Resource: img, GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{
		Labels: map[string]string{"env": "prod"}, LabelFingerprint: proto.String(im.GetLabelFingerprint())}}))
	if im, err := images.Get(ctx, &computepb.GetImageRequest{Project: p, Image: img}); err != nil || im.GetLabels()["env"] != "prod" {
		t.Fatalf("image labels: %v %v", im.GetLabels(), err)
	}
	m, err := mis.Get(ctx, &computepb.GetMachineImageRequest{Project: p, MachineImage: mi})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mis.SetLabels(ctx, &computepb.SetLabelsMachineImageRequest{Project: p, Resource: mi, GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{
		Labels: map[string]string{"Bad": "x"}, LabelFingerprint: proto.String(m.GetLabelFingerprint())}}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("an uppercase label key: %v", err)
	}
	wait(mis.SetLabels(ctx, &computepb.SetLabelsMachineImageRequest{Project: p, Resource: mi, GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{
		Labels: map[string]string{"team": "a"}, LabelFingerprint: proto.String(m.GetLabelFingerprint())}}))
	if m, err := mis.Get(ctx, &computepb.GetMachineImageRequest{Project: p, MachineImage: mi}); err != nil || m.GetLabels()["team"] != "a" {
		t.Fatalf("machine image labels: %v %v", m.GetLabels(), err)
	}

	// The provisioning model, after create.
	spot := &computepb.Scheduling{ProvisioningModel: proto.String("SPOT"), InstanceTerminationAction: proto.String("STOP"),
		AutomaticRestart: proto.Bool(false), OnHostMaintenance: proto.String("TERMINATE")}
	if _, err := insts.SetScheduling(ctx, &computepb.SetSchedulingInstanceRequest{Project: p, Zone: zone, Instance: vm2, SchedulingResource: spot}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("setScheduling to Spot while running: %v", err)
	}
	wait(insts.Stop(ctx, &computepb.StopInstanceRequest{Project: p, Zone: zone, Instance: vm2}))
	wait(insts.SetScheduling(ctx, &computepb.SetSchedulingInstanceRequest{Project: p, Zone: zone, Instance: vm2, SchedulingResource: spot}))
	wait(insts.Start(ctx, &computepb.StartInstanceRequest{Project: p, Zone: zone, Instance: vm2}))
	if i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vm2}); err != nil ||
		i.GetScheduling().GetProvisioningModel() != "SPOT" || i.GetStatus() != "RUNNING" {
		t.Fatalf("vm2 after setScheduling: %v %v", i.GetScheduling(), err)
	}

	// Promote vm1's ephemeral address.
	wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{
		Name: proto.String(promoted), Address: proto.String(ip1)}}))
	a, err := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: promoted})
	if err != nil || a.GetAddress() != ip1 || a.GetStatus() != "IN_USE" || a.GetAddressType() != "EXTERNAL" || a.GetNetworkTier() != "PREMIUM" ||
		len(a.GetUsers()) != 1 || !strings.HasSuffix(a.GetUsers()[0], "/instances/"+vm1) {
		t.Fatalf("promoted address: %v %v", a, err)
	}
	if _, err := addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: promoted}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("deleting an IN_USE address: %v", err)
	}

	// A reserved address given to vm2 in place of its ephemeral one.
	wait(addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{Name: proto.String(fixed)}}))
	fa, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: fixed})
	wait(insts.DeleteAccessConfig(ctx, &computepb.DeleteAccessConfigInstanceRequest{Project: p, Zone: zone, Instance: vm2,
		NetworkInterface: "nic0", AccessConfig: "External NAT"}))
	if ip := natOf(vm2); ip != "" {
		t.Fatalf("vm2 still has %s", ip)
	}
	wait(insts.AddAccessConfig(ctx, &computepb.AddAccessConfigInstanceRequest{Project: p, Zone: zone, Instance: vm2, NetworkInterface: "nic0",
		AccessConfigResource: &computepb.AccessConfig{Name: proto.String("External NAT"), Type: proto.String("ONE_TO_ONE_NAT"), NatIP: proto.String(fa.GetAddress())}}))
	if ip := natOf(vm2); ip != fa.GetAddress() {
		t.Fatalf("vm2's natIP = %s, want %s", ip, fa.GetAddress())
	}
	if fa, _ := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: fixed}); fa.GetStatus() != "IN_USE" {
		t.Fatalf("fixed = %v", fa)
	}

	// Deleting vm1 releases its promoted address.
	wait(insts.Delete(ctx, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vm1}))
	if a, err := addrs.Get(ctx, &computepb.GetAddressRequest{Project: p, Region: region, Address: promoted}); err != nil || a.GetStatus() != "RESERVED" || len(a.GetUsers()) != 0 {
		t.Fatalf("promoted after its VM's delete: %v %v", a, err)
	}
}

// TestComputeRegionalDisksThroughTheGoClient (#2139), the official
// compute/apiv1 REST client: regionDiskTypes; a regional disk's refusals
// (one replica zone, a zone of another region, pd-extreme, pd-standard
// under 200 GB); insert, get, list, setLabels, resize; a VM of replica zone
// b given it at insert writes a file; detached, a VM of replica zone a
// attaches it and reads the file back (the replicas are one storage); a
// VM of zone c is refused; createSnapshot; delete refused while attached,
// then done.
// covers-pending (EPF: make this "covers:" once run): compute.regionDiskTypes.list, compute.regionDiskTypes.get, compute.regionDisks.insert, compute.regionDisks.get, compute.regionDisks.list, compute.regionDisks.setLabels, compute.regionDisks.resize, compute.regionDisks.createSnapshot, compute.regionDisks.delete, compute.instances.attachDisk, compute.instances.detachDisk, compute.regionOperations.get
func TestComputeRegionalDisksThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	rds, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rdts, _ := computeapi.NewRegionDiskTypesRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	snaps, _ := computeapi.NewSnapshotsRESTClient(ctx, opts...)
	p := h.Project()
	const region = "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	disk, vmB, vmA, vmC, snap := "a1-rd-"+sfx, "a1-rdb-"+sfx, "a1-rda-"+sfx, "a1-rdc-"+sfx, "a1-rds-"+sfx
	wait := a1Waiter(ctx, t)
	t.Cleanup(func() {
		c := context.Background()
		for _, v := range [][2]string{{"us-central1-b", vmB}, {"us-central1-a", vmA}, {"us-central1-c", vmC}} {
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: v[0], Instance: v[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := rds.Delete(c, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: disk}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := snaps.Delete(c, &computepb.DeleteSnapshotRequest{Project: p, Snapshot: snap}); err == nil {
			_ = op.Wait(c)
		}
	})
	var types []string
	for it := rdts.List(ctx, &computepb.ListRegionDiskTypesRequest{Project: p, Region: region}); ; {
		dt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, dt.GetName())
	}
	if strings.Join(types, ",") != "pd-balanced,pd-ssd,pd-standard" {
		t.Fatalf("regional disk types = %v", types)
	}
	if dt, err := rdts.Get(ctx, &computepb.GetRegionDiskTypeRequest{Project: p, Region: region, DiskType: "pd-standard"}); err != nil || dt.GetValidDiskSize() != "200GB-65536GB" {
		t.Fatalf("regional pd-standard: %v %v", dt, err)
	}
	two := []string{"zones/us-central1-a", "zones/us-central1-b"}
	for _, bad := range []*computepb.Disk{
		{Name: proto.String(disk), ReplicaZones: []string{"zones/us-central1-a"}},
		{Name: proto.String(disk), ReplicaZones: []string{"zones/us-central1-a", "zones/us-east1-b"}},
		{Name: proto.String(disk), ReplicaZones: two, Type: proto.String("regions/" + region + "/diskTypes/pd-extreme"), SizeGb: proto.Int64(500)},
		{Name: proto.String(disk), ReplicaZones: two, Type: proto.String("regions/" + region + "/diskTypes/pd-standard"), SizeGb: proto.Int64(100)},
	} {
		if _, err := rds.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: bad}); computeCode(err) != http.StatusBadRequest {
			t.Errorf("regionDisks.insert %v: %v, want 400", bad, err)
		}
	}
	wait(rds.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: &computepb.Disk{
		Name: proto.String(disk), ReplicaZones: two, SizeGb: proto.Int64(10), Type: proto.String("regions/" + region + "/diskTypes/pd-balanced")}}))
	d, err := rds.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: disk})
	if err != nil || d.GetStatus() != "READY" || len(d.GetReplicaZones()) != 2 || !strings.HasSuffix(d.GetRegion(), "/regions/"+region) {
		t.Fatalf("regional disk: %v %v", d, err)
	}
	if it := rds.List(ctx, &computepb.ListRegionDisksRequest{Project: p, Region: region}); true {
		found := false
		for {
			x, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			found = found || x.GetName() == disk
		}
		if !found {
			t.Fatal("regionDisks.list lacks the disk")
		}
	}
	wait(rds.SetLabels(ctx, &computepb.SetLabelsRegionDiskRequest{Project: p, Region: region, Resource: disk,
		RegionSetLabelsRequestResource: &computepb.RegionSetLabelsRequest{Labels: map[string]string{"tier": "ha"}, LabelFingerprint: proto.String(d.GetLabelFingerprint())}}))
	wait(rds.Resize(ctx, &computepb.ResizeRegionDiskRequest{Project: p, Region: region, Disk: disk,
		RegionDisksResizeRequestResource: &computepb.RegionDisksResizeRequest{SizeGb: proto.Int64(20)}}))
	if d, err := rds.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: disk}); err != nil || d.GetSizeGb() != 20 || d.GetLabels()["tier"] != "ha" {
		t.Fatalf("after setLabels and resize: %v %v", d, err)
	}

	sh := newA1SSH(t, addr, p)
	md := []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(sh.keyLine)}}
	nic := func() *computepb.NetworkInterface {
		return &computepb.NetworkInterface{Network: proto.String("global/networks/default")}
	}
	regional := &computepb.AttachedDisk{Source: proto.String("regions/" + region + "/disks/" + disk), DeviceName: proto.String("ha")}
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: "us-central1-b", InstanceResource: a1VM(vmB, "us-central1-b", nic(), md, regional)}))
	sh.run("us-central1-b", vmB, "echo REPLICATED-"+sfx+" | sudo tee /mnt/disks/ha/mark && sync")
	if _, err := rds.Delete(ctx, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: disk}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("deleting an attached regional disk: %v", err)
	}
	wait(insts.DetachDisk(ctx, &computepb.DetachDiskInstanceRequest{Project: p, Zone: "us-central1-b", Instance: vmB, DeviceName: "ha"}))
	var wg sync.WaitGroup
	for _, v := range [][2]string{{"us-central1-a", vmA}, {"us-central1-c", vmC}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: v[0], InstanceResource: a1VM(v[1], v[0], nic(), md)}))
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	if _, err := insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: "us-central1-c", Instance: vmC,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("regions/" + region + "/disks/" + disk)}}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("attaching in a zone that is not a replica zone: %v", err)
	}
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: "us-central1-a", Instance: vmA,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("projects/" + p + "/regions/" + region + "/disks/" + disk), DeviceName: proto.String("ha")}}))
	if out := sh.run("us-central1-a", vmA, "cat /mnt/disks/ha/mark"); !strings.Contains(out, "REPLICATED-"+sfx) {
		t.Fatalf("the other replica zone's VM reads %q", out)
	}
	wait(insts.DetachDisk(ctx, &computepb.DetachDiskInstanceRequest{Project: p, Zone: "us-central1-a", Instance: vmA, DeviceName: "ha"}))
	wait(rds.CreateSnapshot(ctx, &computepb.CreateSnapshotRegionDiskRequest{Project: p, Region: region, Disk: disk,
		SnapshotResource: &computepb.Snapshot{Name: proto.String(snap)}}))
	if s, err := snaps.Get(ctx, &computepb.GetSnapshotRequest{Project: p, Snapshot: snap}); err != nil || s.GetStorageLocations()[0] != region ||
		!strings.HasSuffix(s.GetSourceDisk(), "/regions/"+region+"/disks/"+disk) {
		t.Fatalf("snapshot of the regional disk: %v %v", s, err)
	}
	wait(rds.Delete(ctx, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: disk}))
	if _, err := rds.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: disk}); computeCode(err) != http.StatusNotFound {
		t.Fatalf("after delete: %v", err)
	}
}

// TestComputeIPv6ThroughTheGoClient (#2140), the official compute/apiv1
// REST client: a network with a ULA internal IPv6 range; a dual-stack
// subnet with internal IPv6 (a /64 of the network's /48) and one with
// external IPv6; an IPv4-only subnet patched to dual-stack; dual-stack VMs
// given /96 addresses (ipv6Address, or an ipv6AccessConfigs entry), a
// dual-stack interface on an IPv4-only subnet refused; and over the VPC
// network an IPv6 connection between two VMs that a rule of IPv6 ranges
// allows, a port it does not allow refused by the implied deny.
// covers-pending (EPF: make this "covers:" once run): compute.networks.insert, compute.networks.get, compute.subnetworks.insert, compute.subnetworks.get, compute.subnetworks.patch, compute.firewalls.insert, compute.instances.insert, compute.instances.get
func TestComputeIPv6ThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	p := h.Project()
	const zone, region = "us-central1-a", "us-central1"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	nw := "a1-v6-" + sfx
	in6, ex6, v4 := nw+"-in", nw+"-ex", nw+"-v4"
	srv, cli, ext := "a1-v6s-"+sfx, "a1-v6c-"+sfx, "a1-v6x-"+sfx
	fw := nw + "-allow6"
	wait := a1Waiter(ctx, t)
	t.Cleanup(func() {
		c := context.Background()
		var wg sync.WaitGroup
		for _, n := range []string{srv, cli, ext} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: n}); err == nil {
					_ = op.Wait(c)
				}
			}()
		}
		wg.Wait()
		if op, err := fws.Delete(c, &computepb.DeleteFirewallRequest{Project: p, Firewall: fw}); err == nil {
			_ = op.Wait(c)
		}
		for _, s := range []string{in6, ex6, v4} {
			if op, err := subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: p, Region: region, Subnetwork: s}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := nets.Delete(c, &computepb.DeleteNetworkRequest{Project: p, Network: nw}); err == nil {
			_ = op.Wait(c)
		}
	})
	wait(nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: p, NetworkResource: &computepb.Network{
		Name: proto.String(nw), AutoCreateSubnetworks: proto.Bool(false), EnableUlaInternalIpv6: proto.Bool(true)}}))
	n, err := nets.Get(ctx, &computepb.GetNetworkRequest{Project: p, Network: nw})
	if err != nil || !n.GetEnableUlaInternalIpv6() || !strings.HasPrefix(n.GetInternalIpv6Range(), "fd2") || !strings.HasSuffix(n.GetInternalIpv6Range(), "/48") {
		t.Fatalf("network: %v %v", n, err)
	}
	sub := func(name, cidr, access string) *computepb.Subnetwork {
		s := &computepb.Subnetwork{Name: proto.String(name), Network: proto.String("global/networks/" + nw), IpCidrRange: proto.String(cidr)}
		if access != "" {
			s.StackType, s.Ipv6AccessType = proto.String("IPV4_IPV6"), proto.String(access)
		}
		return s
	}
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: sub(in6, "10.61.0.0/24", "INTERNAL")}))
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: sub(ex6, "10.62.0.0/24", "EXTERNAL")}))
	wait(subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: p, Region: region, SubnetworkResource: sub(v4, "10.63.0.0/24", "")}))
	si, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: in6})
	if err != nil || si.GetStackType() != "IPV4_IPV6" || !strings.HasSuffix(si.GetInternalIpv6Prefix(), "/64") ||
		!strings.HasPrefix(si.GetInternalIpv6Prefix(), strings.TrimSuffix(n.GetInternalIpv6Range(), "::/48")) {
		t.Fatalf("internal dual-stack subnet: %v %v", si, err)
	}
	if se, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: ex6}); err != nil || !strings.HasPrefix(se.GetExternalIpv6Prefix(), "2600:190") {
		t.Fatalf("external dual-stack subnet: %v %v", se, err)
	}
	dual := func(s string) *computepb.NetworkInterface {
		return &computepb.NetworkInterface{Subnetwork: proto.String("regions/" + region + "/subnetworks/" + s), StackType: proto.String("IPV4_IPV6")}
	}
	if _, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: a1VM("a1-bad-"+sfx, zone, dual(v4), nil)}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("a dual-stack interface on an IPv4-only subnet: %v", err)
	}
	sv4, _ := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: v4})
	wait(subs.Patch(ctx, &computepb.PatchSubnetworkRequest{Project: p, Region: region, Subnetwork: v4, SubnetworkResource: &computepb.Subnetwork{
		StackType: proto.String("IPV4_IPV6"), Ipv6AccessType: proto.String("INTERNAL"), Fingerprint: proto.String(sv4.GetFingerprint())}}))
	if sv4, err := subs.Get(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: v4}); err != nil || sv4.GetStackType() != "IPV4_IPV6" || sv4.GetInternalIpv6Prefix() == "" {
		t.Fatalf("patched to dual-stack: %v %v", sv4, err)
	}
	wait(fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: p, FirewallResource: &computepb.Firewall{
		Name: proto.String(fw), Network: proto.String("global/networks/" + nw), SourceRanges: []string{"::/0"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"8080"}}}}}))

	sh := newA1SSH(t, addr, p)
	script := "#!/bin/bash\nfor p in 8080 8081; do systemd-run --unit=listen6-$p nc -6 -lk $p; done\n"
	md := []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(sh.keyLine)}, {Key: proto.String("startup-script"), Value: proto.String(script)}}
	var wg sync.WaitGroup
	for _, v := range [][2]string{{srv, in6}, {cli, in6}, {ext, ex6}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: a1VM(v[0], zone, dual(v[1]), md)}))
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	nicOf := func(name string) *computepb.NetworkInterface {
		t.Helper()
		i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: name})
		if err != nil {
			t.Fatal(err)
		}
		return i.GetNetworkInterfaces()[0]
	}
	sn, cn, xn := nicOf(srv), nicOf(cli), nicOf(ext)
	if sn.GetStackType() != "IPV4_IPV6" || sn.GetIpv6AccessType() != "INTERNAL" || sn.GetInternalIpv6PrefixLength() != 96 || sn.GetIpv6Address() == "" || sn.GetIpv6Address() == cn.GetIpv6Address() {
		t.Fatalf("internal dual-stack NICs: %v / %v", sn, cn)
	}
	if acs := xn.GetIpv6AccessConfigs(); len(acs) != 1 || acs[0].GetType() != "DIRECT_IPV6" || acs[0].GetExternalIpv6PrefixLength() != 96 || !strings.HasPrefix(acs[0].GetExternalIpv6(), "2600:190") {
		t.Fatalf("external dual-stack NIC: %v", xn)
	}

	// IPv6 over the VPC network: 8080 allowed from ::/0, 8081 not.
	probe := func(port string) bool {
		return strings.Contains(sh.run(zone, cli, "nc -6 -z -w 3 "+sn.GetIpv6Address()+" "+port+" && echo OPEN || echo CLOSED"), "OPEN")
	}
	deadline := time.Now().Add(2 * time.Minute)
	for !probe("8080") {
		if time.Now().After(deadline) {
			t.Fatalf("IPv6 8080 never opened:\n%s\n%s", sh.run(zone, cli, "ip -6 addr show vpc0; ip -6 route; ip -6 neigh"),
				sh.run(zone, srv, "ip -6 addr show vpc0; ss -ltn; sudo nft list ruleset"))
		}
		time.Sleep(3 * time.Second)
	}
	if probe("8081") {
		t.Error("IPv6 8081 connected: no rule allows it")
	}
}
