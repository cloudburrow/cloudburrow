//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// lb2160Fixture makes an external Application Load Balancer in front of
// an instance group through the console's API, and returns its name.
func lb2160Fixture(t *testing.T, project string) string {
	t.Helper()
	group := lbFixtureGroup(t, project)
	name := fmt.Sprintf("lbe-%d", time.Now().UnixNano()%1e6)
	body, _ := json.Marshal(map[string]string{"name": name, "lbType": "application", "facing": "external", "deployment": "global", "protocol": "HTTP",
		"port": "80", "backendType": "instanceGroup", "instanceGroup": "us-central1-a/" + group, "portName": "http", "balancingMode": "UTILIZATION"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/netservices-lb?project="+url.QueryEscape(project), string(body)); code != http.StatusOK {
		t.Fatalf("the load balancer: %d %s", code, out)
	}
	return name
}

// editFieldDefault is a field's value in a page's Edit form, through the
// console API.
func editFieldDefault(t *testing.T, service, project, field string, path ...string) string {
	t.Helper()
	q := "?project=" + url.QueryEscape(project)
	for _, p := range path {
		q += "&name=" + url.QueryEscape(p)
	}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/"+service+q, "")
	var d struct {
		Edit struct {
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("the detail of %v: %d %s", path, code, body)
	}
	for _, f := range d.Edit.Fields {
		if f.Name == field {
			return f.Default
		}
	}
	return ""
}

// TestLoadBalancerEditThroughTheBrowser2160 (#2160): a load balancer's
// page has Edit, which opens the Edit page of Google's steps (Frontend
// configuration, Backend configuration, Routing rules); Review and
// finalize lists the change (the timeout) and Update saves it, read back
// through the console API. The keyboard subtest walks the steps with Tab
// and Enter alone.
func TestLoadBalancerEditThroughTheBrowser2160(t *testing.T) {
	needService(t, "netservices-lb")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	lb := lb2160Fixture(t, project)
	detail := "/net-services/loadbalancing/list/loadBalancers/global/" + lb + q
	p := open(t)
	p.navigate(detail)
	p.waitFor(settled)
	p.clickText("#view .page-actions #detail-edit", "Edit")
	p.waitFor(`new URLSearchParams(location.search).get("edit") === "1" && document.querySelector("#view form.create-page #f-port") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll("#view form.create-page .create-step-title")].map((s) => s.textContent)`, &steps)
	if strings.Join(steps, "|") != "Frontend configuration|Backend configuration|Routing rules" {
		t.Errorf("the Edit page's steps are %v", steps)
	}
	p.clickText("#view form.create-page .create-step-continue button", "Continue")
	p.waitFor(`(() => { const f = document.querySelector("#f-timeoutSec"); return !!f && f.getClientRects().length > 0; })()`)
	p.setField("#view form.create-page #f-timeoutSec", "45")
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open #review-diff") !== null`)
	var diff string
	p.eval(`document.querySelector("#review-diff tbody").textContent`, &diff)
	if !strings.Contains(diff, "Timeout") || !strings.Contains(diff, "45") {
		t.Errorf("Review and finalize lists %q; want the timeout's change to 45", diff)
	}
	p.run(chromedp.Click(chromedp.CSS(`#review-deploy`)))
	p.waitFor(`!new URLSearchParams(location.search).has("edit") && document.querySelector("#view form.create-page") === null`)
	if got := editFieldDefault(t, "netservices-lb", project, "timeoutSec", "global", lb); got != "45" {
		t.Errorf("the timeout after Update reads %q; want 45", got)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-services/loadbalancing/list/loadBalancers/global/" + lb + q + "&edit=1")
		k.waitFor(`document.querySelector("#view form.create-page .create-step-continue button") !== null`)
		k.tabTo(`a.closest(".create-step-continue") !== null && a.textContent.trim() === "Continue"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`(() => { const f = document.querySelector("#f-timeoutSec"); return !!f && f.getClientRects().length > 0; })()`)
		k.tabTo(`a.id === "f-timeoutSec"`)
		k.run(chromedp.KeyEvent("0"))
		var v string
		k.eval(`document.querySelector("#f-timeoutSec").value`, &v)
		if !strings.HasSuffix(v, "0") {
			t.Errorf("typing into Timeout gave %q", v)
		}
	})
}

// TestCloudArmorThroughTheBrowser2160 (#2160): Cloud Armor policies' Create
// policy has Google's steps (Configure policy, Add more rules, Apply policy
// to targets) and makes a policy, listed; its Policy details page has
// Details, Rules and Targets, the Rules tab's Add rule adds a rule of
// Basic mode's IP range, listed by its priority. The keyboard subtest
// opens Add rule with Tab and Enter, its first field focused.
func TestCloudArmorThroughTheBrowser2160(t *testing.T) {
	needService(t, "netsecurity-armor")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	name := fmt.Sprintf("armor-%d", time.Now().UnixNano()%1e6)
	p := open(t)
	p.navigate("/net-security/securitypolicies/list" + q)
	p.clickText("#view button", "Create policy")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	if strings.Join(steps, "|") != "Configure policy|Add more rules (optional)|Apply policy to targets (optional)" {
		t.Errorf("Create policy's steps are %v", steps)
	}
	setField(p, "#view", "f-name", name)
	p.clickText(".create-step-continue button", "Continue")
	p.clickText(".create-step-continue button", "Continue")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/securitypolicies/list" && ` + rowReady(name))

	p.navigate("/net-security/securitypolicies/list/global/" + name + q + "&tab=rules")
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent.trim()).join(",")`, &tabs)
	if tabs != "Details,Rules,Targets" {
		t.Errorf("the policy's tabs = %s", tabs)
	}
	p.clickText("#view .section-actions button", "Add rule")
	p.waitFor(`document.querySelector(".modal.is-open #f-srcIpRanges") !== null`)
	p.setField(".modal.is-open #f-srcIpRanges", "192.0.2.0/24")
	p.setField(".modal.is-open #f-action", "deny")
	p.setField(".modal.is-open #f-priority", "1000")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null && ` + rowReady("Priority 1000"))
	code, body := consoleDo(t, http.MethodGet, "/api/detail/netsecurity-armor"+q+"&name=global&name="+name, "")
	if code != http.StatusOK || !strings.Contains(body, `"Action":"Deny (403)"`) || !strings.Contains(body, `"Match":"192.0.2.0/24"`) {
		t.Errorf("the policy's rules read %d %s", code, body)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-security/securitypolicies/list/global/" + name + q + "&tab=rules")
		k.waitFor(`document.querySelector("#view .section-actions button") !== null`)
		k.tabTo(`a.closest(".section-actions") !== null && a.textContent.trim() === "Add rule"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open") !== null && document.querySelector(".modal.is-open").contains(document.activeElement)`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
	})
}

// cdn2160Fixture makes an external Application Load Balancer in front of
// a backend bucket, and returns the bucket backend's name.
func cdn2160Fixture(t *testing.T, project string) string {
	t.Helper()
	name := fmt.Sprintf("cdn-%d", time.Now().UnixNano()%1e6)
	body, _ := json.Marshal(map[string]string{"name": name, "lbType": "application", "facing": "external", "deployment": "global", "protocol": "HTTP",
		"port": "80", "backendType": "bucket", "bucket": name + "-assets"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/netservices-lb?project="+url.QueryEscape(project), string(body)); code != http.StatusOK {
		t.Fatalf("the load balancer: %d %s", code, out)
	}
	return name + "-bucket"
}

// TestCloudCDNThroughTheBrowser2160 (#2160): Cloud CDN's Add origin turns
// Cloud CDN on for a backend bucket, listed with its associated load
// balancer; its Origin details page has Details, Restricted content and
// Cache invalidation, whose Invalidate cache (Host, Path) lists the
// invalidation. The keyboard subtest opens Add signing key with Tab and
// Enter.
func TestCloudCDNThroughTheBrowser2160(t *testing.T) {
	needService(t, "netservices-cdn")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	origin := cdn2160Fixture(t, project)
	p := open(t)
	p.navigate("/net-services/cdn/list" + q)
	p.clickText("#view button", "Add origin")
	p.waitFor(`document.querySelector("#f-origin") !== null`)
	setField(p, "#view", "f-origin", "backendBuckets/global/"+origin)
	var more bool
	p.eval(`[...document.querySelectorAll(".create-step-continue button")].some((b) => b.getClientRects().length)`, &more)
	if more {
		p.clickText(".create-step-continue button", "Continue")
	}
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-services/cdn/list" && ` + rowReady(origin))

	p.navigate("/net-services/cdn/list/backendBuckets/global/" + origin + q + "&tab=invalidation")
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent.trim()).join(",")`, &tabs)
	if tabs != "Details,Restricted content,Cache invalidation" {
		t.Errorf("the origin's tabs = %s", tabs)
	}
	p.clickText("#view .section-actions button", "Invalidate cache")
	p.waitFor(`document.querySelector(".modal.is-open #f-path") !== null`)
	p.setField(".modal.is-open #f-path", "/images/*")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	code, body := consoleDo(t, http.MethodGet, "/api/detail/netservices-cdn"+q+"&name=backendBuckets&name=global&name="+origin, "")
	if code != http.StatusOK || !strings.Contains(body, `"Load balancer":"`+strings.TrimSuffix(origin, "-bucket")+`"`) {
		t.Errorf("the origin's invalidations read %d %s", code, body)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-services/cdn/list/backendBuckets/global/" + origin + q + "&tab=restricted")
		k.waitFor(`document.querySelector("#view .section-actions button") !== null`)
		k.tabTo(`a.closest(".section-actions") !== null && a.textContent.trim() === "Add signing key"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open #f-keyName") !== null`)
		k.run(chromedp.KeyEvent(kb.Escape))
		k.waitFor(`document.querySelector(".modal.is-open") === null`)
	})
}

// TestIAPThroughTheBrowser2160 (#2160): Identity-Aware Proxy lists the
// load balancer's backend service with IAP Off; its Turn on IAP, confirmed
// with Turn On (clicked, nothing typed), turns it On. The keyboard subtest
// reaches the row's Turn off IAP with Tab.
func TestIAPThroughTheBrowser2160(t *testing.T) {
	needService(t, "security-iap")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	lb := lb2160Fixture(t, project)
	bs := lb + "-backend-service"
	p := open(t)
	p.navigate("/security/iap" + q)
	p.waitFor(rowReady(bs))
	p.clickText("#view tbody tr button", "Turn on IAP")
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	p.clickText(".modal.is-open button", "Turn On")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	code, body := consoleDo(t, http.MethodGet, "/api/resources/security-iap"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"IAP":"On"`) {
		t.Errorf("after Turn on IAP the resources read %d %s", code, body)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/security/iap" + q)
		k.waitFor(rowReady(bs))
		k.tabTo(`a.tagName === "BUTTON" && a.textContent.trim() === "Turn off IAP"`)
	})
}

// TestCertificateManagerThroughTheBrowser2160 (#2160): Certificate
// Manager's tabs are Google's (Certificates, Certificate Maps, DNS
// Authorizations, Issuance Configs, Classic Certificates); Create
// certificate map makes a map, listed, whose page has its entries tab.
// The keyboard subtest moves along the tab strip with Tab.
func TestCertificateManagerThroughTheBrowser2160(t *testing.T) {
	needService(t, "ccm-certificates")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	want := "Certificates,Certificate Maps,DNS Authorizations,Issuance Configs,Classic Certificates"
	for i, path := range []string{"/security/ccm/list/certificates", "/security/ccm/list/certificateMaps", "/security/ccm/list/dnsAuthorizations",
		"/security/ccm/list/issuanceConfigs", "/security/ccm/list/lbCertificates"} {
		p.navigate(path + q)
		p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		var strip string
		p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
		names := strings.Split(want, ",")
		names[i] += "*"
		if strip != strings.Join(names, ",") {
			t.Errorf("%s's tabs = %s", path, strip)
		}
	}
	name := fmt.Sprintf("map-%d", time.Now().UnixNano()%1e6)
	p.navigate("/security/ccm/list/certificateMaps" + q)
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	setField(p, "#view", "f-name", name)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/security/ccm/list/certificateMaps" && ` + rowReady(name))
	p.navigate("/security/ccm/list/certificateMaps/global/" + name + q)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent.trim()).join(",")`, &tabs)
	if tabs != "Details,Certificate map entries" {
		t.Errorf("the map's tabs = %s", tabs)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/security/ccm/list/certificates" + q)
		k.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		k.tabTo(`a.closest("nav.page-tabs") !== null && a.textContent === "Certificate Maps"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`location.pathname === "/security/ccm/list/certificateMaps"`)
	})
}

// TestLoadBalancingRound2MeetsWCAG2160 (#2160): Cloud Armor policies, Cloud
// CDN and Identity-Aware Proxy, Certificate Manager's tabs, with their
// create pages, a load balancer's Edit page and a policy's page meet WCAG
// (contrast in both themes, keyboard, names, motion, reflow).
func TestLoadBalancingRound2MeetsWCAG2160(t *testing.T) {
	needService(t, "netsecurity-armor")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	lb := lb2160Fixture(t, project)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Cloud Armor policies", "/net-security/securitypolicies/list", project)...)
	pages = append(pages, listAndCreate("Cloud CDN", "/net-services/cdn/list", project)...)
	pages = append(pages, screen("Identity-Aware Proxy", "/security/iap", project))
	pages = append(pages, listAndCreate("Certificate Manager certificates", "/security/ccm/list/certificates", project)...)
	pages = append(pages, listAndCreate("Certificate maps", "/security/ccm/list/certificateMaps", project)...)
	pages = append(pages, listAndCreate("DNS authorizations", "/security/ccm/list/dnsAuthorizations", project)...)
	pages = append(pages, listAndCreate("Issuance configs", "/security/ccm/list/issuanceConfigs", project)...)
	pages = append(pages, a11yPage{"Load balancer Edit page", "/net-services/loadbalancing/list/loadBalancers/global/" + lb + q + "&edit=1",
		`document.querySelector("#view form.create-page #f-port") !== null`, ""})
	auditScreens(t, pages)
}
