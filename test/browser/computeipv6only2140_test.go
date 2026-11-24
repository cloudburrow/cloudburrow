//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestIPv6OnlySubnetAndInstanceThroughTheBrowser2140 (#2140), in the
// compute shard, as vpc/docs/create-modify-vpc-networks ("Add an IPv6-only
// subnet") and compute/docs/ip-addresses/configure-ipv6-address describe
// them: Create VPC network with IP stack type IPv6 (single-stack) hides the
// IPv4 range and makes a subnet with only its /64; Create instance with IP
// stack type IPv6 (single-stack) on it hides the IPv4 addresses and makes
// a VM with no IPv4 address. Each is read back through the Compute Engine
// API. Needs CLOUDBURROW_TEST_COMPUTE.
func TestIPv6OnlySubnetAndInstanceThroughTheBrowser2140(t *testing.T) {
	needService(t, "compute-instances")
	caddr := endpoint(t, envBDRCompute)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	const vm = "browser-v6only"
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, "http://"+caddr+"/compute/v1/projects/"+project+"/zones/us-central1-a/instances/"+vm, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		time.Sleep(5 * time.Second)
	})
	p := open(t)

	p.navigate("/networking/networks/list/create" + q)
	p.waitFor(`document.querySelector("#f-subnetStackType-2") !== null`)
	p.clickExpr(`document.querySelector("#f-subnetStackType-2")`)
	p.waitFor(`document.querySelector("#f-subnetRange").closest(".form-row").hidden &&
		!document.querySelector("#f-subnetIpv6AccessType").closest(".form-row").hidden`)
	p.submitForm(map[string]string{"name": "browser-v6o", "subnetName": "browser-v6o-a", "subnetRegion": "us-central1",
		"subnetStackType": "IPV6_ONLY", "subnetIpv6AccessType": "External"}, "name")
	p.waitFor(`location.pathname === "/networking/networks/list" && ` + rowReady("browser-v6o"))
	sub := computeDo(t, caddr, "GET", project, "regions/us-central1/subnetworks/browser-v6o-a", nil)
	if sub["stackType"] != "IPV6_ONLY" || sub["ipv6AccessType"] != "EXTERNAL" || sub["ipCidrRange"] != nil {
		t.Fatalf("the IPv6-only subnet reads %v", sub)
	}

	p.navigate("/compute/instances" + q)
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("#f-stackType-2") !== null`)
	p.clickExpr(`document.querySelector("#f-stackType-2")`)
	p.waitFor(`document.querySelector("#f-internalIP").closest(".form-row").hidden &&
		document.querySelector("#f-externalIP").closest(".form-row").hidden`)
	p.submitForm(map[string]string{"name": vm, "machineType": "e2-small", "image": "debian-cloud/debian-12",
		"network": "browser-v6o", "subnetwork": "browser-v6o-a", "stackType": "IPV6_ONLY"}, "name")
	p.waitFor(`location.pathname === "/compute/instances" && ` + rowReady(vm))
	inst := computeDo(t, caddr, "GET", project, "zones/us-central1-a/instances/"+vm, nil)
	var nic map[string]any
	if nics, _ := inst["networkInterfaces"].([]any); len(nics) > 0 {
		nic, _ = nics[0].(map[string]any)
	}
	if nic["stackType"] != "IPV6_ONLY" || nic["networkIP"] != nil || nic["accessConfigs"] != nil {
		t.Fatalf("the IPv6-only instance's interface reads %v", nic)
	}
	if ip, _ := nic["ipv6Address"].(string); ip == "" {
		if acs, _ := nic["ipv6AccessConfigs"].([]any); len(acs) == 0 {
			t.Errorf("the IPv6-only instance has no IPv6 address: %v", nic)
		}
	}
	if s, _ := nic["subnetwork"].(string); !strings.HasSuffix(s, "/subnetworks/browser-v6o-a") {
		t.Errorf("the instance's subnetwork = %q", s)
	}
}

// TestIPv6OnlyFormsMeetWCAG2140 (#2140): Create VPC network and Create
// instance with IP stack type IPv6 (single-stack) chosen, and Add subnet,
// meet WCAG (contrast in both themes, keyboard, names, motion, reflow).
func TestIPv6OnlyFormsMeetWCAG2140(t *testing.T) {
	needService(t, "compute-instances")
	project := servedProject(t)
	pick := func(id string) string {
		return `(() => { const r = document.querySelector("#` + id + `"); if (r) r.click(); return true; })()`
	}
	net := screen("Create VPC network, IPv6 (single-stack)", "/networking/networks/list/create", project)
	net.then = pick("f-subnetStackType-2")
	vm := screen("Create instance, IPv6 (single-stack)", "/compute/instances/create", project)
	vm.then = pick("f-stackType-2")
	add := screen("Add subnet", "/networking/networks/list/default", project)
	add.then = `(() => { const b = [...document.querySelectorAll("#view .page-actions button, #view .section-actions button")].find((b) => b.textContent.trim() === "Add subnet");
		if (b) b.click(); return true; })()`
	auditScreens(t, []a11yPage{net, vm, add})
}
