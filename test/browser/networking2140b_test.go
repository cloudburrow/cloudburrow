//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestCloudRouterAndNATThroughTheBrowser (#2140), in the compute shard:
// Network Connectivity's Cloud Routers page and Network services' Cloud
// NAT page. Create a router is made on its page with a private ASN, read
// back through the console API; Create Cloud NAT gateway makes a gateway
// on a new router (Create new router) with Automatic addresses; both are
// deleted from their rows' menus.
func TestCloudRouterAndNATThroughTheBrowser(t *testing.T) {
	needService(t, "hybrid-routers")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	list := func(service string) string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+q, "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", service, code, body)
		}
		return body
	}
	p := open(t)

	p.navigate("/hybrid/routers/list" + q)
	p.clickText("#view button", "Create router")
	p.submitForm(map[string]string{"name": "browser-router", "network": "default", "region": "us-central1", "asn": "64514"}, "name")
	p.waitFor(`location.pathname === "/hybrid/routers/list" && ` + rowReady("browser-router"))
	if body := list("hybrid-routers"); !strings.Contains(body, `"Google ASN":"64514"`) {
		t.Errorf("the router made on the page reads %s", body)
	}

	p.navigate("/net-services/nat/list" + q)
	p.clickText("#view button", "Create Cloud NAT gateway")
	p.submitForm(map[string]string{"name": "browser-nat", "network": "default", "region": "us-east1", "router": "__new__",
		"routerName": "browser-nat-router"}, "name")
	p.waitFor(`location.pathname === "/net-services/nat/list" && ` + rowReady("browser-nat"))
	if body := list("netservices-nat"); !strings.Contains(body, `"Cloud Router":"browser-nat-router"`) || !strings.Contains(body, `"NAT IP addresses":"Automatic"`) {
		t.Errorf("the gateway made on the page reads %s", body)
	}
	vmRowMenu(p, "browser-nat", "Delete", nil, byButton)
	p.waitFor(`!(` + rowReady("browser-nat") + `)`)

	p.navigate("/hybrid/routers/list" + q)
	vmRowMenu(p, "browser-nat-router", "Delete", nil, byButton)
	vmRowMenu(p, "browser-router", "Delete", nil, byButton)
	p.waitFor(`!(` + rowReady("browser-router") + `)`)
}

// TestCloudRouterAndNATMeetWCAG2140 (#2140): the Cloud Routers and Cloud
// NAT pages and their create pages meet WCAG (contrast in both themes,
// keyboard, names, motion, reflow), as the compute shard's other screens
// do.
func TestCloudRouterAndNATMeetWCAG2140(t *testing.T) {
	needService(t, "hybrid-routers")
	project := servedProject(t)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Cloud Routers", "/hybrid/routers/list", project)...)
	pages = append(pages, listAndCreate("Cloud NAT", "/net-services/nat/list", project)...)
	auditScreens(t, pages)
}
