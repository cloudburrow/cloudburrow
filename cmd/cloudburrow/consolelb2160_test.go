package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// lb2160Fixture is the console over a Compute API with an external
// Application Load Balancer "shop" (an instance group's backend service
// "shop-backend-service", a second backend service "api" routed /api/*,
// and a backend bucket "assets").
func lb2160Fixture(t *testing.T) (computeScreen, string) {
	t.Helper()
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	const project = "lb2-console"
	if _, err := screen.call("POST", project, "zones/us-central1-a/instanceGroups", nil, map[string]any{"name": "web",
		"namedPorts": []any{map[string]any{"name": "http", "port": float64(80)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := screen.call("POST", project, "global/backendServices", nil, map[string]any{"name": "api", "loadBalancingScheme": "EXTERNAL_MANAGED", "protocol": "HTTP"}); err != nil {
		t.Fatal(err)
	}
	if _, err := screen.call("POST", project, "global/backendBuckets", nil, map[string]any{"name": "assets", "bucketName": "assets-bucket"}); err != nil {
		t.Fatal(err)
	}
	if _, err := (computeLBProvider{screen}).Create(ctx, project, map[string]string{"lbType": "application", "facing": "external", "deployment": "global", "name": "shop",
		"protocol": "HTTP", "port": "80", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/web", "portName": "http",
		"balancingMode": "UTILIZATION", "routing": `{"/api/*":"api"}`}); err != nil {
		t.Fatal(err)
	}
	return screen, project
}

// TestConsoleLoadBalancerEdit2160: a load balancer's page offers Edit, a
// page of Google's steps (Frontend configuration, Backend configuration,
// Routing rules) ending in Review and finalize and Update, prefilled from
// the records; Update changes the backend service's timeout, session
// affinity and Cloud CDN, the URL map's host and path rules (a rule to a
// backend bucket), and the forwarding rule's port, which replaces the rule
// under its own name; an unchanged form changes nothing.
func TestConsoleLoadBalancerEdit2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	lb := computeLBProvider{screen}
	d, _ := lb.Detail(ctx, project, []string{"global", "shop"})
	f := d.Edit
	if f == nil || f.Steps != "continue" || !f.Page || f.Submit != "Review and finalize" || f.Review != "Update" {
		t.Fatalf("the Edit form = %+v", f)
	}
	var sections []string
	vals := map[string]string{}
	for _, x := range f.Fields {
		if len(sections) == 0 || sections[len(sections)-1] != x.Section {
			sections = append(sections, x.Section)
		}
		vals[x.Name] = x.Default
	}
	if strings.Join(sections, "|") != "Frontend configuration|Backend configuration|Routing rules" {
		t.Fatalf("the Edit steps = %v", sections)
	}
	if vals["port"] != "80" || vals["timeoutSec"] != "30" || vals["enableCDN"] != "false" || vals["iap"] != "false" ||
		vals["defaultBackend"] != "global/backendServices/shop-backend-service" || !strings.Contains(vals["routing"], "/api/*") {
		t.Fatalf("the Edit form's values = %v", vals)
	}
	if err := lb.Edit(ctx, project, []string{"global", "shop"}, vals); err != nil {
		t.Fatalf("an unchanged Update: %v", err)
	}
	before, _ := screen.call("GET", project, "global/forwardingRules/shop-forwarding-rule", nil, nil)
	vals["timeoutSec"], vals["sessionAffinity"], vals["enableCDN"], vals["cacheMode"], vals["defaultTtl"] = "45", "CLIENT_IP", "true", "FORCE_CACHE_ALL", "120"
	vals["routing"] = `{"/api/*":"api","/static/*":"global/backendBuckets/assets"}`
	vals["port"] = "8080"
	if err := lb.Edit(ctx, project, []string{"global", "shop"}, vals); err != nil {
		t.Fatal(err)
	}
	bs, _ := screen.call("GET", project, "global/backendServices/shop-backend-service", nil, nil)
	if bs["timeoutSec"] != float64(45) || bs["sessionAffinity"] != "CLIENT_IP" || bs["enableCDN"] != true ||
		vpcStr(vmMap(bs, "cdnPolicy"), "cacheMode") != "FORCE_CACHE_ALL" || vmMap(bs, "cdnPolicy")["defaultTtl"] != float64(120) {
		t.Fatalf("the backend service after Update = %v", bs)
	}
	m, _ := screen.call("GET", project, "global/urlMaps/shop", nil, nil)
	if got := urlMapServiceLinks(m); !hasString(got, "projects/"+project+"/global/backendBuckets/assets") || !hasString(got, "projects/"+project+"/global/backendServices/api") {
		t.Fatalf("the URL map after Update names %v", got)
	}
	r, err := screen.call("GET", project, "global/forwardingRules/shop-forwarding-rule", nil, nil)
	if err != nil || vpcStr(r, "portRange") != "8080-8080" || vpcStr(r, "target") != vpcStr(before, "target") {
		t.Fatalf("the forwarding rule after Update = %v, %v", r, err)
	}
	d, _ = lb.Detail(ctx, project, []string{"global", "shop"})
	if routes := d.Sections[2].Listing; len(routes.Items) < 3 {
		t.Fatalf("routing rules after Update = %+v", routes.Items)
	}
}

// TestConsoleCloudArmor2160: Create policy's steps make a backend
// security policy with a default deny rule, a first rule and a target;
// the list shows its type, scope, rules and targets; its page's Rules tab
// lists the rules by priority, Add rule (Advanced mode's expression,
// throttle) adds one, Edit changes one and the default rule, Delete
// removes one; the Targets tab lists the backend service, Apply policy to
// new target and Remove attach and detach; an edge policy goes on a
// backend bucket; Delete removes the policy.
func TestConsoleCloudArmor2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	armor := computeArmorProvider{screen}
	fields := armor.CreateFormFor(ctx, project)
	if tf := vpcField(fields, "target"); tf == nil || tf.Type != "select" || !hasString(tf.Options, "global/backendServices/api") {
		t.Fatalf("the target field = %+v", tf)
	}
	if _, err := armor.Create(ctx, project, map[string]string{"name": "shield", "type": "CLOUD_ARMOR", "scope": "global", "defaultAction": "deny", "denyStatus": "404",
		"rule_mode": "basic", "rule_srcIpRanges": "192.0.2.0/24", "rule_action": "allow", "rule_priority": "1000", "target": "global/backendServices/api"}); err != nil {
		t.Fatal(err)
	}
	l, _ := armor.List(ctx, project)
	if r := vpcRow(l, "shield"); r == nil || r.Fields["Type"] != "Backend security policy" || r.Fields["Rules"] != "2" || r.Fields["Targets"] != "1" {
		t.Fatalf("the policy's row = %+v", r)
	}
	path := []string{"global", "shield"}
	if err := armor.ActAt(ctx, project, path, "addrule", map[string]string{"mode": "advanced", "expression": "request.path.matches('/admin')",
		"action": "throttle", "rateCount": "10", "rateInterval": "60", "enforceOnKey": "IP", "exceedAction": "deny(429)", "priority": "500"}); err != nil {
		t.Fatal(err)
	}
	d, _ := armor.Detail(ctx, project, path)
	rules := d.Sections[1].Listing
	if len(rules.Items) != 3 || rules.Items[0].Fields["Priority"] != "500" || rules.Items[0].Fields["Action"] != "Throttle" ||
		rules.Items[0].Fields["Type"] != "Advanced" || rules.Items[2].Fields["Type"] != "Default" || rules.Items[2].Fields["Action"] != "Deny (404)" {
		t.Fatalf("the Rules tab = %+v", rules.Items)
	}
	if err := armor.ActAt(ctx, project, []string{"global", "shield", "rule", "1000"}, "editrule", map[string]string{"mode": "basic",
		"srcIpRanges": "198.51.100.0/24", "action": "deny", "responseCode": "403"}); err != nil {
		t.Fatal(err)
	}
	if err := armor.ActAt(ctx, project, []string{"global", "shield", "rule", "2147483647"}, "editrule", map[string]string{"action": "allow"}); err != nil {
		t.Fatal(err)
	}
	if err := armor.ActAt(ctx, project, []string{"global", "shield", "rule", "500"}, "deleterule", nil); err != nil {
		t.Fatal(err)
	}
	d, _ = armor.Detail(ctx, project, path)
	rules = d.Sections[1].Listing
	if len(rules.Items) != 2 || rules.Items[0].Fields["Match"] != "198.51.100.0/24" || rules.Items[0].Fields["Action"] != "Deny (403)" || rules.Items[1].Fields["Action"] != "Allow" {
		t.Fatalf("the Rules tab after Edit and Delete = %+v", rules.Items)
	}
	targets := d.Sections[2].Listing
	if len(targets.Items) != 1 || targets.Items[0].Name != "api" {
		t.Fatalf("the Targets tab = %+v", targets.Items)
	}
	if err := armor.ActAt(ctx, project, path, "applytarget", map[string]string{"target": "global/backendServices/shop-backend-service"}); err != nil {
		t.Fatal(err)
	}
	if err := armor.ActAt(ctx, project, targets.Items[0].Target, "removetarget", nil); err != nil {
		t.Fatal(err)
	}
	bs, _ := screen.call("GET", project, "global/backendServices/shop-backend-service", nil, nil)
	api, _ := screen.call("GET", project, "global/backendServices/api", nil, nil)
	if !strings.HasSuffix(vpcStr(bs, "securityPolicy"), "/securityPolicies/shield") || vpcStr(api, "securityPolicy") != "" {
		t.Fatalf("after Apply and Remove: shop %q, api %q", vpcStr(bs, "securityPolicy"), vpcStr(api, "securityPolicy"))
	}
	// The load balancer's Edit offers the policy.
	lbd, _ := (computeLBProvider{screen}).Detail(ctx, project, []string{"global", "shop"})
	for _, f := range lbd.Edit.Fields {
		if f.Name == "securityPolicy" && (f.Default != "shield" || !hasString(f.Options, "shield")) {
			t.Fatalf("the Edit page's policy field = %+v", f)
		}
	}
	// An edge policy on the bucket.
	if _, err := armor.Create(ctx, project, map[string]string{"name": "edge", "type": "CLOUD_ARMOR_EDGE", "defaultAction": "allow", "target": "global/backendBuckets/assets"}); err != nil {
		t.Fatal(err)
	}
	bb, _ := screen.call("GET", project, "global/backendBuckets/assets", nil, nil)
	if !strings.HasSuffix(vpcStr(bb, "edgeSecurityPolicy"), "/securityPolicies/edge") {
		t.Fatalf("the bucket's edge policy = %v", bb)
	}
	if err := armor.ActAt(ctx, project, []string{"global", "shop-backend-service"}, "delete", nil); err == nil {
		t.Fatal("deleting a policy that does not exist succeeded")
	}
}

// TestConsoleCloudCDN2160: Add origin turns Cloud CDN on for a backend
// bucket with a cache mode and TTLs; the Cloud CDN list shows it with its
// associated load balancer; its page shows the cache settings; Add
// signing key (generated, shown once) and Delete; Invalidate cache on the
// associated load balancer, listed on the Cache invalidation tab; Edit
// changes the cache mode; Remove turns Cloud CDN off.
func TestConsoleCloudCDN2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	if err := (computeLBProvider{screen}).Edit(ctx, project, []string{"global", "shop"}, map[string]string{"routing": `{"/static/*":"global/backendBuckets/assets"}`,
		"defaultBackend": "global/backendServices/shop-backend-service"}); err != nil {
		t.Fatal(err)
	}
	cdn := computeCDNProvider{screen}
	if f := vpcField(cdn.CreateFormFor(ctx, project), "origin"); f == nil || !hasString(f.Options, "backendBuckets/global/assets") {
		t.Fatalf("Add origin's origins = %+v", f)
	}
	if _, err := cdn.Create(ctx, project, map[string]string{"origin": "backendBuckets/global/assets", "cacheMode": "CACHE_ALL_STATIC", "defaultTtl": "600", "clientTtl": "60", "maxTtl": "3600"}); err != nil {
		t.Fatal(err)
	}
	l, _ := cdn.List(ctx, project)
	if r := vpcRow(l, "assets"); r == nil || r.Fields["Associated load balancers"] != "shop" || r.Fields["Cache mode"] != "Cache static content" {
		t.Fatalf("the origin's row = %+v (%+v)", r, l.Items)
	}
	path := []string{"backendBuckets", "global", "assets"}
	res, err := cdn.ActAtResult(ctx, project, path, "addkey", map[string]string{"keyName": "key1", "method": "generate", "maxAge": "1800"})
	if err != nil || res == nil || len(res.Items) != 1 || len(res.Items[0].Fields["Key value"]) != 24 {
		t.Fatalf("Add signing key: %+v, %v", res, err)
	}
	if err := cdn.ActAt(ctx, project, path, "invalidate", map[string]string{"urlMap": "shop", "path": "/static/*"}); err != nil {
		t.Fatal(err)
	}
	d, _ := cdn.Detail(ctx, project, path)
	if keys := d.Sections[1].Listing; len(keys.Items) != 1 || keys.Items[0].Name != "key1" || keys.Items[0].Fields["Cache entry maximum age"] != "1800 seconds" {
		t.Fatalf("the Restricted content tab = %+v", keys.Items)
	}
	if inv := d.Sections[2].Listing; len(inv.Items) != 1 || inv.Items[0].Fields["Load balancer"] != "shop" {
		t.Fatalf("the Cache invalidation tab = %+v", inv)
	}
	if err := cdn.ActAt(ctx, project, append(append([]string{}, path...), "key", "key1"), "deletekey", nil); err != nil {
		t.Fatal(err)
	}
	if err := cdn.Edit(ctx, project, path, map[string]string{"cacheMode": "USE_ORIGIN_HEADERS", "defaultTtl": "600", "clientTtl": "60"}); err != nil {
		t.Fatal(err)
	}
	bb, _ := screen.call("GET", project, "global/backendBuckets/assets", nil, nil)
	if p := vmMap(bb, "cdnPolicy"); vpcStr(p, "cacheMode") != "USE_ORIGIN_HEADERS" || len(vpcStrs(p, "signedUrlKeyNames")) != 0 {
		t.Fatalf("the bucket's CDN policy = %v", p)
	}
	if err := cdn.ActAt(ctx, project, path, "remove", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ = cdn.List(ctx, project); vpcRow(l, "assets") != nil {
		t.Fatal("Remove left the origin listed")
	}
}

// fakeV1 is a minimal Certificate Manager and IAP v1 for the console's
// tests: records by name, create through ?xId=, list by collection,
// :getIamPolicy and :setIamPolicy, :iapSettings; everything else under
// /compute/v1/ goes to the Compute API.
type fakeV1 struct {
	mu     sync.Mutex
	recs   map[string]map[string]any
	policy map[string]map[string]any
	next   http.Handler
}

func (f *fakeV1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		f.next.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/")
	out := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if i := strings.LastIndex(name, ":"); i > 0 {
		res, verb := name[:i], name[i+1:]
		switch verb {
		case "getIamPolicy":
			out(f.policy[res])
		case "setIamPolicy":
			p := body["policy"].(map[string]any)
			p["etag"] = "BwE="
			f.policy[res] = p
			out(p)
		case "testIamPermissions":
			out(body)
		case "iapSettings":
			if r.Method == http.MethodPatch {
				f.recs[res] = body
			}
			out(f.recs[res])
		}
		return
	}
	parts := strings.Split(name, "/")
	switch r.Method {
	case http.MethodPost:
		for k, v := range r.URL.Query() {
			if strings.HasSuffix(k, "Id") {
				body["name"] = name + "/" + v[0]
				f.recs[name+"/"+v[0]] = body
			}
		}
		out(map[string]any{"name": "operations/x", "done": true})
	case http.MethodDelete:
		delete(f.recs, name)
		out(map[string]any{"done": true})
	default:
		if rec, ok := f.recs[name]; ok {
			out(rec)
			return
		}
		coll := parts[len(parts)-1]
		var items []any
		for k, v := range f.recs {
			kp := strings.Split(k, "/")
			if len(kp) == len(parts)+1 && kp[len(kp)-2] == coll && (parts[3] == "-" || strings.HasPrefix(k, name+"/")) {
				items = append(items, v)
			}
		}
		out(map[string]any{coll: items})
	}
}

// TestConsoleCertificateManagerAndIAP2160: Certificate Manager's Add
// Certificate (Google-managed, DNS authorization), Create DNS
// authorization, Create certificate map with an entry and the primary
// entry, Create certificate issuance config, each listed on its tab and
// deleted; IAP's HTTPS resources list the backend services, Turn on IAP
// sets the backend service's iap.enabled, the Principals tab reads and
// writes the IAP resource's policy, and Settings writes IapSettings.
func TestConsoleCertificateManagerAndIAP2160(t *testing.T) {
	ctx := context.Background()
	screen, project := lb2160Fixture(t)
	fake := &fakeV1{recs: map[string]map[string]any{}, policy: map[string]map[string]any{}, next: screen.svc.api}
	screen.svc.handler = fake
	cs := ccmScreen{screen}
	if _, err := (ccmDNSAuthProvider{cs}).Create(ctx, project, map[string]string{"name": "auth", "domain": "example.com", "location": "global"}); err != nil {
		t.Fatal(err)
	}
	certs := ccmCertsProvider{cs}
	if _, err := certs.Create(ctx, project, map[string]string{"name": "www", "location": "global", "type": "managed", "caType": "public",
		"domains": "www.example.com", "authorization": "dns", "dnsAuthorizations": "auth"}); err != nil {
		t.Fatal(err)
	}
	rec := fake.recs["projects/"+project+"/locations/global/certificates/www"]
	m := vmMap(rec, "managed")
	if strings.Join(vpcStrs(m, "dnsAuthorizations"), ",") != "projects/"+project+"/locations/global/dnsAuthorizations/auth" || strings.Join(vpcStrs(m, "domains"), ",") != "www.example.com" {
		t.Fatalf("the certificate sent = %v", rec)
	}
	if l, _ := certs.List(ctx, project); vpcRow(l, "www") == nil || vpcRow(l, "www").Fields["Type"] != "Google-managed" {
		t.Fatalf("Certificates = %+v", l)
	}
	maps := ccmMapsProvider{cs}
	if _, err := maps.Create(ctx, project, map[string]string{"name": "shop-map"}); err != nil {
		t.Fatal(err)
	}
	mp := []string{"global", "shop-map"}
	if err := maps.ActAt(ctx, project, mp, "addentry", map[string]string{"entry": "www", "matchType": "hostname", "hostname": "www.example.com", "certificates": "www"}); err != nil {
		t.Fatal(err)
	}
	if err := maps.ActAt(ctx, project, mp, "addentry", map[string]string{"entry": "primary", "matchType": "primary", "certificates": "www"}); err != nil {
		t.Fatal(err)
	}
	d, _ := maps.Detail(ctx, project, mp)
	if ents := d.Sections[1].Listing; len(ents.Items) != 2 {
		t.Fatalf("the map's entries = %+v", ents.Items)
	}
	if l, _ := maps.List(ctx, project); vpcRow(l, "shop-map") == nil || vpcRow(l, "shop-map").Fields["Entries"] != "2" {
		t.Fatalf("Certificate Maps = %+v", l.Items)
	}
	if err := maps.ActAt(ctx, project, []string{"global", "shop-map", "entry", "www"}, "deleteentry", nil); err != nil {
		t.Fatal(err)
	}
	iss := ccmIssuanceProvider{cs}
	if _, err := iss.Create(ctx, project, map[string]string{"name": "private", "location": "global", "caPool": "projects/" + project + "/locations/us-central1/caPools/pool",
		"lifetime": "30", "rotation": "34", "keyAlgorithm": "ECDSA_P256"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := iss.List(ctx, project); vpcRow(l, "private") == nil || vpcRow(l, "private").Fields["Lifetime"] != "2592000s" {
		t.Fatalf("Issuance Configs = %+v", l.Items)
	}
	if l, _ := (ccmDNSAuthProvider{cs}).List(ctx, project); vpcRow(l, "auth") == nil {
		t.Fatalf("DNS Authorizations = %+v", l.Items)
	}
	if err := certs.ActAt(ctx, project, []string{"global", "www"}, "delete", nil); err != nil {
		t.Fatal(err)
	}

	iap := iapProvider{screen}
	l, _ := iap.List(ctx, project)
	r := vpcRow(l, "shop-backend-service")
	if r == nil || r.Fields["IAP"] != "Off" || len(r.Info) != 1 || !r.Info[0].Permissions {
		t.Fatalf("the HTTPS resource row = %+v", r)
	}
	path := []string{"global", "shop-backend-service"}
	if err := iap.ActAt(ctx, project, path, "turnon", nil); err != nil {
		t.Fatal(err)
	}
	bs, _ := screen.call("GET", project, "global/backendServices/shop-backend-service", nil, nil)
	if vmMap(bs, "iap")["enabled"] != true {
		t.Fatalf("the backend service's iap after Turn on IAP = %v", bs["iap"])
	}
	pol, err := iap.GetPolicy(ctx, project, path)
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/iap.httpsResourceAccessor", Members: []string{"user:ana@example.com"}})
	if err := iap.SetPolicy(ctx, project, path, pol); err != nil {
		t.Fatal(err)
	}
	res := "projects/" + project + "/iap_web/compute/services/shop-backend-service"
	if got, _ := json.Marshal(fake.policy[res]); !strings.Contains(string(got), "roles/iap.httpsResourceAccessor") || !strings.Contains(string(got), "user:ana@example.com") {
		t.Fatalf("the IAP policy set = %s", got)
	}
	if err := iap.ActAt(ctx, project, path, "settings", map[string]string{"allowHttpOptions": "true", "cookieDomain": "example.com"}); err != nil {
		t.Fatal(err)
	}
	if s := fake.recs[res]; vpcStr(vmMap(s, "applicationSettings"), "cookieDomain") != "example.com" {
		t.Fatalf("the IAP settings = %v", s)
	}
	if l, _ = iap.List(ctx, project); vpcRow(l, "shop-backend-service").Fields["IAP"] != "On" {
		t.Fatalf("after Turn on IAP: %+v", vpcRow(l, "shop-backend-service"))
	}
}
