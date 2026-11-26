package main

import (
	"context"
	"strings"
	"testing"
)

// Instance schedules (#2206): Create a schedule, the list, Add instances
// to schedule, the schedule's page, Delete refused while it has VMs,
// Remove instances, Delete; and a disk with a customer-supplied key, its
// page, and its VM's Start with encryption key.
func TestConsoleComputeInstanceSchedules(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "sched-p"
	vms, disks, scheds := computeInstancesProvider{s}, computeDisksProvider{s}, computeInstanceSchedulesProvider{s}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "bad", "region": "us-central1", "startSchedule": "0,30 9 * * *"}); err == nil {
		t.Error("a schedule with two starts an hour was made")
	}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "office", "region": "us-central1", "startSchedule": "0 9 * * 1-5",
		"stopSchedule": "0 18 * * 1-5", "timeZone": "Europe/Paris", "description": "office hours"}); err != nil {
		t.Fatal(err)
	}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "vm-1", "zone": "us-central1-a", "machineType": "e2-small",
		"image": "debian-cloud/debian-12"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	sp := []string{"us-central1", "office"}
	if err := scheds.ActAt(ctx, project, sp, "add", map[string]string{"instance": "zones/us-central1-a/instances/vm-1"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	l, err := scheds.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Start schedule"] != "0 9 * * 1-5" || l.Items[0].Fields["Time zone"] != "Europe/Paris" ||
		l.Items[0].Fields["In use by"] != "1 VM instances" {
		t.Fatalf("Instance schedules: %+v %v", l.Items, err)
	}
	det, err := scheds.Detail(ctx, project, sp)
	if err != nil || det.Sections[1].Listing.Total != 1 {
		t.Fatalf("the schedule's page: %+v %v", det, err)
	}
	for _, a := range scheds.DetailActions(ctx, project, sp) {
		if a.ID == "delete" && a.Unavailable == "" {
			t.Error("Delete is offered while the schedule has VMs")
		}
	}
	if err := scheds.ActAt(ctx, project, sp, "remove", map[string]string{"instance": "zones/us-central1-a/instances/vm-1"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := scheds.ActAt(ctx, project, sp, "delete", nil); err != nil {
		t.Fatal(err)
	}

	// A customer-supplied key.
	const raw = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	if _, err := disks.Create(ctx, project, map[string]string{"name": "csek", "zone": "us-central1-a", "sizeGb": "10", "type": "pd-balanced",
		"encryption": "Customer-supplied encryption key", "rawKey": raw}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	dd, err := disks.Detail(ctx, project, []string{"us-central1-a", "csek"})
	if err != nil {
		t.Fatal(err)
	}
	enc := ""
	for _, p := range dd.Sections[0].Groups[0].Properties {
		if p.Label == "Encryption" {
			enc = p.Value
		}
	}
	if !strings.HasPrefix(enc, "Customer-supplied") {
		t.Errorf("the CSEK disk's Encryption: %q", enc)
	}
	if _, err := s.call("POST", project, "zones/us-central1-a/instances/vm-1/attachDisk", nil, map[string]any{
		"source": "zones/us-central1-a/disks/csek", "deviceName": "csek", "diskEncryptionKey": map[string]any{"rawKey": raw}}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	path := []string{"us-central1-a", "vm-1"}
	if err := vms.ActAt(ctx, project, path, "stop", nil); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	var start *struct{ label string }
	var field string
	for _, a := range vms.DetailActions(ctx, project, path) {
		if a.ID == "start" {
			start = &struct{ label string }{a.Label}
			for _, f := range a.Fields {
				field = f.Name
			}
		}
	}
	if start == nil || start.label != "Start with encryption key" || !strings.HasPrefix(field, "csek:") {
		t.Fatalf("Start for a VM with a CSEK disk: %v %q", start, field)
	}
	if err := vms.ActAt(ctx, project, path, "start", map[string]string{field: raw}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if o, _ := s.call("GET", project, "zones/us-central1-a/instances/vm-1", nil, nil); vpcStr(o, "status") != "RUNNING" {
		t.Errorf("after Start with encryption key: %s", vpcStr(o, "status"))
	}
}
