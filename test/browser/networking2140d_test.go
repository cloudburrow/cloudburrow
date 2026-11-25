//go:build browser

package browser

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// computeDo calls the instance's Compute Engine API (the compute shard's
// CLOUDBURROW_TEST_COMPUTE), failing t on anything but 200.
func computeDo(t *testing.T, caddr, method, project, path string, body any) map[string]any {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "http://"+caddr+"/compute/v1/projects/"+project+"/"+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s = %d: %s", method, path, resp.StatusCode, b)
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// routerWithPolicyAndSet makes router r1 in us-central1 with an import
// route policy and a prefix named set, through the Compute Engine API.
func routerWithPolicyAndSet(t *testing.T, caddr, project string) {
	t.Helper()
	const base = "regions/us-central1/routers"
	computeDo(t, caddr, "POST", project, base, map[string]any{"name": "r1", "network": "global/networks/default", "bgp": map[string]any{"asn": 64512}})
	computeDo(t, caddr, "POST", project, base+"/r1/updateRoutePolicy", map[string]any{"name": "imp", "type": "ROUTE_POLICY_TYPE_IMPORT",
		"terms": []any{map[string]any{"priority": 10, "match": map[string]any{"expression": "destination.inAnyRange(prefix('10.0.0.0/8'))"},
			"actions": []any{map[string]any{"expression": "accept()"}}}}})
	computeDo(t, caddr, "POST", project, base+"/r1/updateNamedSet", map[string]any{"name": "api-set", "type": "NAMED_SET_TYPE_COMMUNITY",
		"elements": []any{map[string]any{"expression": "'64500:100'"}}})
}

// TestRouterNamedSetsThroughTheBrowser2140d (#2140), in the compute
// shard: a Cloud Router's Named sets tab beside Route policies lists the
// set the API made; Create named set (Prefix, two elements) on the page;
// the set's Edit elements under its fingerprint; a malformed element is
// refused with the API's message; Delete from the row's menu. Needs
// CLOUDBURROW_TEST_COMPUTE.
func TestRouterNamedSetsThroughTheBrowser2140d(t *testing.T) {
	needService(t, "hybrid-routers")
	caddr := endpoint(t, envBDRCompute)
	project := uniqueProject(t)
	routerWithPolicyAndSet(t, caddr, project)
	q := "?project=" + url.QueryEscape(project)
	sets := func() string {
		t.Helper()
		m := computeDo(t, caddr, "GET", project, "regions/us-central1/routers/r1/listNamedSets", nil)
		b, _ := json.Marshal(m["result"])
		return string(b)
	}
	p := open(t)
	p.navigate("/hybrid/routers/list/us-central1/r1" + q)
	p.waitFor(settled + ` && document.querySelector("#view [role=tab]") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view .tab-strip [role=tab]")].map((t) => t.textContent.trim()).join(",")`, &tabs)
	if !strings.Contains(tabs, "Route policies,Named sets") {
		t.Errorf("the router's tabs = %s, want Named sets after Route policies", tabs)
	}
	p.clickText(`#view [role="tab"]`, "Named sets")
	p.waitFor(rowReady("api-set"))

	p.pageAction("Create named set", map[string]string{"setName": "allowed-prefixes", "setType": "NAMED_SET_TYPE_PREFIX",
		"elements": "prefix('10.0.0.0/20').orLonger()\nprefix('10.1.0.0/16')"}, "")
	if got := sets(); !strings.Contains(got, `"name":"allowed-prefixes"`) || !strings.Contains(got, "prefix('10.1.0.0/16')") || !strings.Contains(got, "NAMED_SET_TYPE_PREFIX") {
		t.Errorf("the set made on the page reads %s", got)
	}

	p.navigate("/hybrid/routers/list/us-central1/r1" + q + "&tab=namedsets")
	p.rowAction("Named sets", "allowed-prefixes", "Edit elements", map[string]string{"elements": "prefix('192.168.0.0/16')"}, "")
	if got := sets(); !strings.Contains(got, "192.168.0.0/16") || strings.Contains(got, "10.1.0.0/16") {
		t.Errorf("after Edit elements: %s", got)
	}

	// An unclosed string literal: the API's 400, shown on the form.
	p.navigate("/hybrid/routers/list/us-central1/r1" + q + "&tab=namedsets")
	p.clickText(p.findAction("Create named set"), "Create named set")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	p.setField(".modal.is-open #f-setName", "bad-set")
	p.setField(".modal.is-open #f-elements", "'64500:100")
	p.eval(`document.querySelector('.modal.is-open button[type="submit"]').click()`, nil)
	if msg := p.modalRefusal(); !strings.Contains(msg, "elements[0]") {
		t.Errorf("a malformed element was refused with %q", msg)
	}
	p.forgive() // the refusal's 400, which the page shows
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)

	p.navigate("/hybrid/routers/list/us-central1/r1" + q + "&tab=namedsets")
	p.clickText(`#view [role="tab"]`, "Named sets")
	vmRowMenu(p, "allowed-prefixes", "Delete", nil, byButton)
	if got := sets(); strings.Contains(got, "allowed-prefixes") {
		t.Errorf("after Delete: %s", got)
	}
}

// pscPublishedEndpoint makes, through the Compute Engine API, an internal
// passthrough load balancer (#2153) published as service attachment svc on
// a PRIVATE_SERVICE_CONNECT subnet, and an endpoint ep to it.
func pscPublishedEndpoint(t *testing.T, caddr, project string) {
	t.Helper()
	const r = "regions/us-central1/"
	computeDo(t, caddr, "POST", project, r+"subnetworks", map[string]any{"name": "psc-nat-d", "network": "global/networks/default",
		"ipCidrRange": "10.0.19.0/24", "purpose": "PRIVATE_SERVICE_CONNECT"})
	computeDo(t, caddr, "POST", project, r+"healthChecks", map[string]any{"name": "ilb-hc", "type": "TCP", "tcpHealthCheck": map[string]any{"port": 80}})
	computeDo(t, caddr, "POST", project, r+"backendServices", map[string]any{"name": "ilb", "protocol": "TCP", "loadBalancingScheme": "INTERNAL",
		"healthChecks": []any{"projects/" + project + "/" + r + "healthChecks/ilb-hc"}})
	computeDo(t, caddr, "POST", project, r+"forwardingRules", map[string]any{"name": "ilb", "loadBalancingScheme": "INTERNAL", "IPProtocol": "TCP",
		"ports": []any{"80"}, "network": "global/networks/default", "backendService": "projects/" + project + "/" + r + "backendServices/ilb"})
	computeDo(t, caddr, "POST", project, r+"serviceAttachments", map[string]any{"name": "svc", "targetService": "projects/" + project + "/" + r + "forwardingRules/ilb",
		"natSubnets": []any{r + "subnetworks/psc-nat-d"}, "connectionPreference": "ACCEPT_AUTOMATIC"})
	computeDo(t, caddr, "POST", project, r+"forwardingRules", map[string]any{"name": "ep", "network": "global/networks/default",
		"subnetwork": r + "subnetworks/default", "target": "projects/" + project + "/" + r + "serviceAttachments/svc"})
}

// TestPSCEndpointGlobalAccessThroughTheBrowser2140d (#2140), in the
// compute shard: an endpoint for a published service's page reads Global
// access Disabled; Edit, Enable global access, Save turns it on, as the
// API then reads, and Edit again turns it off. Publish service offers the
// internal load balancer by Load balancer type. Needs
// CLOUDBURROW_TEST_COMPUTE.
func TestPSCEndpointGlobalAccessThroughTheBrowser2140d(t *testing.T) {
	needService(t, "netservices-psc-endpoints")
	caddr := endpoint(t, envBDRCompute)
	project := uniqueProject(t)
	pscPublishedEndpoint(t, caddr, project)
	q := "?project=" + url.QueryEscape(project)
	global := func() bool {
		t.Helper()
		return computeDo(t, caddr, "GET", project, "regions/us-central1/forwardingRules/ep", nil)["allowPscGlobalAccess"] == true
	}
	p := open(t)
	path := "/net-services/psc/list/consumers/us-central1/ep" + q
	p.navigate(path)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("Global access")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Disabled") {
		t.Errorf("the endpoint's page does not read Global access Disabled: %s", text)
	}
	p.editAnywhere(map[string]string{"allowPscGlobalAccess": "true"})
	if !global() {
		t.Error("Enable global access, Save: the endpoint's allowPscGlobalAccess is off")
	}
	p.navigate(path)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("Enabled")`)
	p.editAnywhere(map[string]string{"allowPscGlobalAccess": "false"})
	if global() {
		t.Error("unchecked, Save: the endpoint's allowPscGlobalAccess is still on")
	}

	// Publish service's Target details offer the internal load balancer.
	p.navigate("/net-services/psc/published/create" + q)
	p.waitFor(`document.querySelector("#f-targetService") !== null`)
	var opts string
	p.eval(`[...document.querySelectorAll("#f-targetService option")].map((o) => o.textContent.trim()).join(",")`, &opts)
	if !strings.Contains(opts, "ilb (us-central1)") {
		t.Errorf("Publish service's internal load balancers = %s", opts)
	}
}

// TestRouterRoutePoliciesAndNamedSetsMeetWCAG2140d (#2140): a Cloud
// Router's Route policies and Named sets tabs, Create route policy and
// Create named set, and an endpoint for a published service's page with
// its Edit meet WCAG (contrast in both themes, keyboard, names, motion,
// reflow).
func TestRouterRoutePoliciesAndNamedSetsMeetWCAG2140d(t *testing.T) {
	needService(t, "hybrid-routers")
	caddr := endpoint(t, envBDRCompute)
	project := servedProject(t)
	// The served project is shared: names of this test's own.
	if _, body := consoleDo(t, http.MethodGet, "/api/detail/hybrid-routers?project="+url.QueryEscape(project)+"&name=us-central1&name=r1", ""); !strings.Contains(body, "Named sets") {
		routerWithPolicyAndSet(t, caddr, project)
	}
	base := "/hybrid/routers/list/us-central1/r1"
	clickJS := func(label string) string {
		return `(() => { const b = [...document.querySelectorAll("#view .section-actions button, #view .page-actions button")].find((b) => b.textContent.trim() === ` +
			"\"" + label + "\"" + `); if (b) b.click(); return true; })()`
	}
	rp := screen("a Cloud Router's Route policies", base+"?tab=routepolicies", project)
	ns := screen("a Cloud Router's Named sets", base+"?tab=namedsets", project)
	createRP := screen("Create route policy", base+"?tab=routepolicies", project)
	createRP.then = clickJS("Create route policy")
	createNS := screen("Create named set", base+"?tab=namedsets", project)
	createNS.then = clickJS("Create named set")
	pages := []a11yPage{rp, ns, createRP, createNS}
	if _, body := consoleDo(t, http.MethodGet, "/api/detail/netservices-psc-endpoints?project="+url.QueryEscape(project)+"&name=us-central1&name=ep", ""); !strings.Contains(body, "Global access") {
		pscPublishedEndpoint(t, caddr, project)
	}
	ep := screen("an endpoint for a published service", "/net-services/psc/list/consumers/us-central1/ep", project)
	edit := screen("an endpoint's Edit", "/net-services/psc/list/consumers/us-central1/ep", project)
	// The endpoint's own page, not Page not found: Connected endpoints is at
	// Google's /net-services/psc/list/consumers since #2141.
	ep.ready += ` && document.querySelector("#view").textContent.includes("Global access")`
	edit.ready += ` && document.querySelector("#view").textContent.includes("Global access")`
	edit.then = `(() => { const b = document.querySelector("#view #detail-edit") ||
		[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Edit"); if (b) b.click(); return true; })()`
	auditScreens(t, append(pages, ep, edit))
}
