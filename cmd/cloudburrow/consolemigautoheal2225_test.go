package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Autohealing in the console (#2225): Create instance group's Autohealing
// section (Health check, Initial delay) makes a group with the policy; its
// page shows it under Autohealing, a Health check status column, and Edit
// autohealing changes and clears it.
func TestConsoleMIGAutohealing2225(t *testing.T) {
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
	const project = "c2225"
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
	call("POST", "regions/us-central1/healthChecks", map[string]any{"name": "rhc", "type": "TCP", "tcpHealthCheck": map[string]any{"port": 80.0}})

	groups := computeGroupsProvider{s}
	var hcField, delayField *console.Field
	fields := groups.CreateFormFor(ctx, project)
	for i := range fields {
		switch fields[i].Name {
		case "healthCheck":
			hcField = &fields[i]
		case "initialDelay":
			delayField = &fields[i]
		}
	}
	if hcField == nil || delayField == nil || hcField.Section != "Autohealing" ||
		strings.Join(hcField.Options, ",") != "No health check,global/hc,us-central1/rhc" || hcField.Default != "No health check" ||
		delayField.Default != "300" || delayField.ShowWhen != "healthCheck=global/hc|us-central1/rhc" {
		t.Fatalf("Autohealing fields: %+v %+v", hcField, delayField)
	}
	if _, err := groups.Create(ctx, project, map[string]string{"kind": groupManaged, "name": "g", "template": "global/tpl",
		"locationType": groupSingleZone, "zone": "us-central1-a", "autoscaling": autoscaleOff, "size": "1",
		"healthCheck": "global/hc", "initialDelay": "120"}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.Create(ctx, project, map[string]string{"kind": groupManaged, "name": "bad", "template": "global/tpl",
		"autoscaling": autoscaleOff, "size": "0", "healthCheck": "global/hc", "initialDelay": "3601"}); err == nil {
		t.Fatal("an initial delay of 3601 was taken")
	}
	settle(t, s, project)
	detail := func() console.Detail {
		d, err := groups.Detail(ctx, project, []string{"us-central1-a", "g"})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	props := func(d console.Detail) map[string]string {
		out := map[string]string{}
		for _, sec := range d.Sections {
			for _, g := range sec.Groups {
				if g.Heading == "Autohealing" {
					for _, p := range g.Properties {
						out[p.Label] = p.Value
					}
				}
			}
		}
		return out
	}
	d := detail()
	if p := props(d); p["Health check"] != "global/hc" || p["Initial delay"] != "120 seconds" {
		t.Fatalf("Autohealing details = %v", p)
	}
	vms := d.Sections[0].Listing
	if strings.Join(vms.Columns, ",") != "Zone,Status,Current action,Health check status,Template,Internal IP,Last error" || len(vms.Items) != 1 {
		t.Fatalf("VMs = %+v", vms)
	}
	// No VM answers the TCP probe here, and no firewall rule lets the
	// probes in: Unknown until a threshold is reached, then Timeout.
	if h := vms.Items[0].Fields["Health check status"]; h != "Unknown" && h != "Timeout" {
		t.Fatalf("Health check status = %q", h)
	}
	var edit *console.Action
	for _, a := range groups.DetailActions(ctx, project, []string{"us-central1-a", "g"}) {
		if a.ID == "editautohealing" {
			edit = &a
		}
	}
	if edit == nil || edit.Label != "Edit autohealing" || edit.Fields[0].Default != "global/hc" || edit.Fields[1].Default != "120" {
		t.Fatalf("Edit autohealing = %+v", edit)
	}
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "g"}, "editautohealing", map[string]string{"healthCheck": "us-central1/rhc", "initialDelay": "0"}); err != nil {
		t.Fatal(err)
	}
	if p := props(detail()); p["Health check"] != "us-central1/rhc" || p["Initial delay"] != "0 seconds" {
		t.Fatalf("edited = %v", p)
	}
	if err := groups.ActAt(ctx, project, []string{"us-central1-a", "g"}, "editautohealing", map[string]string{"healthCheck": noHealthCheck}); err != nil {
		t.Fatal(err)
	}
	if p := props(detail()); p["Health check"] != noHealthCheck {
		t.Fatalf("cleared = %v", p)
	}
	if h := detail().Sections[0].Listing.Items[0].Fields["Health check status"]; h != "—" {
		t.Fatalf("Health check status without autohealing = %q", h)
	}
}
