//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestWorkbenchInstancesThroughThePages (#2173): Agent Platform >
// Notebooks > Workbench, as Google's "Create a Vertex AI Workbench
// instance by using the Console" walks it: the Instances page's Create new
// (Create instance: Name, Zone, Machine type, idle shutdown) makes an
// instance listed Active, whose page shows its details (its row's Open
// JupyterLab, the instance's own JupyterLab, is
// TestWorkbenchJupyterLabFromTheConsole's, #2198); Stop and Start move it; Reset asks to
// confirm with Reset; Delete with Confirm removes it. The console API reads
// each result back; every screen meets WCAG 2.1 AA.
func TestWorkbenchInstancesThroughThePages(t *testing.T) {
	needService(t, "workbench-instances")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	name := fmt.Sprintf("browser-wb-%d", time.Now().UnixNano()%1e6)
	status := func() string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/workbench-instances"+q, "")
		if code != http.StatusOK {
			t.Fatalf("list = %d: %s", code, body)
		}
		var l struct {
			Items []struct {
				Name, Status string
				Fields       map[string]string
			}
		}
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		for _, it := range l.Items {
			if it.Name == name {
				return it.Status
			}
		}
		return ""
	}
	waitStatus := func(want string) {
		t.Helper()
		got := status()
		for deadline := time.Now().Add(10 * time.Minute); got != want && time.Now().Before(deadline); got = status() {
			time.Sleep(2 * time.Second)
		}
		if got != want {
			t.Fatalf("the instance is %q, want %q", got, want)
		}
	}

	p := open(t)
	listPath := "/agent-platform/workbench/instances" + q
	p.navigate(listPath)
	p.waitFor(settled)
	p.clickText("#view button", "Create new")
	// Create new opens the Create instance page, as Google's does.
	p.waitFor(`document.querySelector("#view #f-name") !== null`)
	var createPath string
	p.eval(`location.pathname + location.search`, &createPath)
	var title string
	p.eval(`(document.querySelector("#view h1") || {}).textContent.trim() || ""`, &title)
	if title != "Create instance" {
		t.Errorf("the create page's title = %q, want Create instance", title)
	}
	p.submitForm(map[string]string{"name": name, "zone": "us-central1-a", "idleMinutes": "60"}, "name")
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/workbench-instances"+q+"&name="+url.QueryEscape(name), "")
	})
	p.waitForWithin(listedRow("/agent-platform/workbench/instances", name), 30*time.Minute)
	waitStatus("Active")

	// The instance's page: its details.
	instancePath := "/agent-platform/workbench/instances/" + url.PathEscape(name) + q
	p.navigate(instancePath)
	p.waitFor(`(document.querySelector("#view") || {textContent: ""}).textContent.includes("Data disk")`)

	// Stop and Start from the row's menu.
	p.navigate(listPath)
	p.colabRowItem(name, "Stop")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	waitStatus("Stopped")
	p.navigate(listPath)
	p.colabRowItem(name, "Start")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, 10*time.Minute)
	waitStatus("Active")

	// Reset asks to confirm with Reset, and wipes the kernel's state.
	p.navigate(listPath)
	p.colabRowItem(name, "Reset")
	p.pressConfirm("Reset")
	waitStatus("Active")

	auditScreens(t, []a11yPage{
		{"workbench instances", listPath, rowReady(name), ""},
		{"create instance page", createPath, `document.querySelector("#view #f-name") !== null`, ""},
		{"workbench instance", instancePath, settled, ""},
	})

	// Delete, with Confirm, as Google's Instances page asks.
	p = open(t)
	p.navigate(listPath)
	p.openRowDelete(name, "Delete")
	p.pressConfirm("Confirm")
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, name))
	if s := status(); s != "" {
		t.Errorf("a deleted instance is still listed, %q", s)
	}
}
