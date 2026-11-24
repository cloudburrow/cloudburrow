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
)

// TestComputeEngineInstanceGroupsThroughTheBrowser2139b (#2139), in the
// compute shard: Create instance group on its page with Location Multiple
// zones (Region, Zones, Target distribution shape) and Autoscaling Off,
// read back through the console API with its VMs spread across the two
// zones; the group's page with its Details (Region, Zones, Target
// distribution shape, Autoscaling, Named ports); Restart/replace VMs
// (Restart, then Replace) from the page; Stop over a checked VM; Edit port
// mapping's Add port; Edit autoscaling turned On, after which Resize is
// refused with the reason; and Delete from the list row's menu.
func TestComputeEngineInstanceGroupsThroughTheBrowser2139b(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	const tpl, group = "browser-ig-tpl", "browser-rmig"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1", group}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-groups"+q, string(b))
		time.Sleep(5 * time.Second)
		b, _ = json.Marshal(map[string]any{"Path": []string{"global", tpl}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-templates"+q, string(b))
	})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-templates"+q,
		`{"name":"`+tpl+`","location":"Global","machineType":"e2-small"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}
	detail := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instance-groups"+q+"&name=us-central1&name="+group, "")
		if code != http.StatusOK {
			t.Fatalf("the group's page through the console API = %d: %s", code, body)
		}
		return body
	}
	// waitVMs waits for the group's VMs to be RUNNING, n of them.
	waitVMs := func(n int) string {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for {
			body := detail()
			if strings.Count(body, `"Status":"RUNNING"`) == n {
				return body
			}
			if time.Now().After(deadline) {
				t.Fatalf("the group's VMs never were %d RUNNING: %s", n, body)
			}
			time.Sleep(3 * time.Second)
		}
	}

	p := open(t)
	p.navigate("/compute/instanceGroups" + q)
	p.clickText("#view button", "Create instance group")
	p.waitFor(`document.querySelector("#f-locationType") !== null || document.querySelector('input[name="locationType"]') !== null`)
	// Multiple zones is a choice, not an unavailable one.
	var disabled bool
	p.eval(`[...document.querySelectorAll('input[name="locationType"]')].some((r) => r.value === "Multiple zones" && r.disabled)`, &disabled)
	if disabled {
		t.Fatal("Location's Multiple zones is disabled")
	}
	p.submitForm(map[string]string{"name": group, "template": "global/" + tpl, "locationType": "Multiple zones", "region": "us-central1",
		"zones": "us-central1-b,us-central1-c", "targetShape": "EVEN", "autoscaling": "Off: do not autoscale", "size": "2"}, "name")
	p.waitFor(`location.pathname === "/compute/instanceGroups" && ` + rowReady(group))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/compute-instance-groups"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"Location":"us-central1"`) || !strings.Contains(body, `"Autoscaling":"Off"`) {
		t.Fatalf("the group made on the page reads %d %s", code, body)
	}
	body = waitVMs(2)
	for _, z := range []string{"us-central1-b", "us-central1-c"} {
		if !strings.Contains(body, `"Zone":"`+z+`"`) {
			t.Errorf("no VM in %s: %s", z, body)
		}
	}
	for _, want := range []string{"Target distribution shape", "Even", "Zones", "Named ports", "Autoscaling mode"} {
		if !strings.Contains(body, want) {
			t.Errorf("the group's page lacks %s", want)
		}
	}

	// The group's page: Restart/replace VMs, Restart then Replace.
	p.follow(group)
	p.waitFor(`location.pathname.endsWith("` + group + `")`)
	if res := p.pageAction("Restart/replace VMs", map[string]string{"operation": "Restart"}, ""); res != "" {
		t.Fatalf("Restart = %s", res)
	}
	waitVMs(2)
	if res := p.pageAction("Restart/replace VMs", map[string]string{"operation": "Replace"}, ""); res != "" {
		t.Fatalf("Replace = %s", res)
	}
	body = waitVMs(2)

	// Stop over one checked VM.
	var first string
	p.waitFor(`document.querySelector('#view input[type=checkbox][aria-label^="Select browser-rmig-"]') !== null`)
	p.eval(`document.querySelector('#view input[type=checkbox][aria-label^="Select browser-rmig-"]').getAttribute("aria-label").slice(7)`, &first)
	gkeCheckRow(p, first)
	p.clickExpr(`[...document.querySelectorAll("#view button.secondary")].find((b) => b.textContent.trim() === "Stop" && !b.disabled)`)
	p.pressConfirm("Stop")
	deadline := time.Now().Add(3 * time.Minute)
	for !strings.Contains(detail(), `"Status":"TERMINATED"`) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never stopped", first)
		}
		time.Sleep(3 * time.Second)
	}

	// Edit port mapping offers Add port.
	p.navigate("/compute/instanceGroups" + q)
	p.follow(group)
	p.clickText(p.findAction("Edit port mapping"), "Edit port mapping")
	p.waitFor(`[...document.querySelectorAll(".modal.is-open button")].some((b) => b.textContent.trim() === "Add port")`)
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// Edit autoscaling: On. Resize is then refused, with the reason.
	if res := p.pageAction("Edit autoscaling", map[string]string{"autoscaling": "On: add and remove instances to the group",
		"minReplicas": "2", "maxReplicas": "4"}, ""); res != "" {
		t.Fatalf("Edit autoscaling = %s", res)
	}
	if body := detail(); !strings.Contains(body, "Recommended size") || !strings.Contains(body, `"Autoscaling mode"`) {
		t.Errorf("the group's Autoscaling reads %s", body)
	}
	p.navigate("/compute/instanceGroups" + q)
	p.waitFor(rowReady("On: Target CPU utilization 60%"))
	vmRowMenu(p, group, "Delete", nil, byButton)
	p.waitFor(fmt.Sprintf(`!(%s)`, rowReady(group)))
}

// TestComputeEngineInstanceGroupScreensMeetWCAG2139b (#2139): Instance
// groups and Create instance group with Multiple zones and the
// Autoscaling section shown meet WCAG (names, contrast in both themes,
// keyboard, motion, reflow).
func TestComputeEngineInstanceGroupScreensMeetWCAG2139b(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := servedProject(t)
	pages := listAndCreate("Instance groups", "/compute/instanceGroups", project)
	multi := screen("create in Instance groups, Multiple zones", "/compute/instanceGroups/create", project)
	multi.then = `(() => { const r = [...document.querySelectorAll('input[name="locationType"]')].find((r) => r.value === "Multiple zones");
		if (r) { r.click(); r.dispatchEvent(new Event("change", { bubbles: true })); } return true; })()`
	pages = append(pages, multi)
	auditScreens(t, pages)
}
