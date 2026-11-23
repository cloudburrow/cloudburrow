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
	p.waitFor(`["f-templateId", "f-locationType", "f-region"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"templateId": "browser-template"}, "templateId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/secrets-parameters/templates" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, name))
	if got := detail("parametermanager-templates", name); !strings.Contains(got, "YAML") {
		t.Errorf("the template reads back as %s", got)
	}
	p.follow(name)
	var tplPath string
	p.eval(`location.pathname + location.search`, &tplPath)
	payload := "log: {{.level}}\n"
	// Google's Create version opens the Create template version page.
	p.clickText(p.findAction("Create version"), "Create version")
	p.waitFor(`new URLSearchParams(location.search).get("action") === "newversion"`)
	p.submitForm(map[string]string{"versionId": "tv1", "payload": payload}, "versionId")
	p.waitFor(`new URLSearchParams(location.search).get("action") === null`)
	if got := detail("parametermanager-templates", name); !strings.Contains(got, "tv1") {
		t.Fatalf("New version made no version: %s", got)
	}
	p.navigate(tplPath)
	p.follow("tv1")
	var versionPath string
	p.eval(`location.pathname + location.search`, &versionPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("log: {{.level}}")`)
	// Render is Google's Render template version page (#2143), which
	// offers the template's location's parameter and its version, then the
	// render's own page with its Rendered value tab.
	p.clickText(p.findAction("Render"), "Render")
	p.waitFor(`document.querySelector("#f-parameterVersion") !== null && document.querySelector("#f-parameterVersion").value !== ""`)
	p.submitForm(map[string]string{}, "parameterVersion")
	p.waitFor(`location.pathname.includes("/render/") && document.querySelector("#view").textContent.includes("Rendered value")`)
	p.clickText("#view [role=tab]", "Rendered value")
	var rows string
	p.eval(`document.querySelector("#view").textContent`, &rows)
	if !strings.Contains(rows, "log: debug") {
		t.Errorf("Render answered %q; want the rendered value, log: debug", rows)
	}
	p.navigate(versionPath)
	p.clickText(p.findAction("Disable"), "Disable")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
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

// TestRequestQuotasThroughThePage (#2063, #2090): Parameter Manager's
// Quotas & System Limits tab lists its read, write and render quotas, from
// the Cloud Quotas API, at Google's defaults and enforced locally; Edit
// quota lowers the read quota for the project through a QuotaPreference,
// which the tab then shows beside the default. The tab meets WCAG 2.1 AA.
func TestRequestQuotasThroughThePage(t *testing.T) {
	needService(t, "parametermanager")
	needService(t, "serviceusage")
	project := uniqueProject(t)
	path := "/apis/api/parametermanager.googleapis.com/quotas"
	p := open(t)
	p.navigate(path + "?project=" + project)
	row := `[...document.querySelectorAll("#api-quotas-table tbody tr")].find((r) => r.textContent.includes("Read requests per minute per project"))`
	p.waitFor(row + ` !== undefined`)
	var text string
	p.eval(`document.querySelector("#api-quotas-table").textContent`, &text)
	for _, want := range []string{"Read requests per minute per project", "1,200", "Render requests per minute per project", "1,800", "parametermanager.googleapis.com/write_requests"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Quotas tab lacks %q: %s", want, text)
		}
	}
	p.eval(row+`.textContent`, &text)
	if !strings.Contains(text, "Yes") {
		t.Errorf("the read quota is not enforced locally: %q", text)
	}
	p.clickExpr(row + `.querySelector("button")`)
	p.waitFor(`document.querySelector("#quota-edit") !== null`)
	p.setField("#quota-edit #f-value", "1100")
	p.setField("#quota-edit #f-justification", "browser")
	p.clickExpr(`document.querySelector('#quota-edit button[type="submit"]')`)
	p.waitFor(`document.querySelector("#quota-edit") === null && ` + row + `.textContent.includes("1,100 (default 1,200)")`)

	auditScreens(t, []a11yPage{screen("API quotas tab", path, project)})
}
