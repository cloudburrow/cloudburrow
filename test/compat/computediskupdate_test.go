//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestComputeHyperdiskBalancedHAThroughTheGoClient (#2206): Hyperdisk
// Balanced High Availability's documented limits (size, IOPS, throughput,
// access modes, its machine series), regionDisks.update and disks.update
// by update mask, and a multi-writer volume attached read-write to a C3
// instance in each replica zone, through the official Go client.
// covers: compute.regionDisks.update, compute.disks.update
func TestComputeHyperdiskBalancedHAThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	rdisks, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	insts, _ := computeapi.NewInstancesRESTClient(ctx, opts...)
	p := h.Project()
	const region, za, zb = "us-central1", "us-central1-a", "us-central1-b"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	hd, zd, vma, vmb, e2 := "hd-"+sfx, "hdz-"+sfx, "hd-vma-"+sfx, "hd-vmb-"+sfx, "hd-e2-"+sfx
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
		for _, v := range [][2]string{{za, vma}, {zb, vmb}, {za, e2}} {
			if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: p, Zone: v[0], Instance: v[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := rdisks.Delete(c, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: hd}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: za, Disk: zd}); err == nil {
			_ = op.Wait(c)
		}
	})
	ha := "regions/" + region + "/diskTypes/hyperdisk-balanced-high-availability"
	zones := []string{"zones/" + za, "zones/" + zb}
	for name, d := range map[string]*computepb.Disk{
		"3 GiB":                   {Name: proto.String(hd), Type: proto.String(ha), SizeGb: proto.Int64(3), ReplicaZones: zones},
		"IOPS without a size":     {Name: proto.String(hd), Type: proto.String(ha), ProvisionedIops: proto.Int64(4000), ReplicaZones: zones},
		"IOPS above 500 per GiB":  {Name: proto.String(hd), Type: proto.String(ha), SizeGb: proto.Int64(10), ProvisionedIops: proto.Int64(6000), ReplicaZones: zones},
		"throughput above IOPS/4": {Name: proto.String(hd), Type: proto.String(ha), SizeGb: proto.Int64(100), ProvisionedIops: proto.Int64(4000), ProvisionedThroughput: proto.Int64(1001), ReplicaZones: zones},
		"read-only many":          {Name: proto.String(hd), Type: proto.String(ha), SizeGb: proto.Int64(100), AccessMode: proto.String("READ_ONLY_MANY"), ReplicaZones: zones},
	} {
		if _, err := rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: d}); computeCode(err) != http.StatusBadRequest {
			t.Errorf("a volume with %s: %v, want 400", name, err)
		}
	}
	wait(rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: &computepb.Disk{
		Name: proto.String(hd), Type: proto.String(ha), SizeGb: proto.Int64(100), ReplicaZones: zones}}))
	d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: hd})
	if err != nil || d.GetProvisionedIops() != 3600 || d.GetProvisionedThroughput() != 290 || d.GetAccessMode() != "READ_WRITE_SINGLE" {
		t.Fatalf("the defaults of a 100 GiB volume: %v %v", d, err)
	}

	// regionDisks.update: performance, then a second change within 4 hours.
	wait(rdisks.Update(ctx, &computepb.UpdateRegionDiskRequest{Project: p, Region: region, Disk: hd,
		Paths:        proto.String("provisionedIops,provisionedThroughput"),
		DiskResource: &computepb.Disk{Name: proto.String(hd), ProvisionedIops: proto.Int64(8000), ProvisionedThroughput: proto.Int64(600)}}))
	if d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: hd}); err != nil || d.GetProvisionedIops() != 8000 || d.GetProvisionedThroughput() != 600 {
		t.Fatalf("after regionDisks.update: %v %v", d, err)
	}
	if _, err := rdisks.Update(ctx, &computepb.UpdateRegionDiskRequest{Project: p, Region: region, Disk: hd, Paths: proto.String("provisionedIops"),
		DiskResource: &computepb.Disk{Name: proto.String(hd), ProvisionedIops: proto.Int64(9000)}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a second performance change within 4 hours: %v, want 400", err)
	}
	wait(rdisks.Update(ctx, &computepb.UpdateRegionDiskRequest{Project: p, Region: region, Disk: hd, UpdateMask: proto.String("size_gb"),
		DiskResource: &computepb.Disk{Name: proto.String(hd), SizeGb: proto.Int64(120)}}))

	// Machine series, then multi-writer on a C3 instance in each replica zone.
	vm := func(zone, name, mt string) {
		wait(insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
			Name: proto.String(name), MachineType: proto.String("zones/" + zone + "/machineTypes/" + mt),
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true), InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
			NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}}}}))
	}
	vm(za, e2, "e2-standard-2")
	vm(za, vma, "c3-standard-4")
	vm(zb, vmb, "c3-standard-4")
	src := "projects/" + p + "/regions/" + region + "/disks/" + hd
	attach := func(zone, inst string) error {
		op, err := insts.AttachDisk(ctx, &computepb.AttachDiskInstanceRequest{Project: p, Zone: zone, Instance: inst,
			AttachedDiskResource: &computepb.AttachedDisk{Source: proto.String(src), DeviceName: proto.String("shared")}})
		if err == nil {
			err = op.Wait(ctx)
		}
		return err
	}
	if err := attach(za, e2); computeCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "machine type") {
		t.Errorf("attach to an E2 instance: %v, want 400", err)
	}
	if err := attach(za, vma); err != nil {
		t.Fatal(err)
	}
	if err := attach(zb, vmb); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a second writer in single-writer mode: %v, want 400", err)
	}
	if _, err := rdisks.Update(ctx, &computepb.UpdateRegionDiskRequest{Project: p, Region: region, Disk: hd, Paths: proto.String("accessMode"),
		DiskResource: &computepb.Disk{Name: proto.String(hd), AccessMode: proto.String("READ_WRITE_MANY")}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("the access mode while attached: %v, want 400", err)
	}
	wait(insts.DetachDisk(ctx, &computepb.DetachDiskInstanceRequest{Project: p, Zone: za, Instance: vma, DeviceName: "shared"}))
	wait(rdisks.Update(ctx, &computepb.UpdateRegionDiskRequest{Project: p, Region: region, Disk: hd, Paths: proto.String("accessMode"),
		DiskResource: &computepb.Disk{Name: proto.String(hd), AccessMode: proto.String("READ_WRITE_MANY")}}))
	if err := attach(za, vma); err != nil {
		t.Fatal(err)
	}
	if err := attach(zb, vmb); err != nil {
		t.Fatalf("a second writer in multi-writer mode: %v", err)
	}
	if d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: hd}); err != nil || len(d.GetUsers()) != 2 || d.GetAccessMode() != "READ_WRITE_MANY" {
		t.Fatalf("the multi-writer volume: %v %v", d, err)
	}
	for _, v := range [][2]string{{za, vma}, {zb, vmb}} {
		wait(insts.DetachDisk(ctx, &computepb.DetachDiskInstanceRequest{Project: p, Zone: v[0], Instance: v[1], DeviceName: "shared"}))
	}

	// disks.update of a zonal Persistent Disk: size and architecture; no IOPS.
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{Name: proto.String(zd), SizeGb: proto.Int64(10)}}))
	if _, err := disks.Update(ctx, &computepb.UpdateDiskRequest{Project: p, Zone: za, Disk: zd, Paths: proto.String("provisionedIops"),
		DiskResource: &computepb.Disk{Name: proto.String(zd), ProvisionedIops: proto.Int64(3000)}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("IOPS of pd-balanced: %v, want 400", err)
	}
	wait(disks.Update(ctx, &computepb.UpdateDiskRequest{Project: p, Zone: za, Disk: zd, Paths: proto.String("sizeGb,architecture"),
		DiskResource: &computepb.Disk{Name: proto.String(zd), SizeGb: proto.Int64(15), Architecture: proto.String("X86_64")}}))
	if z, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: zd}); err != nil || z.GetSizeGb() != 15 || z.GetArchitecture() != "X86_64" {
		t.Errorf("after disks.update: %v %v", z, err)
	}
}

// TestGcloudComputeDisksUpdate (#2206): gcloud compute disks create and
// update of a Hyperdisk Balanced High Availability volume (--region,
// --provisioned-iops, --provisioned-throughput, --access-mode), with
// egress to Google blocked and every command against Compute Engine here.
func TestGcloudComputeDisksUpdate(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	rdisks, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
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
	const region = "us-central1"
	hd := fmt.Sprintf("gh-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() { _, _ = run("compute", "disks", "delete", hd, "--region", region) })
	if out, err := run("compute", "disks", "create", hd, "--region", region, "--replica-zones", region+"-a,"+region+"-b",
		"--type", "hyperdisk-balanced-high-availability", "--size", "10GB", "--provisioned-iops", "6000"); err == nil {
		t.Errorf("6,000 IOPS on a 10 GiB volume: %s", out)
	}
	gc("compute", "disks", "create", hd, "--region", region, "--replica-zones", region+"-a,"+region+"-b",
		"--type", "hyperdisk-balanced-high-availability", "--size", "50GB", "--provisioned-iops", "5000", "--provisioned-throughput", "300")
	if got := gc("compute", "disks", "describe", hd, "--region", region, "--format=value(provisionedIops,provisionedThroughput,accessMode)"); !strings.Contains(got, "5000") ||
		!strings.Contains(got, "300") || !strings.Contains(got, "READ_WRITE_SINGLE") {
		t.Fatalf("disks describe: %s", got)
	}
	gc("compute", "disks", "update", hd, "--region", region, "--provisioned-iops", "20000", "--provisioned-throughput", "500")
	gc("compute", "disks", "update", hd, "--region", region, "--access-mode", "READ_WRITE_MANY")
	if out, err := run("compute", "disks", "update", hd, "--region", region, "--provisioned-iops", "10000"); err == nil || !strings.Contains(out, "due to being rate limited") {
		t.Errorf("a second performance change within 4 hours: %v %s", err, out)
	}
	d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: h.Project(), Region: region, Disk: hd})
	if err != nil || d.GetProvisionedIops() != 20000 || d.GetProvisionedThroughput() != 500 || d.GetAccessMode() != "READ_WRITE_MANY" {
		t.Fatalf("after gcloud compute disks update: %v %v", d, err)
	}
	gc("compute", "disks", "delete", hd, "--region", region)
}

// TestTerraformComputeRegionDiskHyperdiskHA (#2206): a
// google_compute_region_disk of Hyperdisk Balanced High Availability with
// provisioned IOPS and throughput, its access_mode changed in place (the
// provider's regionDisks.update?paths=accessMode), through `cloudburrow
// terraform` with egress to Google blocked.
func TestTerraformComputeRegionDiskHyperdiskHA(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	rdisks, err := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	m := newTFModule(t)
	const region = "us-central1"
	name := fmt.Sprintf("tfhd-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		if op, err := rdisks.Delete(context.Background(), &computepb.DeleteRegionDiskRequest{Project: m.project, Region: region, Disk: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	module := func(mode string) string {
		return fmt.Sprintf(`
resource "google_compute_region_disk" "hd" {
  project                = %[1]q
  name                   = %[2]q
  region                 = %[3]q
  type                   = "hyperdisk-balanced-high-availability"
  size                   = 100
  provisioned_iops       = 6000
  provisioned_throughput = 400
  access_mode            = %[4]q
  replica_zones          = ["%[3]s-a", "%[3]s-b"]
}
`, m.project, name, region, mode)
	}
	m.write(module("READ_WRITE_SINGLE"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: m.project, Region: region, Disk: name})
	if err != nil || d.GetProvisionedIops() != 6000 || d.GetProvisionedThroughput() != 400 || d.GetAccessMode() != "READ_WRITE_SINGLE" {
		t.Fatalf("the volume Terraform made: %v, %v", d, err)
	}
	m.planClean()
	id := d.GetId()
	m.write(module("READ_WRITE_MANY"))
	m.apply()
	if d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: m.project, Region: region, Disk: name}); err != nil || d.GetAccessMode() != "READ_WRITE_MANY" || d.GetId() != id {
		t.Fatalf("after the access mode change: %v, %v", d, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: m.project, Region: region, Disk: name}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the volume after destroy: %v", err)
	}
}
