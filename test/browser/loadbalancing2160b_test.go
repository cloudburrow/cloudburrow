//go:build browser

package browser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Load balancing round 3 (#2160): Cloud Armor's rule page, labels and
// other tabs, Cloud Service Mesh's routing rule maps (target gRPC
// proxies) and Certificate Manager's edits.

// lb3Compute calls the instance's Compute Engine JSON API (the loopback
// endpoint in CLOUDBURROW_TEST_COMPUTE, without credentials, as the API
// takes them locally) at projects/PROJECT/rel; it skips where the
// variable is unset.
func lb3Compute(t *testing.T, method, project, rel string, body any) map[string]any {
	t.Helper()
	addr := endpoint(t, envBDRCompute)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+addr+"/compute/v1/projects/"+project+"/"+rel, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, rel, resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// pageTabs is the page's tab strip, the current tab starred.
func pageTabs(p *tab) string {
	var strip string
	p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
	return strip
}

// TestCloudArmorRound3ThroughTheBrowser2160 (#2160): Cloud Armor policies'
// tabs (Security policies, Network edge security services, Preconfigured
// WAF rules); a rule's own page by priority (Rule details) whose Edit page
// ends in Update and changes the rule; the policy's Edit sets its labels,
// shown on Policy details; Create network edge security service makes one,
// listed; the WAF tab lists the rule sets and a set's page its signatures.
// The keyboard subtest moves along the tab strip with Tab and Enter.
func TestCloudArmorRound3ThroughTheBrowser2160(t *testing.T) {
	needService(t, "netsecurity-armor")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	name := fmt.Sprintf("armor3-%d", time.Now().UnixNano()%1e6)
	body, _ := json.Marshal(map[string]string{"name": name, "type": "CLOUD_ARMOR", "scope": "global", "defaultAction": "allow",
		"rule_mode": "basic", "rule_srcIpRanges": "192.0.2.0/24", "rule_action": "deny", "rule_responseCode": "403", "rule_priority": "1000"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/netsecurity-armor"+q, string(body)); code != http.StatusOK {
		t.Fatalf("the policy: %d %s", code, out)
	}
	p := open(t)
	want := []string{"Security policies", "Network edge security services", "Preconfigured WAF rules"}
	for i, path := range []string{"/net-security/securitypolicies/list", "/net-security/securitypolicies/networkEdgeSecurityServices", "/net-security/securitypolicies/wafRules"} {
		p.navigate(path + q)
		p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		names := append([]string{}, want...)
		names[i] += "*"
		if got := pageTabs(p); got != strings.Join(names, ",") {
			t.Errorf("%s's tabs = %s", path, got)
		}
	}

	// The rule's page and Edit rule.
	p.navigate("/net-security/securitypolicies/list/global/" + name + "/rule/1000" + q)
	p.waitFor(settled + ` && document.querySelector("#view .page-actions #detail-edit") !== null`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Rule details") || !strings.Contains(text, "192.0.2.0/24") {
		t.Errorf("the rule's page reads %q", text)
	}
	p.clickText("#view .page-actions #detail-edit", "Edit")
	p.waitFor(`new URLSearchParams(location.search).get("edit") === "1" && document.querySelector("#view form #f-description") !== null`)
	p.setField("#view form #f-description", "block the lab")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"]`)))
	p.waitFor(`!new URLSearchParams(location.search).has("edit")`)
	code, out := consoleDo(t, http.MethodGet, "/api/detail/netsecurity-armor"+q+"&name=global&name="+name+"&name=rule&name=1000", "")
	if code != http.StatusOK || !strings.Contains(out, "block the lab") {
		t.Errorf("the rule after Update reads %d %s", code, out)
	}

	// Labels, through Edit policy.
	lb, _ := json.Marshal(map[string]any{"Path": []string{"global", name}, "Action": "editpolicy", "Values": map[string]string{"description": "front door", "labels": `{"env":"prod"}`}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/netsecurity-armor"+q, string(lb)); code != http.StatusOK {
		t.Fatalf("Edit policy: %d %s", code, out)
	}
	p.navigate("/net-security/securitypolicies/list/global/" + name + q + "&tab=details")
	p.waitFor(settled + ` && document.querySelector("#view [role=tab]") !== null`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "env=prod") {
		t.Errorf("Policy details does not show the label: %q", text)
	}

	// A network edge security service.
	nname := fmt.Sprintf("netpol-%d", time.Now().UnixNano()%1e6)
	nb, _ := json.Marshal(map[string]string{"name": nname, "type": "CLOUD_ARMOR_NETWORK", "scope": "regional", "region": "us-central1", "defaultAction": "allow"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/netsecurity-armor"+q, string(nb)); code != http.StatusOK {
		t.Fatalf("the network policy: %d %s", code, out)
	}
	svc := fmt.Sprintf("ddos-%d", time.Now().UnixNano()%1e6)
	p.navigate("/net-security/securitypolicies/networkEdgeSecurityServices" + q)
	p.clickText("#view button", "Create network edge security service")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	setField(p, "#view", "f-name", svc)
	setField(p, "#view", "f-securityPolicy", "regions/us-central1/securityPolicies/"+nname)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/securitypolicies/networkEdgeSecurityServices" && ` + rowReady(svc))

	// The WAF rule sets.
	p.navigate("/net-security/securitypolicies/wafRules" + q)
	p.waitFor(rowReady("sqli-v33-stable"))
	p.navigate("/net-security/securitypolicies/wafRules/sqli-v33-stable" + q + "&tab=signatures")
	p.waitFor(settled + ` && document.querySelector("#view tbody tr") !== null`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "owasp-crs-v030301-id942100-sqli") {
		t.Errorf("the rule set's signatures read %q", text)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-security/securitypolicies/list" + q)
		k.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		k.tabTo(`a.closest("nav.page-tabs") !== null && a.textContent === "Preconfigured WAF rules"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`location.pathname === "/net-security/securitypolicies/wafRules"`)
	})
}

// TestRoutingRuleMapsThroughTheBrowser2160 (#2160): Cloud Service Mesh's
// Routing rule maps: Create routing rule map (a gRPC backend service of the
// INTERNAL_SELF_MANAGED scheme made through the API) makes the URL map,
// the target gRPC proxy and the 0.0.0.0 forwarding rule, listed with
// Validate for proxyless; its page shows the routing rules and Edit turns
// Validate for proxyless off. The keyboard subtest reaches Create with Tab.
func TestRoutingRuleMapsThroughTheBrowser2160(t *testing.T) {
	needService(t, "servicemesh-routing")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	sfx := time.Now().UnixNano() % 1e6
	hc, bs, um, rule := fmt.Sprintf("ghc-%d", sfx), fmt.Sprintf("greeter-%d", sfx), fmt.Sprintf("gmap-%d", sfx), fmt.Sprintf("grule-%d", sfx)
	lb3Compute(t, http.MethodPost, project, "global/healthChecks", map[string]any{"name": hc, "type": "GRPC", "grpcHealthCheck": map[string]any{"port": 50051}})
	lb3Compute(t, http.MethodPost, project, "global/backendServices", map[string]any{"name": bs, "protocol": "GRPC", "loadBalancingScheme": "INTERNAL_SELF_MANAGED",
		"healthChecks": []any{"global/healthChecks/" + hc}})
	p := open(t)
	p.navigate("/net-services/trafficdirector/routingRuleMaps" + q)
	p.clickText("#view button", "Create routing rule map")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	setField(p, "#view", "f-name", um)
	setField(p, "#view", "f-rule", rule)
	setField(p, "#view", "f-hosts", "helloworld-gce")
	setField(p, "#view", "f-defaultService", bs)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-services/trafficdirector/routingRuleMaps" && ` + rowReady(rule))
	px := lb3Compute(t, http.MethodGet, project, "global/targetGrpcProxies/"+um+"-proxy", nil)
	if px["validateForProxyless"] != true {
		t.Errorf("the target gRPC proxy = %v", px)
	}
	p.navigate("/net-services/trafficdirector/routingRuleMaps/" + rule + q + "&tab=rules")
	p.waitFor(settled + ` && document.querySelector("#view tbody tr") !== null`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "helloworld-gce") {
		t.Errorf("the routing rules read %q", text)
	}
	eb, _ := json.Marshal(map[string]any{"Path": []string{rule}, "Values": map[string]string{"validateForProxyless": "false", "description": ""}})
	if code, out := consoleDo(t, http.MethodPatch, "/api/resources/servicemesh-routing"+q, string(eb)); code != http.StatusOK {
		t.Fatalf("Edit: %d %s", code, out)
	}
	if px = lb3Compute(t, http.MethodGet, project, "global/targetGrpcProxies/"+um+"-proxy", nil); px["validateForProxyless"] == true {
		t.Errorf("the proxy after Edit = %v", px)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-services/trafficdirector/routingRuleMaps" + q)
		k.waitFor(rowReady(rule))
		k.tabTo(`a.tagName === "BUTTON" && a.textContent.trim() === "Create routing rule map"`)
	})
}

// TestCertificateManagerEditsThroughTheBrowser2160 (#2160): a certificate
// map's page has Edit, whose description and labels are saved and shown
// under Details; a DNS authorization's row opens its page, whose Edit
// saves its description. The keyboard subtest opens Edit with Tab and
// Enter.
func TestCertificateManagerEditsThroughTheBrowser2160(t *testing.T) {
	needService(t, "ccm-certificates")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	m := fmt.Sprintf("emap-%d", time.Now().UnixNano()%1e6)
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/ccm-maps"+q, `{"name":"`+m+`"}`); code != http.StatusOK {
		t.Fatalf("the map: %d %s", code, out)
	}
	p := open(t)
	p.navigate("/security/ccm/list/certificateMaps/global/" + m + q)
	p.waitFor(settled + ` && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit")`)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-description") !== null`)
	p.setField(".modal.is-open #f-description", "shop front")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.waitFor(`document.querySelector("#view").textContent.includes("shop front")`)

	a := fmt.Sprintf("eauth-%d", time.Now().UnixNano()%1e6)
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/ccm-dnsauth"+q, `{"name":"`+a+`","domain":"example.com","location":"global"}`); code != http.StatusOK {
		t.Fatalf("the DNS authorization: %d %s", code, out)
	}
	p.navigate("/security/ccm/list/dnsAuthorizations" + q)
	p.waitFor(rowReady(a))
	p.clickText("#view tbody a", a)
	p.waitFor(`location.pathname === "/security/ccm/list/dnsAuthorizations/global/` + a + `" && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit")`)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-description") !== null`)
	p.setField(".modal.is-open #f-description", "apex")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null && document.querySelector("#view").textContent.includes("apex")`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/security/ccm/list/certificateMaps/global/" + m + q)
		k.waitFor(settled + ` && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit")`)
		k.tabTo(`a.closest(".page-actions") !== null && a.textContent.trim() === "Edit"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open") !== null && document.querySelector(".modal.is-open").contains(document.activeElement)`)
		k.run(chromedp.KeyEvent(kb.Escape))
	})
}

// TestLoadBalancingRound3MeetsWCAG2160 (#2160): Cloud Armor's Network edge
// security services and Preconfigured WAF rules tabs with the create page,
// a rule's page and its Edit page, the WAF rule set's page, the Routing
// rule maps with their create page, and Certificate Manager's DNS
// authorization page meet WCAG (contrast in both themes, keyboard, names,
// motion, reflow).
func TestLoadBalancingRound3MeetsWCAG2160(t *testing.T) {
	needService(t, "netsecurity-armor")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	name := fmt.Sprintf("armorw-%d", time.Now().UnixNano()%1e6)
	body, _ := json.Marshal(map[string]string{"name": name, "type": "CLOUD_ARMOR", "scope": "global", "defaultAction": "allow",
		"rule_mode": "basic", "rule_srcIpRanges": "192.0.2.0/24", "rule_action": "deny", "rule_priority": "1000"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/netsecurity-armor"+q, string(body)); code != http.StatusOK {
		t.Fatalf("the policy: %d %s", code, out)
	}
	rule := "/net-security/securitypolicies/list/global/" + name + "/rule/1000" + q
	var pages []a11yPage
	pages = append(pages, listAndCreate("Network edge security services", "/net-security/securitypolicies/networkEdgeSecurityServices", project)...)
	pages = append(pages, screen("Preconfigured WAF rules", "/net-security/securitypolicies/wafRules", project))
	pages = append(pages, a11yPage{"WAF rule set", "/net-security/securitypolicies/wafRules/sqli-v33-stable" + q, `document.querySelector("#view [role=tab]") !== null`, ""})
	pages = append(pages, a11yPage{"Cloud Armor rule", rule, `document.querySelector("#view .page-actions #detail-edit") !== null`, ""})
	pages = append(pages, a11yPage{"Cloud Armor Edit rule", rule + "&edit=1", `document.querySelector("#view form #f-description") !== null`, ""})
	pages = append(pages, listAndCreate("Routing rule maps", "/net-services/trafficdirector/routingRuleMaps", project)...)
	auditScreens(t, pages)
}
