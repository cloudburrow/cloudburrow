//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPrivateServiceConnectAndSharedVPCThroughTheBrowser2140 (#2140), in
// the compute shard: Private Service Connect's Connected endpoints and
// Published services tabs; Connect endpoint for All Google APIs made on its
// page (its IP address reserved for it), read back through the console API
// as Accepted, and deleted from the row's menu; the Shared VPC page with
// Set up Shared VPC for a project that is neither host nor service project.
func TestPrivateServiceConnectAndSharedVPCThroughTheBrowser2140(t *testing.T) {
	needService(t, "netservices-psc-endpoints")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	for _, c := range []struct{ path, want string }{
		{"/net-services/psc/list/consumers", "Connected endpoints*,Published services,Connection Policies"},
		{"/net-services/psc/published", "Connected endpoints,Published services*,Connection Policies"},
	} {
		p.navigate(c.path + q)
		p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		var strip string
		p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
		if strip != c.want {
			t.Errorf("%s's tabs = %s, want %s", c.path, strip, c.want)
		}
	}

	p.navigate("/net-services/psc/list/consumers" + q)
	p.clickText("#view button", "Connect endpoint")
	p.submitForm(map[string]string{"name": "browserapis", "network": "default", "ipAddress": "10.255.255.250"}, "name")
	p.waitFor(`location.pathname === "/net-services/psc/list/consumers" && ` + rowReady("browserapis"))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/netservices-psc-endpoints"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"Status":"Accepted"`) || !strings.Contains(body, `"IP address":"10.255.255.250"`) {
		t.Errorf("the endpoint made on the page reads %d %s", code, body)
	}
	vmRowMenu(p, "browserapis", "Delete", nil, byButton)

	p.navigate("/networking/xpn" + q)
	p.waitFor(settled)
	var buttons string
	p.eval(`[...document.querySelectorAll("#view button")].map((b) => b.textContent.trim()).join("|")`, &buttons)
	if !strings.Contains(buttons, "Set up Shared VPC") {
		t.Errorf("Shared VPC's buttons = %s", buttons)
	}
}

// TestPrivateServiceConnectAndSharedVPCMeetWCAG2140 (#2140): Private Service
// Connect's tabs and create pages and the Shared VPC page meet WCAG
// (contrast in both themes, keyboard, names, motion, reflow).
func TestPrivateServiceConnectAndSharedVPCMeetWCAG2140(t *testing.T) {
	needService(t, "netservices-psc-endpoints")
	project := servedProject(t)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Connected endpoints", "/net-services/psc/list/consumers", project)...)
	pages = append(pages, listAndCreate("Published services", "/net-services/psc/published", project)...)
	pages = append(pages, screen("Shared VPC", "/networking/xpn", project))
	auditScreens(t, pages)
}
