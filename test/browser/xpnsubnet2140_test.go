//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSharedVPCIndividualSubnetsThroughTheBrowser2140 (#2140), in the
// compute shard: on a Shared VPC host's page, Attach projects offers
// Individual subnets (subnet-level permissions), no longer unavailable,
// and choosing it shows the host's subnets to share; Share subnets grants
// Compute Network User on the chosen subnet (us-central1's default) to a
// principal, which the host's Subnets in host project tab then shows for
// that subnet only; the service project's view lists the host's subnets
// (subnetworks.listUsable).
func TestSharedVPCIndividualSubnetsThroughTheBrowser2140(t *testing.T) {
	needService(t, "compute-instances")
	host, service, _ := sharedVPCProjects(t)
	const member = "user:browser-dev@example.com"
	q := "?project=" + url.QueryEscape(host)
	p := open(t)

	p.navigate("/networking/xpn" + q)
	p.clickText("#view button", "Attach projects")
	p.waitFor(`document.querySelector(".modal.is-open #f-sharing-1") !== null`)
	var disabled bool
	p.eval(`document.querySelector(".modal.is-open #f-sharing-1").disabled`, &disabled)
	if disabled {
		t.Fatal("Individual subnets (subnet-level permissions) is still unavailable")
	}
	p.clickExpr(`document.querySelector(".modal.is-open #f-sharing-1")`)
	p.waitFor(`(() => { const c = document.querySelector('.modal.is-open [id="f-subnet:us-central1/default"]');
		return !!c && !c.closest(".form-row").hidden; })()`)
	p.navigate("/networking/xpn" + q)

	p.clickText("#view button", "Share subnets")
	p.waitFor(`document.querySelector('[id="f-subnet:us-central1/default"]') !== null`)
	p.eval(`(() => { const s = document.querySelector(".modal.is-open") ? ".modal.is-open " : "";
		const c = document.querySelector(s + '[id="f-subnet:us-central1/default"]'); c.checked = true;
		c.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.submitForm(map[string]string{"members": member}, "members")
	p.waitFor(`location.pathname === "/networking/xpn" && document.querySelector(".modal.is-open") === null`)

	code, body := consoleDo(t, http.MethodGet, "/api/resources/vpc-sharedvpc"+q, "")
	if code != http.StatusOK || strings.Count(body, member) != 1 {
		t.Errorf("the host's subnets after Share subnets (%d): want %s on one subnet: %s", code, member, body)
	}
	code, body = consoleDo(t, http.MethodGet, "/api/resources/vpc-sharedvpc?project="+url.QueryEscape(service), "")
	if code != http.StatusOK || !strings.Contains(body, `"Project":"`+host+`"`) || !strings.Contains(body, `"Region":"us-central1"`) {
		t.Errorf("the service project's shared subnets (%d): %s", code, body)
	}
}

// TestSharedVPCShareSubnetsMeetsWCAG2140 (#2140): a Shared VPC host's page
// with its Share subnets form meets WCAG (contrast in both themes,
// keyboard, names, motion, reflow).
func TestSharedVPCShareSubnetsMeetsWCAG2140(t *testing.T) {
	needService(t, "compute-instances")
	host, _, _ := sharedVPCProjects(t)
	auditScreens(t, []a11yPage{screen("Shared VPC host", "/networking/xpn", host)})
}
