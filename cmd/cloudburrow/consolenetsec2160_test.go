package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
)

// TestConsoleNetworkSecurityPolicies2160: the authorization policies,
// service mesh authorization policies and client TLS policies pages
// create from their JSON, list, show, edit and delete; a field the kind
// lacks and text that is not JSON are refused.
func TestConsoleNetworkSecurityPolicies2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	cm, _ := certificatemanager.New(certificatemanager.Options{})
	ns, _ := networksecurity.New(networksecurity.Options{})
	useNetSec(screen.svc.api, ns, cm, nil)
	nsrest, api := networksecurity.NewRESTHandler(ns), screen.svc.api
	screen.svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ns.Handles(r) {
			nsrest.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
	for _, tc := range []struct{ id, spec, column, want string }{
		{"netsecurity-clienttls", `{"sni": "backend.example.com"}`, "SNI", "backend.example.com"},
		{"netsecurity-meshauthz", `{"action": "ALLOW", "rules": [{"sources": [{"ipBlocks": ["10.0.0.0/8"]}]}]}`, "Action", "ALLOW"},
	} {
		p := newNetsecProvider(screen, tc.id)
		if _, f := p.CreateForm(); f[len(f)-1].Name != "spec" || f[len(f)-1].Default == "" {
			t.Fatalf("%s: form %+v", tc.id, f)
		}
		if _, err := p.Create(ctx, project, map[string]string{"name": "p1", "location": "global", "spec": "{"}); err == nil {
			t.Fatalf("%s: bad JSON accepted", tc.id)
		}
		if _, err := p.Create(ctx, project, map[string]string{"name": "p1", "location": "global", "spec": `{"nope": 1}`}); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("%s: unknown field: %v", tc.id, err)
		}
		if _, err := p.Create(ctx, project, map[string]string{"name": "p1", "location": "global", "description": "d", "spec": tc.spec}); err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		l, err := p.List(ctx, project)
		if err != nil || len(l.Items) != 1 || l.Items[0].Fields[tc.column] != tc.want || l.Items[0].Fields["Location"] != "Global" {
			t.Fatalf("%s: list %+v %v", tc.id, l.Items, err)
		}
		d, _ := p.Detail(ctx, project, []string{"global", "p1"})
		if d.Unavailable != "" || d.Edit == nil || !strings.Contains(d.Edit.Fields[1].Default, tc.want) {
			t.Fatalf("%s: detail %+v", tc.id, d)
		}
		if err := p.Edit(ctx, project, []string{"global", "p1"}, map[string]string{"description": "changed", "spec": d.Edit.Fields[1].Default}); err != nil {
			t.Fatalf("%s: edit %v", tc.id, err)
		}
		if d, _ = p.Detail(ctx, project, []string{"global", "p1"}); d.Edit.Fields[0].Default != "changed" {
			t.Fatalf("%s: after edit %+v", tc.id, d.Edit.Fields[0])
		}
		if err := p.ActAt(ctx, project, []string{"global", "p1"}, "delete", nil); err != nil {
			t.Fatalf("%s: delete %v", tc.id, err)
		}
		if l, _ = p.List(ctx, project); len(l.Items) != 0 {
			t.Fatalf("%s: after delete %+v", tc.id, l.Items)
		}
	}
	// An authorization policy targets the fixture's forwarding rules.
	p := newNetsecProvider(screen, "netsecurity-authz")
	rules, err := screen.call("GET", project, "global/forwardingRules", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := listAny(rules["items"])
	if len(items) == 0 {
		t.Skip("the fixture has no global forwarding rule")
	}
	rule := strAny(items[0].(map[string]any)["selfLink"])
	spec := `{"target": {"loadBalancingScheme": "EXTERNAL_MANAGED", "resources": ["` + relOf(rule) + `"]}, "httpRules": [{"to": {"operations": [{"paths": [{"prefix": "/admin"}]}]}}]}`
	if _, err := p.Create(ctx, project, map[string]string{"name": "deny-admin", "location": "global", "action": "DENY", "spec": spec}); err != nil {
		t.Fatal(err)
	}
	l, _ := p.List(ctx, project)
	if len(l.Items) != 1 || l.Items[0].Fields["Action"] != "DENY" || l.Items[0].Fields["Rules"] != "1" || l.Items[0].Fields["Targets"] == "—" {
		t.Fatalf("authz list %+v", l.Items)
	}
	// CUSTOM with Cloud IAP, from the form's selects; CUSTOM without a
	// provider is refused.
	if _, err := p.Create(ctx, project, map[string]string{"name": "iap", "location": "global", "action": "CUSTOM", "spec": spec}); err == nil {
		t.Fatal("CUSTOM without a provider accepted")
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "iap", "location": "global", "action": "CUSTOM", "customProvider": "cloudIap", "spec": spec}); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Detail(ctx, project, []string{"global", "iap"})
	var prov string
	for _, f := range d.Edit.Fields {
		if f.Name == "customProvider" {
			prov = f.Default
		}
	}
	if prov != "cloudIap" {
		t.Fatalf("the edit form's provider: %+v", d.Edit.Fields)
	}
	if l, _ := p.List(ctx, project); len(l.Items) != 2 || l.Items[1].Fields["Custom provider"] != "Cloud IAP" {
		t.Fatalf("authz list %+v", l.Items)
	}
}
