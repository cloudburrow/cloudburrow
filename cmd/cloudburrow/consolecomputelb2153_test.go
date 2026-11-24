package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleLoadBalancing2153: Create load balancer's steps make an
// external Application Load Balancer in front of an unmanaged instance
// group (its health check, backend service, URL map with a path rule to a
// second backend service, target HTTP proxy and forwarding rule); the Load
// balancers, Backends and Frontends tabs list it; its page shows the
// frontend, routing rules and backends; a proxy Network Load Balancer is
// made too; Delete removes the rules, proxies and URL map and keeps the
// backends; Health checks, Network endpoint groups and Classic
// Certificates create, list and delete; a load balancer's rules are not
// Private Service Connect endpoints.
func TestConsoleLoadBalancing2153(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "lb-console"
	lb := computeLBProvider{screen}
	if _, err := screen.call("POST", project, "zones/us-central1-a/instanceGroups", nil, map[string]any{"name": "web",
		"namedPorts": []any{map[string]any{"name": "http", "port": float64(80)}}}); err != nil {
		t.Fatal(err)
	}
	hcs := computeHealthChecksProvider{screen}
	if _, err := hcs.Create(ctx, project, map[string]string{"name": "other-hc", "protocol": "HTTP", "port": "80", "requestPath": "/healthz",
		"checkIntervalSec": "5", "timeoutSec": "5", "healthyThreshold": "2", "unhealthyThreshold": "2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := screen.call("POST", project, "global/backendServices", nil, map[string]any{"name": "api", "loadBalancingScheme": "EXTERNAL_MANAGED",
		"protocol": "HTTP", "healthChecks": []any{"global/healthChecks/other-hc"}}); err != nil {
		t.Fatal(err)
	}
	fields := lb.CreateFormFor(ctx, project)
	if got := vpcFieldNames(fields); !strings.HasPrefix(got, "lbType,facing,deployment,region,network,name,protocol,port,ipVersion,subnetwork,ipAddress,certificate,domains,backendType,instanceGroup") {
		t.Fatalf("Create load balancer fields = %s", got)
	}
	if f := vpcField(fields, "instanceGroup"); f == nil || f.Type != "select" || strings.Join(f.Options, ",") != "us-central1-a/web" {
		t.Fatalf("instance group field = %+v", f)
	}
	if _, err := lb.Create(ctx, project, map[string]string{"lbType": "application", "facing": "external", "deployment": "global", "name": "shop",
		"protocol": "HTTP", "port": "80", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/web", "portName": "http",
		"balancingMode": "UTILIZATION", "routing": `{"/api/*":"api"}`}); err != nil {
		t.Fatal(err)
	}
	l, err := lb.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	r := vpcRow(l, "shop")
	if r == nil || r.Fields["Load balancer type"] != "Application" || r.Fields["Access type"] != "External" || r.Fields["Protocols"] != "HTTP" ||
		r.Fields["Region"] != "Global" || r.Fields["Backends"] != "2 backend services" {
		t.Fatalf("load balancer row = %+v", r)
	}
	d, _ := lb.Detail(ctx, project, []string{"global", "shop"})
	if d.Unavailable != "" || len(d.Sections) != 4 {
		t.Fatalf("load balancer page = %+v", d)
	}
	front := d.Sections[1].Listing
	if len(front.Items) != 1 || front.Items[0].Fields["IP:Port"] != "127.0.0.1:80" || !strings.HasPrefix(front.Items[0].Fields["Local address"], "127.0.0.1:") {
		t.Fatalf("frontend = %+v", front.Items)
	}
	routes := d.Sections[2].Listing
	if len(routes.Items) < 2 || routes.Items[0].Fields["Paths"] != "/api/*" || routes.Items[0].Fields["Backend"] != "api" {
		t.Fatalf("routing rules = %+v", routes.Items)
	}
	if back := d.Sections[3].Listing; len(back.Items) != 2 {
		t.Fatalf("backends = %+v", back.Items)
	}
	// A proxy Network Load Balancer, regional.
	if _, err := lb.Create(ctx, project, map[string]string{"lbType": "network-proxy", "facing": "external", "deployment": "regional", "region": "us-central1",
		"name": "tcp", "protocol": "TCP", "port": "110", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/web", "portName": "http"}); err == nil ||
		!strings.Contains(err.Error(), "proxy-only subnet") {
		t.Fatalf("a regional proxy load balancer without a proxy-only subnet: %v", err)
	}
	if _, err := lb.Create(ctx, project, map[string]string{"lbType": "network-proxy", "facing": "external", "deployment": "global",
		"name": "tcp", "protocol": "TCP", "port": "110", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/web", "portName": "http"}); err != nil {
		t.Fatal(err)
	}
	if l, _ = lb.List(ctx, project); vpcRow(l, "tcp-backend-service") == nil || vpcRow(l, "tcp-backend-service").Fields["Load balancer type"] != "Network (Proxy)" {
		t.Fatalf("load balancers = %+v", l.Items)
	}
	if fl, _ := (computeLBFrontendsProvider{screen}).List(ctx, project); vpcRow(fl, "shop-forwarding-rule") == nil || vpcRow(fl, "tcp-forwarding-rule") == nil {
		t.Fatalf("frontends = %+v", fl.Items)
	}
	if bl, _ := (computeLBBackendsProvider{screen}).List(ctx, project); vpcRow(bl, "api") == nil || vpcRow(bl, "shop-backend-service") == nil {
		t.Fatalf("backends = %+v", bl.Items)
	}
	if el, _ := (computePSCEndpointsProvider{screen}).List(ctx, project); len(el.Items) != 0 {
		t.Fatalf("load balancers' rules as PSC endpoints: %+v", el.Items)
	}
	if err := lb.ActAt(ctx, project, []string{"global", "shop"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ = lb.List(ctx, project); vpcRow(l, "shop") != nil {
		t.Fatal("the load balancer is still listed")
	}
	if _, err := screen.call("GET", project, "global/backendServices/shop-backend-service", nil, nil); err != nil {
		t.Fatalf("Delete kept no backend service: %v", err)
	}
	if hl, _ := hcs.List(ctx, project); vpcRow(hl, "other-hc") == nil || vpcRow(hl, "shop-health-check") == nil ||
		vpcRow(hl, "other-hc").Fields["Port"] != "80" {
		t.Fatalf("health checks = %+v", hl.Items)
	}

	negs := computeNEGsProvider{screen}
	if _, err := negs.Create(ctx, project, map[string]string{"name": "run-neg", "type": "SERVERLESS", "region": "us-central1", "cloudRunService": "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := negs.Create(ctx, project, map[string]string{"name": "web-neg", "type": "INTERNET_FQDN_PORT", "defaultPort": "443", "endpoint": "example.com"}); err != nil {
		t.Fatal(err)
	}
	nl, _ := negs.List(ctx, project)
	if r := vpcRow(nl, "run-neg"); r == nil || r.Fields["Network endpoint type"] != "Serverless" || r.Fields["Scope"] != "us-central1" {
		t.Fatalf("serverless NEG row = %+v", r)
	}
	if r := vpcRow(nl, "web-neg"); r == nil || r.Fields["Size"] != "1" {
		t.Fatalf("internet NEG row = %+v", r)
	}
	if d, _ := negs.Detail(ctx, project, []string{"global", "global", "web-neg"}); d.Unavailable != "" || len(d.Sections[1].Listing.Items) != 1 {
		t.Fatalf("internet NEG page = %+v", d)
	}
	if err := negs.ActAt(ctx, project, []string{"regions", "us-central1", "run-neg"}, "delete", nil); err != nil {
		t.Fatal(err)
	}

	certs := computeLBCertsProvider{screen}
	if _, err := certs.Create(ctx, project, map[string]string{"name": "shop-cert", "type": "MANAGED", "domains": "shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	cl, _ := certs.List(ctx, project)
	if r := vpcRow(cl, "shop-cert"); r == nil || r.Fields["Type"] != "Google-managed" || r.Status != "Provisioning" || r.Fields["Domains"] != "shop.example.com" {
		t.Fatalf("certificate row = %+v", r)
	}
	if err := certs.ActAt(ctx, project, []string{"global", "shop-cert"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
}
