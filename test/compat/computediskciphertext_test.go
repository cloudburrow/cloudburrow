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
	"strconv"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// nodeGrep counts the files under path on the cluster's node that hold
// marker. ok is false when the test has no cluster to look at.
func nodeGrep(t *testing.T, path, marker string) (n int, ok bool) {
	t.Helper()
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		return 0, false
	}
	g := exec.Command("docker", "exec", cluster+"-control-plane", "grep", "-r", "-l", "-F", marker, path)
	var out bytes.Buffer
	g.Stdout = &out
	_ = g.Run() // grep exits 1 when it finds nothing
	return len(strings.Fields(out.String())), true
}

// TestComputeDiskVolumeIsCiphertextWhileTheVMWrites (#2206), the official
// compute/apiv1 and kms/apiv1 clients: a running VM writes a known string
// through a Cloud KMS (CMEK) disk and a customer-supplied-key (CSEK) disk,
// over and over, while the test reads the bytes this machine holds for
// each disk. The disk's volume is the dm-crypt image alone and the string
// is nowhere in it, while the node's decrypted mount, the VM's view, does
// hold it (so the search would find it if it were in the clear). Then,
// with the VM stopped, the KMS key version disabled: the VM can't start
// (400, Cloud KMS error) and nothing is decrypted on the node; the version
// enabled again: the VM starts and reads back what it wrote.
// covers: compute.disks.insert, compute.disks.get, compute.instances.insert, compute.instances.attachDisk, compute.instances.start, compute.instances.stop, compute.instances.startWithEncryptionKey
func TestComputeDiskVolumeIsCiphertextWhileTheVMWrites(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	addr := h.Endpoint(EnvCompute)
	if os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services compute,kms")
	}
	if os.Getenv("CLOUDBURROW_TEST_CLUSTER") == "" || os.Getenv(envKubeconfig) == "" {
		t.Skip("needs the instance's cluster (CLOUDBURROW_TEST_CLUSTER and " + envKubeconfig + ") to read the disks' bytes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	p := h.Project()
	const region, za = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	vm, cmek, csek := "ct-vm-"+sfx, "ct-cmek-"+sfx, "ct-csek-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := diskKeys(t, h, "ct-"+sfx, region)
	kc := kmsClients(t, h)["grpc"]
	setState := func(version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %s %v: %v", version, st, err)
		}
	}
	v1 := keys[region] + "/cryptoKeyVersions/1"
	t.Cleanup(func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: za, Instance: vm}); err == nil {
			_ = op.Wait(c)
		}
		for _, d := range []string{cmek, csek} {
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: za, Disk: d}); err == nil {
				_ = op.Wait(c)
			}
		}
	})
	raw := base64.StdEncoding.EncodeToString([]byte("ciphertext-test-key-32-bytes-ok!"))
	csekKey := &computepb.CustomerEncryptionKey{RawKey: proto.String(raw)}
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(cmek), SizeGb: proto.Int64(10), DiskEncryptionKey: &computepb.CustomerEncryptionKey{KmsKeyName: proto.String(keys[region])}}}))
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(csek), SizeGb: proto.Int64(10), DiskEncryptionKey: csekKey}}))
	sh := newVMShell(t, addr, p)
	wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: za, InstanceResource: &computepb.Instance{
		Name: proto.String(vm), MachineType: proto.String("zones/" + za + "/machineTypes/n2-standard-2"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("ssh-keys"), Value: proto.String(sh.keyLine)}}},
	}}))
	for _, d := range []struct {
		name string
		key  *computepb.CustomerEncryptionKey
	}{{cmek, nil}, {csek, csekKey}} {
		wait(insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: za, Instance: vm,
			AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String("zones/" + za + "/disks/" + d.name), DeviceName: proto.String(d.name),
				DiskEncryptionKey: d.key}}))
	}
	run := func(cmd string) string {
		t.Helper()
		out, err := sh.run(za, vm, cmd)
		if err != nil {
			t.Fatalf("%s: %v %s", cmd, err, out)
		}
		return strings.TrimSpace(out)
	}
	// The VM writes the string through each disk, over and over, while the
	// test looks at the bytes on this machine.
	marker := map[string]string{cmek: "cmek-plaintext-" + sfx, csek: "csek-plaintext-" + sfx}
	count := func(d string) int {
		t.Helper()
		n, _ := strconv.Atoi(run("grep -c " + marker[d] + " /mnt/disks/" + d + "/known.txt || true"))
		return n
	}
	for _, d := range []string{cmek, csek} {
		run(fmt.Sprintf("sudo sh -c 'nohup sh -c \"while [ ! -e /tmp/ct-stop ]; do echo %s >> /mnt/disks/%s/known.txt; sync; sleep 1; done\" >/dev/null 2>&1 &'", marker[d], d))
	}
	time.Sleep(5 * time.Second)
	for _, d := range []string{cmek, csek} {
		if got := count(d); got == 0 {
			t.Fatalf("the VM's writes to %s: no lines", d)
		}
		names, found, _ := diskStorage(t, mustID(t, ctx, disks, p, za, d), marker[d])
		if names != ".cloudburrow-crypt" || found {
			t.Errorf("%s's volume on this machine holds %q (the string in the clear: %v), want only the dm-crypt image", d, names, found)
		}
		// The positive control: the decrypted mount the VM uses holds it.
		if n, _ := nodeGrep(t, "/var/lib/cloudburrow-crypt", marker[d]); n == 0 {
			t.Errorf("%s: the string is not in the node's decrypted mount either, so the search proves nothing", d)
		}
	}
	lines := count(cmek)
	run("sudo touch /tmp/ct-stop && sleep 3 && sync")

	// The key version disabled: no start, and nothing decrypted on the
	// node. Enabled again: the data is back.
	wait(insts.Stop(ctx, &computepb.StopInstanceRequest{Project: p, Zone: za, Instance: vm}))
	if n, _ := nodeGrep(t, "/var/lib/cloudburrow-crypt", marker[cmek]); n != 0 {
		t.Errorf("the stopped VM's CMEK disk is still decrypted on the node")
	}
	setState(v1, kmspb.CryptoKeyVersion_DISABLED)
	startWith := func() error {
		op, err := insts.StartWithEncryptionKey(ctx, &computepb.StartWithEncryptionKeyInstanceRequest{Project: p, Zone: za, Instance: vm,
			InstancesStartWithEncryptionKeyRequestResource: &computepb.InstancesStartWithEncryptionKeyRequest{Disks: []*computepb.CustomerEncryptionKeyProtectedDisk{{
				Source: proto.String("projects/" + p + "/zones/" + za + "/disks/" + csek), DiskEncryptionKey: csekKey}}}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	if err := startWith(); computeCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "Cloud KMS error") {
		t.Fatalf("start with the key version disabled: %v, want 400 Cloud KMS error", err)
	}
	if v, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: za, Instance: vm}); err != nil || v.GetStatus() != "TERMINATED" {
		t.Errorf("the VM after the refused start: %v %v, want TERMINATED", v.GetStatus(), err)
	}
	if n, _ := nodeGrep(t, "/var/lib/cloudburrow-crypt", marker[cmek]); n != 0 {
		t.Errorf("the CMEK disk was decrypted on the node with its key version disabled")
	}
	setState(v1, kmspb.CryptoKeyVersion_ENABLED)
	if err := startWith(); err != nil {
		t.Fatalf("start with the key version enabled again: %v", err)
	}
	if got := count(cmek); got < lines || got == 0 {
		t.Errorf("the CMEK disk after the restart: %d lines of the string, had %d", got, lines)
	}
	if count(csek) == 0 {
		t.Errorf("the CSEK disk after the restart has lost the string")
	}
}

func mustID(t *testing.T, ctx context.Context, disks *computeapi.DisksClient, p, zone, name string) uint64 {
	t.Helper()
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zone, Disk: name})
	if err != nil {
		t.Fatal(err)
	}
	return d.GetId()
}
