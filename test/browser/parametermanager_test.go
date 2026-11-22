//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestParameterManagerThroughThePage (#2054), in the storage shard, whose
// instance serves Parameter Manager beside Secret Manager. Reached from
// Secret Manager's menu under Security and identity: Create parameter (a
// JSON parameter with a label), New version on its page (a payload that
// references a secret version), the version's Payload and Rendered payload
// tabs (the reference replaced by the secret's value), Disable (Delete is
// TestConsoleParameterManager's). The console API reads each
// result back. Every screen meets WCAG 2.1 AA, as the console's others do.
func TestParameterManagerThroughThePage(t *testing.T) {
	needService(t, "parametermanager")
	needService(t, "secrets")
	project := uniqueProject(t)
	fixture(t, "secrets", project, `{"secretId":"pm-browser-secret","payload":"rendered-value"}`, "pm-browser-secret")
	name := "projects/" + project + "/locations/global/parameters/browser-config"
	version := "v1"
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/parametermanager?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}

	p := open(t)
	// Secret Manager's drawer row, then Parameter Manager in its menu.
	p.navigateByDrawer("Security and identity", "/secrets", "Secret Manager")
	p.waitFor(`[...document.querySelectorAll('a[href^="/secrets-parameters"]')].some((a) => a.textContent.includes("Parameter Manager"))`)
	p.navigate("/secrets-parameters?project=" + project)
	p.clickText("#view button", "Create parameter")
	// Google's Parameter format and Location sections (#2143): Multi-region
	// global by default.
	p.waitFor(`["f-parameterId", "f-locationType", "f-multiRegion", "f-region"].every((id) => document.getElementById(id) !== null) &&
		[...document.querySelectorAll(".create-step-title, .form-section-title, legend, h2, h3")].some((h) => h.textContent.trim() === "Parameter format") &&
		[...document.querySelectorAll('#view input[type="radio"]')].some((r) => r.value === "JSON")`)
	p.eval(`(() => { const r = [...document.querySelectorAll('#view input[type="radio"]')].find((r) => r.value === "JSON");
		r.click(); return true; })()`, nil)
	p.submitForm(map[string]string{"parameterId": "browser-config"}, "parameterId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/secrets-parameters" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, name))
	if got := detail(name); !strings.Contains(got, "JSON") || !strings.Contains(got, "principal://parametermanager.googleapis.com/") {
		t.Errorf("the parameter reads back as %s", got)
	}
	p.follow(name)
	var paramPath string
	p.eval(`location.pathname + location.search`, &paramPath)
	payload := `{"password": "__REF__(\"//secretmanager.googleapis.com/projects/` + project + `/secrets/pm-browser-secret/versions/1\")"}`
	// New version opens Google's Create parameter version page (#2143).
	p.clickText(p.findAction("New version"), "New version")
	p.waitFor(`new URLSearchParams(location.search).get("action") === "newversion" && document.querySelector("#view h1, #view h2") !== null &&
		[...document.querySelectorAll("#view h1, #view h2")].some((h) => h.textContent.includes("Create parameter version"))`)
	p.submitForm(map[string]string{"versionId": "v1", "payload": payload}, "versionId")
	p.waitFor(`new URLSearchParams(location.search).get("action") === null`)
	if got := detail(name); !strings.Contains(got, "v1") {
		t.Fatalf("New version made no version: %s", got)
	}
	p.navigate(paramPath)
	p.follow("v1")
	var versionPath string
	p.eval(`location.pathname + location.search`, &versionPath)
	p.clickText(`#view [role="tab"]`, "Rendered payload")
	p.waitFor(`document.querySelector("#view").textContent.includes('"password": "rendered-value"')`)
	var d struct{ Sections []struct{ ID, Text string } }
	if err := json.Unmarshal([]byte(detail(name, version)), &d); err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Sections {
		if s.ID == "payload" && s.Text != payload {
			t.Errorf("the Payload tab reads %q", s.Text)
		}
		if s.ID == "rendered" && s.Text != `{"password": "rendered-value"}` {
			t.Errorf("the Rendered payload tab reads %q", s.Text)
		}
	}
	// Disable asks for nothing typed, as Google's does (#2143).
	p.clickText(p.findAction("Disable"), "Disable")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null && document.querySelector(".modal.is-open #confirm-input") === null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Enable")`)
	if got := detail(name, version); !strings.Contains(got, "Disabled") {
		t.Errorf("after Disable the version reads %s", got)
	}

	auditScreens(t, []a11yPage{
		screen("Parameter Manager", "/secrets-parameters", project),
		screen("create parameter", "/secrets-parameters/create", project),
		{"parameter", paramPath, settled, ""},
		{"parameter version", versionPath, settled, ""},
	})
}
