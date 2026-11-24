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

// TestComputeEngineUpdateVMsThroughTheBrowser2139 (#2139), in the compute
// shard: a zonal managed group of two VMs made through the console API;
// its page's Update VMs with the New template of another machine type,
// Update type Automatic, Maximum surge 1 and Maximum unavailable 0; the
// group's VMs then all on the new template, read back through the console
// API.
func TestComputeEngineUpdateVMsThroughTheBrowser2139(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	const tpl, tpl2, group = "browser-up-tpl", "browser-up-tpl2", "browser-upmig"
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", group}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-groups"+q, string(b))
		time.Sleep(5 * time.Second)
		for _, n := range []string{tpl, tpl2} {
			b, _ = json.Marshal(map[string]any{"Path": []string{"global", n}, "Action": "delete"})
			consoleDo(t, http.MethodPost, "/api/actions/compute-instance-templates"+q, string(b))
		}
	})
	for n, mt := range map[string]string{tpl: "e2-small", tpl2: "e2-medium"} {
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-templates"+q,
			`{"name":"`+n+`","location":"Global","machineType":"`+mt+`"}`); code != http.StatusOK {
			t.Fatalf("create the template %s = %d: %s", n, code, body)
		}
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-groups"+q,
		`{"kind":"New managed instance group (stateless)","name":"`+group+`","template":"global/`+tpl+`","locationType":"Single zone","zone":"us-central1-a","autoscaling":"Off: do not autoscale","size":"2"}`); code != http.StatusOK {
		t.Fatalf("create the group = %d: %s", code, body)
	}
	detail := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instance-groups"+q+"&name=us-central1-a&name="+group, "")
		if code != http.StatusOK {
			t.Fatalf("the group's page through the console API = %d: %s", code, body)
		}
		return body
	}
	wait := func(what string, ok func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for {
			body := detail()
			if ok(body) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never: %s", what, body)
			}
			time.Sleep(3 * time.Second)
		}
	}
	wait("two VMs RUNNING", func(b string) bool { return strings.Count(b, `"Status":"RUNNING"`) == 2 })

	p := open(t)
	p.navigate("/compute/instanceGroups" + q)
	p.follow(group)
	p.waitFor(`location.pathname.endsWith("` + group + `")`)
	if res := p.pageAction("Update VMs", map[string]string{"template": "global/" + tpl2, "updateType": "Automatic",
		"maxSurge": "1", "maxUnavailable": "0", "replacementMethod": "Substitute"}, ""); res != "" {
		t.Fatalf("Update VMs = %s", res)
	}
	wait("both VMs on "+tpl2, func(b string) bool {
		return strings.Count(b, `"Status":"RUNNING"`) == 2 && strings.Count(b, `"Template":"`+tpl2+`"`) == 2 && !strings.Contains(b, `"Template":"`+tpl+`"`)
	})
}

// TestComputeEngineUpdateVMsMeetsWCAG2139 (#2139): a managed group's Update
// VMs dialog, with the Automatic update's Maximum surge and unavailable
// shown, meets WCAG (names, contrast in both themes, keyboard, motion,
// reflow).
func TestComputeEngineUpdateVMsMeetsWCAG2139(t *testing.T) {
	needService(t, "compute-instance-groups")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-templates"+q,
		`{"name":"a11y-up-tpl","location":"Global","machineType":"e2-small"}`); code != http.StatusOK {
		t.Fatalf("create the template = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/compute-instance-groups"+q,
		`{"kind":"New managed instance group (stateless)","name":"a11y-upmig","template":"global/a11y-up-tpl","locationType":"Single zone","zone":"us-central1-a","autoscaling":"Off: do not autoscale","size":"0"}`); code != http.StatusOK {
		t.Fatalf("create the group = %d: %s", code, body)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", "a11y-upmig"}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instance-groups"+q, string(b))
	})
	page := screen("Update VMs dialog", "/compute/instanceGroups/us-central1-a/a11y-upmig", project)
	page.then = `(() => { const b = [...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Update VMs");
		if (b) b.click();
		const f = document.querySelector(".modal.is-open #f-updateType");
		if (f) { f.value = "Automatic"; f.dispatchEvent(new Event("change", { bubbles: true })); } return true; })()`
	auditScreens(t, []a11yPage{page})
}
