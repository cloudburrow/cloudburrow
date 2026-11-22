//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
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
)

// TestComputeImageImportFromCloudStorageThroughTheGoClient (#2139), the
// official compute/apiv1 REST client and Cloud Storage client: images.insert
// with rawDisk.source, as compute/docs/import/import-existing-image
// documents it. Google's refusals first (a malformed URL; a missing object
// and an archive that is not a .tar.gz of a single disk.raw, each failing
// its operation and leaving no image), then a real import: a disk.raw
// holding an ext4 file system of the Debian VM image's root directories and
// a marker file, made with mke2fs -d in a network-less container of that
// image, archived as Google asks (tar --format=oldgnu -Sczf), uploaded to
// the instance's Cloud Storage, imported, and booted: the instance's
// startup-script reads the marker the imported disk carried.
//
// Needs Compute Engine (with VMs) and Cloud Storage served, and docker on
// this machine.
// covers: compute.images.insert, compute.images.get, compute.images.delete, compute.globalOperations.get, compute.instances.insert, compute.instances.getSerialPortOutput, compute.instances.delete
func TestComputeImageImportFromCloudStorageThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
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
	// Not a single disk.raw: a member of another name.
	var bad bytes.Buffer
	zw := gzip.NewWriter(&bad)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "image.vmdk", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("vmdk"))
	_ = tw.Close()
	_ = zw.Close()
	put("bad.tar.gz", &bad)

	if _, err := images.Insert(ctx, &computepb.InsertImageRequest{Project: p, ImageResource: &computepb.Image{
		Name: proto.String("ct-imp-url-" + sfx), RawDisk: &computepb.RawDisk{Source: proto.String("https://example.com/" + bucketName + "/bad.tar.gz")}}}); computeCode(err) != http.StatusBadRequest {
		t.Fatalf("a source outside Cloud Storage: %v, want 400", err)
	}
	for obj, want := range map[string]string{"missing.tar.gz": "was not found", "bad.tar.gz": "not a valid image"} {
		name := "ct-imp-" + strings.TrimSuffix(obj, ".tar.gz") + "-" + sfx
		op, err := images.Insert(ctx, &computepb.InsertImageRequest{Project: p, ImageResource: &computepb.Image{
			Name: proto.String(name), RawDisk: &computepb.RawDisk{Source: proto.String("https://storage.googleapis.com/" + bucketName + "/" + obj)}}})
		if err != nil {
			t.Fatalf("%s: %v", obj, err)
		}
		if err := op.Wait(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: operation error %v, want %q", obj, err, want)
		}
		if _, err := images.Get(ctx, &computepb.GetImageRequest{Project: p, Image: name}); computeCode(err) != http.StatusNotFound {
			t.Fatalf("%s: a failed import left its image: %v", obj, err)
		}
	}

	// A real disk.raw, from the Debian VM image's own files.
	dir := t.TempDir()
	ref := buildVMImage(t, ctx, compute.PublicImages[0], filepath.Join(dir, "ctx"))
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -e; command -v mke2fs >/dev/null || { echo 'mke2fs (e2fsprogs) is not in the VM image' >&2; exit 3; }; " +
		"mkdir /r; for d in etc home opt root srv usr var; do cp -a /$d /r/; done; " +
		"for l in bin lib lib64 sbin; do [ -L /$l ] && cp -a /$l /r/; done; mkdir -p /r/proc /r/sys /r/dev /r/run /r/tmp; " +
		"echo IMPORTED-MARK-" + sfx + " > /r/etc/cloudburrow-imported; " +
		"mke2fs -q -t ext4 -L root -d /r /out/disk.raw 3G; cd /out && tar --format=oldgnu -Sczf disk.tar.gz disk.raw && rm -f disk.raw"
	if b, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--label", "cloudburrow.dev/owned=true",
		"--entrypoint", "/bin/sh", "-v", out+":/out", ref, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("make disk.raw: %v: %s", err, b)
	}
	f, err := os.Open(filepath.Join(out, "disk.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	put("disk.tar.gz", f)
	_ = f.Close()

	imgName, vmName := "ct-imported-"+sfx, "ct-imp-vm-"+sfx
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
		Name: proto.String(imgName), Family: proto.String("ct-imported"),
		RawDisk: &computepb.RawDisk{Source: proto.String("https://storage.googleapis.com/" + bucketName + "/disk.tar.gz")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	img, err := images.Get(ctx, &computepb.GetImageRequest{Project: p, Image: imgName})
	if err != nil {
		t.Fatal(err)
	}
	if img.GetStatus() != "READY" || img.GetSourceType() != "RAW" || img.GetDiskSizeGb() != 3 || img.GetArchiveSizeBytes() <= 0 ||
		img.GetRawDisk().GetContainerType() != "TAR" || !strings.HasSuffix(img.GetRawDisk().GetSource(), "/disk.tar.gz") {
		t.Fatalf("imported image: %v", img)
	}

	op, err = insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(vmName),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("global/images/" + imgName),
				// The 3 GB image is below pd-balanced's 10 GB minimum, which
				// Google enforces on the disk, so the disk is sized.
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
		if err == nil && strings.Contains(so.GetContents(), "IMPORTED-MARK-"+sfx) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the imported disk's marker never reached the serial port (%v): %.2000s", err, so.GetContents())
		}
		time.Sleep(3 * time.Second)
	}
}

// buildVMImage builds a public image's VM container image as CloudBurrow
// does (a no-op when it is already built) and returns its reference.
func buildVMImage(t *testing.T, ctx context.Context, img compute.PublicImage, dir string) string {
	t.Helper()
	ref := img.ImageRef()
	if exec.CommandContext(ctx, "docker", "image", "inspect", ref).Run() == nil {
		return ref
	}
	for p, c := range img.BuildContext() {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(p, "root/usr/lib/cloudburrow/") {
			mode = 0o755
		}
		if err := os.WriteFile(full, c, mode); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := exec.CommandContext(ctx, "docker", "build", "--build-arg", "BASE="+img.Base, "--label", "cloudburrow.dev/owned=true",
		"--label", "cloudburrow.dev/vm-image="+img.Family, "-t", ref, dir).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", ref, err, b)
	}
	return ref
}
