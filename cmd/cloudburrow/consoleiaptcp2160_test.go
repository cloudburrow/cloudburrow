package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/iap"
)

// TestConsoleIAPTCP2160: Identity-Aware Proxy's SSH and TCP resources
// lists the project's VM instances and destination groups; Create
// destination group, Edit and Delete go through IAP's API; a row's
// Principals grant IAP-Secured Tunnel User on its tunnel resource.
func TestConsoleIAPTCP2160(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	ia, err := iap.New(iap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rest := iap.NewRESTHandler(ia)
	screen.svc.iap = ia
	screen.svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if iap.Handles(r.URL.Path) {
			rest.ServeHTTP(w, r)
			return
		}
		screen.svc.api.ServeHTTP(w, r)
	})
	const project = "iap-tcp"
	if _, err := screen.call("POST", project, "zones/us-central1-a/instances", nil, map[string]any{"name": "bastion",
		"machineType":       "zones/us-central1-a/machineTypes/e2-small",
		"disks":             []any{map[string]any{"boot": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
		"networkInterfaces": []any{map[string]any{"network": "global/networks/default"}}}); err != nil {
		t.Fatal(err)
	}
	p := iapTCPProvider{screen}
	if _, f := p.CreateForm(); len(f) != 4 || f[0].Name != "name" || f[1].Name != "region" || f[2].Label != "IP address" || f[3].Label != "FQDNs" {
		t.Fatalf("Create destination group's fields = %+v", f)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "lab", "region": "us-central1", "cidrs": "10.1.2.0/24, 172.16.0.0/12", "fqdns": "*.corp.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "empty", "region": "us-central1"}); err == nil {
		t.Fatal("a destination group with no destinations was created")
	}
	l, err := p.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	vm, g := vpcRow(l, "bastion"), vpcRow(l, "lab")
	if vm == nil || vm.Fields["Type"] != "VM instance" || len(vm.Info) != 1 || !vm.Info[0].Permissions {
		t.Fatalf("the instance row = %+v", vm)
	}
	if g == nil || g.Fields["Location"] != "us-central1" || g.Fields["Destinations"] != "10.1.2.0/24, 172.16.0.0/12, *.corp.example" || len(g.Actions) != 2 {
		t.Fatalf("the group row = %+v", g)
	}
	if err := p.ActAt(ctx, project, g.Target, "edit", map[string]string{"cidrs": "10.9.0.0/16", "fqdns": ""}); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Detail(ctx, project, g.Target)
	if d.Title != "lab" || d.Sections[0].Groups[0].Properties[2].Value != "10.9.0.0/16" {
		t.Fatalf("the group's page = %+v", d)
	}
	pol, err := p.GetPolicy(ctx, project, vm.Target)
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/iap.tunnelResourceAccessor", Members: []string{"user:ana@example.com"},
		Conditional: &console.Condition{Title: "ssh", Expression: "destination.port == 22"}})
	if err := p.SetPolicy(ctx, project, vm.Target, pol); err != nil {
		t.Fatal(err)
	}
	stored := ia.Policy("projects/" + project + "/iap_tunnel/zones/us-central1-a/instances/bastion")
	if stored == nil {
		t.Fatal("no policy on the instance's tunnel resource")
	}
	if got, _ := p.GetPolicy(ctx, project, vm.Target); len(got.Bindings) != 1 || got.Bindings[0].Conditional == nil {
		t.Fatalf("the policy read back = %+v", got)
	}
	if p.PolicyResource(ctx, project, g.Target) != "//iap.googleapis.com/projects/iap-tcp/iap_tunnel/locations/us-central1/destGroups/lab" {
		t.Fatal(p.PolicyResource(ctx, project, g.Target))
	}
	if err := p.ActAt(ctx, project, g.Target, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, project); vpcRow(l, "lab") != nil {
		t.Fatal("the deleted group is listed")
	}
}
