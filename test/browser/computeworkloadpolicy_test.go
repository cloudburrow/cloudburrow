//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestComputeGroupWorkloadPolicyThroughTheBrowser (#2206), in the compute
// shard: a workload policy made through the Compute Engine API; Create
// instance group's Workload policy field makes a zonal group of no VMs
// that uses it, as the API reads back, and the group's Details name it.
// Its WCAG subtest audits Create instance group and the group's page; its
// keyboard subtest reaches the Workload policy field by Tab and types in
// it. Needs CLOUDBURROW_TEST_COMPUTE.
func TestComputeGroupWorkloadPolicyThroughTheBrowser(t *testing.T) {
	needService(t, "compute-instance-groups")
	caddr := endpoint(t, envBDRCompute)
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	const tpl, group, pol = "browser-wl-tpl", "browser-wl-mig", "browser-wl-ht"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", group}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-groups"+q, string(b))
		time.Sleep(5 * time.Second)
		b, _ = json.Marshal(map[string]any{"Path": []string{"global", tpl}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-templates"+q, string(b))
	})
	computeDo(t, caddr, "POST", project, "regions/us-central1/resourcePolicies", map[string]any{"name": pol,
		"workloadPolicy": map[string]any{"type": "HIGH_THROUGHPUT", "maxTopologyDistance": "BLOCK"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-templates"+q,
		`{"name":"`+tpl+`","location":"Global","machineType":"e2-small"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}

	p := open(t)
	p.navigate("/compute/instanceGroups" + q)
	p.clickText("#view button", "Create instance group")
	p.waitFor(`document.querySelector("#f-workloadPolicy") !== null`)
	p.submitForm(map[string]string{"name": group, "template": "global/" + tpl, "locationType": "Single zone", "zone": "us-central1-a",
		"autoscaling": "Off: do not autoscale", "size": "0", "workloadPolicy": pol}, "name")
	p.waitFor(`location.pathname === "/compute/instanceGroups" && ` + rowReady(group))
	m := computeDo(t, caddr, "GET", project, "zones/us-central1-a/instanceGroupManagers/"+group, nil)
	rp, _ := m["resourcePolicies"].(map[string]any)
	if wp, _ := rp["workloadPolicy"].(string); !strings.HasSuffix(wp, "/regions/us-central1/resourcePolicies/"+pol) {
		t.Fatalf("the group made on the page has workload policy %v", m["resourcePolicies"])
	}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instance-groups"+q+"&name=us-central1-a&name="+group, "")
	if code != http.StatusOK || !strings.Contains(body, `"Label":"Workload policy","Value":"`+pol+`"`) {
		t.Errorf("the group's Details don't name its workload policy: %d %.800s", code, body)
	}
	p.follow(group)
	p.waitFor(`location.pathname.endsWith("` + group + `")`)
	var path string
	p.eval(`location.pathname`, &path)
	groupPage := path + q

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"Create instance group", "/compute/instanceGroups/create" + q, `document.querySelector("#f-workloadPolicy") !== null`, ""},
			{"a group with a workload policy", groupPage, settled, ""},
		})
	})
	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/compute/instanceGroups/create" + q)
		k.waitFor(`document.querySelector("#f-workloadPolicy") !== null`)
		k.tabTo(`document.activeElement.id === "f-workloadPolicy"`)
		k.run(chromedp.KeyEvent("w"))
		k.waitFor(`document.querySelector("#f-workloadPolicy").value.endsWith("w")`)
	})
}
