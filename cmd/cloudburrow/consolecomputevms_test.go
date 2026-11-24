package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// consoleFakeVMs is a VMBackend that does nothing, with room to spare.
type consoleFakeVMs struct{}

func (consoleFakeVMs) CreateDisk(context.Context, compute.DiskSpec) error                 { return nil }
func (consoleFakeVMs) CopyDisk(context.Context, compute.DiskSpec, compute.DiskSpec) error { return nil }
func (consoleFakeVMs) DeleteDisk(context.Context, compute.DiskSpec) error                 { return nil }
func (consoleFakeVMs) ResizeDisk(context.Context, compute.DiskSpec) error                 { return nil }
func (consoleFakeVMs) Snapshot(context.Context, compute.DiskSpec, string) error           { return nil }
func (consoleFakeVMs) DeleteSnapshot(context.Context, string) error                       { return nil }
func (consoleFakeVMs) Start(context.Context, compute.VMSpec) error                        { return nil }
func (consoleFakeVMs) Stop(context.Context, compute.VMSpec) error                         { return nil }
func (consoleFakeVMs) Suspend(context.Context, compute.VMSpec) error                      { return nil }
func (consoleFakeVMs) Resume(context.Context, compute.VMSpec) error                       { return nil }
func (consoleFakeVMs) SerialOutput(context.Context, compute.VMSpec) (string, error) {
	return "boot ok\n", nil
}
func (consoleFakeVMs) SSHAddress(compute.VMSpec) string { return "127.0.0.1:2222" }
func (consoleFakeVMs) FreeBytes() (uint64, error)       { return 1 << 40, nil }

func computeVMConsoleFixture(t *testing.T) computeScreen {
	t.Helper()
	api, err := compute.New(compute.Options{VMs: consoleFakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	return computeScreen{svc: &computeService{api: api}}
}

// settle waits until no operation of the project's zone is running.
func settle(t *testing.T, s computeScreen, project string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := s.list(project, "zones/us-central1-a/operations")
		if err != nil {
			t.Fatal(err)
		}
		busy := false
		for _, op := range l {
			busy = busy || vpcStr(op, "status") != "DONE"
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("operations never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The Compute Engine pages (#1965): Create an instance with a startup
// script, metadata, labels and the default service account; the VM
// instances list (zone, IPs, the SSH port) with each status's actions;
// SSH's commands; the instance's page and tabs (the serial port, the
// boot disk); Edit (labels, metadata, tags); Stop, Start / Resume,
// Suspend, Reset; Disks' Create disk, Attach and Detach disk on the
// instance's page, Create snapshot, Edit size, Delete refused while
// attached; Snapshots' Create disk and Delete; Images with the refused
// projects; Machine types; Delete.
func TestConsoleComputeVMs(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "p"
	vms, disks, snaps := computeInstancesProvider{s}, computeDisksProvider{s}, computeSnapshotsProvider{s}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "web-1", "zone": "us-central1-a", "machineType": "e2-small",
		"image": "debian-cloud/debian-12", "bootDiskSize": "10", "network": "default", "externalIP": "Ephemeral",
		"labels": `{"env":"dev"}`, "startupScript": "#!/bin/bash\necho hi", "serviceAccount": "Compute Engine default service account",
		"metadata": console.FormatGroup([]map[string]string{{"key": "team", "value": "burrow"}})}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	l, err := vms.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	r := vpcRow(l, "web-1")
	if r == nil || r.Status != "RUNNING" || r.Fields["Zone"] != "us-central1-a" || r.Fields["External IP"] != "127.0.0.1" ||
		r.Fields["Connect"] != "SSH (port 2222)" || !strings.HasPrefix(r.Fields["Internal IP"], "10.128.") {
		t.Fatalf("the instance's row: %+v", r)
	}
	if got := vpcActionIDs(r.Actions); got != "ssh,stop,suspend,reset,delete" {
		t.Errorf("a running instance's actions: %s", got)
	}
	path := []string{"us-central1-a", "web-1"}
	res, err := vms.ActAtResult(ctx, project, path, "ssh", nil)
	if err != nil || len(res.Items) != 2 || !strings.Contains(res.Items[0].Fields["Command"], `--ssh-flag="-p 2222"`) {
		t.Fatalf("SSH: %+v %v", res, err)
	}
	d, err := vms.Detail(ctx, project, path)
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the instance's page: %+v %v", d, err)
	}
	var tabs []string
	for _, sec := range d.Sections {
		tabs = append(tabs, sec.Label)
	}
	if strings.Join(tabs, ",") != "Details,Storage,Networking,Serial port 1 (console)" || d.Sections[3].Text != "boot ok\n" {
		t.Fatalf("the instance's tabs: %v, serial %q", tabs, d.Sections[3].Text)
	}
	if b := d.Sections[1].Listing.Items; len(b) != 1 || b[0].Fields["Type"] != "Boot disk" {
		t.Fatalf("Storage: %+v", b)
	}
	o, _ := vms.instance(project, path)
	if md := console.FormatGroup(vmMetadata(o)); !strings.Contains(md, "startup-script") || !strings.Contains(md, "team") {
		t.Fatalf("metadata: %s", md)
	}
	if sa := vmMaps(o, "serviceAccounts"); len(sa) != 1 {
		t.Fatalf("service accounts: %v", sa)
	}
	if err := vms.Edit(ctx, project, path, map[string]string{"labels": `{"env":"prod"}`, "networkTags": "http-server",
		"machineType": "e2-small", "metadata": console.FormatGroup([]map[string]string{{"key": "team", "value": "core"}})}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	o, _ = vms.instance(project, path)
	if vmLabels(o)["env"] != "prod" || strings.Join(vpcStrs(vmMap(o, "tags"), "items"), ",") != "http-server" ||
		console.FormatGroup(vmMetadata(o)) != console.FormatGroup([]map[string]string{{"key": "team", "value": "core"}}) {
		t.Fatalf("after Edit: %v %v %v", o["labels"], o["tags"], vmMetadata(o))
	}

	// Disks: Create disk, attach, a refused delete, detach.
	if _, err := disks.Create(ctx, project, map[string]string{"name": "data-1", "zone": "us-central1-a", "source": "Blank disk", "type": "pd-ssd", "sizeGb": "20"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	acts := vms.DetailActions(ctx, project, path)
	if got := vpcActionIDs(acts); got != "ssh,stop,suspend,reset,attachdisk,delete" {
		t.Fatalf("the page's actions: %s", got)
	}
	if err := vms.ActAt(ctx, project, path, "attachdisk", map[string]string{"disk": "data-1", "deviceName": "data"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	dl, _ := disks.List(ctx, project)
	if r := vpcRow(dl, "data-1"); r == nil || r.Fields["In use by"] != "web-1" || r.Fields["Size"] != "20 GB" || r.Actions[3].Unavailable == "" {
		t.Fatalf("the attached disk's row: %+v", r)
	}
	if err := vms.ActAt(ctx, project, path, "detachdisk", map[string]string{"deviceName": "data"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	dp := []string{"us-central1-a", "data-1"}
	if err := disks.ActAt(ctx, project, dp, "resize", map[string]string{"sizeGb": "30"}); err != nil {
		t.Fatal(err)
	}
	if err := disks.ActAt(ctx, project, dp, "resize", map[string]string{"sizeGb": "5"}); err == nil {
		t.Error("shrinking a disk was accepted")
	}
	if err := disks.ActAt(ctx, project, dp, "snapshot", map[string]string{"name": "snap-1"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	sl, err := snaps.List(ctx, project)
	if r := vpcRow(sl, "snap-1"); err != nil || r == nil || r.Fields["Source disk"] != "data-1" {
		t.Fatalf("Snapshots: %+v %v", sl, err)
	}
	if err := snaps.ActAt(ctx, project, []string{"snap-1"}, "createdisk", map[string]string{"name": "restored", "zone": "us-central1-a"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)

	// Stop, Start / Resume, Suspend, Reset.
	for _, step := range []struct{ action, want, actions string }{
		{"stop", "TERMINATED", "start,attachdisk,delete"},
		{"start", "RUNNING", ""},
		{"suspend", "SUSPENDED", "start,stop,attachdisk,delete"},
		{"start", "RUNNING", ""},
		{"reset", "RUNNING", ""},
	} {
		if err := vms.ActAt(ctx, project, path, step.action, nil); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
		settle(t, s, project)
		o, _ := vms.instance(project, path)
		if vpcStr(o, "status") != step.want {
			t.Fatalf("after %s: %s", step.action, vpcStr(o, "status"))
		}
		if step.actions != "" {
			if got := vpcActionIDs(vms.DetailActions(ctx, project, path)); got != step.actions {
				t.Errorf("after %s the page offers %s", step.action, got)
			}
		}
	}

	il, err := computeImagesProvider{s}.List(ctx, project)
	if err != nil || len(il.Items) != 3 || !strings.Contains(il.Note, "cos-cloud") {
		t.Fatalf("Images: %+v %v", il, err)
	}
	ml, err := computeMachineTypesProvider{s}.List(ctx, project)
	if r := vpcRow(ml, "e2-medium"); err != nil || r == nil || r.Fields["vCPUs"] != "2" || r.Fields["Memory"] != "4 GB" {
		t.Fatalf("Machine types: %+v %v", r, err)
	}

	// Delete, and the auto-delete boot disk goes with the instance.
	if err := vms.ActAt(ctx, project, path, "delete", nil); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if l, _ := vms.List(ctx, project); len(l.Items) != 0 {
		t.Fatalf("after Delete: %+v", l.Items)
	}
	for _, name := range []string{"data-1", "restored"} {
		if err := disks.ActAt(ctx, project, []string{"us-central1-a", name}, "delete", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := snaps.Delete(ctx, project, "snap-1"); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if dl, _ := disks.List(ctx, project); len(dl.Items) != 0 {
		t.Errorf("disks left: %+v", dl.Items)
	}

	none := computeInstancesProvider{computeScreen{svc: &computeService{}}}
	if _, err := none.List(ctx, project); err == nil || err.Error() != computeNotStarted {
		t.Errorf("without the API: %v", err)
	}
}

// Metadata (#1965): the project's common instance metadata, Edit.
func TestConsoleComputeProjectMetadata(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	p := computeMetadataProvider{s}
	acts := p.ListActions(ctx, "p")
	if len(acts) != 1 || acts[0].ID != "edit" {
		t.Fatalf("Metadata's actions: %+v", acts)
	}
	fp := acts[0].Fields[1].Default
	if err := p.ActAt(ctx, "p", nil, "edit", map[string]string{"fingerprint": fp,
		"metadata": console.FormatGroup([]map[string]string{{"key": "ssh-keys", "value": "alice:ssh-ed25519 AAAA alice"}})}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, "p")
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "ssh-keys" {
		t.Fatalf("after Edit: %+v %v", l, err)
	}
	if err := p.ActAt(ctx, "p", nil, "edit", map[string]string{"fingerprint": fp, "metadata": ""}); err == nil {
		t.Error("an edit under a stale fingerprint was accepted")
	}
}
