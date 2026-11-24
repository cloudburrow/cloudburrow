package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

func netsecFixture2160b(t *testing.T) (computeScreen, string, *certificatemanager.Server) {
	t.Helper()
	cm, _ := certificatemanager.New(certificatemanager.Options{})
	ns, _ := networksecurity.New(networksecurity.Options{})
	api0, err := compute.New(compute.Options{VMs: consoleFakeVMs{}, NetSec: computeNetSec{ns: ns, cm: cm}})
	if err != nil {
		t.Fatal(err)
	}
	screen, project := computeScreen{svc: &computeService{api: api0}}, "lb2-console"
	useNetSec(screen.svc.api, ns, cm, nil)
	nsrest, cmrest, api := networksecurity.NewRESTHandler(ns), certificatemanager.NewRESTHandler(cm), screen.svc.api
	screen.svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case ns.Handles(r):
			nsrest.ServeHTTP(w, r)
		case cm.Handles(r):
			cmrest.ServeHTTP(w, r)
		default:
			api.ServeHTTP(w, r)
		}
	})
	return screen, project, cm
}

// TestConsoleAddressGroups2160: the Address groups page creates a group,
// lists it with its type, purpose, capacity and items, shows its In use
// by as POLICY(rule priority:N) once a Cloud Armor rule matches it, edits
// its items, sets the Cloud Armor tier, and deletes it.
func TestConsoleAddressGroups2160(t *testing.T) {
	ctx := context.Background()
	screen, project, _ := netsecFixture2160b(t)
	p := netsecAddressGroupsProvider{ccmScreen{screen}}
	if _, err := p.Create(ctx, project, map[string]string{"name": "bad", "location": "global", "type": "IPV4", "purposeArmor": "true",
		"capacity": "10", "items": "198.51.100.0/24\n203.0.113.7"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("list: %+v %v", l, err)
	}
	f := l.Items[0].Fields
	if f["Type"] != "IPv4" || f["Purpose"] != "Cloud Armor" || f["Capacity"] != "10" || f["Items"] != "2" || f["In use by"] != "—" {
		t.Fatalf("row: %+v", f)
	}
	acts := p.ListActions(ctx, project)
	if len(acts) != 1 || acts[0].Fields[0].Default != "CA_STANDARD" {
		t.Fatalf("list actions: %+v", acts)
	}
	if err := p.ActAt(ctx, project, nil, "armor-tier", map[string]string{"tier": "CA_ENTERPRISE_PAYGO"}); err != nil {
		t.Fatal(err)
	}
	if acts = p.ListActions(ctx, project); acts[0].Fields[0].Default != "CA_ENTERPRISE_PAYGO" {
		t.Fatalf("after the tier: %+v", acts[0].Fields[0])
	}
	if _, err := screen.call("POST", project, "global/securityPolicies", nil, map[string]any{"name": "sp", "rules": []any{map[string]any{
		"priority": 100, "action": "deny(403)", "match": map[string]any{"expr": map[string]any{"expression": "evaluateAddressGroup('bad', origin.ip)"}}}}}); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Detail(ctx, project, []string{"global", "bad"})
	if d.Unavailable != "" || d.Edit == nil {
		t.Fatalf("detail: %+v", d)
	}
	found := false
	for _, pr := range d.Sections[0].Groups[0].Properties {
		found = found || pr.Label == "In use by" && pr.Value == "sp(rule priority:100)"
	}
	if !found {
		t.Fatalf("In use by: %+v", d.Sections[0].Groups[0].Properties)
	}
	if err := p.Edit(ctx, project, []string{"global", "bad"}, map[string]string{"description": "edited", "items": "198.51.100.0/24\n127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if l, _ = p.List(ctx, project); l.Items[0].Fields["Items"] != "2" {
		t.Fatalf("after edit: %+v", l.Items[0].Fields)
	}
	if d, _ = p.Detail(ctx, project, []string{"global", "bad"}); !strings.Contains(d.Edit.Fields[1].Default, "127.0.0.1") || d.Edit.Fields[0].Default != "edited" {
		t.Fatalf("after edit: %+v", d.Edit.Fields)
	}
	if err := p.ActAt(ctx, project, []string{"global", "bad"}, "delete", nil); err == nil {
		t.Fatal("a referenced group was deleted")
	}
	if _, err := screen.call("DELETE", project, "global/securityPolicies/sp", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{"global", "bad"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ = p.List(ctx, project); len(l.Items) != 0 {
		t.Fatalf("after delete: %+v", l.Items)
	}
}

// TestConsoleBackendAuthentication2160: the Backend Authentication tab
// creates a config of the public roots of trust, lists and shows it,
// edits it and deletes it; one without a trust config or the public roots
// is refused; the load balancer Edit page's Backend authentication
// section sets an HTTPS backend service's tlsSettings.
func TestConsoleBackendAuthentication2160(t *testing.T) {
	ctx := context.Background()
	screen, project, _ := netsecFixture2160b(t)
	p := netsecBackendAuthProvider{ccmScreen{screen}}
	if _, err := p.Create(ctx, project, map[string]string{"name": "none", "location": "global"}); err == nil {
		t.Fatal("a config without a trust config or the public roots was accepted")
	}
	if _, err := p.Create(ctx, project, map[string]string{"name": "public", "location": "global", "publicRoots": "true", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Public roots of trust"] != "Enabled" || l.Items[0].Fields["Trust config"] != "—" {
		t.Fatalf("list: %+v %v", l.Items, err)
	}
	d, _ := p.Detail(ctx, project, []string{"global", "public"})
	if d.Unavailable != "" || d.Edit == nil || d.Edit.Fields[1].Default != "true" {
		t.Fatalf("detail: %+v", d)
	}
	if err := p.Edit(ctx, project, []string{"global", "public"}, map[string]string{"description": "changed", "publicRoots": "true"}); err != nil {
		t.Fatal(err)
	}
	if d, _ = p.Detail(ctx, project, []string{"global", "public"}); d.Edit.Fields[0].Default != "changed" {
		t.Fatalf("after edit: %+v", d.Edit.Fields)
	}
	// The Backend authentication section of an HTTPS backend service.
	if _, err := screen.call("POST", project, "global/backendServices", nil, map[string]any{"name": "secure", "protocol": "HTTPS",
		"loadBalancingScheme": "EXTERNAL_MANAGED"}); err != nil {
		t.Fatal(err)
	}
	fields := lbBackendAuthFields(computeLBProvider{screen}.ccmBackendAuthNames(project, "global"), map[string]any{})
	if len(fields) != 4 || fields[0].Default != "false" || len(fields[3].Options) != 2 || fields[3].Options[1] != "public" {
		t.Fatalf("fields: %+v", fields)
	}
	ts := lbTLSSettings(project, "global", map[string]string{"backendAuth": "true", "tlsSni": "b.example", "tlsSans": "b.example, spiffe://x/y", "tlsAuthConfig": "public"})
	if _, err := screen.call("PATCH", project, "global/backendServices/secure", nil, map[string]any{"tlsSettings": ts}); err != nil {
		t.Fatal(err)
	}
	if l, _ = p.List(ctx, project); l.Items[0].Fields["In use by"] != "secure" {
		t.Fatalf("In use by: %+v", l.Items[0].Fields)
	}
	if err := p.ActAt(ctx, project, []string{"global", "public"}, "delete", nil); err == nil {
		t.Fatal("a config in use was deleted")
	}
	if _, err := screen.call("PATCH", project, "global/backendServices/secure", nil, map[string]any{"tlsSettings": lbTLSSettings(project, "global", map[string]string{})}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{"global", "public"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
}

// TestConsoleOrganizationAddressGroups2160: with the organization chosen
// in the resource selector, the Address groups page creates, lists,
// shows, edits and deletes the organization's groups
// (organizations/N/locations/L/addressGroups), which the project's
// listing does not show; a folder has none.
func TestConsoleOrganizationAddressGroups2160(t *testing.T) {
	screen, project, _ := netsecFixture2160b(t)
	p := netsecAddressGroupsProvider{ccmScreen{screen}}
	org := console.WithScopeValue(context.Background(), scopeOrgParam, strings.TrimPrefix(resourcemanager.OrganizationName, "organizations/"))
	if _, err := p.Create(org, project, map[string]string{"name": "org-bad", "location": "global", "type": "IPV4", "purposeArmor": "true",
		"capacity": "10", "items": "198.51.100.0/24"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(org, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "org-bad" || l.Items[0].Fields["Items"] != "1" {
		t.Fatalf("the organization's list: %+v %v", l, err)
	}
	if !strings.Contains(l.Note, "evaluateOrganizationAddressGroup") || len(p.ListActions(org, project)) != 0 {
		t.Errorf("the organization's page: note %q, actions %+v", l.Note, p.ListActions(org, project))
	}
	if pl, _ := p.List(context.Background(), project); len(pl.Items) != 0 {
		t.Errorf("the project lists the organization's group: %+v", pl.Items)
	}
	if o, err := screen.v1(context.Background(), "GET", "/v1/"+resourcemanager.OrganizationName+"/locations/global/addressGroups/org-bad", nil); err != nil || num2s(o, "capacity") != "10" {
		t.Fatalf("the API's group: %v %v", o, err)
	}
	d, _ := p.Detail(org, project, []string{"global", "org-bad"})
	if d.Unavailable != "" || d.Edit == nil {
		t.Fatalf("detail: %+v", d)
	}
	if err := p.Edit(org, project, []string{"global", "org-bad"}, map[string]string{"items": "198.51.100.0/24\n192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if l, _ = p.List(org, project); l.Items[0].Fields["Items"] != "2" {
		t.Errorf("after the edit: %+v", l.Items[0].Fields)
	}
	// Clone items from a project's group into the organization's.
	if _, err := p.Create(context.Background(), project, map[string]string{"name": "proj-src", "location": "global", "type": "IPV4", "purposeArmor": "true",
		"capacity": "10", "items": "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(org, project, []string{"global", "org-bad"}, "clone-items",
		map[string]string{"source": "projects/" + project + "/locations/global/addressGroups/proj-src"}); err != nil {
		t.Fatal(err)
	}
	if l, _ = p.List(org, project); l.Items[0].Fields["Items"] != "3" {
		t.Errorf("after Clone items: %+v", l.Items[0].Fields)
	}
	if err := p.ActAt(org, project, []string{"global", "org-bad"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ = p.List(org, project); len(l.Items) != 0 {
		t.Errorf("after the delete: %+v", l.Items)
	}
	folder := console.WithScopeValue(context.Background(), scopeFolderParam, "123")
	if l, _ = p.List(folder, project); !strings.Contains(l.Prompt, "not a folder") {
		t.Errorf("a folder's page: %+v", l)
	}
}
