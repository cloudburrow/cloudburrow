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

	"github.com/chromedp/chromedp"
)

// TestLoggingSinkRowsAndExclusionRows (#1863 round 2), in the storage
// shard: Create sink's exclusion filters are rows with Add exclusion, each
// with its name, filter and Disable; a disabled row reaches the API
// disabled. Preview logs opens the Logs Explorer on the inclusion filter;
// Troubleshoot sink answers. The sink's row menu has Edit sink, which opens
// its edit form, and Delete sink.
func TestLoggingSinkRowsAndExclusionRows(t *testing.T) {
	needService(t, "logging-sinks")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/sinks/browser-rows"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&name="+url.QueryEscape(name), "")
	})
	p.navigate("/logs/sinks/create?project=" + project)
	p.run(chromedp.WaitVisible(chromedp.CSS(`form.create-page #f-destination`)))
	p.eval(`[...document.querySelectorAll("form.create-page fieldset.is-radio input[type=radio]")].find((r) => r.value === "Pub/Sub topic").click(), true`, nil)
	setField(p, "form.create-page", "f-name", "browser-rows")
	setField(p, "form.create-page", "f-destination", "projects/"+project+"/topics/t")
	setField(p, "form.create-page", "f-filter", "severity>=ERROR")
	var checks string
	p.eval(`[...document.querySelectorAll(".create-check-buttons button")].map((b) => b.textContent).join("|")`, &checks)
	if checks != "Preview logs|Troubleshoot sink" {
		t.Errorf("Create sink's checks = %q", checks)
	}
	p.eval(`window.open = (u) => { window.__opened = u; return null; }, true`, nil)
	p.eval(`document.querySelector("#create-check-preview-logs").click(), true`, nil)
	p.waitFor(`(window.__opened || "").startsWith("/logs?") && window.__opened.includes("severity")`)
	p.eval(`document.querySelector("#create-check-troubleshoot-sink").click(), true`, nil)
	p.waitFor(`(document.querySelector("#create-check-result").textContent || "").startsWith("No problems found")`)

	for i, ex := range [][2]string{{"no-debug", "severity=DEBUG"}, {"no-info", "severity=INFO"}} {
		p.clickText("form.create-page button", "Add exclusion")
		p.eval(fmt.Sprintf(`(() => { const r = document.querySelectorAll("form.create-page .group-row")[%d];
			const [n, f] = r.querySelectorAll("input[type=text], textarea");
			n.value = %q; n.dispatchEvent(new Event("input"));
			f.value = %q; f.dispatchEvent(new Event("input"));
			if (%d === 1) { const d = r.querySelector("input[type=checkbox]"); d.checked = true; d.dispatchEvent(new Event("change")); }
			return true; })()`, i, ex[0], ex[1], i), nil)
	}
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"]`)))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+name)
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/sinks" && document.querySelector(%q) !== null`, row))
	code, body := consoleDo(t, http.MethodGet, "/api/detail/logging-sinks?project="+project+"&name="+url.QueryEscape(name), "")
	if code != http.StatusOK || !strings.Contains(body, "no-debug") || !strings.Contains(body, "no-info") {
		t.Fatalf("the sink's exclusions = %d %s", code, body)
	}

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name))))
	var items string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent.trim()).join("|")`, &items)
	if !strings.Contains(items, "Edit sink") || !strings.HasSuffix(items, "Delete sink") {
		t.Errorf("the row's menu = %q, want Edit sink and Delete sink", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit sink")
	p.waitFor(`document.querySelectorAll(".modal .group-row").length === 2`)
	var disabled string
	p.eval(`[...document.querySelectorAll(".modal .group-row input[type=checkbox]")].map((c) => String(c.checked)).join(",")`, &disabled)
	if disabled != "false,true" {
		t.Errorf("the edit form's Disable per exclusion = %q, want false,true", disabled)
	}
}

// TestLoggingExplorerRound2 (#1863 round 2), in the storage shard: the
// Actions menu has Create log alert, disabled because Cloud Monitoring is
// not served, and Highlight in results, which marks the text; Preferences
// has Manage summary fields, whose field is drawn as a chip; the query
// editor suggests the field names the entries carry; an entry can be
// pinned, which puts it on the Timeline; and the Timeline's drag handles
// narrow the range and Run.
func TestLoggingExplorerRound2(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
		"entries": []map[string]any{{"severity": "ERROR", "jsonPayload": map[string]string{"method": "GET", "message": "round2-hello"}}}})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
		t.Fatalf("entries:write = %d: %s", code, out)
	}
	p := open(t)
	p.eval(`localStorage.removeItem("cloudburrow.logs.summary"), true`, nil)
	p.navigate("/logs?project=" + project)
	p.waitFor(`document.querySelector("#view tr.log-entry") !== null`)

	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions"]`)))
	var alert bool
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].some((b) => b.textContent === "Create log alert" && b.disabled && b.title.includes("Cloud Monitoring"))`, &alert)
	if !alert {
		t.Error("Actions has no disabled Create log alert naming Cloud Monitoring")
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Highlight in results")
	p.waitFor(`document.querySelector("#log-highlight-text") !== null`)
	setField(p, ".modal", "log-highlight-text", "hello")
	p.clickText(".modal button", "Highlight")
	p.waitFor(`[...document.querySelectorAll("#view mark.log-highlight")].some((m) => m.textContent === "hello")`)

	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Preferences"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Manage summary fields")
	p.waitFor(`document.querySelector("#log-summary-input") !== null`)
	setField(p, ".modal", "log-summary-input", "jsonPayload.method")
	p.clickText(".modal button", "Add")
	p.clickText(".modal button", "Apply")
	p.waitFor(`[...document.querySelectorAll("#view .log-summary-chip")].some((c) => c.textContent === "GET")`)

	p.eval(`(() => { const q = document.querySelector("#log-query"); q.focus(); q.value = "jsonPayload.me"; q.setSelectionRange(14, 14); q.dispatchEvent(new Event("input")); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#log-query-suggestions li")].some((li) => li.textContent === "jsonPayload.method")`)
	p.eval(`(() => { const q = document.querySelector("#log-query"); q.value = "jsonPayload.method="; q.setSelectionRange(19, 19); q.dispatchEvent(new Event("input")); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#log-query-suggestions li")].some((li) => li.textContent === "GET")`)
	p.eval(`(() => { const q = document.querySelector("#log-query"); q.value = ""; q.dispatchEvent(new Event("input")); return true; })()`, nil)

	p.eval(`document.querySelector("#view tr.log-entry .log-pin").click(), true`, nil)
	p.waitFor(`document.querySelector("#view tr.log-entry.is-pinned") !== null && document.querySelectorAll("#log-pins .log-pin-mark").length === 1`)

	p.waitFor(`document.querySelector("#timeline-start") !== null`)
	p.eval(`(() => { const s = document.querySelector("#timeline-start"); s.value = String(+s.max - 2); s.dispatchEvent(new Event("input")); return true; })()`, nil)
	p.waitFor(`!document.querySelector("#timeline-run").disabled`)
	p.eval(`document.querySelector("#timeline-run").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-time-range").value === "custom" && new URL(location.href).searchParams.has("from")`)
}

// TestLoggingAdminLogAnalytics (#1863 round 2), in the run shard, whose
// instance serves Cloud Logging and BigQuery: a bucket upgraded to Log
// Analytics with a link is listed in the Log views panel by its query
// name; choosing it fills Google's default query, and Run query answers
// from the linked dataset in BigQuery.
func TestLoggingAdminLogAnalytics(t *testing.T) {
	needService(t, "logging-analytics")
	project := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_BIGQUERY_PROJECT"))
	if project == "" {
		t.Skip("CLOUDBURROW_TEST_BIGQUERY_PROJECT is not set: the linked dataset is in the project BigQuery serves")
	}
	bucket := "projects/" + project + "/locations/global/buckets/analytics-b"
	create, _ := json.Marshal(map[string]string{"name": "analytics-b", "location": "global", "retention": "30", "analytics": "true",
		"link": "true", "linkId": "analytics_b"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-buckets?project="+project, string(create)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create the bucket = %d %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Action": "delete-link", "Values": map[string]string{"linkId": "analytics_b"}})
		consoleDo(t, http.MethodPost, "/api/actions/logging-buckets?project="+project, string(b))
		consoleDo(t, http.MethodDelete, "/api/resources/logging-buckets?project="+project+"&name="+url.QueryEscape(bucket), "")
	})
	p := open(t)
	p.navigate("/logs/analytics?project=" + project)
	ref := project + ".global.analytics-b._AllLogs"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".log-view-list button")].some((b) => b.textContent === %q)`, ref))
	p.clickText(".log-view-list button", ref)
	var q string
	p.eval(`document.querySelector("#view textarea.query-editor").value`, &q)
	if !strings.Contains(q, "`"+ref+"`") {
		t.Errorf("the default query = %q", q)
	}
	p.eval(`document.querySelector("#view textarea.query-editor").value = "SELECT COUNT(*) AS n FROM `+"`"+ref+"`"+`", true`, nil)
	p.clickText("#view .query-pane button", "Run query")
	p.waitFor(`(document.querySelector("#view .query-results") || { textContent: "" }).textContent.match(/row|No rows/) !== null || !document.querySelector("#view .query-pane .form-error").hidden`)
	var failure string
	p.eval(`(() => { const e = document.querySelector("#view .query-pane .form-error"); return e.hidden ? "" : e.textContent; })()`, &failure)
	if failure != "" {
		t.Errorf("Run query failed: %s", failure)
	}
}
