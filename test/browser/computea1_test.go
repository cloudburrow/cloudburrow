//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestComputeA1Screens (#2139, #2140), in the compute shard: Create a
// disk's Regional location with its Replica zone makes a regional disk the
// Disks list shows in both zones; Create VPC network with ULA internal
// IPv6 and a dual-stack subnet, the Subnets list's IP stack type and IPv6
// columns; Reserve external static IP address, its row RESERVED. Each is
// read back through the console API and deleted from its row's menu.
func TestComputeA1Screens(t *testing.T) {
	needService(t, "compute-instances")
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
	eventually := func(service, want string) string {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			body := list(service)
			if strings.Contains(body, want) {
				return body
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never showed %s: %s", service, want, body)
			}
			time.Sleep(2 * time.Second)
		}
	}
	p := open(t)

	// A regional disk.
	p.navigate("/compute/disks" + q)
	p.clickText("#view button", "Create disk")
	p.submitForm(map[string]string{"name": "browser-ha", "location": "Regional", "zone": "us-central1-a", "replicaZone": "us-central1-b",
		"type": "pd-balanced", "sizeGb": "10"}, "name")
	p.waitFor(`location.pathname === "/compute/disks" && ` + rowReady("browser-ha"))
	eventually("compute-disks", `"Zone":"us-central1-a, us-central1-b"`)
	vmRowMenu(p, "browser-ha", "Delete", nil, "browser-ha")

	// A dual-stack network and subnet.
	p.navigate("/networking/networks/list" + q)
	p.clickText("#view button", "Create VPC network")
	p.submitForm(map[string]string{"name": "browser-v6", "ulaIpv6": "true", "subnetName": "browser-v6a", "subnetRegion": "us-central1",
		"subnetRange": "10.70.0.0/24", "subnetStackType": "IPV4_IPV6", "subnetIpv6AccessType": "Internal"}, "name")
	p.waitFor(`location.pathname === "/networking/networks/list" && ` + rowReady("browser-v6"))
	body := eventually("vpc-subnets", `"IP stack type":"IPv4 and IPv6 (dual-stack)"`)
	if !strings.Contains(body, `"Internal IPv6 ranges":"fd`) {
		t.Errorf("the dual-stack subnet reads %s", body)
	}
	p.navigate("/networking/subnets/list" + q)
	p.waitFor(rowReady("browser-v6a"))
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).join(",")`, &heads)
	if !strings.Contains(heads, "IP stack type") || !strings.Contains(heads, "Internal IPv6 ranges") || !strings.Contains(heads, "External IPv6 ranges") {
		t.Errorf("the Subnets list's columns = %s", heads)
	}

	// A reserved external address.
	p.navigate("/networking/addresses/list" + q)
	vpcListAction(p, "reserve-external", map[string]string{"name": "browser-ext", "ipType": "Regional", "region": "us-central1", "attachTo": "None"})
	eventually("vpc-addresses", `"browser-ext"`)
	p.navigate("/networking/addresses/list" + q)
	p.waitFor(rowReady("browser-ext"))
	vmRowMenu(p, "browser-ext", "Release static address", nil, "browser-ext")
}

// TestComputeA1ScreensMeetWCAG (#2139, #2140): the pages A1 changed meet
// WCAG (contrast in both themes, keyboard, names, motion, reflow): Create
// a disk, Create VPC network, the Subnets list and External IP addresses.
func TestComputeA1ScreensMeetWCAG(t *testing.T) {
	needService(t, "compute-instances")
	project := servedProject(t)
	pages := append(listAndCreate("Disks", "/compute/disks", project),
		screen("Create VPC network", "/networking/networks/list/create", project),
		screen("Subnets", "/networking/subnets/list", project),
		screen("External IP addresses", "/networking/addresses/list", project),
		screen("Machine images", "/compute/machineImages", project))
	auditScreens(t, pages)
}
