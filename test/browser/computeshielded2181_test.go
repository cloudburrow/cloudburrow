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

	"github.com/chromedp/chromedp"
)

// shieldedEditOpen opens the instance page's Edit, for the Edit dialog's
// audit.
const shieldedEditOpen = `[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Edit").click()`

// TestShieldedVMConsole2181 (#2181), in the compute shard: Create an
// instance's Security pane opens with Google's Shielded VM group, "Turn
// on all settings for the most secure configuration." over Turn on Secure
// Boot (unchecked), Turn on vTPM and Turn on Integrity Monitoring
// (checked), Integrity Monitoring holding the vTPM on; a VM created with
// Secure Boot on shows Secure Boot, vTPM and Integrity Monitoring On in
// its Details page's Shielded VM; its Edit draws the three disabled, with
// the reason, while it runs; its boot's integrity monitoring events are in
// the Logs Explorer under compute.googleapis.com/shielded_vm_integrity;
// stopped, Edit turns Integrity Monitoring off with
// updateShieldedInstanceConfig. The instance page and its Edit dialog meet
// WCAG (contrast in both themes, keyboard, names, motion, reflow).
func TestShieldedVMConsole2181(t *testing.T) {
	needService(t, "compute-instances")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	const vm = "browser-shielded"
	detail := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/compute-instances"+q+"&name=us-central1-a&name="+vm, "")
		if code != http.StatusOK {
			t.Fatalf("the instance's detail = %d: %s", code, body)
		}
		return body
	}
	status := func() string {
		_, body := consoleDo(t, http.MethodGet, "/api/resources/compute-instances"+q, "")
		var l struct {
			Items []struct{ Name, Status string }
		}
		_ = json.Unmarshal([]byte(body), &l)
		for _, r := range l.Items {
			if r.Name == vm {
				return r.Status
			}
		}
		return ""
	}
	waitStatus := func(want string, d time.Duration) {
		t.Helper()
		for deadline := time.Now().Add(d); status() != want; time.Sleep(2 * time.Second) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never became %s: %s", vm, want, status())
			}
		}
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{"us-central1-a", vm}, "Action": "delete"})
		consoleDo(t, http.MethodPost, "/api/actions/compute-instances"+q, string(b))
	})

	// Create an instance > Security > Shielded VM.
	p := open(t)
	p.navigate("/compute/instances" + q)
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector('#view fieldset.form-subsection[data-subsection="Shielded VM"]') !== null`)
	var pane struct {
		Pane, Legend, Note string
		Labels             []string
		Checked            []bool
		AfterSA            bool
	}
	p.eval(`(() => { const fs = document.querySelector('#view fieldset.form-subsection[data-subsection="Shielded VM"]');
		const ids = ["enableSecureBoot", "enableVtpm", "enableIntegrityMonitoring"];
		const sa = document.querySelector("#f-serviceAccount");
		return { Pane: fs.closest("section.form-section").querySelector(".form-section-title").textContent,
			Legend: fs.querySelector("legend").textContent, Note: fs.querySelector(".form-subsection-note").textContent,
			Labels: ids.map((i) => document.querySelector('label[for="f-' + i + '"]').textContent.trim()),
			Checked: ids.map((i) => document.querySelector("#f-" + i).checked),
			AfterSA: !!(fs.compareDocumentPosition(sa) & Node.DOCUMENT_POSITION_FOLLOWING) }; })()`, &pane)
	if pane.Pane != "Security" || pane.Legend != "Shielded VM" || pane.Note != "If possible, Google recommends enabling Secure Boot." || !pane.AfterSA ||
		strings.Join(pane.Labels, ",") != "Turn on Secure Boot,Turn on vTPM,Turn on Integrity Monitoring" || fmt.Sprint(pane.Checked) != "[false true true]" {
		t.Fatalf("Create an instance's Shielded VM = %+v", pane)
	}
	// Integrity Monitoring needs the vTPM: checked, it holds the vTPM on.
	var held bool
	p.eval(`document.querySelector("#f-enableVtpm").disabled && document.querySelector("#f-enableVtpm").checked`, &held)
	if !held {
		t.Error("Turn on Integrity Monitoring does not hold Turn on vTPM on")
	}
	p.submitForm(map[string]string{"name": vm, "machineType": "e2-small", "image": "debian-cloud/debian-12", "enableSecureBoot": "true"}, "name")
	p.waitFor(`location.pathname === "/compute/instances" && ` + rowReady(vm))
	waitStatus("RUNNING", 10*time.Minute)

	// The Details page's Shielded VM.
	p.navigate("/compute/instances" + q)
	p.follow(vm)
	p.waitFor(`location.pathname.endsWith("` + vm + `")`)
	var instPage string
	p.eval(`location.pathname + location.search`, &instPage)
	p.waitFor(`[...document.querySelectorAll("#view *")].some((h) => !h.children.length && h.textContent.trim() === "Shielded VM")`)
	for _, want := range []string{`"label":"Secure Boot","value":"On"`, `"label":"vTPM","value":"On"`, `"label":"Integrity Monitoring","value":"On"`} {
		if body := detail(); !strings.Contains(body, want) {
			t.Fatalf("the Details page lacks %s: %.3000s", want, body)
		}
	}
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Secure Boot") || !strings.Contains(text, "Integrity Monitoring") {
		t.Fatalf("the Details page does not draw Shielded VM: %.2000s", text)
	}

	// Edit while it runs: disabled, the reason given, nothing sent.
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-enableSecureBoot") !== null`)
	var running struct {
		Disabled []bool
		Checked  []bool
		Reason   string
	}
	p.eval(`(() => { const ids = ["enableSecureBoot", "enableVtpm", "enableIntegrityMonitoring"];
		const f = (i) => document.querySelector(".modal.is-open #f-" + i);
		return { Disabled: ids.map((i) => f(i).disabled), Checked: ids.map((i) => f(i).checked),
			Reason: document.querySelector(".modal.is-open").textContent.includes("Stop the VM to change these settings.") ? "yes" : "" }; })()`, &running)
	if fmt.Sprint(running.Disabled) != "[true true true]" || fmt.Sprint(running.Checked) != "[true true true]" || running.Reason == "" {
		t.Fatalf("a running VM's Edit Shielded VM = %+v", running)
	}
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Integrity monitoring's events, in the Logs Explorer.
	filter := `logName="projects/` + project + `/logs/compute.googleapis.com%2Fshielded_vm_integrity"`
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(3 * time.Second) {
		_, logs := consoleDo(t, http.MethodGet, "/api/logs?"+url.Values{"project": {project}, "contains": {"BootReportEvent"}, "limit": {"50"}}.Encode(), "")
		if strings.Contains(logs, "lateBootReportEvent") && strings.Contains(logs, "shielded_vm_integrity") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no integrity monitoring event reached Cloud Logging: %.2000s", logs)
		}
	}
	logsPage := "/logs?scope=project&project=" + url.QueryEscape(project) + "&query=" + url.QueryEscape(filter)
	p.navigate(logsPage)
	p.waitForWithin(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("BootReportEvent"))`, time.Minute)

	auditScreens(t, []a11yPage{
		{"VM instance details with Shielded VM", instPage, settled, ""},
		{"VM instance Edit dialog", instPage, settled, shieldedEditOpen},
		{"Logs Explorer with integrity monitoring events", logsPage, settled + ` && [...document.querySelectorAll("#view tbody tr")].length > 0`, ""},
	})

	// Stopped: Edit turns Integrity Monitoring off.
	p.navigate("/compute/instances" + q)
	vmRowMenu(p, vm, "Stop", map[string]string{"noGracefulShutdown": "false"}, "")
	waitStatus("TERMINATED", 3*time.Minute)
	p.navigate(instPage)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-enableIntegrityMonitoring") !== null && !document.querySelector(".modal.is-open #f-enableIntegrityMonitoring").disabled`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-enableIntegrityMonitoring"); f.click(); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(time.Minute); !strings.Contains(detail(), `"label":"Integrity Monitoring","value":"Off"`); time.Sleep(2 * time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("Integrity Monitoring was never turned off: %.3000s", detail())
		}
	}
	if body := detail(); !strings.Contains(body, `"label":"Secure Boot","value":"On"`) || !strings.Contains(body, `"label":"vTPM","value":"On"`) {
		t.Fatalf("Edit changed more than Integrity Monitoring: %.3000s", body)
	}
}

// TestShieldedVMConsole2181MeetsWCAG (#2181): Create an instance, with its
// Security pane's Shielded VM, meets WCAG (contrast in both themes,
// keyboard, names, motion, reflow).
func TestShieldedVMConsole2181MeetsWCAG(t *testing.T) {
	needService(t, "compute-instances")
	project := servedProject(t)
	auditScreens(t, []a11yPage{{"Create an instance with Shielded VM", "/compute/instances/create?project=" + url.QueryEscape(project),
		settled + ` && document.querySelector('#view fieldset[data-subsection="Shielded VM"]') !== null`, ""}})
}
