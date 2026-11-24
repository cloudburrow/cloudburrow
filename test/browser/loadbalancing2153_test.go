//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// lbFixtureGroup makes an unmanaged instance group for the wizard's
// backend, through the console's Instance groups page.
func lbFixtureGroup(t *testing.T, project string) string {
	t.Helper()
	name := fmt.Sprintf("lb-web-%d", time.Now().UnixNano()%1e6)
	code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-groups?project="+url.QueryEscape(project),
		fmt.Sprintf(`{"name":%q,"kind":"New unmanaged instance group","zone":"us-central1-a","network":"default"}`, name))
	if code != http.StatusOK {
		t.Fatalf("the instance group: %d %s", code, body)
	}
	return name
}

// TestLoadBalancingWizardThroughTheBrowser2153 (#2153), in the served
// shard: Load balancing's Load balancers, Backends and Frontends tabs;
// Create load balancer's steps (Load balancer type, Frontend
// configuration, Backend configuration, Routing rules) make an external
// Application Load Balancer in front of an instance group, read back
// through the console API with its frontend's local address; its page has
// Frontend, Routing rules and Backend; it is deleted from the row's menu.
// The keyboard subtest walks the steps with Tab and Enter alone.
func TestLoadBalancingWizardThroughTheBrowser2153(t *testing.T) {
	needService(t, "netservices-lb")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	group := lbFixtureGroup(t, project)
	p := open(t)
	for _, c := range []struct{ path, want string }{
		{"/net-services/loadbalancing/list/loadBalancers", "Load balancers*,Backends,Frontends"},
		{"/net-services/loadbalancing/list/backends", "Load balancers,Backends*,Frontends"},
		{"/net-services/loadbalancing/list/frontends", "Load balancers,Backends,Frontends*"},
	} {
		p.navigate(c.path + q)
		p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		var strip string
		p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
		if strip != c.want {
			t.Errorf("%s's tabs = %s, want %s", c.path, strip, c.want)
		}
	}

	p.navigate("/net-services/loadbalancing/list/loadBalancers" + q)
	p.clickText("#view button", "Create load balancer")
	p.waitFor(`document.querySelector("#f-lbType") !== null || document.querySelector("[name=lbType]") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	if strings.Join(steps, "|") != "Load balancer type|Frontend configuration|Backend configuration|Routing rules" {
		t.Errorf("the wizard's steps are %v", steps)
	}
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-name", "browserlb")
	setField(p, "#view", "f-port", "80")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-instanceGroup", "us-central1-a/"+group)
	setField(p, "#view", "f-portName", "http")
	p.clickText(".create-step-continue button", "Continue")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-services/loadbalancing/list/loadBalancers" && ` + rowReady("browserlb"))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/netservices-lb"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"Load balancer type":"Application"`) || !strings.Contains(body, `"Protocols":"HTTP"`) {
		t.Errorf("the load balancer made on the page reads %d %s", code, body)
	}
	code, body = consoleDo(t, http.MethodGet, "/api/resources/netservices-lb-frontends"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"Address":"127.0.0.1:80"`) || !strings.Contains(body, `"Local address":"127.0.0.1:`) {
		t.Errorf("the frontend reads %d %s", code, body)
	}
	p.navigate("/net-services/loadbalancing/list/loadBalancers/global/browserlb" + q)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent.trim()).join(",")`, &tabs)
	if tabs != "Details,Frontend,Routing rules,Backend" {
		t.Errorf("the load balancer's tabs = %s", tabs)
	}
	p.navigate("/net-services/loadbalancing/list/loadBalancers" + q)
	vmRowMenu(p, "browserlb", "Delete", nil, byButton)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-services/loadbalancing/list/loadBalancers/create" + q)
		k.waitFor(`document.querySelector(".create-step-continue button") !== null`)
		// Tab to the first step's Continue and press Enter: the second
		// step opens with its name field, which typing fills.
		k.tabTo(`a.closest(".create-step-continue") !== null && a.textContent.trim() === "Continue"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`(() => { const f = document.querySelector("#f-name"); return !!f && f.getClientRects().length > 0; })()`)
		k.tabTo(`a.id === "f-name"`)
		k.run(chromedp.KeyEvent("kblb"))
		var v string
		k.eval(`document.querySelector("#f-name").value`, &v)
		if v != "kblb" {
			t.Errorf("typing into Load balancer name gave %q", v)
		}
		// Shift+Tab goes back to where it came from.
		k.run(chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(chromedp.ModifierShift)))
		var back bool
		k.eval(`document.activeElement !== null && document.activeElement.id !== "f-name"`, &back)
		if !back {
			t.Error("Shift+Tab stayed on Load balancer name")
		}
	})
}

// TestLoadBalancingMeetsWCAG2153 (#2153): Load balancing's tabs, Create
// load balancer, Health checks, Network endpoint groups and Classic
// Certificates, with their create pages, meet WCAG (contrast in both
// themes, keyboard, names, motion, reflow).
func TestLoadBalancingMeetsWCAG2153(t *testing.T) {
	needService(t, "netservices-lb")
	project := servedProject(t)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Load balancers", "/net-services/loadbalancing/list/loadBalancers", project)...)
	pages = append(pages, screen("Backends", "/net-services/loadbalancing/list/backends", project),
		screen("Frontends", "/net-services/loadbalancing/list/frontends", project))
	pages = append(pages, listAndCreate("Health checks", "/compute/healthChecks", project)...)
	pages = append(pages, listAndCreate("Network endpoint groups", "/compute/networkendpointgroups/list", project)...)
	pages = append(pages, listAndCreate("Classic Certificates", "/security/ccm/list/lbCertificates", project)...)
	auditScreens(t, pages)
}
