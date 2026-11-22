//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// colabRowItem opens a list row's Actions menu and chooses item.
func (p *tab) colabRowItem(name, item string) {
	p.t.Helper()
	menu := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, menu))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.run(chromedp.Click(chromedp.CSS(menu)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, item)
}

// TestColabEnterpriseRuntimesThroughThePages (#2105): Agent Platform >
// Colab Enterprise, as Google's "Create a runtime template" and "Create a
// runtime" walk it. Runtime templates' New template opens Create new
// runtime template (Runtime basics, Configure compute, Environment,
// Networking and security); the template lists by its display name, its
// ID opens its page, whose Edit (Edit runtime template, Update) renames
// it; the row's Set as Default sets it. Runtimes' Create (Create Agent
// Platform runtime: Runtime template and Runtime name) starts a runtime
// from it, listed Running; Stop and Start move it, and Delete with
// Confirm removes it; the template's Delete removes the template. The
// console API reads each result back; every screen meets WCAG 2.1 AA and
// New template is reached by keyboard.
func TestColabEnterpriseRuntimesThroughThePages(t *testing.T) {
	needService(t, "colab-templates")
	needService(t, "colab-runtimes")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	display := fmt.Sprintf("Browser template %d", time.Now().UnixNano()%1e6)
	type listing struct {
		Items []struct {
			Name, Status string
			Fields       map[string]string
		}
	}
	list := func(svc string) listing {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+svc+q+"&region=us-central1", "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", svc, code, body)
		}
		var l listing
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	templateID := func(name string) string {
		for _, it := range list("colab-templates").Items {
			if it.Fields["Display name"] == name {
				return it.Name
			}
		}
		return ""
	}

	p := open(t)
	templatesPath := "/agent-platform/colab/runtime-templates" + q
	runtimesPath := "/agent-platform/colab/runtimes" + q
	// New template, by keyboard: Tab to it, Enter opens its page.
	p.navigate(templatesPath)
	p.waitFor(settled)
	p.tabTo(`a.textContent.trim() === "New template" || (a.tagName === "BUTTON" && a.textContent.trim() === "New template")`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	var createPath, heading string
	p.eval(`location.pathname + location.search`, &createPath)
	p.eval(`document.querySelector("#view h1").textContent.trim()`, &heading)
	if heading != "Create new runtime template" {
		t.Errorf("the create page's heading = %q, want Create new runtime template", heading)
	}
	var sections []string
	p.eval(`[...document.querySelectorAll("#view form .create-step-title, #view form .form-section-title, #view form h2, #view form h3, #view form legend")].map((n) => n.textContent.trim())`, &sections)
	for _, want := range []string{"Runtime basics", "Configure compute", "Environment", "Networking and security"} {
		found := false
		for _, s := range sections {
			found = found || s == want || len(s) > len(want) && s[len(s)-len(want):] == want
		}
		if !found {
			t.Errorf("the create page has no %q section: %q", want, sections)
		}
	}
	p.submitForm(map[string]string{"displayName": display, "region": "us-central1", "idleMinutes": "30", "env": "STAGE=browser"}, "displayName")
	p.waitFor(`location.pathname === "/agent-platform/colab/runtime-templates" && ` + rowReady(display))
	id := templateID(display)
	if id == "" {
		t.Fatalf("the Runtime templates list has no %q", display)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-templates"+q+"&region=us-central1&name="+url.QueryEscape(id), "")
	})
	for _, it := range list("colab-templates").Items {
		if it.Name == id && (it.Fields["Machine type"] != "e2-standard-4" || it.Fields["Idle shutdown"] != "30 minutes" || it.Fields["Data disk"] != "100 GB pd-standard") {
			t.Errorf("the template's row = %+v", it.Fields)
		}
	}

	// Its ID opens its page; Edit renames it.
	p.follow(id)
	var templatePath string
	p.eval(`location.pathname + location.search`, &templatePath)
	p.pageAction("Edit", map[string]string{"displayName": display + " edited"}, "")
	if templateID(display+" edited") != id {
		t.Errorf("after Edit, the template is not listed as %q", display+" edited")
	}
	display += " edited"

	// Set as Default, from its row.
	p.navigate(templatesPath)
	p.waitFor(rowReady(display))
	p.colabRowItem(id, "Set as Default")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// Runtimes: Create from the template.
	p.navigate(runtimesPath)
	p.waitFor(settled)
	p.clickText("#view button", "Create")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-notebookRuntimeTemplate option")].some((o) => o.value === %q)`, id))
	var dialogTitle string
	p.eval(`(document.querySelector(".modal.is-open h2") || {}).textContent || ""`, &dialogTitle)
	if dialogTitle != "Create Agent Platform runtime" {
		t.Errorf("the create runtime title = %q", dialogTitle)
	}
	runtimeName := "browser-runtime"
	p.submitForm(map[string]string{"notebookRuntimeTemplate": id, "displayName": runtimeName}, "displayName")
	p.waitForWithin(`location.pathname === "/agent-platform/colab/runtimes" && `+rowReady(runtimeName), 30*time.Minute)
	runtimeID := ""
	for _, it := range list("colab-runtimes").Items {
		if it.Fields["Runtime name"] == runtimeName && it.Fields["Runtime template"] == id {
			runtimeID = it.Name
			if it.Status != "Running" {
				t.Errorf("the new runtime is %q, want Running", it.Status)
			}
		}
	}
	if runtimeID == "" {
		t.Fatalf("the Runtimes list has no %q", runtimeName)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/colab-runtimes"+q+"&region=us-central1&name="+url.QueryEscape(runtimeID), "")
	})
	status := func() string {
		for _, it := range list("colab-runtimes").Items {
			if it.Name == runtimeID {
				return it.Status
			}
		}
		return ""
	}
	p.navigate(runtimesPath)
	p.colabRowItem(runtimeID, "Stop")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if s := status(); s != "Stopped" {
		t.Errorf("after Stop the runtime is %q", s)
	}
	p.navigate(runtimesPath)
	p.colabRowItem(runtimeID, "Start")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, 10*time.Minute)
	// Start is an operation: the runtime is Starting until its container is up.
	started := status()
	for deadline := time.Now().Add(10 * time.Minute); started == "Starting" && time.Now().Before(deadline); started = status() {
		time.Sleep(2 * time.Second)
	}
	if started != "Running" {
		t.Errorf("after Start the runtime is %q", started)
	}

	auditScreens(t, []a11yPage{
		{"runtime templates", templatesPath, rowReady(display), ""},
		{"create runtime template", createPath, settled + ` && document.querySelector("#view form input") !== null`, ""},
		{"runtime template details", templatePath, settled, ""},
		{"runtimes", runtimesPath, rowReady(runtimeName), ""},
		{"create runtime dialog", runtimesPath, rowReady(runtimeName),
			`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Create").click()`},
	})

	// Delete, with Confirm, as Google's runtimes page asks.
	p = open(t)
	p.navigate(runtimesPath)
	p.openRowDelete(runtimeID, "Delete")
	p.confirmOnlyDialog()
	p.pressDelete()
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, runtimeName))
	if s := status(); s != "" {
		t.Errorf("a deleted runtime is still listed, %q", s)
	}
	p.navigate(templatesPath)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, display))
}
