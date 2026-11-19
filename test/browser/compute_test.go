//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestVPCNetworkThroughTheBrowser (#1936), in a shard whose instance serves
// the compute service. From the drawer's Networking row, VPC network's
// pages: Create VPC network (custom, with a first subnet) on its create
// page; on the network's page Add subnet and, on the subnet's row, Expand
// IP range; Create firewall rule on Firewall; Create route on Routes;
// Reserve internal static IP address and Allocate IP range for private
// services access on IP addresses, then Release; each change is read back
// through the console API from the compute.Server the Compute Engine API
// serves. An overlapping subnet is refused in the dialog with the API's
// words.
func TestVPCNetworkThroughTheBrowser(t *testing.T) {
	needService(t, "vpc-networks")
	project := uniqueProject(t)
	resources := func(service string) string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+"?project="+project, "")
		if code != http.StatusOK {
			t.Fatalf("list %s through the console API = %d: %s", service, code, body)
		}
		return body
	}
	detail := func(path ...string) string {
		t.Helper()
		q := "/api/detail/vpc-networks?project=" + project
		for _, s := range path {
			q += "&name=" + url.QueryEscape(s)
		}
		code, body := consoleDo(t, http.MethodGet, q, "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	rowWith := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, text)
	}

	p := open(t)
	p.navigateByDrawer("Networking", "/networking/networks/list", "VPC network")
	p.navigate("/networking/networks/list?project=" + project)
	p.waitFor(rowWith("default"))
	p.clickText("#view button", "Create VPC network")
	p.submitForm(map[string]string{"name": "browser-vpc", "subnetName": "browser-sub", "subnetRegion": "us-central1",
		"subnetRange": "10.50.0.0/24"}, "name")
	p.waitFor(`location.pathname === "/networking/networks/list" && ` + rowWith("browser-vpc"))
	if body := detail("browser-vpc"); !strings.Contains(body, "browser-sub") || !strings.Contains(body, "10.50.0.0/24") {
		t.Fatalf("the network made on the page reads %s", body)
	}

	// The network's page: Add subnet, an overlap refused, Expand IP range.
	p.follow("browser-vpc")
	p.waitFor(`location.pathname.endsWith("browser-vpc")`)
	p.pageAction("Add subnet", map[string]string{"name": "browser-sub2", "region": "us-east1", "ipCidrRange": "10.60.0.0/24"}, "")
	if body := detail("browser-vpc"); !strings.Contains(body, "browser-sub2") {
		t.Fatalf("Add subnet: the network reads %s", body)
	}
	p.clickText(".page-actions button, #view button", "Add subnet")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	for id, v := range map[string]string{"name": "clash", "region": "us-east1", "ipCidrRange": "10.60.0.128/25"} {
		p.setField(".modal.is-open #f-"+id, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	if msg := p.modalRefusal(); !strings.Contains(msg, "conflicts with existing subnetwork 'browser-sub2'") {
		t.Errorf("an overlapping subnet was refused with %q", msg)
	}
	// The refusal's 400 is the answer this step expects.
	p.forgive()
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.rowAction("Subnets", "browser-sub2", "Expand IP range", map[string]string{"ipCidrRange": "10.60.0.0/22"}, "")
	if body := detail("browser-vpc", "subnets", "us-east1", "browser-sub2"); !strings.Contains(body, "10.60.0.0/22") {
		t.Errorf("Expand IP range: the subnet reads %s", body)
	}

	// Firewall.
	p.navigate("/networking/firewalls/list?project=" + project)
	p.clickText("#view button", "Create firewall rule")
	p.submitForm(map[string]string{"name": "browser-allow-ssh", "network": "browser-vpc", "sourceRanges": "35.235.240.0/20",
		"protocols": "tcp:22"}, "name")
	p.waitFor(`location.pathname === "/networking/firewalls/list" && ` + rowWith("browser-allow-ssh"))
	if body := resources("vpc-firewalls"); !strings.Contains(body, `"Protocols / ports":"tcp:22"`) {
		t.Errorf("the firewall rule made on the page reads %s", body)
	}

	// Routes.
	p.navigate("/networking/routes/list?project=" + project)
	p.clickText("#view button", "Create route")
	p.submitForm(map[string]string{"name": "browser-route", "network": "browser-vpc", "destRange": "192.168.10.0/24",
		"nextHop": "Specify an IP address", "nextHopIp": "10.50.0.9"}, "name")
	p.waitFor(`location.pathname === "/networking/routes/list" && ` + rowWith("browser-route"))
	if body := resources("vpc-routes"); !strings.Contains(body, "10.50.0.9") {
		t.Errorf("the route made on the page reads %s", body)
	}

	// IP addresses: an internal address and a private services access range.
	p.navigate("/networking/addresses/list?project=" + project)
	vpcListAction(p, "reserve-internal", map[string]string{"name": "browser-int", "network": "browser-vpc", "region": "us-central1",
		"subnetwork": "browser-sub", "address": "10.50.0.20"})
	vpcListAction(p, "allocate-psa", map[string]string{"name": "browser-psa", "network": "browser-vpc", "prefixLength": "20"})
	p.waitFor(rowWith("browser-int") + " && " + rowWith("browser-psa"))
	var l struct {
		Items []struct {
			Name   string
			Fields map[string]string
			Target []string
		}
	}
	if err := json.Unmarshal([]byte(resources("vpc-addresses")), &l); err != nil || len(l.Items) != 2 {
		t.Fatalf("addresses after Reserve: %v %+v", err, l)
	}
	for _, it := range l.Items {
		if it.Name == "browser-int" && it.Fields["IP address"] != "10.50.0.20" {
			t.Errorf("browser-int is %s", it.Fields["IP address"])
		}
	}
	// Release, from each row's menu.
	for _, name := range []string{"browser-int", "browser-psa"} {
		find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, name)
		p.waitFor(find + ` !== undefined`)
		p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
		p.clickText(".overflow-item", "Release static address")
		p.confirmByName(name)
		p.waitFor(`document.querySelector(".modal") === null && !` + rowWith(name))
	}

	// Clean up through the console API, in the order the API allows.
	consoleDo(t, http.MethodDelete, "/api/resources/vpc-routes?project="+project+"&name=browser-route", "")
	consoleDo(t, http.MethodDelete, "/api/resources/vpc-firewalls?project="+project+"&name=browser-allow-ssh", "")
	for _, sub := range [][]string{{"us-central1", "browser-sub"}, {"us-east1", "browser-sub2"}} {
		b, _ := json.Marshal(map[string]any{"Path": []string{"browser-vpc", "subnets", sub[0], sub[1]}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/vpc-networks?project="+project, string(b))
	}
	if code, body := consoleDo(t, http.MethodDelete, "/api/resources/vpc-networks?project="+project+"&name=browser-vpc", ""); code != http.StatusOK {
		t.Errorf("delete browser-vpc = %d: %s", code, body)
	}
}

// vpcListAction runs a list screen's own action (#list-action-ID) with the
// values given and waits for its dialog to close.
func vpcListAction(p *tab, id string, values map[string]string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	btn := "#list-action-" + id
	p.waitFor(fmt.Sprintf(`(() => { const b = document.querySelector(%q); return !!b && b.getClientRects().length > 0; })()`, btn))
	p.run(chromedp.Click(chromedp.CSS(btn)))
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	for f, v := range values {
		p.setField(".modal.is-open #f-"+f, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var refusal string
	p.eval(`[...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refusal)
	if refusal != "" {
		p.t.Fatalf("%s was refused: %s", id, refusal)
	}
}
