package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
	"github.com/cloudburrow/cloudburrow/internal/service/networkservices"
)

// TestConsoleSecureWebProxy2160: the URL lists, TLS inspection policies,
// SWP Policies (with Add rule, Edit rule, Delete rule) and Web Proxies
// pages create, list, show, edit and delete through the APIs, and a web
// proxy lists where it listens.
func TestConsoleSecureWebProxy2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	cm, _ := certificatemanager.New(certificatemanager.Options{})
	ns, _ := networksecurity.New(networksecurity.Options{})
	nsv, _ := networkservices.New(networkservices.Options{})
	useNetSec(screen.svc.api, ns, cm, nil)
	screen.svc.useNetSvc(screen.svc.api, ns, nsv, cm)
	nsv.Start()
	t.Cleanup(nsv.Stop)
	nsrest, nsvrest, api := networksecurity.NewRESTHandler(ns), networkservices.NewRESTHandler(nsv), screen.svc.api
	screen.svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case ns.Handles(r):
			nsrest.ServeHTTP(w, r)
		case nsv.Handles(r):
			nsvrest.ServeHTTP(w, r)
		default:
			api.ServeHTTP(w, r)
		}
	})
	s := swpScreen{ccmScreen{screen}}
	region := "us-central1"

	ul := swpURLListsProvider{s}
	if _, err := ul.Create(ctx, project, map[string]string{"name": "allowed", "region": region, "values": "example.com\n*.example.org"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := ul.List(ctx, project); len(l.Items) != 1 || l.Items[0].Fields["Entries"] != "2" {
		t.Fatalf("URL lists: %+v", l.Items)
	}
	if err := ul.Edit(ctx, project, []string{region, "allowed"}, map[string]string{"values": "example.com"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := ul.Detail(ctx, project, []string{region, "allowed"}); d.Unavailable != "" || d.Sections[1].Text != "example.com" {
		t.Fatalf("URL list detail: %+v", d)
	}

	tp := swpTLSProvider{s}
	if _, err := tp.Create(ctx, project, map[string]string{"name": "inspect", "region": region, "caPool": "pool"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := tp.List(ctx, project); len(l.Items) != 1 || l.Items[0].Fields["CA pool"] != "pool" {
		t.Fatalf("TLS inspection policies: %+v", l.Items)
	}

	pp := swpPoliciesProvider{s}
	if _, err := pp.Create(ctx, project, map[string]string{"name": "pol", "region": region, "description": "basic"}); err != nil {
		t.Fatal(err)
	}
	path := []string{region, "pol"}
	if err := pp.ActAt(ctx, project, path, "addrule", map[string]string{"name": "allow-example", "priority": "1", "action": "ALLOW", "enabled": "true",
		"sessionMatcher": "host() == 'www.example.com'"}); err != nil {
		t.Fatal(err)
	}
	if err := pp.ActAt(ctx, project, path, "addrule", map[string]string{"name": "bad", "priority": "2", "sessionMatcher": "request.method == 'GET'"}); err == nil {
		t.Error("Add rule accepted a sessionMatcher reading the request")
	}
	d, _ := pp.Detail(ctx, project, path)
	if d.Unavailable != "" || len(d.Sections) != 2 || len(d.Sections[1].Listing.Items) != 1 || d.Sections[1].Listing.Items[0].Fields["Action"] != "Allow" {
		t.Fatalf("policy detail: %+v", d)
	}
	rule := []string{region, "pol", "rule", "allow-example"}
	if err := pp.ActAt(ctx, project, rule, "editrule", map[string]string{"priority": "5", "action": "DENY", "enabled": "false", "sessionMatcher": "true"}); err != nil {
		t.Fatal(err)
	}
	d, _ = pp.Detail(ctx, project, path)
	if f := d.Sections[1].Listing.Items[0].Fields; f["Action"] != "Deny" || f["Status"] != "Disabled" || f["Priority"] != "5" {
		t.Fatalf("after Edit rule: %+v", f)
	}

	wp := swpProxiesProvider{s}
	_, fields := wp.CreateForm()
	labels := []string{}
	for _, f := range fields {
		labels = append(labels, f.Label)
	}
	if got := strings.Join(labels, ","); got != "Name,Description,Routing mode,Region,Network,Subnetwork,Web proxy IP address,Ports,Certificate,Associated policy" {
		t.Errorf("Create a secure web proxy's fields: %s", got)
	}
	if _, err := wp.Create(ctx, project, map[string]string{"name": "swp", "region": region, "network": "default", "subnetwork": "default", "ports": "443", "policy": "pol"}); err == nil ||
		!strings.Contains(err.Error(), "proxy-only subnet") {
		t.Fatalf("Create web proxy without a proxy-only subnet: %v", err)
	}
	if _, err := screen.call("POST", project, "regions/"+region+"/subnetworks", nil, map[string]any{"name": "proxy-only", "network": "global/networks/default",
		"ipCidrRange": "10.10.0.0/23", "purpose": "REGIONAL_MANAGED_PROXY", "role": "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wp.Create(ctx, project, map[string]string{"name": "swp", "region": region, "network": "default", "subnetwork": "default", "ports": "443", "policy": "pol"}); err != nil {
		t.Fatalf("Create web proxy: %v", err)
	}
	l, _ := wp.List(ctx, project)
	if len(l.Items) != 1 || !strings.HasPrefix(l.Items[0].Fields["Listening at"], "443=127.0.0.1:") || l.Items[0].Fields["Associated policy"] != "pol" {
		t.Fatalf("web proxies: %+v", l.Items)
	}
	if d, _ := wp.Detail(ctx, project, []string{region, "swp"}); d.Unavailable != "" || d.Edit == nil {
		t.Fatalf("web proxy detail: %+v", d)
	}
	if err := pp.ActAt(ctx, project, path, "delete", nil); err == nil {
		t.Error("a policy a web proxy names was deleted")
	}
	if err := wp.ActAt(ctx, project, []string{region, "swp"}, "delete", nil); err != nil {
		t.Fatal(err)
	}

	if err := pp.ActAt(ctx, project, rule, "deleterule", nil); err != nil {
		t.Fatal(err)
	}
	for _, del := range []func() error{
		func() error { return pp.ActAt(ctx, project, path, "delete", nil) },
		func() error { return tp.ActAt(ctx, project, []string{region, "inspect"}, "delete", nil) },
		func() error { return ul.ActAt(ctx, project, []string{region, "allowed"}, "delete", nil) },
	} {
		if err := del(); err != nil {
			t.Fatal(err)
		}
	}
}
