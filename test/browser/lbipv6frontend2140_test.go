//go:build browser

package browser

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// lbIPv6Fixture makes, through the Compute Engine API, a custom mode
// network with ULA internal IPv6, a dual-stack INTERNAL subnet and an
// unmanaged instance group on it.
func lbIPv6Fixture(t *testing.T, caddr, project string) {
	t.Helper()
	computeDo(t, caddr, "POST", project, "global/networks", map[string]any{"name": "lb6", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true})
	time.Sleep(time.Second)
	computeDo(t, caddr, "POST", project, "regions/us-central1/subnetworks", map[string]any{"name": "lb6-in", "network": "global/networks/lb6",
		"ipCidrRange": "10.97.0.0/24", "stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"})
	computeDo(t, caddr, "POST", project, "zones/us-central1-a/instanceGroups", map[string]any{"name": "lb6-web", "network": "global/networks/lb6"})
	time.Sleep(time.Second)
}

// TestIPv6LoadBalancerFrontendThroughTheBrowser2140 (#2140), in the
// compute shard: Create load balancer's Frontend configuration has
// Google's IP version (IPv4, IPv6); choosing IPv6 shows Subnetwork, and an
// internal passthrough Network Load Balancer made with it has an IPV6
// forwarding rule on a /96 of the subnet, read back through the Compute
// Engine API. The keyboard subtest picks IPv6 with the arrow keys and
// sees Subnetwork appear. Needs CLOUDBURROW_TEST_COMPUTE.
func TestIPv6LoadBalancerFrontendThroughTheBrowser2140(t *testing.T) {
	needService(t, "netservices-lb")
	caddr := endpoint(t, envBDRCompute)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	lbIPv6Fixture(t, caddr, project)
	p := open(t)
	p.navigate("/net-services/loadbalancing/list/loadBalancers/create" + q)
	p.waitFor(`document.querySelector("#f-lbType-2") !== null`)
	p.clickExpr(`document.querySelector("#f-lbType-2")`)
	p.clickExpr(`document.querySelector("#f-facing-1")`)
	setField(p, "#view", "f-network", "lb6")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-name", "browserlb6")
	setField(p, "#view", "f-protocol", "TCP")
	setField(p, "#view", "f-port", "80")
	var label string
	p.eval(`document.querySelector("label[for=f-ipVersion-1]") ? document.querySelector("label[for=f-ipVersion-1]").textContent.trim() : ""`, &label)
	if !strings.Contains(label, "IPv6") {
		t.Errorf("the IP version's second option is %q, want IPv6", label)
	}
	p.waitFor(`document.querySelector("#f-subnetwork").closest(".form-row").hidden`)
	p.clickExpr(`document.querySelector("#f-ipVersion-1")`)
	p.waitFor(`!document.querySelector("#f-subnetwork").closest(".form-row").hidden`)
	setField(p, "#view", "f-subnetwork", "lb6-in")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-instanceGroup", "us-central1-a/lb6-web")
	p.clickText(".create-step-continue button", "Continue")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-services/loadbalancing/list/loadBalancers"`)
	rule := computeDo(t, caddr, "GET", project, "regions/us-central1/forwardingRules/browserlb6-forwarding-rule", nil)
	if rule["ipVersion"] != "IPV6" || !strings.HasSuffix(rule["IPAddress"].(string), "/96") || !strings.HasSuffix(rule["subnetwork"].(string), "/subnetworks/lb6-in") ||
		rule["loadBalancingScheme"] != "INTERNAL" {
		t.Fatalf("the rule made on the page reads %v", rule)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-services/loadbalancing/list/loadBalancers/create" + q)
		k.waitFor(`document.querySelector(".create-step-continue button") !== null`)
		k.tabTo(`a.closest(".create-step-continue") !== null && a.textContent.trim() === "Continue"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`(() => { const f = document.querySelector("#f-ipVersion-0"); return !!f && f.getClientRects().length > 0; })()`)
		k.tabTo(`a.name === "ipVersion"`)
		k.run(chromedp.KeyEvent(kb.ArrowRight))
		k.waitFor(`document.querySelector("#f-ipVersion-1").checked && !document.querySelector("#f-subnetwork").closest(".form-row").hidden`)
		k.tabTo(`a.id === "f-subnetwork"`)
		k.run(chromedp.KeyEvent("lb6-in"))
		var v string
		k.eval(`document.querySelector("#f-subnetwork").value`, &v)
		if v != "lb6-in" {
			t.Errorf("typing into Subnetwork gave %q", v)
		}
	})
}

// TestIPv6LoadBalancerFrontendMeetsWCAG2140 (#2140): Create load balancer
// with IPv6 chosen in Frontend configuration meets WCAG (contrast in both
// themes, keyboard, names, motion, reflow).
func TestIPv6LoadBalancerFrontendMeetsWCAG2140(t *testing.T) {
	needService(t, "netservices-lb")
	project := servedProject(t)
	s := screen("Create load balancer, IPv6 frontend", "/net-services/loadbalancing/list/loadBalancers/create", project)
	s.then = `(() => { const r = document.querySelector("#f-ipVersion-1"); if (r) r.click(); return true; })()`
	auditScreens(t, []a11yPage{s})
}
