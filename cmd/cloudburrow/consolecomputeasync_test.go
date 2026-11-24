package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Asynchronous Replication pages (#2206): a regional disk made on
// Create a disk, its Permissions tab; its Create secondary disk in us-east1, which starts
// replication; Asynchronous replication listing the secondary with its
// primary and ACTIVE; Consistency groups' Create consistency group, Assign
// disks, the group's members, Clone consistency group, Terminate
// replication (STOPPED on both pages), Remove disks and Delete.
func TestConsoleComputeAsyncReplication(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "async-p"
	disks, async, groups := computeDisksProvider{s}, computeAsyncReplicationProvider{s}, computeConsistencyGroupsProvider{s}
	until := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	status := func(rel string) string {
		d, err := s.call("GET", project, rel, nil, nil)
		if err != nil {
			return err.Error()
		}
		return vpcStr(d, "status")
	}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "prim", "location": "Regional", "zone": "us-central1-a",
		"replicaZone": "us-central1-b", "sizeGb": "10", "type": "pd-balanced"}); err != nil {
		t.Fatal(err)
	}
	until("the primary READY", func() bool { return status("regions/us-central1/disks/prim") == "READY" })
	// The Permissions tab of the regional disk.
	rp := []string{"us-central1", "prim"}
	if disks.PolicyOn(ctx, rp) == nil {
		t.Fatal("the regional disk has no Permissions tab")
	}
	pol, err := disks.GetPolicy(ctx, project, rp)
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/compute.storageAdmin", Members: []string{"user:ops@example.com"}})
	if err := disks.SetPolicy(ctx, project, rp, pol); err != nil {
		t.Fatal(err)
	}
	if got, err := disks.GetPolicy(ctx, project, rp); err != nil || len(got.Bindings) != 1 {
		t.Fatalf("the regional disk's policy: %+v %v", got, err)
	}
	if got, err := disks.TestPermissions(ctx, project, rp, []string{"compute.disks.get"}); err != nil {
		t.Fatalf("testIamPermissions: %v %v", got, err)
	}
	acts := disks.DetailActions(ctx, project, []string{"us-central1", "prim"})
	if got := vpcActionIDs(acts); !strings.Contains(got, "secondary") || strings.Contains(got, "terminatereplication") {
		t.Fatalf("the primary's actions: %s", got)
	}
	if err := disks.ActAt(ctx, project, []string{"us-central1", "prim"}, "secondary", map[string]string{"name": "sec", "location": "Regional",
		"zone": "us-east1-b", "replicaZone": "us-east1-c"}); err != nil {
		t.Fatal(err)
	}
	l, err := async.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	r := vpcRow(l, "sec")
	if r == nil || r.Fields["Primary disk"] != "prim" || r.Fields["Replication state"] != "ACTIVE" ||
		r.Fields["Secondary location"] != "us-east1 (us-east1-b, us-east1-c)" || r.Fields["Primary location"] != "us-central1 (us-central1-a, us-central1-b)" {
		t.Fatalf("the secondary's row: %+v", r)
	}
	det, err := async.Detail(ctx, project, []string{"us-east1", "sec"})
	if err != nil || len(det.Sections[0].Groups) != 2 || det.Sections[0].Groups[1].Heading != "Asynchronous replication" {
		t.Fatalf("the secondary's page: %+v %v", det, err)
	}
	if got := vpcActionIDs(async.DetailActions(ctx, project, []string{"us-east1", "sec"})); got != "terminatereplication" {
		t.Errorf("the replicating secondary's actions: %s", got)
	}

	if _, err := groups.Create(ctx, project, map[string]string{"name": "cg", "region": "us-east1"}); err != nil {
		t.Fatal(err)
	}
	until("the group", func() bool { return status("regions/us-east1/resourcePolicies/cg") == "READY" })
	gacts := groups.DetailActions(ctx, project, []string{"us-east1", "cg"})
	if got := vpcActionIDs(gacts); got != "assign,remove,terminatereplication,clone,delete" || len(gacts[0].Fields[0].Options) != 1 ||
		gacts[0].Fields[0].Options[0] != "regions/us-east1/disks/sec" || gacts[1].Unavailable == "" {
		t.Fatalf("the group's actions: %s %+v", got, gacts[0].Fields[0].Options)
	}
	if err := groups.ActAt(ctx, project, []string{"us-east1", "cg"}, "assign", map[string]string{"disk": "regions/us-east1/disks/sec"}); err != nil {
		t.Fatal(err)
	}
	until("the disk assigned", func() bool {
		gd, err := groups.Detail(ctx, project, []string{"us-east1", "cg"})
		return err == nil && len(gd.Sections) == 2 && vpcRow(gd.Sections[1].Listing, "sec") != nil
	})
	gl, _ := groups.List(ctx, project)
	if r := vpcRow(gl, "cg"); r == nil || r.Fields["Disks"] != "1" || r.Fields["Region"] != "us-east1" {
		t.Fatalf("the group's row: %+v", r)
	}
	if err := groups.ActAt(ctx, project, []string{"us-east1", "cg"}, "clone", nil); err != nil {
		t.Fatal(err)
	}
	until("the clone", func() bool { return status("regions/us-east1/disks/sec-clone") == "READY" })
	if err := groups.ActAt(ctx, project, []string{"us-east1", "cg"}, "terminatereplication", nil); err != nil {
		t.Fatal(err)
	}
	until("replication STOPPED", func() bool {
		l, _ := async.List(ctx, project)
		r := vpcRow(l, "sec")
		return r != nil && r.Fields["Replication state"] == "STOPPED"
	})
	if err := groups.ActAt(ctx, project, []string{"us-east1", "cg"}, "remove", map[string]string{"disk": "regions/us-east1/disks/sec"}); err != nil {
		t.Fatal(err)
	}
	until("the disk removed", func() bool {
		gl, _ := groups.List(ctx, project)
		r := vpcRow(gl, "cg")
		return r != nil && r.Fields["Disks"] == "0"
	})
	if err := groups.ActAt(ctx, project, []string{"us-east1", "cg"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	until("the group deleted", func() bool { return strings.Contains(status("regions/us-east1/resourcePolicies/cg"), "not found") })
}
