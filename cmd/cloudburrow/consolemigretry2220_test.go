package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// A managed group's page while it fails to make a VM (#2220): the VM
// listed with its current action (Creating) and last error, the group's
// Status "Not stable (Creating 1)", and the Errors tab listing what
// listErrors holds; once the network is back, the VM made and the group
// Stable.
func TestConsoleMIGCreationRetries2220(t *testing.T) {
	ctx := context.Background()
	api, err := compute.New(compute.Options{VMs: consoleFakeVMs{}, UpdateEvery: 20 * time.Millisecond, MIGRetryBase: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Stop(context.Background()) })
	s := computeScreen{svc: &computeService{api: api}}
	const project = "c2220"
	call := func(method, path string, body map[string]any) {
		t.Helper()
		if _, err := s.call(method, project, path, nil, body); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		settle(t, s, project)
	}
	call("POST", "global/networks", map[string]any{"name": "nw", "autoCreateSubnetworks": true})
	call("POST", "global/instanceTemplates", map[string]any{"name": "tpl", "properties": map[string]any{"machineType": "e2-small",
		"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/nw"}}}})
	call("DELETE", "global/networks/nw", nil)
	call("POST", "zones/us-central1-a/instanceGroupManagers", map[string]any{"name": "g", "baseInstanceName": "g",
		"instanceTemplate": "global/instanceTemplates/tpl", "targetSize": 1.0})

	groups := computeGroupsProvider{s}
	section := func(d console.Detail, id string) *console.Section {
		for i := range d.Sections {
			if d.Sections[i].ID == id {
				return &d.Sections[i]
			}
		}
		return nil
	}
	prop := func(d console.Detail, label string) string {
		for _, g := range section(d, "details").Groups {
			for _, p := range g.Properties {
				if p.Label == label {
					return p.Value
				}
			}
		}
		return ""
	}
	var d console.Detail
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, err = groups.Detail(ctx, project, []string{"us-central1-a", "g"})
		if err != nil {
			t.Fatal(err)
		}
		if e := section(d, "errors"); e != nil && len(e.Listing.Items) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no errors listed: %+v", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
	vms := section(d, "overview").Listing
	if strings.Join(vms.Columns, ",") != "Zone,Status,Current action,Template,Internal IP,Last error" || len(vms.Items) != 1 {
		t.Fatalf("VMs = %+v", vms)
	}
	vm := vms.Items[0]
	if !strings.HasPrefix(vm.Name, "g-") || vm.Fields["Current action"] != "Creating" || vm.Fields["Status"] != "—" ||
		!strings.HasPrefix(vm.Fields["Last error"], "RESOURCE_NOT_FOUND: ") || vm.Fields["Template"] != "tpl" {
		t.Fatalf("pending VM = %+v", vm)
	}
	if got := prop(d, "Status"); got != "Not stable (Creating 1)" {
		t.Fatalf("Status = %q", got)
	}
	errs := section(d, "errors").Listing
	e := errs.Items[0]
	if errs.NameColumn != "Time" || e.Fields["Instance"] != vm.Name || e.Fields["Action"] != "Creating" || e.Fields["Error code"] != "RESOURCE_NOT_FOUND" ||
		!strings.Contains(e.Fields["Error message"], "creation failed") {
		t.Fatalf("errors = %+v", errs)
	}

	call("POST", "global/networks", map[string]any{"name": "nw", "autoCreateSubnetworks": true})
	deadline = time.Now().Add(10 * time.Second)
	for {
		d, err = groups.Detail(ctx, project, []string{"us-central1-a", "g"})
		if err != nil {
			t.Fatal(err)
		}
		if prop(d, "Status") == "Stable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never stable: %q", prop(d, "Status"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	vm = section(d, "overview").Listing.Items[0]
	if vm.Fields["Status"] != "RUNNING" || vm.Fields["Current action"] != "None" || vm.Fields["Last error"] != "—" {
		t.Fatalf("made VM = %+v", vm)
	}
}
