//go:build compat

package compat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// TestComputeVMsThroughTheGoClient (#1963), the official compute/apiv1
// REST client: the public images (debian-12 by family, cos-stable refused
// with its reason), the machine types, a blank disk, a VM from debian-12
// whose startup-script runs at boot (its line read back from the serial
// port), an ssh-keys setMetadata the guest applies (an SSH login through
// the forwarded port, sudo, the internal IP from its subnet), stop, a
// setMachineType refused while running and accepted stopped, start (the
// boot disk kept what the guest wrote, the script ran again), attachDisk
// (the disk a mount at /mnt/disks/DEVICE), a snapshot of it and a disk made
// from the snapshot, resize, suspend and resume, setLabels, detachDisk and
// the deletes, every operation waited on.
// covers: compute.images.get, compute.images.getFromFamily, compute.images.list, compute.machineTypes.list, compute.machineTypes.get, compute.disks.insert, compute.disks.get, compute.disks.list, compute.disks.delete, compute.disks.resize, compute.disks.createSnapshot, compute.snapshots.get, compute.snapshots.delete, compute.instances.insert, compute.instances.get, compute.instances.list, compute.instances.delete, compute.instances.start, compute.instances.stop, compute.instances.reset, compute.instances.suspend, compute.instances.resume, compute.instances.setMetadata, compute.instances.setLabels, compute.instances.setMachineType, compute.instances.getSerialPortOutput, compute.instances.attachDisk, compute.instances.detachDisk, compute.zoneOperations.get, compute.zoneOperations.wait
func TestComputeVMsThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	images, err := computeapi.NewImagesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	mts, _ := computeapi.NewMachineTypesRESTClient(ctx, opts...)
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	snaps, _ := computeapi.NewSnapshotsRESTClient(ctx, opts...)
	p := h.Project()
	const zone = "us-central1-a"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vmName, dataName, fromSnap, snapName := "ct-vm-"+sfx, "ct-data-"+sfx, "ct-restored-"+sfx, "ct-snap-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}

	img, err := images.GetFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: "debian-cloud", Family: "debian-12"})
	if err != nil {
		t.Fatal(err)
	}
	if img.GetFamily() != "debian-12" || img.GetStatus() != "READY" || !strings.HasPrefix(img.GetName(), "debian-12-") {
		t.Fatalf("debian-12 image: %v", img)
	}
	if _, err := images.Get(ctx, &computepb.GetImageRequest{Project: "debian-cloud", Image: img.GetName()}); err != nil {
		t.Fatal(err)
	}
	if it := images.List(ctx, &computepb.ListImagesRequest{Project: "rocky-linux-cloud"}); true {
		im, err := it.Next()
		if err != nil || im.GetFamily() != "rocky-linux-9" {
			t.Fatalf("rocky-linux-cloud images: %v %v", im, err)
		}
	}
	if _, err := images.GetFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: "cos-cloud", Family: "cos-stable"}); computeCode(err) != http.StatusNotImplemented || !strings.Contains(err.Error(), "kernel") {
		t.Fatalf("cos-stable: %v, want 501 with the reason", err)
	}
	found := false
	for it := mts.List(ctx, &computepb.ListMachineTypesRequest{Project: p, Zone: zone}); ; {
		mt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || mt.GetName() == "e2-small" && mt.GetGuestCpus() == 2 && mt.GetMemoryMb() == 2048
	}
	if !found {
		t.Fatal("e2-small not listed")
	}
	if mt, err := mts.Get(ctx, &computepb.GetMachineTypeRequest{Project: p, Zone: zone, MachineType: "e2-medium"}); err != nil || mt.GetMemoryMb() != 4096 {
		t.Fatalf("e2-medium: %v %v", mt, err)
	}

	cleanup := func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vmName}); err == nil {
			_ = op.Wait(c)
		}
		for _, d := range []string{dataName, fromSnap} {
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: zone, Disk: d}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := snaps.Delete(c, &computepb.DeleteSnapshotRequest{Project: p, Snapshot: snapName}); err == nil {
			_ = op.Wait(c)
		}
	}
	t.Cleanup(cleanup)

	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zone, DiskResource: &computepb.Disk{
		Name: proto.String(dataName), SizeGb: proto.Int64(10), Type: proto.String("zones/" + zone + "/diskTypes/pd-ssd")}}))
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zone, Disk: dataName}); err != nil || d.GetStatus() != "READY" || d.GetSizeGb() != 10 {
		t.Fatalf("disk: %v %v", d, err)
	}

	script := "#!/bin/bash\necho STARTUP-MARK-$(hostname)\necho boot >> /var/tmp/boots\n"
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(vmName),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default"),
			AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT")}}}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(script)}}},
		Labels:   map[string]string{"env": "test"},
	}}))
	inst, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vmName})
	if err != nil {
		t.Fatal(err)
	}
	ip := inst.GetNetworkInterfaces()[0].GetNetworkIP()
	if inst.GetStatus() != "RUNNING" || !strings.HasPrefix(ip, "10.128.") {
		t.Fatalf("instance: %s %s", inst.GetStatus(), ip)
	}
	serialHas := func(want string, n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			out, err := insts.GetSerialPortOutput(ctx, &computepb.GetSerialPortOutputInstanceRequest{Project: p, Zone: zone, Instance: vmName})
			if err == nil && strings.Count(out.GetContents(), want) >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("serial port output never had %q (%v): %.2000s", want, err, out.GetContents())
			}
			time.Sleep(2 * time.Second)
		}
	}
	serialHas("startup-script: STARTUP-MARK-"+vmName, 1)

	// ssh-keys, as gcloud compute ssh adds them.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	keyLine := "tester:" + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " tester"
	wait(insts.SetMetadata(ctx, &computepb.SetMetadataInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		MetadataResource: &computepb.Metadata{Fingerprint: inst.GetMetadata().Fingerprint, Items: []*computepb.Items{
			{Key: proto.String("startup-script"), Value: proto.String(script)}, {Key: proto.String("ssh-keys"), Value: proto.String(keyLine)}}}}))
	run := func(cmd string) string {
		t.Helper()
		sshAddr := vmSSHAddress(t, addr, p, zone, vmName)
		var out string
		deadline := time.Now().Add(2 * time.Minute)
		for {
			c, err := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
			if err == nil {
				s, err2 := c.NewSession()
				if err2 == nil {
					b, err3 := s.CombinedOutput(cmd)
					out, err = string(b), err3
				} else {
					err = err2
				}
				c.Close()
				if err == nil {
					return out
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("ssh %s %q: %v: %s", sshAddr, cmd, err, out)
			}
			time.Sleep(2 * time.Second)
		}
	}
	if out := run("cat /var/tmp/boots; ip -4 addr show vpc0; sudo -n true && echo SUDO-OK"); !strings.Contains(out, "boot") || !strings.Contains(out, ip) || !strings.Contains(out, "SUDO-OK") {
		t.Fatalf("over SSH: %s", out)
	}

	wait(insts.Stop(ctx, &computepb.StopInstanceRequest{Project: p, Zone: zone, Instance: vmName}))
	if i, _ := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vmName}); i.GetStatus() != "TERMINATED" {
		t.Fatalf("after stop: %s", i.GetStatus())
	}
	wait(insts.SetMachineType(ctx, &computepb.SetMachineTypeInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		InstancesSetMachineTypeRequestResource: &computepb.InstancesSetMachineTypeRequest{MachineType: proto.String("zones/" + zone + "/machineTypes/e2-medium")}}))
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + zone + "/disks/" + dataName), DeviceName: proto.String("data")}}))
	wait(insts.Start(ctx, &computepb.StartInstanceRequest{Project: p, Zone: zone, Instance: vmName}))
	i, _ := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vmName})
	if i.GetStatus() != "RUNNING" || !strings.HasSuffix(i.GetMachineType(), "/e2-medium") || len(i.GetDisks()) != 2 {
		t.Fatalf("after start: %s %s %d disks", i.GetStatus(), i.GetMachineType(), len(i.GetDisks()))
	}
	if _, err := insts.SetMachineType(ctx, &computepb.SetMachineTypeInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		InstancesSetMachineTypeRequestResource: &computepb.InstancesSetMachineTypeRequest{MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small")}}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("setMachineType while running: %v, want 400", err)
	}
	// The boot disk kept the first boot's line, and the script ran again
	// (once its line is on the new boot's serial port).
	serialHas("startup-script: STARTUP-MARK-"+vmName, 1)
	if out := run("cat /var/tmp/boots | wc -l; nproc; echo hello | sudo tee /mnt/disks/data/hello.txt"); !strings.HasPrefix(strings.TrimSpace(out), "2") {
		t.Fatalf("after restart: %s", out)
	}

	wait(insts.SetLabels(ctx, &computepb.SetLabelsInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		InstancesSetLabelsRequestResource: &computepb.InstancesSetLabelsRequest{LabelFingerprint: i.LabelFingerprint, Labels: map[string]string{"env": "prod"}}}))
	wait(insts.Suspend(ctx, &computepb.SuspendInstanceRequest{Project: p, Zone: zone, Instance: vmName}))
	if i, _ := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: vmName}); i.GetStatus() != "SUSPENDED" || i.GetLabels()["env"] != "prod" {
		t.Fatalf("after suspend: %s %v", i.GetStatus(), i.GetLabels())
	}
	wait(insts.Resume(ctx, &computepb.ResumeInstanceRequest{Project: p, Zone: zone, Instance: vmName}))
	if out := run("cat /var/tmp/boots | wc -l"); strings.TrimSpace(out) != "2" {
		t.Fatalf("after resume the guest rebooted: %s", out)
	}
	wait(insts.Reset(ctx, &computepb.ResetInstanceRequest{Project: p, Zone: zone, Instance: vmName}))
	serialHas("startup-script: STARTUP-MARK-"+vmName, 1)

	wait(insts.DetachDisk(ctx, &computepb.DetachDiskInstanceRequest{Project: p, Zone: zone, Instance: vmName, DeviceName: "data"}))
	wait(disks.CreateSnapshot(ctx, &computepb.CreateSnapshotDiskRequest{Project: p, Zone: zone, Disk: dataName,
		SnapshotResource: &computepb.Snapshot{Name: proto.String(snapName)}}))
	if s, err := snaps.Get(ctx, &computepb.GetSnapshotRequest{Project: p, Snapshot: snapName}); err != nil || s.GetStatus() != "READY" {
		t.Fatalf("snapshot: %v %v", s, err)
	}
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zone, DiskResource: &computepb.Disk{
		Name: proto.String(fromSnap), SourceSnapshot: proto.String("global/snapshots/" + snapName)}}))
	wait(disks.Resize(ctx, &computepb.ResizeDiskRequest{Project: p, Zone: zone, Disk: fromSnap,
		DisksResizeRequestResource: &computepb.DisksResizeRequest{SizeGb: proto.Int64(20)}}))
	wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: vmName,
		AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + zone + "/disks/" + fromSnap), DeviceName: proto.String("restored")}}))
	if out := run("cat /mnt/disks/restored/hello.txt"); strings.TrimSpace(out) != "hello" {
		t.Fatalf("disk from the snapshot: %q", out)
	}
	if _, err := disks.Delete(ctx, &computepb.DeleteDiskRequest{Project: p, Zone: zone, Disk: fromSnap}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("deleting an attached disk: %v, want 400", err)
	}
	listed := false
	for it := insts.List(ctx, &computepb.ListInstancesRequest{Project: p, Zone: zone}); ; {
		in, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		listed = listed || in.GetName() == vmName
	}
	if !listed {
		t.Fatal("instance not listed")
	}
	wait(insts.Delete(ctx, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vmName}))
	if _, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zone, Disk: vmName}); computeCode(err) != http.StatusNotFound {
		t.Fatalf("the auto-delete boot disk: %v, want 404", err)
	}
	for it := disks.List(ctx, &computepb.ListDisksRequest{Project: p, Zone: zone}); ; {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if d.GetName() == fromSnap && len(d.GetUsers()) != 0 {
			t.Fatalf("%s still has users %v", fromSnap, d.GetUsers())
		}
	}
}

// vmSSHAddress is the host address CloudBurrow forwards a running
// instance's port 22 to (cloudburrowSshAddress, which the Go client's
// generated types do not carry).
func vmSSHAddress(t *testing.T, addr, project, zone, name string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/compute/v1/projects/" + project + "/zones/" + zone + "/instances/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		SSH string `json:"cloudburrowSshAddress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.SSH == "" {
		t.Fatalf("no SSH address: %v", err)
	}
	return v.SSH
}
