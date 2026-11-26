package main

import (
	"context"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Connection draining in the console (#2226): a load balancer's Edit has
// Backend configuration's Connection draining timeout (0 by default, 0 to
// 3600), which Update sets on the backend service; a VM its group deletes
// while a backend service drains shows Deleting and a Health check status
// of Draining on the group's page.
func TestConsoleConnectionDraining2226(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	lb := computeLBProvider{screen}
	d, _ := lb.Detail(ctx, project, []string{"global", "shop"})
	vals := map[string]string{}
	section := ""
	for _, x := range d.Edit.Fields {
		vals[x.Name] = x.Default
		if x.Name == "drainingTimeoutSec" {
			section = x.Section
			if x.Label != "Connection draining timeout" {
				t.Fatalf("the field's label = %q", x.Label)
			}
		}
	}
	if section != "Backend configuration" || vals["drainingTimeoutSec"] != "0" {
		t.Fatalf("Connection draining timeout: section %q, value %q", section, vals["drainingTimeoutSec"])
	}
	vals["drainingTimeoutSec"] = "3601"
	if err := lb.Edit(ctx, project, []string{"global", "shop"}, vals); err == nil {
		t.Fatal("a timeout of 3601 was taken")
	}
	vals["drainingTimeoutSec"] = "60"
	if err := lb.Edit(ctx, project, []string{"global", "shop"}, vals); err != nil {
		t.Fatal(err)
	}
	bs, _ := screen.call("GET", project, "global/backendServices/shop-backend-service", nil, nil)
	if vmMap(bs, "connectionDraining")["drainingTimeoutSec"] != float64(60) {
		t.Fatalf("the backend service after Update = %v", bs["connectionDraining"])
	}
}

func TestConsoleMIGDraining2226(t *testing.T) {
	ctx := context.Background()
	api, err := compute.New(compute.Options{VMs: consoleFakeVMs{}, UpdateEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Stop(context.Background()) })
	s := computeScreen{svc: &computeService{api: api}}
	const project = "c2226"
	call := func(method, path string, body map[string]any) {
		t.Helper()
		if _, err := s.call(method, project, path, nil, body); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		settle(t, s, project)
	}
	call("POST", "global/instanceTemplates", map[string]any{"name": "tpl", "properties": map[string]any{"machineType": "e2-small",
		"disks":             []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/default"}}}})
	call("POST", "global/healthChecks", map[string]any{"name": "hc", "type": "TCP", "tcpHealthCheck": map[string]any{"port": 80.0}})
	call("POST", "zones/us-central1-a/instanceGroupManagers", map[string]any{"name": "g", "instanceTemplate": "global/instanceTemplates/tpl",
		"targetSize": 1.0, "namedPorts": []any{map[string]any{"name": "http", "port": 80.0}},
		"autoHealingPolicies": []any{map[string]any{"healthCheck": "global/healthChecks/hc", "initialDelaySec": 3600.0}}})
	call("POST", "global/backendServices", map[string]any{"name": "bs", "loadBalancingScheme": "EXTERNAL_MANAGED", "portName": "http",
		"healthChecks": []any{"global/healthChecks/hc"}, "connectionDraining": map[string]any{"drainingTimeoutSec": 3600.0},
		"backends": []any{map[string]any{"group": "zones/us-central1-a/instanceGroups/g"}}})
	groups := computeGroupsProvider{s}
	row := func() map[string]string {
		d, err := groups.Detail(ctx, project, []string{"us-central1-a", "g"})
		if err != nil || len(d.Sections[0].Listing.Items) != 1 {
			t.Fatalf("the group's VMs: %+v, %v", d.Sections, err)
		}
		return d.Sections[0].Listing.Items[0].Fields
	}
	l, err := s.call("POST", project, "zones/us-central1-a/instanceGroupManagers/g/listManagedInstances", nil, nil)
	if err != nil || len(vmMaps(l, "managedInstances")) != 1 {
		t.Fatalf("listManagedInstances: %v, %v", l, err)
	}
	vm := vpcStr(vmMaps(l, "managedInstances")[0], "instance")
	if _, err := s.call("POST", project, "zones/us-central1-a/instanceGroupManagers/g/deleteInstances", nil, map[string]any{"instances": []any{vm}}); err != nil {
		t.Fatal(err)
	}
	if r := row(); r["Health check status"] != "Draining" || r["Current action"] != migActionLabel("DELETING") || r["Status"] != "RUNNING" {
		t.Fatalf("the draining VM's row = %v", r)
	}
}
