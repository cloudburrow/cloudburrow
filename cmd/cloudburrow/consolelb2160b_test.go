package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
)

// TestConsoleCloudArmorRound3_2160: a rule's own page by priority
// (getRule) with Edit rule ending in Update (patchRule); the policy's
// labels on its page and in Edit (setLabels); a network edge security
// policy (regional) applied to a target pool and removed; the Network edge
// security services tab (create with the policy, page, Edit, Delete); and
// the Preconfigured WAF rules tab (listPreconfiguredExpressionSets) with a
// rule set's signatures.
func TestConsoleCloudArmorRound3_2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	armor := computeArmorProvider{screen}
	if _, err := armor.Create(ctx, project, map[string]string{"name": "shield", "type": "CLOUD_ARMOR", "scope": "global", "defaultAction": "allow",
		"rule_mode": "basic", "rule_srcIpRanges": "192.0.2.0/24", "rule_action": "deny", "rule_responseCode": "403", "rule_priority": "1000"}); err != nil {
		t.Fatal(err)
	}
	pol := []string{"global", "shield"}
	d, _ := armor.Detail(ctx, project, pol)
	var row *[]string
	for _, r := range d.Sections[1].Listing.Items {
		if r.Fields["Priority"] == "1000" {
			o := r.Opens
			row = &o
		}
	}
	if row == nil || strings.Join(*row, "/") != "global/shield/rule/1000" {
		t.Fatalf("the rule row opens %v", row)
	}
	rd, _ := armor.Detail(ctx, project, *row)
	if rd.Unavailable != "" || rd.Heading != "Rule details" || rd.Edit == nil || rd.Edit.Title != "Edit rule" || rd.Edit.Submit != "Update" {
		t.Fatalf("the rule page = %+v", rd)
	}
	if v := propValue(rd.Sections[0].Groups[0].Properties, "Match"); v != "192.0.2.0/24" {
		t.Fatalf("the rule's match = %q", v)
	}
	vals := map[string]string{}
	for _, f := range rd.Edit.Fields {
		vals[f.Name] = f.Default
	}
	vals["srcIpRanges"], vals["description"] = "198.51.100.0/24", "block the lab"
	if err := armor.Edit(ctx, project, *row, vals); err != nil {
		t.Fatal(err)
	}
	r, _ := screen.call("GET", project, "global/securityPolicies/shield/getRule", map[string][]string{"priority": {"1000"}}, nil)
	if vpcStr(r, "description") != "block the lab" || strings.Join(vpcStrs(vmMap(vmMap(r, "match"), "config"), "srcIpRanges"), ",") != "198.51.100.0/24" || vpcStr(r, "action") != "deny(403)" {
		t.Fatalf("the rule after Update = %v", r)
	}
	// Labels.
	if err := armor.ActAt(ctx, project, pol, "editpolicy", map[string]string{"description": "front door", "labels": `{"env":"prod","team":"web"}`}); err != nil {
		t.Fatal(err)
	}
	o, _ := screen.call("GET", project, "global/securityPolicies/shield", nil, nil)
	if vpcStr(o, "description") != "front door" || vpcStr(vmMap(o, "labels"), "env") != "prod" || vpcStr(vmMap(o, "labels"), "team") != "web" {
		t.Fatalf("the policy after Edit = %v", o)
	}
	d, _ = armor.Detail(ctx, project, pol)
	if v := propValue(d.Sections[0].Groups[0].Properties, "Labels"); v != "env=prod, team=web" {
		t.Fatalf("the Labels property = %q", v)
	}
	// A network edge security policy on a target pool.
	if _, err := armor.Create(ctx, project, map[string]string{"name": "edge-net", "type": "CLOUD_ARMOR_NETWORK", "scope": "global", "defaultAction": "allow"}); err == nil {
		t.Fatal("a global network edge security policy was accepted")
	}
	if _, err := armor.Create(ctx, project, map[string]string{"name": "edge-net", "type": "CLOUD_ARMOR_NETWORK", "scope": "regional", "region": "us-central1", "defaultAction": "allow"}); err != nil {
		t.Fatal(err)
	}
	if _, err := screen.call("POST", project, "regions/us-central1/targetPools", nil, map[string]any{"name": "pool"}); err != nil {
		t.Fatal(err)
	}
	np := []string{"us-central1", "edge-net"}
	if f := vpcField(armor.addRuleAction(true).Fields, "ipProtocols"); f == nil || vpcField(armor.addRuleAction(true).Fields, "expression") != nil {
		t.Fatalf("a network policy's Add rule = %+v", armor.addRuleAction(true).Fields)
	}
	if err := armor.ActAt(ctx, project, np, "addrule", map[string]string{"srcIpRanges": "203.0.113.0/24", "ipProtocols": "udp", "destPorts": "53", "action": "deny", "priority": "100"}); err != nil {
		t.Fatal(err)
	}
	if err := armor.ActAt(ctx, project, []string{"us-central1", "edge-net", "rule", "2147483647"}, "editrule", map[string]string{"action": "deny"}); err != nil {
		t.Fatal(err)
	}
	nr, _ := armor.Detail(ctx, project, []string{"us-central1", "edge-net", "rule", "100"})
	if v := propValue(nr.Sections[0].Groups[0].Properties, "Match"); v != "srcIpRanges: 203.0.113.0/24; ipProtocols: udp; destPorts: 53" {
		t.Fatalf("the network rule's match = %q", v)
	}
	if dr, _ := screen.call("GET", project, "regions/us-central1/securityPolicies/edge-net/getRule", map[string][]string{"priority": {"2147483647"}}, nil); !strings.HasPrefix(vpcStr(dr, "action"), "deny") {
		t.Fatalf("the network policy's default rule = %v", dr)
	}
	o, _ = armor.policy(project, np)
	if a := armor.applyTargetAction(project, o); a.Fields[0].Type != "select" || !hasString(a.Fields[0].Options, "regions/us-central1/targetPools/pool") {
		t.Fatalf("Apply policy to new target = %+v", a.Fields)
	}
	if err := armor.ActAt(ctx, project, np, "applytarget", map[string]string{"target": "regions/us-central1/targetPools/pool"}); err != nil {
		t.Fatal(err)
	}
	d, _ = armor.Detail(ctx, project, np)
	tg := d.Sections[2].Listing
	if len(tg.Items) != 1 || tg.Items[0].Fields["Type"] != "Target pool" {
		t.Fatalf("the network policy's targets = %+v", tg.Items)
	}
	if err := armor.ActAt(ctx, project, tg.Items[0].Target, "removetarget", nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := screen.call("GET", project, "regions/us-central1/targetPools/pool", nil, nil); vpcStr(p, "securityPolicy") != "" {
		t.Fatalf("the pool after Remove = %v", p)
	}
	// Network edge security services.
	nes := armorEdgeServicesProvider{screen}
	if f := vpcField(nes.CreateFormFor(ctx, project), "securityPolicy"); f == nil || f.Type != "select" || !hasString(f.Options, "regions/us-central1/securityPolicies/edge-net") {
		t.Fatalf("the service's policy field = %+v", f)
	}
	if _, err := nes.Create(ctx, project, map[string]string{"name": "ddos", "region": "us-central1", "securityPolicy": "regions/us-central1/securityPolicies/edge-net"}); err != nil {
		t.Fatal(err)
	}
	l, _ := nes.List(ctx, project)
	if rw := vpcRow(l, "ddos"); rw == nil || rw.Fields["Security policy"] != "edge-net" || rw.Fields["Region"] != "us-central1" {
		t.Fatalf("Network edge security services = %+v", l.Items)
	}
	sd, _ := nes.Detail(ctx, project, []string{"us-central1", "ddos"})
	if sd.Edit == nil || sd.Unavailable != "" {
		t.Fatalf("the service's page = %+v", sd)
	}
	if err := nes.Edit(ctx, project, []string{"us-central1", "ddos"}, map[string]string{"description": "advanced network DDoS", "securityPolicy": "regions/us-central1/securityPolicies/edge-net"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := screen.call("GET", project, "regions/us-central1/networkEdgeSecurityServices/ddos", nil, nil); vpcStr(s, "description") != "advanced network DDoS" {
		t.Fatalf("the service after Edit = %v", s)
	}
	if err := nes.ActAt(ctx, project, []string{"us-central1", "ddos"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ = nes.List(ctx, project); len(l.Items) != 0 {
		t.Fatalf("after Delete: %+v", l.Items)
	}
	// Preconfigured WAF rules.
	waf := armorWAFProvider{screen}
	l, _ = waf.List(ctx, project)
	rw := vpcRow(l, "sqli-v33-stable")
	if rw == nil || rw.Fields["Expression"] != "evaluatePreconfiguredWaf('sqli-v33-stable')" || rw.Fields["Signatures"] == "0" {
		t.Fatalf("the WAF rule sets = %+v", l.Items)
	}
	wd, _ := waf.Detail(ctx, project, []string{"sqli-v33-stable"})
	if wd.Unavailable != "" || len(wd.Sections) != 2 || len(wd.Sections[1].Listing.Items) == 0 {
		t.Fatalf("the rule set's page = %+v", wd)
	}
}

// TestConsoleRoutingRuleMaps2160: Cloud Service Mesh's Routing rule maps
// list a target gRPC proxy's INTERNAL_SELF_MANAGED forwarding rule with
// its URL map; Create routing rule map makes the URL map, the target gRPC
// proxy (validateForProxyless) and the 0.0.0.0 forwarding rule; the page
// shows them and its host rules; Edit turns validateForProxyless off;
// Delete removes all three and keeps the backend service.
func TestConsoleRoutingRuleMaps2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	if _, err := screen.call("POST", project, "global/healthChecks", nil, map[string]any{"name": "grpc-hc", "type": "GRPC", "grpcHealthCheck": map[string]any{"port": float64(50051)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := screen.call("POST", project, "global/backendServices", nil, map[string]any{"name": "greeter", "protocol": "GRPC", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"healthChecks": []any{"global/healthChecks/grpc-hc"}}); err != nil {
		t.Fatal(err)
	}
	g := grpcRoutingProvider{screen}
	if f := vpcField(g.CreateFormFor(ctx, project), "defaultService"); f == nil || f.Type != "select" || !hasString(f.Options, "greeter") || hasString(f.Options, "api") {
		t.Fatalf("the backend service field = %+v", f)
	}
	if _, err := g.Create(ctx, project, map[string]string{"name": "greeter-map", "protocol": "GRPC", "validateForProxyless": "true", "rule": "greeter-rule",
		"network": "default", "port": "50051", "hosts": "helloworld-gce", "defaultService": "greeter"}); err != nil {
		t.Fatal(err)
	}
	px, err := screen.call("GET", project, "global/targetGrpcProxies/greeter-map-proxy", nil, nil)
	if err != nil || px["validateForProxyless"] != true {
		t.Fatalf("the target gRPC proxy = %v, %v", px, err)
	}
	l, _ := g.List(ctx, project)
	r := vpcRow(l, "greeter-rule")
	if r == nil || r.Fields["Protocol"] != "gRPC" || r.Fields["Address"] != "0.0.0.0:50051" || r.Fields["URL map"] != "greeter-map" || r.Fields["Validate for proxyless"] != "Enabled" {
		t.Fatalf("Routing rule maps = %+v", l.Items)
	}
	d, _ := g.Detail(ctx, project, []string{"greeter-rule"})
	if d.Unavailable != "" || d.Edit == nil || len(d.Sections[1].Listing.Items) != 1 || d.Sections[1].Listing.Items[0].Fields["Hosts"] != "helloworld-gce" {
		t.Fatalf("the routing rule map's page = %+v", d)
	}
	if err := g.Edit(ctx, project, []string{"greeter-rule"}, map[string]string{"validateForProxyless": "false", "description": "greeter"}); err != nil {
		t.Fatal(err)
	}
	if px, _ = screen.call("GET", project, "global/targetGrpcProxies/greeter-map-proxy", nil, nil); px["validateForProxyless"] == true || vpcStr(px, "description") != "greeter" {
		t.Fatalf("the proxy after Edit = %v", px)
	}
	if err := g.ActAt(ctx, project, []string{"greeter-rule"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"global/forwardingRules/greeter-rule", "global/targetGrpcProxies/greeter-map-proxy", "global/urlMaps/greeter-map"} {
		if _, err := screen.call("GET", project, rel, nil, nil); err == nil {
			t.Errorf("%s survived Delete", rel)
		}
	}
	if _, err := screen.call("GET", project, "global/backendServices/greeter", nil, nil); err != nil {
		t.Errorf("the backend service went with the routing rule map: %v", err)
	}
}

// TestConsoleCertificateManagerEdits2160: over the Certificate Manager
// API itself, each resource's page has Edit: a self-managed certificate's
// description, labels and a new certificate (UpdateCertificate with
// selfManaged); a map's labels; a map entry's page (GetCertificateMapEntry)
// and its certificates; a DNS authorization's and an issuance config's
// pages (Get*) and their description and labels.
func TestConsoleCertificateManagerEdits2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	cm, err := certificatemanager.New(certificatemanager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cmh := certificatemanager.NewRESTHandler(cm)
	api := screen.svc.api
	screen.svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			cmh.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
	cs := ccmScreen{screen}
	certs := ccmCertsProvider{cs}
	cert1, key1 := selfSignedPEM(t, "www.example.com")
	if _, err := certs.Create(ctx, project, map[string]string{"name": "www", "location": "global", "type": "self", "certificate": cert1, "privateKey": key1}); err != nil {
		t.Fatal(err)
	}
	cp := []string{"global", "www"}
	d, _ := certs.Detail(ctx, project, cp)
	if d.Edit == nil || vpcField(d.Edit.Fields, "certificate") == nil || vpcField(d.Edit.Fields, "labels") == nil {
		t.Fatalf("the certificate's Edit = %+v", d.Edit)
	}
	cert2, key2 := selfSignedPEM(t, "shop.example.com")
	if err := certs.Edit(ctx, project, cp, map[string]string{"description": "storefront", "labels": `{"env":"prod"}`, "certificate": cert2, "privateKey": key2}); err != nil {
		t.Fatal(err)
	}
	o, _ := cs.v1(ctx, "GET", "/v1/projects/"+project+"/locations/global/certificates/www", nil)
	if vpcStr(o, "description") != "storefront" || vpcStr(vmMap(o, "labels"), "env") != "prod" || !hasString(vpcStrs(o, "sanDnsnames"), "shop.example.com") {
		t.Fatalf("the certificate after Edit = %v", o)
	}
	maps := ccmMapsProvider{cs}
	if _, err := maps.Create(ctx, project, map[string]string{"name": "shop-map"}); err != nil {
		t.Fatal(err)
	}
	mp := []string{"global", "shop-map"}
	if err := maps.Edit(ctx, project, mp, map[string]string{"description": "shop", "labels": `{"team":"web"}`}); err != nil {
		t.Fatal(err)
	}
	if d, _ = maps.Detail(ctx, project, mp); propValue(d.Sections[0].Groups[0].Properties, "Labels") != "team=web" || d.Edit == nil {
		t.Fatalf("the map's page after Edit = %+v", d)
	}
	if _, err := certs.Create(ctx, project, map[string]string{"name": "alt", "location": "global", "type": "self", "certificate": cert1, "privateKey": key1}); err != nil {
		t.Fatal(err)
	}
	if err := maps.ActAt(ctx, project, mp, "addentry", map[string]string{"entry": "www", "matchType": "hostname", "hostname": "www.example.com", "certificates": "www"}); err != nil {
		t.Fatal(err)
	}
	d, _ = maps.Detail(ctx, project, mp)
	ep := d.Sections[1].Listing.Items[0].Opens
	if strings.Join(ep, "/") != "global/shop-map/entry/www" {
		t.Fatalf("the entry row opens %v", ep)
	}
	ed, _ := maps.Detail(ctx, project, ep)
	if ed.Unavailable != "" || ed.Edit == nil || propValue(ed.Sections[0].Groups[0].Properties, "Hostname") != "www.example.com" {
		t.Fatalf("the entry's page = %+v", ed)
	}
	if err := maps.Edit(ctx, project, ep, map[string]string{"description": "", "labels": "", "certificates": "alt"}); err != nil {
		t.Fatal(err)
	}
	e, _ := cs.v1(ctx, "GET", "/v1/projects/"+project+"/locations/global/certificateMaps/shop-map/certificateMapEntries/www", nil)
	if strings.Join(lastAll(vpcStrs(e, "certificates")), ",") != "alt" {
		t.Fatalf("the entry after Edit = %v", e)
	}
	dns := ccmDNSAuthProvider{cs}
	if _, err := dns.Create(ctx, project, map[string]string{"name": "auth", "domain": "example.com", "location": "global"}); err != nil {
		t.Fatal(err)
	}
	ap := []string{"global", "auth"}
	if l, _ := dns.List(ctx, project); vpcRow(l, "auth") == nil || strings.Join(vpcRow(l, "auth").Opens, "/") != "global/auth" {
		t.Fatalf("DNS Authorizations = %+v", l.Items)
	}
	if err := dns.Edit(ctx, project, ap, map[string]string{"description": "apex", "labels": `{"zone":"main"}`}); err != nil {
		t.Fatal(err)
	}
	dd, _ := dns.Detail(ctx, project, ap)
	if propValue(dd.Sections[0].Groups[0].Properties, "Description") != "apex" || propValue(dd.Sections[0].Groups[0].Properties, "DNS record type") != "CNAME" {
		t.Fatalf("the DNS authorization's page = %+v", dd)
	}
	iss := ccmIssuanceProvider{cs}
	if _, err := iss.Create(ctx, project, map[string]string{"name": "private", "location": "global", "caPool": "projects/" + project + "/locations/us-central1/caPools/pool",
		"lifetime": "30", "rotation": "34", "keyAlgorithm": "ECDSA_P256"}); err != nil {
		t.Fatal(err)
	}
	ip := []string{"global", "private"}
	if err := iss.Edit(ctx, project, ip, map[string]string{"description": "internal", "labels": `{"ca":"pool"}`}); err != nil {
		t.Fatal(err)
	}
	id, _ := iss.Detail(ctx, project, ip)
	if propValue(id.Sections[0].Groups[0].Properties, "Labels") != "ca=pool" || propValue(id.Sections[0].Groups[0].Properties, "Key algorithm") != "ECDSA_P256" {
		t.Fatalf("the issuance config's page = %+v", id)
	}
}

// selfSignedPEM is a P-256 certificate for host and its key, in PEM.
func selfSignedPEM(t *testing.T, host string) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
}
