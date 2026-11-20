//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestParameterTemplatesThroughThePage (#2063), in a shard serving
// Parameter Manager. Reached from Secret Manager's menu: Create parameter
// (YAML) and its version through the console API, then on the Parameter
// templates page Create template, New version on its page, the version's
// Payload tab, Render with the parameter version chosen (the dialog shows
// the rendered value), and Disable. The console API reads each result
// back. Every screen meets WCAG 2.1 AA, as the console's others do.
func TestParameterTemplatesThroughThePage(t *testing.T) {
	needService(t, "parametermanager")
	project := uniqueProject(t)
	param := "projects/" + project + "/locations/global/parameters/browser-values"
	name := "projects/" + project + "/locations/global/templates/browser-template"
	detail := func(provider string, path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/"+provider+"?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	pq := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/parametermanager"+pq,
		`{"parameterId":"browser-values","location":"global","format":"YAML","encryption":"google"}`); code != http.StatusOK {
		t.Fatalf("create the parameter = %d: %s", code, body)
	}
	b, _ := json.Marshal(map[string]any{"Path": []string{param}, "Action": "newversion", "Values": map[string]string{"versionId": "v1", "payload": "level: debug\n"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/parametermanager"+pq, string(b)); code != http.StatusOK {
		t.Fatalf("add the parameter version = %d: %s", code, body)
	}

	p := open(t)
	p.navigateByDrawer("Security and identity", "/secrets", "Secret Manager")
	p.waitFor(`[...document.querySelectorAll('a[href^="/secrets-parameters/templates"]')].some((a) => a.textContent.includes("Parameter templates"))`)
	p.navigate("/secrets-parameters/templates?project=" + project)
	p.clickText("#view button", "Create template")
	p.waitFor(`["f-templateId", "f-location", "f-labels"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"templateId": "browser-template", "location": "global"}, "templateId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/secrets-parameters/templates" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, name))
	if got := detail("parametermanager-templates", name); !strings.Contains(got, "YAML") {
		t.Errorf("the template reads back as %s", got)
	}
	p.follow(name)
	var tplPath string
	p.eval(`location.pathname + location.search`, &tplPath)
	payload := "log: {{.level}}\n"
	p.pageAction("New version", map[string]string{"versionId": "tv1", "payload": payload}, "")
	if got := detail("parametermanager-templates", name); !strings.Contains(got, "tv1") {
		t.Fatalf("New version made no version: %s", got)
	}
	p.navigate(tplPath)
	p.follow("tv1")
	var versionPath string
	p.eval(`location.pathname + location.search`, &versionPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("log: {{.level}}")`)
	rows := p.pageAction("Render", map[string]string{"parameterVersion": param + "/versions/v1"}, "")
	if !strings.Contains(rows, "log: debug") {
		t.Errorf("Render answered %q; want the rendered value, log: debug", rows)
	}
	p.clickText(p.findAction("Disable"), "Disable")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Enable")`)
	if got := detail("parametermanager-templates", name, "tv1"); !strings.Contains(got, "Disabled") {
		t.Errorf("after Disable the version reads %s", got)
	}

	auditScreens(t, []a11yPage{
		screen("Parameter templates", "/secrets-parameters/templates", project),
		screen("create template", "/secrets-parameters/templates/create", project),
		{"template", tplPath, settled, ""},
		{"template version", versionPath, settled, ""},
	})
}

// TestRequestQuotasThroughThePage (#2063): Parameter Manager's Quotas &
// System Limits tab lists its read, write and render quotas at Google's
// defaults; Edit quota lowers the read quota for the project, which the
// tab then shows adjusted, and Reset to default restores it. The tab meets
// WCAG 2.1 AA.
func TestRequestQuotasThroughThePage(t *testing.T) {
	needService(t, "parametermanager")
	project := uniqueProject(t)
	path := "/apis/api/parametermanager.googleapis.com/quotas"
	p := open(t)
	p.navigate(path + "?project=" + project)
	p.waitFor(`document.querySelector("#api-quotas tbody") !== null`)
	var text string
	p.eval(`document.querySelector("#api-quotas").textContent`, &text)
	for _, want := range []string{"Read requests per minute per project", "1,200", "Render requests per minute per project", "1,800"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Quotas tab lacks %q: %s", want, text)
		}
	}
	p.clickText("#api-quotas button", "Edit quota")
	p.waitFor(`document.querySelector(".modal.is-open #f-limit") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-limit"); f.value = "7";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button", "Submit request")
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#api-quotas").textContent.includes("7 (adjusted)")`)
	p.clickText("#api-quotas button", "Reset to default")
	p.waitFor(`document.querySelector("#api-quotas") !== null && !document.querySelector("#api-quotas").textContent.includes("(adjusted)")`)

	auditScreens(t, []a11yPage{screen("API quotas tab", path, project)})
}
