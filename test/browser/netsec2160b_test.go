//go:build browser

package browser

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestAddressGroupsThroughTheBrowser2160 (#2160): Network Security's
// Address groups page, whose Create makes an IPv4 group for Cloud Armor
// from pasted IP addresses, listed with its type, purpose, capacity and
// item count, and opened on its details page with its IP addresses and
// Edit; the page offers the Cloud Armor tier action. The keyboard subtest
// opens Create with Tab and Enter alone.
func TestAddressGroupsThroughTheBrowser2160(t *testing.T) {
	needService(t, "netsecurity-addressgroups")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	name := fmt.Sprintf("ag-%d", time.Now().UnixNano()%1e6)
	p.navigate("/net-security/addressgroups" + q)
	p.waitFor(settled)
	var tier bool
	p.eval(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Cloud Armor tier")`, &tier)
	if !tier {
		t.Error("the Address groups page has no Cloud Armor tier action")
	}
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-name") !== null && document.querySelector("#f-items") !== null && document.querySelector("#f-capacity") !== null`)
	setField(p, "#view", "f-name", name)
	setField(p, "#view", "f-capacity", "10")
	setField(p, "#view", "f-items", "198.51.100.0/24\n203.0.113.7")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/addressgroups" && ` + rowReady(name))
	var row string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))?.textContent ?? ""`, name), &row)
	if !containsAll(row, "IPv4", "Cloud Armor", "10", "Global") {
		t.Errorf("the address group's row = %q", row)
	}
	p.navigate("/net-security/addressgroups/global/" + name + q)
	p.waitFor(`document.body.textContent.includes("Address group details")`)
	var body string
	p.eval(`document.querySelector("#view").textContent`, &body)
	if !containsAll(body, "IP addresses", "198.51.100.0/24", "203.0.113.7", "In use by") {
		t.Errorf("the details page = %q", body)
	}
	var hasEdit bool
	p.eval(`[...document.querySelectorAll("#view button, #view a")].some((b) => b.textContent.trim() === "Edit")`, &hasEdit)
	if !hasEdit {
		t.Error("the address group's page has no Edit")
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-security/addressgroups" + q)
		k.waitFor(settled)
		k.tabTo(`a.tagName === "BUTTON" && a.textContent.trim() === "Create"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#f-name") !== null`)
	})
}

// TestBackendAuthenticationThroughTheBrowser2160 (#2160): the
// Authentication Configuration page's Backend Authentication tab, as
// load-balancing/docs/backend-authenticated-tls-setup has it: Create, a
// name, Location, the public roots of trust, and the config listed with
// them. The keyboard subtest reaches the tab with Tab and Enter alone.
func TestBackendAuthenticationThroughTheBrowser2160(t *testing.T) {
	needService(t, "netsecurity-backendauth")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	name := fmt.Sprintf("bac-%d", time.Now().UnixNano()%1e6)
	p.navigate("/net-security/authconfig/backendAuthentication" + q)
	p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs a[aria-current]")?.textContent === "Backend Authentication"`)
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-name") !== null && document.querySelector("#f-publicRoots") !== null && document.querySelector("#f-trustConfig") !== null`)
	setField(p, "#view", "f-name", name)
	p.run(chromedp.Click(chromedp.CSS(`#f-publicRoots`)))
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/authconfig/backendAuthentication" && ` + rowReady(name))
	var row string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))?.textContent ?? ""`, name), &row)
	if !containsAll(row, "Global", "Enabled") {
		t.Errorf("the Backend Authentication row = %q", row)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-security/authconfig" + q)
		k.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		k.tabTo(`a.closest("nav.page-tabs") !== null && a.textContent === "Backend Authentication"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`location.pathname === "/net-security/authconfig/backendAuthentication"`)
	})
}

// TestNetworkSecurityRound4MeetsWCAG2160 (#2160): Address groups and
// Backend Authentication, and their Create pages, meet WCAG (contrast in
// both themes, keyboard, names, motion, reflow).
func TestNetworkSecurityRound4MeetsWCAG2160(t *testing.T) {
	needService(t, "netsecurity-addressgroups")
	project := servedProject(t)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Address groups", "/net-security/addressgroups", project)...)
	pages = append(pages, listAndCreate("Backend Authentication", "/net-security/authconfig/backendAuthentication", project)...)
	auditScreens(t, pages)
}
