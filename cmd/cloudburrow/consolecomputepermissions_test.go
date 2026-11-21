package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleComputePermissions (#2125): a disk's, a snapshot's and a
// subnet's allow policy is read and written through their own getIamPolicy
// and setIamPolicy, with the etag the page read (a stale one is the API's
// refusal), a conditional binding kept; testIamPermissions answers; the VM
// instances list has its Permissions button, the Disks and Snapshots lists
// their info panel Permissions tab; a network keeps no policy.
func TestConsoleComputePermissions(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "p"
	vms, disks, snaps, nets := computeInstancesProvider{s}, computeDisksProvider{s}, computeSnapshotsProvider{s}, computeNetworksProvider{s}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "data-1", "zone": "us-central1-a", "source": "Blank disk", "type": "pd-ssd", "sizeGb": "20"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if err := disks.ActAt(ctx, project, []string{"us-central1-a", "data-1"}, "snapshot", map[string]string{"name": "snap-1"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if _, err := nets.Create(ctx, project, map[string]string{"name": "app", "subnetMode": "Custom",
		"subnetName": "app-sub", "subnetRegion": "us-central1", "subnetRange": "10.1.0.0/24"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		pe   console.PolicyEditor
		path []string
	}{
		{"disk", disks, []string{"us-central1-a", "data-1"}},
		{"snapshot", snaps, []string{"snap-1"}},
		{"subnet", nets, []string{"app", "subnets", "us-central1", "app-sub"}},
	}
	for _, c := range cases {
		if c.pe.PolicyOn(ctx, c.path) == nil {
			t.Fatalf("%s: no Permissions tab", c.name)
		}
		pol, err := c.pe.GetPolicy(ctx, project, c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		next, err := console.GrantAccess(pol, "roles/compute.viewer", []string{"user:ana@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		next, err = console.GrantConditional(next, "roles/compute.admin", []string{"user:bo@example.com"},
			&console.Condition{Title: "until", Expression: `request.time < timestamp("2030-01-01T00:00:00Z")`})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.pe.SetPolicy(ctx, project, c.path, next); err != nil {
			t.Fatalf("%s: set: %v", c.name, err)
		}
		got, err := c.pe.GetPolicy(ctx, project, c.path)
		if err != nil {
			t.Fatal(err)
		}
		var roles []string
		for _, b := range got.Bindings {
			roles = append(roles, b.Role+"="+strings.Join(b.Members, ","))
			if b.Role == "roles/compute.admin" && (b.Conditional == nil || b.Conditional.Title != "until") {
				t.Errorf("%s: the condition was not kept: %+v", c.name, b)
			}
		}
		if s := strings.Join(roles, ";"); !strings.Contains(s, "roles/compute.viewer=user:ana@example.com") ||
			!strings.Contains(s, "roles/compute.admin=user:bo@example.com") {
			t.Errorf("%s: bindings %s", c.name, s)
		}
		// The etag read before the change is stale now.
		if err := c.pe.SetPolicy(ctx, project, c.path, next); err == nil {
			t.Errorf("%s: a stale etag was accepted", c.name)
		}
		if _, err := c.pe.TestPermissions(ctx, project, c.path, []string{"compute.disks.get"}); err != nil {
			t.Fatalf("%s: test: %v", c.name, err)
		}
	}
	if nets.PolicyOn(ctx, []string{"app"}) != nil {
		t.Error("a VPC network has no allow policy of its own")
	}
	if vms.PolicyOn(ctx, []string{"us-central1-a"}) != nil {
		t.Error("a zone is not an instance")
	}

	vl, err := vms.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if !vl.PermissionsButton {
		t.Error("VM instances has no Permissions button")
	}
	dl, err := disks.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	sl, err := snaps.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []console.Listing{dl, sl} {
		if !l.InfoOnSelect || len(l.Items) == 0 || len(l.Items[0].Info) == 0 || !l.Items[0].Info[0].Permissions {
			t.Errorf("%s: no info panel Permissions tab: %+v", l.Noun, l.Items)
		}
	}
}
