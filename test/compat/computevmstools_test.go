//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
)

// TestGcloudComputeInstances (#1965): the pinned gcloud through
// gcloud-setup, egress to Google blocked, HOME and the SSH key in the
// test's own directories: images describe-from-family, instances create
// (debian-12 by family, a startup script and a label), list (the
// aggregated list), describe, get-serial-port-output (the script's line),
// add-metadata, disks create, attach-disk, `gcloud compute ssh` (the key
// gcloud generates added to the project's metadata, the forwarded port
// given with --ssh-flag="-p PORT" since gcloud always dials port 22),
// stop, start and delete.
// covers: compute.instances.aggregatedList, compute.projects.get, compute.projects.setCommonInstanceMetadata, compute.globalOperations.get
func TestGcloudComputeInstances(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	addr := h.Endpoint(EnvCompute)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client on PATH")
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
	const zone = "us-central1-a"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vm, disk := "gc-vm-"+sfx, "gc-disk-"+sfx
	t.Cleanup(func() {
		_, _ = run("compute", "instances", "delete", vm, "--zone", zone)
		_, _ = run("compute", "disks", "delete", disk, "--zone", zone)
	})

	if got := gc("compute", "images", "describe-from-family", "debian-12", "--project", "debian-cloud", "--format=value(family,status)"); !strings.Contains(got, "debian-12") || !strings.Contains(got, "READY") {
		t.Fatalf("images describe-from-family: %q", got)
	}
	gc("compute", "instances", "create", vm, "--zone", zone, "--machine-type", "e2-small",
		"--image-family", "debian-12", "--image-project", "debian-cloud", "--labels", "env=test",
		"--metadata", "startup-script=#!/bin/bash\necho GCLOUD-MARK-$(hostname)")
	if got := gc("compute", "instances", "list", "--format=value(name,status,networkInterfaces[0].accessConfigs[0].natIP)"); !strings.Contains(got, vm) || !strings.Contains(got, "RUNNING") || !strings.Contains(got, "192.0.2.") {
		t.Fatalf("instances list:\n%s", got)
	}
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=value(status,machineType.basename(),labels.env)"); !strings.Contains(got, "RUNNING") || !strings.Contains(got, "e2-small") || !strings.Contains(got, "test") {
		t.Fatalf("instances describe: %q", got)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, _ := run("compute", "instances", "get-serial-port-output", vm, "--zone", zone)
		if strings.Contains(out, "GCLOUD-MARK-"+vm) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the serial port never showed the startup script's line:\n%s", lastLines(out, 30))
		}
		time.Sleep(3 * time.Second)
	}
	gc("compute", "instances", "add-metadata", vm, "--zone", zone, "--metadata", "team=burrow")
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=json(metadata.items)"); !strings.Contains(got, `"team"`) || !strings.Contains(got, "startup-script") {
		t.Fatalf("after add-metadata: %s", got)
	}
	gc("compute", "disks", "create", disk, "--zone", zone, "--size", "10GB", "--type", "pd-balanced")
	gc("compute", "instances", "attach-disk", vm, "--zone", zone, "--disk", disk, "--device-name", "data")

	// gcloud compute ssh: its key, made by gcloud in this test's
	// directory, goes into the project's ssh-keys; the port is the one
	// CloudBurrow forwards to the guest's 22 on 127.0.0.1, which HostName
	// puts in place of the instance's natIP (an unrouted 192.0.2.0/24
	// address of its own, #2140).
	sshAddr := vmSSHAddress(t, addr, h.Project(), zone, vm)
	host, port, _ := net.SplitHostPort(sshAddr)
	if host != "127.0.0.1" {
		t.Fatalf("cloudburrowSshAddress is %s, want 127.0.0.1:PORT", sshAddr)
	}
	key := filepath.Join(home, "gce-key")
	ssh := func(cmd string) (string, error) {
		return run("compute", "ssh", vm, "--zone", zone, "--ssh-key-file", key, "--strict-host-key-checking=no",
			"--ssh-flag=-p "+port, "--ssh-flag=-o HostName="+host, "--ssh-flag=-o ConnectTimeout=10", "--command", cmd)
	}
	var out string
	var err error
	deadline = time.Now().Add(3 * time.Minute)
	for {
		out, err = ssh("hostname; sudo -n true && echo SUDO-OK; mountpoint /mnt/disks/data && echo DATA-OK")
		if err == nil && strings.Contains(out, "SUDO-OK") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gcloud compute ssh: %v\n%s", err, lastLines(out, 30))
		}
		time.Sleep(5 * time.Second)
	}
	if !strings.Contains(out, "DATA-OK") {
		t.Errorf("the attached disk is not mounted:\n%s", out)
	}
	if _, err := os.Stat(key + ".pub"); err != nil {
		t.Errorf("gcloud made no key in the test's directory: %v", err)
	}
	if got := gc("compute", "project-info", "describe", "--format=json(commonInstanceMetadata.items)"); !strings.Contains(got, "ssh-keys") {
		t.Errorf("the project's metadata has no ssh-keys after gcloud compute ssh: %s", got)
	}

	gc("compute", "instances", "stop", vm, "--zone", zone)
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=value(status)"); strings.TrimSpace(got) != "TERMINATED" {
		t.Fatalf("after stop: %q", got)
	}
	gc("compute", "instances", "start", vm, "--zone", zone)
	if got := gc("compute", "instances", "describe", vm, "--zone", zone, "--format=value(status)"); strings.TrimSpace(got) != "RUNNING" {
		t.Fatalf("after start: %q", got)
	}
	gc("compute", "instances", "delete", vm, "--zone", zone)
	if out, err := run("compute", "instances", "describe", vm, "--zone", zone); err == nil {
		t.Fatalf("the deleted instance still describes:\n%s", out)
	}
	ctx := context.Background()
	disks, err := computeapi.NewDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: h.Project(), Zone: zone, Disk: disk}); err != nil || len(d.GetUsers()) != 0 {
		t.Fatalf("the data disk after the instance's delete: %v users, %v", d.GetUsers(), err)
	}
	gc("compute", "disks", "delete", disk, "--zone", zone)
}

func hasStr(l []string, v string) bool {
	for _, s := range l {
		if s == v {
			return true
		}
	}
	return false
}

// TestTerraformComputeInstance (#1965): google_compute_disk,
// google_compute_instance (debian-12, a startup script, labels, the
// default network with an ephemeral external IP) and
// google_compute_attached_disk apply through `cloudburrow terraform` with
// egress to Google blocked; the official client reads the instance back
// RUNNING with the disk attached; the plan is clean; a label change
// applies in place and the plan is clean again; destroy removes all
// three.
// covers: compute.instances.get, compute.disks.get, compute.instances.attachDisk, compute.instances.detachDisk, compute.instances.setLabels, compute.zoneOperations.get
func TestTerraformComputeInstance(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	disks, _ := computeapi.NewDisksRESTClient(ctx, opts...)
	m := newTFModule(t)
	const zone = "us-central1-a"
	name := fmt.Sprintf("tfvm-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		c := context.Background()
		if op, err := insts.Delete(c, &computepb.DeleteInstanceRequest{Project: m.project, Zone: zone, Instance: name}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: m.project, Zone: zone, Disk: name + "-data"}); err == nil {
			_ = op.Wait(c)
		}
	})
	module := func(env string) string {
		return fmt.Sprintf(`
resource "google_compute_disk" "data" {
  project = %[1]q
  name    = "%[2]s-data"
  zone    = %[3]q
  type    = "pd-balanced"
  size    = 10
}
resource "google_compute_instance" "vm" {
  project      = %[1]q
  name         = %[2]q
  zone         = %[3]q
  machine_type = "e2-small"
  labels       = { env = %[4]q }
  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
    }
  }
  network_interface {
    network = "default"
    access_config {}
  }
  metadata = {
    team = "burrow"
  }
  metadata_startup_script = "echo TF-MARK"
  lifecycle {
    ignore_changes = [attached_disk]
  }
}
resource "google_compute_attached_disk" "data" {
  project     = %[1]q
  zone        = %[3]q
  disk        = google_compute_disk.data.id
  instance    = google_compute_instance.vm.id
  device_name = "data"
}
`, m.project, name, zone, env)
	}
	m.write(module("dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: m.project, Zone: zone, Instance: name})
	if err != nil || i.GetStatus() != "RUNNING" || len(i.GetDisks()) != 2 || i.GetLabels()["env"] != "dev" {
		t.Fatalf("the instance Terraform made: %v, %v", i, err)
	}
	m.planClean()
	m.write(module("prod"))
	m.apply()
	if i, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: m.project, Zone: zone, Instance: name}); err != nil || i.GetLabels()["env"] != "prod" || i.GetStatus() != "RUNNING" {
		t.Fatalf("after the label change: %v, %v", i, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: m.project, Zone: zone, Instance: name}); computeCode(err) != 404 {
		t.Errorf("the instance after destroy: %v", err)
	}
	if _, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: m.project, Zone: zone, Disk: name + "-data"}); computeCode(err) != 404 {
		t.Errorf("the disk after destroy: %v", err)
	}
}
