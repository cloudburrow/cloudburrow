package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleUpdateVMs2139: a managed group's Update VMs form (New
// template, a canary's second template and size, Update type Automatic or
// Selective, Minimal and Most disruptive allowed action, Maximum surge and
// unavailable, Replacement method) patches the group's versions and
// updatePolicy, an Automatic update starting at once within Maximum surge;
// bad values are refused as the API refuses them; Selective leaves the VMs
// as they are until the checked VMs' Update.
func TestConsoleUpdateVMs2139(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "c2139u"
	tpls := computeTemplatesProvider{s}
	for _, n := range []string{"tpl", "tpl-2"} {
		if _, err := tpls.Create(ctx, project, map[string]string{"name": n, "location": "Global", "machineType": map[string]string{"tpl": "e2-small", "tpl-2": "e2-medium"}[n]}); err != nil {
			t.Fatal(err)
		}
	}
	groups := computeGroupsProvider{s}
	for _, g := range []string{"auto", "sel"} {
		if _, err := groups.Create(ctx, project, map[string]string{"kind": groupManaged, "name": g, "template": "global/tpl", "locationType": groupSingleZone,
			"zone": "us-central1-a", "autoscaling": autoscaleOff, "size": "3"}); err != nil {
			t.Fatal(err)
		}
	}
	settle(t, s, project)
	path := []string{"us-central1-a", "auto"}
	up := c2139bAction(groups.DetailActions(ctx, project, path), "updatevms")
	if got := vpcFieldNames(up.Fields); got != "template,canaryTemplate,canarySize,updateType,minimalAction,mostDisruptiveAllowedAction,maxSurge,maxUnavailable,replacementMethod" {
		t.Fatalf("Update VMs fields = %s", got)
	}
	if f := vpcField(up.Fields, "updateType"); f.Default != "Selective" || strings.Join(f.Options, ",") != "Automatic,Selective" {
		t.Fatalf("Update type = %+v", f)
	}
	if f := vpcField(up.Fields, "maxSurge"); f.Default != "1" || f.VisibleWhen["updateType"] != "Automatic" {
		t.Fatalf("Maximum surge = %+v", f)
	}
	if err := groups.ActAt(ctx, project, path, "updatevms", map[string]string{"template": "global/tpl-2", "updateType": "Automatic",
		"replacementMethod": "Recreate", "maxSurge": "1", "maxUnavailable": "1"}); err == nil || !strings.Contains(err.Error(), "maxSurge must be 0") {
		t.Fatalf("Recreate with a surge: %v", err)
	}
	if err := groups.ActAt(ctx, project, path, "updatevms", map[string]string{"template": "global/tpl-2", "updateType": "Automatic",
		"minimalAction": "REPLACE", "mostDisruptiveAllowedAction": "REPLACE", "maxSurge": "1", "maxUnavailable": "0", "replacementMethod": "Substitute"}); err != nil {
		t.Fatal(err)
	}
	mgr, _ := s.call("GET", project, "zones/us-central1-a/instanceGroupManagers/auto", nil, nil)
	pol, _ := mgr["updatePolicy"].(map[string]any)
	if vpcStr(pol, "type") != "PROACTIVE" || !strings.HasSuffix(vpcStr(mgr, "instanceTemplate"), "/tpl-2") {
		t.Fatalf("auto after Update VMs = %v", mgr)
	}
	byName, insts, err := groups.managedVMs(project, "zones/us-central1-a/instanceGroupManagers/auto")
	if err != nil || len(insts) != 4 {
		t.Fatalf("Automatic with Maximum surge 1 makes one VM first: %v %v", byName, err)
	}
	// The form shows the group's policy back.
	up = c2139bAction(groups.DetailActions(ctx, project, path), "updatevms")
	if vpcField(up.Fields, "updateType").Default != "Automatic" || vpcField(up.Fields, "maxUnavailable").Default != "0" {
		t.Errorf("Update VMs after an Automatic update = %+v", up.Fields)
	}

	// Selective: nothing changes until the checked VMs' Update.
	sel := []string{"us-central1-a", "sel"}
	if err := groups.ActAt(ctx, project, sel, "updatevms", map[string]string{"template": "global/tpl-2", "updateType": "Selective"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	d, _ := groups.Detail(ctx, project, sel)
	vms := d.Sections[0].Listing
	for _, it := range vms.Items {
		if it.Fields["Template"] != "tpl" {
			t.Fatalf("Selective changed %s: %+v", it.Name, it.Fields)
		}
	}
	one := vms.Items[0].Name
	if err := groups.ActAt(ctx, project, sel, "applyupdates", map[string]string{groupVMsField: one, "minimalAction": "NONE"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	settle(t, s, project)
	d, _ = groups.Detail(ctx, project, sel)
	changed := 0
	for _, it := range d.Sections[0].Listing.Items {
		if it.Fields["Template"] == "tpl-2" {
			changed++
			if it.Name != one {
				t.Errorf("the update reached %s, not the checked %s", it.Name, one)
			}
		}
	}
	if changed != 1 {
		t.Errorf("after Update on %s: %+v", one, d.Sections[0].Listing.Items)
	}
}
