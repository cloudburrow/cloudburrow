//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// migAutohealing makes a TCP health check, a template and an autohealing
// managed group of one through the console API (Create instance group's
// Autohealing section), and returns the console query.
func migAutohealing(t *testing.T, project, hc, tpl, group string) string {
	t.Helper()
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-healthchecks"+q,
		`{"name":"`+hc+`","scope":"global","protocol":"TCP","port":"80"}`); code != http.StatusOK {
		t.Fatalf("create the health check = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-templates"+q,
		`{"name":"`+tpl+`","location":"Global","machineType":"e2-small","network":"default"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-groups"+q,
		`{"kind":"New managed instance group (stateless)","name":"`+group+`","template":"global/`+tpl+`","locationType":"Single zone","zone":"us-central1-a","autoscaling":"Off: do not autoscale","size":"1","healthCheck":"global/`+hc+`","initialDelay":"120"}`); code != http.StatusOK {
		t.Fatalf("create the group = %d: %s", code, body)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", group}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-groups"+q, string(b))
		time.Sleep(5 * time.Second)
		b, _ = json.Marshal(map[string]any{"Path": []string{"global", tpl}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-templates"+q, string(b))
		b, _ = json.Marshal(map[string]any{"Path": []string{"global", hc}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-healthchecks"+q, string(b))
	})
	return q
}

// TestComputeEngineMIGAutohealingThroughTheBrowser2225 (#2225), in the
// compute shard: Create instance group offers the Autohealing section
// (Health check, with Initial delay shown once one is chosen); a group
// made with a health check shows the Health check status column on its
// VMs and, on its Details tab reached from the keyboard, Autohealing with
// the health check and the 120 seconds initial delay.
func TestComputeEngineMIGAutohealingThroughTheBrowser2225(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	const hc, tpl, group = "browser-ah-hc", "browser-ah-tpl", "browser-ah"
	q := migAutohealing(t, project, hc, tpl, group)

	p := open(t)
	p.navigate("/compute/instanceGroups/create" + q)
	p.waitFor(`document.querySelector('select[name="healthCheck"]') !== null`)
	p.waitFor(`[...document.querySelectorAll('select[name="healthCheck"] option')].some((o) => o.value === "global/` + hc + `")`)
	p.eval(`(() => { const s = document.querySelector('select[name="healthCheck"]'); s.value = "global/`+hc+`"; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`(() => { const d = document.querySelector('input[name="initialDelay"]'); return d !== null && d.offsetParent !== null && d.value === "300"; })()`)

	p.navigate("/compute/instanceGroups/us-central1-a/" + group + q)
	p.waitFor(`[...document.querySelectorAll("#view th")].some((c) => c.textContent.trim() === "Health check status")`)
	p.waitFor(`document.querySelector('[role="tab"][aria-selected="true"]') !== null`)
	p.eval(`document.querySelector('[role="tab"][aria-selected="true"]').focus()`, nil)
	p.eval(`document.activeElement.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true }))`, nil)
	p.waitFor(`(document.querySelector('[role="tab"][aria-selected="true"]') || {}).textContent.trim() === "Details"`)
	p.waitFor(`(document.querySelector('[role="tabpanel"]') || {}).textContent.includes("Autohealing")`)
	p.waitFor(`(document.querySelector('[role="tabpanel"]') || {}).textContent.includes("120 seconds")`)
	p.waitFor(`(document.querySelector('[role="tabpanel"]') || {}).textContent.includes("global/` + hc + `")`)

	_, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instance-groups"+q+"&name=us-central1-a&name="+group, "")
	if !strings.Contains(body, `"Health check status"`) {
		t.Fatalf("the group's page: %s", body)
	}
}

// TestComputeEngineMIGAutohealingMeetsWCAG2225 (#2225): Create instance
// group with a health check chosen (the Autohealing section and Initial
// delay shown) and an autohealing group's page, Overview and Details,
// meet WCAG (names, contrast in both themes, keyboard, motion, reflow).
func TestComputeEngineMIGAutohealingMeetsWCAG2225(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	const hc, tpl, group = "a11y-ah-hc", "a11y-ah-tpl", "a11y-ah"
	migAutohealing(t, project, hc, tpl, group)
	create := screen("create in Instance groups, Autohealing", "/compute/instanceGroups/create", project)
	create.then = `(() => { const s = document.querySelector('select[name="healthCheck"]');
		if (s && s.options.length > 1) { s.value = s.options[1].value; s.dispatchEvent(new Event("change", { bubbles: true })); } return true; })()`
	auditScreens(t, []a11yPage{
		create,
		screen("an autohealing group's Overview", "/compute/instanceGroups/us-central1-a/"+group, project),
		screen("an autohealing group's Details", "/compute/instanceGroups/us-central1-a/"+group+"?tab=details", project),
	})
}
