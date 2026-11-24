//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPrivateNATThroughTheBrowser2140 (#2140), in the compute shard:
// Create Cloud NAT gateway with NAT type Private hides Cloud NAT IP
// addresses and shows the Cloud NAT rule (next hop, Private NAT subnet);
// the gateway made on a new router with a PRIVATE_NAT subnet (made through
// the Compute Engine API) lists as Private, and the router's details show
// the Route policies tab. Needs CLOUDBURROW_TEST_COMPUTE.
func TestPrivateNATThroughTheBrowser2140(t *testing.T) {
	needService(t, "hybrid-routers")
	caddr := endpoint(t, envBDRCompute)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	resp, err := http.Post("http://"+caddr+"/compute/v1/projects/"+project+"/regions/us-central1/subnetworks", "application/json",
		strings.NewReader(`{"name":"browser-pnat","network":"global/networks/default","ipCidrRange":"100.64.90.0/24","purpose":"PRIVATE_NAT"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PRIVATE_NAT subnet: %v %v", resp, err)
	}
	resp.Body.Close()
	p := open(t)
	p.navigate("/net-services/nat/list" + q)
	p.clickText("#view button", "Create Cloud NAT gateway")
	p.waitFor(`document.querySelector("#f-type-1") !== null`)
	p.eval(`(() => { const r = document.querySelector("#f-type-1"); r.checked = true;
		r.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`(() => { const a = document.querySelector("#f-natIpAllocateOption-0"), s = document.querySelector("#f-natSubnet");
		return !!a && !!a.closest(".form-row") && a.closest(".form-row").hidden && !!s && !s.closest(".form-row").hidden; })()`)
	p.setField("#f-network", "default")
	p.setField("#f-region", "us-central1")
	p.submitForm(map[string]string{"name": "browser-pnat-gw", "router": "__new__", "routerName": "browser-pnat-router",
		"natSubnet": "browser-pnat", "ruleNumber": "100"}, "name")
	p.waitFor(`location.pathname === "/net-services/nat/list" && ` + rowReady("browser-pnat-gw"))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/netservices-nat"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"NAT type":"Private"`) {
		t.Errorf("the Private NAT gateway made on the page reads (%d) %s", code, body)
	}
	code, body = consoleDo(t, http.MethodGet, "/api/detail/hybrid-routers"+q+"&name=us-central1&name=browser-pnat-router", "")
	if code != http.StatusOK || !strings.Contains(body, "Route policies") {
		t.Errorf("the router's details (%d) have no Route policies tab: %s", code, body)
	}
}

// TestPrivateNATMeetsWCAG2140 (#2140): Create Cloud NAT gateway with NAT
// type Private chosen meets WCAG (contrast in both themes, keyboard,
// names, motion, reflow).
func TestPrivateNATMeetsWCAG2140(t *testing.T) {
	needService(t, "hybrid-routers")
	project := servedProject(t)
	pg := screen("create Private NAT", "/net-services/nat/list/create", project)
	pg.then = `(() => { const r = document.querySelector("#f-type-1"); if (r) { r.checked = true;
		r.dispatchEvent(new Event("change", { bubbles: true })); } return true; })()`
	auditScreens(t, []a11yPage{pg})
}
