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

// Secure Web Proxy (#2160): Network Security's Secure Web Proxy page with
// its Web proxies, Policies, URL lists and TLS inspection policies tabs.

// TestSecureWebProxyThroughTheBrowser2160 (#2160): the page's four tabs;
// Create a policy makes one, whose page opens on Rules, where Add rule
// (Priority, Name, Description, Action, Status, Session Match,
// Application Match) adds a rule listed with its session match; Create a
// secure web proxy has the quickstart's fields in its order and makes a
// web proxy, listed with the address it listens at. The keyboard subtest
// reaches Add rule with Tab and opens its dialog with Enter.
func TestSecureWebProxyThroughTheBrowser2160(t *testing.T) {
	needService(t, "netsecurity-swp-proxies")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	sfx := time.Now().UnixNano() % 1e6
	pol, gw := fmt.Sprintf("swpol-%d", sfx), fmt.Sprintf("swp-%d", sfx)
	region := "us-central1"
	lb3Compute(t, http.MethodPost, project, "regions/"+region+"/subnetworks", map[string]any{"name": "proxy-only", "network": "global/networks/default",
		"ipCidrRange": "10.10.0.0/23", "purpose": "REGIONAL_MANAGED_PROXY", "role": "ACTIVE"})

	p := open(t)
	p.navigate("/net-security/swp/policies/list" + q)
	p.waitFor(settled)
	if got := pageTabs(p); got != "Web proxies,Policies*,URL lists,TLS inspection policies" {
		t.Errorf("the tabs read %q", got)
	}
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-name") !== null`)
	setField(p, "#view", "f-name", pol)
	setField(p, "#view", "f-description", "basic Secure Web Proxy policy")
	setField(p, "#view", "f-region", region)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/swp/policies/list" && ` + rowReady(pol))

	p.navigate("/net-security/swp/policies/list/" + region + "/" + pol + q)
	p.waitFor(settled + ` && [...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Add rule")`)
	p.clickText("#view button", "Add rule")
	p.waitFor(`document.querySelector(".modal.is-open #f-sessionMatcher") !== null`)
	var labels string
	p.eval(`[...document.querySelectorAll(".modal.is-open label")].map((l) => l.textContent.trim()).filter((s) => s).join("|")`, &labels)
	for _, l := range []string{"Priority", "Name", "Description", "Action", "Status", "Session Match", "Application Match"} {
		if !strings.Contains(labels, l) {
			t.Errorf("Add rule has no %q: %s", l, labels)
		}
	}
	p.setField(".modal.is-open #f-priority", "1")
	p.setField(".modal.is-open #f-name", "allow-example")
	p.setField(".modal.is-open #f-sessionMatcher", "host() == 'www.example.com'")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null && document.querySelector("#view").textContent.includes("host() == 'www.example.com'")`)

	p.navigate("/net-security/swp/web-proxies/list" + q)
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-ports") !== null`)
	var order string
	p.eval(`[...document.querySelectorAll("#view form [id^=f-]")].map((f) => f.id.replace(/^f-/, "")).filter((v, i, a) => a.indexOf(v) === i).join(",")`, &order)
	if !strings.HasPrefix(order, "name,description,routingMode") || !strings.Contains(order, "region,network,subnetwork,address,ports,certificate,policy") {
		t.Errorf("Create a secure web proxy's fields: %s", order)
	}
	setField(p, "#view", "f-name", gw)
	setField(p, "#view", "f-region", region)
	setField(p, "#view", "f-ports", "443")
	setField(p, "#view", "f-policy", pol)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/swp/web-proxies/list" && ` + rowReady(gw))
	var row string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent).join("\n")`, &row)
	if !strings.Contains(row, "443=127.0.0.1:") || !strings.Contains(row, pol) {
		t.Errorf("the web proxy's row reads %q", row)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-security/swp/policies/list/" + region + "/" + pol + q)
		k.waitFor(settled + ` && [...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Add rule")`)
		k.tabTo(`a.tagName === "BUTTON" && a.textContent.trim() === "Add rule"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".modal.is-open") !== null && document.querySelector(".modal.is-open").contains(document.activeElement)`)
		k.run(chromedp.KeyEvent(kb.Escape))
	})

	for _, path := range []string{"/api/actions/netsecurity-swp-proxies", "/api/actions/netsecurity-swp-policies"} {
		name := gw
		if strings.HasSuffix(path, "policies") {
			name = pol
		}
		b, _ := json.Marshal(map[string]any{"Path": []string{region, name}, "Action": "delete"})
		if code, out := consoleDo(t, http.MethodPost, path+q, string(b)); code != http.StatusOK {
			t.Errorf("delete %s: %d %s", name, code, out)
		}
	}
}

// TestSecureWebProxyMeetsWCAG2160 (#2160): the four tabs with their
// create pages, a policy's page and its Add rule dialog meet WCAG
// (contrast in both themes, keyboard, names, motion, reflow).
func TestSecureWebProxyMeetsWCAG2160(t *testing.T) {
	needService(t, "netsecurity-swp-policies")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	pol := fmt.Sprintf("swpw-%d", time.Now().UnixNano()%1e6)
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/netsecurity-swp-policies"+q, `{"name":"`+pol+`","region":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("the policy: %d %s", code, out)
	}
	var pages []a11yPage
	pages = append(pages, listAndCreate("Web proxies", "/net-security/swp/web-proxies/list", project)...)
	pages = append(pages, listAndCreate("SWP Policies", "/net-security/swp/policies/list", project)...)
	pages = append(pages, listAndCreate("URL lists", "/net-security/swp/urllists/list", project)...)
	pages = append(pages, listAndCreate("TLS inspection policies", "/net-security/swp/tls-inspection/list", project)...)
	detail := "/net-security/swp/policies/list/us-central1/" + pol + q
	pages = append(pages, a11yPage{"SWP policy", detail, settled + ` && document.querySelector("#view [role=tab]") !== null`, ""})
	pages = append(pages, a11yPage{"Add rule", detail, settled + ` && [...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Add rule")`,
		`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Add rule").click()`})
	auditScreens(t, pages)
}
