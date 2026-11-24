//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/vdisk"
)

// TestComputeImageImportFromAVirtualDiskThroughTheGoClient (#2139), the
// official compute/apiv1 REST client and Cloud Storage client: a VMDK
// (stream-optimized, as VMware exports one) and a dynamic VHD, each the
// virtual disk of one ext4 raw disk made as
// TestComputeImageImportFromCloudStorageThroughTheGoClient makes its
// disk.raw (mke2fs -d of the Debian VM image's root directories and a
// marker file, in a network-less container), uploaded to the instance's
// Cloud Storage and imported with images.insert's rawDisk.source naming
// the file, as compute/docs/import/importing-virtual-disks imports a
// VMDK or VHD. Each image is READY, of the raw disk's size, and boots: the
// instance's startup-script reads the marker the imported disk carried. A
// descriptor-only VMDK is refused on its operation.
//
// Needs Compute Engine (with VMs) and Cloud Storage served, and docker on
// this machine.
// covers: compute.images.insert, compute.images.get, compute.images.delete, compute.globalOperations.get, compute.instances.insert, compute.instances.getSerialPortOutput, compute.instances.delete
func TestComputeImageImportFromAVirtualDiskThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	images, err := computeapi.NewImagesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	p := h.Project()
	const zone = "us-central1-a"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	bh := bucket(t, h, sc)
	bucketName := p + "-bucket"
	put := func(name string, r io.Reader) {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		if _, err := io.Copy(w, r); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// The raw disk, then its two virtual disks.
	dir := t.TempDir()
	ref := buildVMImage(t, ctx, compute.PublicImages[0], filepath.Join(dir, "ctx"))
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -e; command -v mke2fs >/dev/null || { echo 'mke2fs (e2fsprogs) is not in the VM image' >&2; exit 3; }; " +
		"mkdir /r; for d in etc home opt root srv usr var; do cp -a /$d /r/; done; " +
		"for l in bin lib lib64 sbin; do [ -L /$l ] && cp -a /$l /r/; done; mkdir -p /r/proc /r/sys /r/dev /r/run /r/tmp; " +
		"echo VDISK-MARK-" + sfx + " > /r/etc/cloudburrow-imported; " +
		"mke2fs -q -t ext4 -L root -d /r /out/disk.raw 3G"
	if b, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--label", "cloudburrow.dev/owned=true",
		"--entrypoint", "/bin/sh", "-v", out+":/out", ref, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("make disk.raw: %v: %s", err, b)
	}
	raw, err := os.Open(filepath.Join(out, "disk.raw"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	st, _ := raw.Stat()
	for _, f := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{"disk.vmdk", func(w io.Writer) error { return vdisk.WriteVMDK(w, raw, st.Size(), true) }},
		{"disk.vhd", func(w io.Writer) error { return vdisk.WriteVHD(w, raw, st.Size(), true) }},
	} {
		path := filepath.Join(out, f.name)
		w, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.write(w); err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		_ = w.Close()
		r, _ := os.Open(path)
		put(f.name, r)
		_ = r.Close()
		_ = os.Remove(path)
	}
	_ = os.Remove(filepath.Join(out, "disk.raw"))
	put("desc.vmdk", strings.NewReader("# Disk DescriptorFile\nversion=1\nCID=fffffffe\nparentCID=ffffffff\ncreateType=\"monolithicFlat\"\nRW 6291456 FLAT \"disk-flat.vmdk\" 0\n"))

	// A descriptor-only VMDK names its data in another file.
	refused := "ct-vd-desc-" + sfx
	op, err := images.Insert(ctx, &computepb.InsertImageRequest{Project: p, ImageResource: &computepb.Image{
		Name: proto.String(refused), RawDisk: &computepb.RawDisk{Source: proto.String("gs://" + bucketName + "/desc.vmdk")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err == nil || !strings.Contains(err.Error(), "only monolithic VMDK files are supported") {
		t.Fatalf("a descriptor-only VMDK: %v", err)
	}

	for i, file := range []string{"disk.vmdk", "disk.vhd"} {
		imgName, vmName := fmt.Sprintf("ct-vd-%d-%s", i, sfx), fmt.Sprintf("ct-vd-vm-%d-%s", i, sfx)
		t.Cleanup(func() {
			c := context.Background()
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vmName}); err == nil {
				_ = op.Wait(c)
			}
			if op, err := images.Delete(c, &computepb.DeleteImageRequest{Project: p, Image: imgName}); err == nil {
				_ = op.Wait(c)
			}
		})
		op, err := images.Insert(ctx, &computepb.InsertImageRequest{Project: p, ImageResource: &computepb.Image{
			Name:    proto.String(imgName),
			RawDisk: &computepb.RawDisk{Source: proto.String("https://storage.googleapis.com/" + bucketName + "/" + file)}}})
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		img, err := images.Get(ctx, &computepb.GetImageRequest{Project: p, Image: imgName})
		if err != nil {
			t.Fatal(err)
		}
		if img.GetStatus() != "READY" || img.GetSourceType() != "RAW" || img.GetDiskSizeGb() != 3 || img.GetArchiveSizeBytes() <= 0 {
			t.Fatalf("%s: imported image %v", file, img)
		}
		op, err = insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
			Name:        proto.String(vmName),
			MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("global/images/" + imgName),
					DiskSizeGb: proto.Int64(10)}}},
			NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
			Metadata: &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"),
				Value: proto.String("#!/bin/bash\ncat /etc/cloudburrow-imported\n")}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(4 * time.Minute)
		for {
			so, err := insts.GetSerialPortOutput(ctx, &computepb.GetSerialPortOutputInstanceRequest{Project: p, Zone: zone, Instance: vmName})
			if err == nil && strings.Contains(so.GetContents(), "VDISK-MARK-"+sfx) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the imported disk's marker never reached the serial port (%v): %.2000s", file, err, so.GetContents())
			}
			time.Sleep(3 * time.Second)
		}
		if op, err := insts.Delete(ctx, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vmName}); err == nil {
			_ = op.Wait(ctx)
		}
	}
}
