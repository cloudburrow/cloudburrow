//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// menuItems opens a row's More menu and answers each item's label, with
// " (disabled)" after a greyed-out one, then closes it.
func (p *tab) menuItems(row string) string {
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+row))))
	p.waitFor(`document.querySelector(".overflow-menu:not([hidden])") !== null`)
	var got string
	p.eval(`[...document.querySelectorAll(".overflow-menu:not([hidden]) [role=menuitem]")].map((b) =>
		b.textContent.trim() + (b.disabled ? " (disabled)" : "")).join("|")`, &got)
	p.eval(`document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })), true`, nil)
	p.eval(`[...document.querySelectorAll(".overflow-menu")].forEach((m) => { m.hidden = true; }), true`, nil)
	return got
}

// TestLoggingRequiredBucketCustomIndexesAndTraceScopesThroughTheBrowser
// (#2108): Logs Storage greys out _Required's Edit bucket and Delete bucket
// and _Default's Delete bucket, and _Required's page has no Edit bucket and
// says why; Edit bucket's Custom indexes adds an integer index the Logging
// API reads back, listed on the bucket's Custom indexes tab; the Log Router
// greys out the _Required sink's Edit, Disable and Delete; the Trace
// Scopes tab of Settings lists _Default as the default and Set as default
// moves it to a new trace scope in the Observability API; and these pages
// pass the WCAG 2.1 AA audit.
func TestLoggingRequiredBucketCustomIndexesAndTraceScopesThroughTheBrowser(t *testing.T) {
	needService(t, "logging-buckets")
	needService(t, "logging-trace-scopes")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the bucket and the trace scope are read through the Logging port", envLogging)
	}
	api := "http://" + logAddr
	project := uniqueProject(t)
	bucket := "projects/" + project + "/locations/global/buckets/idx2108"
	if code, out := do(t, http.MethodPost, api+"/v2/projects/"+project+"/locations/global/buckets?bucketId=idx2108", `{"retentionDays":30}`, ""); code != http.StatusOK {
		t.Fatalf("buckets.create = %d: %s", code, out)
	}
	required := "projects/" + project + "/locations/global/buckets/_Required"
	def := "projects/" + project + "/locations/global/buckets/_Default"

	p := open(t)
	p.navigate("/logs/storage?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+required)))
	if got := p.menuItems(required); !strings.Contains(got, "Edit bucket (disabled)") || !strings.Contains(got, "Delete bucket (disabled)") {
		t.Errorf("_Required's menu = %q; want Edit bucket and Delete bucket greyed out", got)
	}
	if got := p.menuItems(def); !strings.Contains(got, "Delete bucket (disabled)") || strings.Contains(got, "Edit bucket (disabled)") {
		t.Errorf("_Default's menu = %q; want Delete bucket alone greyed out", got)
	}

	p.navigate("/logs/storage/" + url.PathEscape(required) + "?project=" + project)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("400 days")`)
	var page string
	p.eval(`document.querySelector("#view").textContent`, &page)
	var editable bool
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit bucket")`, &editable)
	if editable || !strings.Contains(page, "You can't change the retention period of the _Required log bucket") {
		t.Errorf("_Required's page offers Edit bucket %v, or does not say why not", editable)
	}

	// Edit bucket's Custom indexes: Add index, a field path and Integer.
	p.navigate("/logs/storage/" + url.PathEscape(bucket) + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit bucket")`)
	p.clickText("#view .page-actions button", "Edit bucket")
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	p.clickText(".modal button", "Add index")
	p.waitFor(`document.querySelector('.modal [aria-label="Field path 1"]') !== null`)
	p.eval(`(() => { const f = document.querySelector('.modal [aria-label="Field path 1"]'); f.value = "jsonPayload.req_status";
		f.dispatchEvent(new Event("input", { bubbles: true }));
		const s = document.querySelector('.modal [aria-label="Type 1"]'); s.value = "INDEX_TYPE_INTEGER";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	var body string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if _, body = do(t, http.MethodGet, api+"/v2/"+bucket, "", ""); strings.Contains(body, "req_status") {
			break
		}
	}
	var b struct {
		IndexConfigs []struct{ FieldPath, Type, CreateTime string }
	}
	_ = json.Unmarshal([]byte(body), &b)
	if len(b.IndexConfigs) != 1 || b.IndexConfigs[0].FieldPath != "jsonPayload.req_status" || b.IndexConfigs[0].Type != "INDEX_TYPE_INTEGER" || b.IndexConfigs[0].CreateTime == "" {
		t.Fatalf("the API reads the bucket as %s", body)
	}
	p.navigate("/logs/storage/" + url.PathEscape(bucket) + "?project=" + project)
	p.waitFor(settled + ` && [...document.querySelectorAll('#view [role="tab"]')].some((b) => b.textContent.trim() === "Custom indexes")`)
	p.eval(`(() => { [...document.querySelectorAll('#view [role="tab"]')].find((b) => b.textContent.trim() === "Custom indexes").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#view").textContent.includes("jsonPayload.req_status") && document.querySelector("#view").textContent.includes("Integer")`)

	// Log Router: the _Required sink, its Edit, Disable and Delete greyed out.
	sink := "projects/" + project + "/sinks/_Required"
	p.navigate("/logs/sinks?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+sink)))
	if got := p.menuItems(sink); !strings.Contains(got, "Edit sink (disabled)") || !strings.Contains(got, "Disable sink (disabled)") ||
		!strings.Contains(got, "Delete sink (disabled)") {
		t.Errorf("the _Required sink's menu = %q", got)
	}

	// Trace Scopes, a tab of Settings beside Log Scopes.
	scope := "projects/" + project + "/locations/global/traceScopes/team"
	if code, out := do(t, http.MethodPost, api+"/v1/projects/"+project+"/locations/global/traceScopes?traceScopeId=team",
		`{"resourceNames":["projects/`+project+`"],"description":"the team"}`, ""); code != http.StatusOK {
		t.Fatalf("traceScopes.create = %d: %s", code, out)
	}
	t.Cleanup(func() { do(t, http.MethodDelete, api+"/v1/"+scope, "", "") })
	p.navigate("/logs/settings?project=" + project)
	p.waitFor(`document.querySelector("#view .page-tabs") !== null`)
	p.clickText("#view .page-tabs a", "Trace Scopes")
	defScope := "projects/" + project + "/locations/global/traceScopes/_Default"
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/trace-scopes" && document.querySelector(%q) !== null && document.querySelector(%q) !== null`,
		fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+scope), fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+defScope)))
	var tabs string
	p.eval(`[...document.querySelectorAll("#view .page-tabs a")].map((a) => a.textContent.trim() + (a.getAttribute("aria-current") === "page" ? "*" : "")).join("|")`, &tabs)
	if tabs != "Settings|Log Scopes|Trace Scopes*|Observability Settings|Observability Buckets" {
		t.Errorf("the Settings tabs = %q", tabs)
	}
	if got := p.menuItems(defScope); !strings.Contains(got, "Edit scope (disabled)") || !strings.Contains(got, "Delete scope (disabled)") ||
		!strings.Contains(got, "Set as default (disabled)") {
		t.Errorf("_Default's menu = %q", got)
	}
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+scope))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Set as default")
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+scope)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null && document.querySelector(%q).textContent.includes("Custom (Default)")`, row, row))
	code, out := do(t, http.MethodGet, api+"/v1/projects/"+project+"/locations/global/scopes/_Default", "", "")
	var sc struct{ TraceScope string }
	_ = json.Unmarshal([]byte(out), &sc)
	if code != http.StatusOK || sc.TraceScope != scope {
		t.Errorf("the Observability _Default scope after Set as default = %d %s", code, out)
	}

	auditScreens(t, []a11yPage{
		{"Logs Storage with _Required", "/logs/storage?project=" + project, settled + ` && document.querySelector("#view table") !== null`, ""},
		{"a bucket with custom indexes", "/logs/storage/" + url.PathEscape(bucket) + "?project=" + project,
			settled + ` && [...document.querySelectorAll('#view [role="tab"]')].some((b) => b.textContent.trim() === "Custom indexes")`,
			`[...document.querySelectorAll('#view [role="tab"]')].find((b) => b.textContent.trim() === "Custom indexes").click()`},
		{"the _Required bucket", "/logs/storage/" + url.PathEscape(required) + "?project=" + project, settled, ""},
		{"Trace Scopes tab", "/logs/trace-scopes?project=" + project, settled + ` && document.querySelector("#view table") !== null`, ""},
		{"Create trace scope", "/logs/trace-scopes/create?project=" + project, settled + ` && document.querySelector("#view form input") !== null`, ""},
	})
}
