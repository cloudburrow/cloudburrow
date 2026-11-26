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

// migFailing makes a template whose network is then deleted, and a
// managed group of one from it, whose VM the group fails to make and
// retries (#2220); it returns the console query and a func that makes the
// network again.
func migFailing(t *testing.T, project, net, tpl, group string) (string, func()) {
	t.Helper()
	q := "?project=" + url.QueryEscape(project)
	mkNet := func() {
		t.Helper()
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/vpc-networks"+q, `{"name":"`+net+`","subnetMode":"Automatic"}`); code != http.StatusOK {
			t.Fatalf("create the network = %d: %s", code, body)
		}
	}
	mkNet()
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-templates"+q,
		`{"name":"`+tpl+`","location":"Global","machineType":"e2-small","network":"`+net+`"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodDelete, "/api/resources/vpc-networks"+q+"&name="+net, ""); code != http.StatusOK {
		t.Fatalf("delete the network = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-groups"+q,
		`{"kind":"New managed instance group (stateless)","name":"`+group+`","template":"global/`+tpl+`","locationType":"Single zone","zone":"us-central1-a","autoscaling":"Off: do not autoscale","size":"1"}`); code != http.StatusOK {
		t.Fatalf("create the group = %d: %s", code, body)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", group}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-groups"+q, string(b))
		time.Sleep(5 * time.Second)
		b, _ = json.Marshal(map[string]any{"Path": []string{"global", tpl}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-templates"+q, string(b))
		consoleDo(t, http.MethodDelete, "/api/resources/vpc-networks"+q+"&name="+net, "")
	})
	return q, mkNet
}

// TestComputeEngineMIGCreationRetriesThroughTheBrowser2220 (#2220), in
// the compute shard: a managed group whose VM cannot be made (its
// template's network deleted) shows the VM on its page as Creating with
// its last error (RESOURCE_NOT_FOUND) and the group's Status Not stable;
// the Errors tab, reached from the keyboard (ArrowRight along the tab
// strip), lists the failure; once the network is made again the VM is
// RUNNING and the group Stable.
func TestComputeEngineMIGCreationRetriesThroughTheBrowser2220(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	const net, tpl, group = "browser-mr-net", "browser-mr-tpl", "browser-mr"
	q, mkNet := migFailing(t, project, net, tpl, group)
	detail := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instance-groups"+q+"&name=us-central1-a&name="+group, "")
		if code != http.StatusOK {
			t.Fatalf("the group's page through the console API = %d: %s", code, body)
		}
		return body
	}
	waitBody := func(what string, d time.Duration, ok func(string) bool) string {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			body := detail()
			if ok(body) {
				return body
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: never: %s", what, body)
			}
			time.Sleep(2 * time.Second)
		}
	}
	waitBody("the failure in the Errors tab", 2*time.Minute, func(b string) bool {
		return strings.Contains(b, `"Error code":"RESOURCE_NOT_FOUND"`) && strings.Contains(b, `"Current action":"Creating"`) &&
			strings.Contains(b, "Not stable (Creating 1)")
	})

	p := open(t)
	p.navigate("/compute/instanceGroups/us-central1-a/" + group + q)
	p.waitFor(`[...document.querySelectorAll("#view td")].some((c) => c.textContent.includes("RESOURCE_NOT_FOUND"))`)
	p.waitFor(`[...document.querySelectorAll("#view td")].some((c) => c.textContent.trim() === "Creating")`)
	// The Errors tab from the keyboard: focus the selected tab, then
	// ArrowRight to Errors.
	p.waitFor(`document.querySelector('[role="tab"][aria-selected="true"]') !== null`)
	p.eval(`document.querySelector('[role="tab"][aria-selected="true"]').focus()`, nil)
	for i := 0; i < 2; i++ {
		p.eval(`document.activeElement.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true }))`, nil)
		time.Sleep(200 * time.Millisecond)
	}
	p.waitFor(`(document.querySelector('[role="tab"][aria-selected="true"]') || {}).textContent.trim() === "Errors"`)
	p.waitFor(`[...document.querySelectorAll('[role="tabpanel"] td')].some((c) => c.textContent.includes("creation failed"))`)
	p.waitFor(`[...document.querySelectorAll('[role="tabpanel"] th')].some((c) => c.textContent.trim() === "Time")`)

	mkNet()
	body := waitBody("the group stable", 5*time.Minute, func(b string) bool { return strings.Contains(b, `"value":"Stable"`) })
	if !strings.Contains(body, `"Status":"RUNNING"`) || !strings.Contains(body, `"Current action":"None"`) {
		t.Fatalf("the made VM: %s", body)
	}
}

// TestComputeEngineMIGErrorsMeetWCAG2220 (#2220): a failing managed
// group's page, its Overview (the pending VM with its last error) and its
// Errors tab, meet WCAG (names, contrast in both themes, keyboard,
// motion, reflow).
func TestComputeEngineMIGErrorsMeetWCAG2220(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	const net, tpl, group = "a11y-mr-net", "a11y-mr-tpl", "a11y-mr"
	q, _ := migFailing(t, project, net, tpl, group)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instance-groups"+q+"&name=us-central1-a&name="+group, "")
		if strings.Contains(body, `"Error code":"RESOURCE_NOT_FOUND"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no error recorded: %s", body)
		}
		time.Sleep(2 * time.Second)
	}
	auditScreens(t, []a11yPage{
		screen("a failing managed group's Overview", "/compute/instanceGroups/us-central1-a/"+group, project),
		screen("a failing managed group's Errors", "/compute/instanceGroups/us-central1-a/"+group+"?tab=errors", project),
	})
}
