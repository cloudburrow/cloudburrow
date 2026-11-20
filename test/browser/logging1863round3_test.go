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

const menuItems = `[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent.trim()).join("|")`

// TestLoggingExplorerRound3 (#1863 round 3), in the storage shard: the
// Timeline's Zoom in, Zoom out, Backward and Forward, and a bar's pop-up
// with Scroll to time and Zoom to time; Pin to Fields pane on an expanded
// value; Manage summary fields' Hide or show default summary fields; Save
// in the Query pane and the Query library's Recent, Saved and Suggested
// queries with Run, and a saved query's Edit and Delete; then the
// Explorer, the Query library and Save query through the WCAG 2.1 AA audit.
func TestLoggingExplorerRound3(t *testing.T) {
	needService(t, "logging-saved-queries")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
		"entries": []map[string]any{{"severity": "ERROR", "jsonPayload": map[string]string{"method": "POST", "message": "round3-hello"}},
			{"severity": "INFO", "textPayload": "round3-earlier", "timestamp": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)}}})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
		t.Fatalf("entries:write = %d: %s", code, out)
	}
	t.Cleanup(func() {
		_, body := consoleDo(t, http.MethodGet, "/api/resources/logging-saved-queries?project="+project, "")
		var l struct{ Items []struct{ Name string } }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			consoleDo(t, http.MethodDelete, "/api/resources/logging-saved-queries?project="+project+"&name="+url.QueryEscape(it.Name), "")
		}
	})
	p := open(t)
	p.navigate("/logs?project=" + project)
	p.eval(`["cloudburrow.logs.summary", "cloudburrow.logs.pinnedFields", "cloudburrow.logs.recentQueries"].forEach((k) => localStorage.removeItem(k)), true`, nil)
	explorer := "/logs?scope=project&project=" + project
	p.navigate(explorer)
	hello := `[...document.querySelectorAll("#view tr.log-entry td.log-summary")].some((td) => td.textContent.includes("round3-hello"))`
	p.waitFor(hello)

	// A bar's pop-up.
	p.waitFor(`document.querySelector("#log-timeline .timeline-column") !== null`)
	p.eval(`[...document.querySelectorAll("#log-timeline .timeline-column")].find((c) => c.querySelector(".timeline-bar")).click(), true`, nil)
	var items string
	p.eval(menuItems, &items)
	if items != "Scroll to time|Zoom to time" {
		t.Errorf("a Timeline bar's pop-up = %q", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Scroll to time")
	p.waitFor(`document.querySelector("#view tr.log-entry.is-located") !== null`)
	// Zoom in, then Backward, each a custom range.
	p.eval(`document.querySelector("#timeline-zoom-in").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-time-range").value === "custom"`)
	var from1, from2 string
	p.eval(`new URL(location.href).searchParams.get("from")`, &from1)
	p.waitFor(`document.querySelector("#timeline-back") !== null`)
	p.eval(`document.querySelector("#timeline-back").click(), true`, nil)
	p.waitFor(fmt.Sprintf(`new URL(location.href).searchParams.get("from") !== %q`, from1))
	p.eval(`new URL(location.href).searchParams.get("from")`, &from2)
	if from2 >= from1 {
		t.Errorf("Backward moved the start from %s to %s", from1, from2)
	}
	p.navigate(explorer)
	p.waitFor(hello)

	// Pin to Fields pane.
	p.eval(`[...document.querySelectorAll("#view tr.log-entry")].find((r) => r.textContent.includes("round3-hello")).querySelector(".log-expand").click(), true`, nil)
	p.waitFor(`document.querySelector('#view tr.log-detail li[data-field="resource.labels.source"]') !== null`)
	p.eval(`document.querySelector('#view tr.log-detail li[data-field="resource.labels.source"] .overflow-trigger').click(), true`, nil)
	p.eval(menuItems, &items)
	if items != "Show matching entries|Hide matching entries|Pin to Fields pane" {
		t.Errorf("an expanded value's menu = %q", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Pin to Fields pane")
	p.waitFor(`[...document.querySelectorAll('#log-fields .log-field-group.is-pinned[data-field="resource.labels.source"] .log-field-value')].some((b) => b.textContent.includes("logging/app"))`)

	// Hide or show default summary fields.
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Preferences"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Manage summary fields")
	p.waitFor(`document.querySelector("#log-summary-defaults") !== null`)
	p.eval(`(() => { const c = document.querySelector("#log-summary-defaults"); c.checked = false; return true; })()`, nil)
	p.clickText(".modal button", "Apply")
	p.waitFor(`document.querySelector("#view table.hide-default-summary") !== null && getComputedStyle(document.querySelector("#view th.log-default-summary")).display === "none"`)

	// Save, then the Query library.
	p.eval(`(() => { const q = document.querySelector("#log-query"); q.value = "severity>=ERROR"; return true; })()`, nil)
	p.eval(`document.querySelector("#log-run-query").click(), true`, nil)
	p.waitFor(hello)
	p.eval(`document.querySelector("#log-save-query").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-save-name") !== null`)
	var labels string
	p.eval(`[...document.querySelectorAll(".modal label")].map((l) => l.textContent).join("|")`, &labels)
	if labels != "Name|Description (optional)|Query|Include custom summary fields|Truncate summary fields|Share with project" {
		t.Errorf("Save query's fields = %q", labels)
	}
	setField(p, ".modal", "log-save-name", "Round3 errors")
	p.eval(`document.querySelector("#log-save-share").checked = true, true`, nil)
	p.clickText(".modal button", "Save query")
	p.waitFor(`document.querySelector("#log-save-name") === null`)
	code, body := consoleDo(t, http.MethodGet, "/api/resources/logging-saved-queries?project="+project, "")
	var saved struct {
		Items []struct{ Fields map[string]string }
	}
	if err := json.Unmarshal([]byte(body), &saved); code != http.StatusOK || err != nil || len(saved.Items) != 1 ||
		saved.Items[0].Fields["filter"] != "severity>=ERROR" || saved.Items[0].Fields["visibility"] != "SHARED" {
		t.Fatalf("the saved query = %d %s", code, body)
	}

	p.eval(`document.querySelector("#log-query-library").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-library-tab-recent") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll(".modal [role=tab]")].map((b) => b.textContent).join("|")`, &tabs)
	if tabs != "Recent queries|Saved queries|Suggested queries" {
		t.Errorf("the Query library's tabs = %q", tabs)
	}
	p.waitFor(`[...document.querySelectorAll(".modal .query-library-text")].some((s) => s.textContent === "severity>=ERROR")`)
	p.eval(`document.querySelector("#log-library-tab-suggested").click(), true`, nil)
	p.waitFor(`[...document.querySelectorAll(".modal .query-library-item h3")].some((h) => h.textContent === "Errors")`)
	p.eval(`document.querySelector("#log-library-tab-saved").click(), true`, nil)
	p.waitFor(`[...document.querySelectorAll(".modal .query-library-item h3")].some((h) => h.textContent === "Round3 errors")`)
	p.eval(`document.querySelector('.modal .overflow-trigger[aria-label="Actions for saved query Round3 errors"]').click(), true`, nil)
	p.eval(menuItems, &items)
	if items != "Edit|Delete" {
		t.Errorf("a saved query's More options = %q", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit")
	p.waitFor(`document.querySelector("#log-save-name") !== null && document.querySelector("#log-save-name").value === "Round3 errors"`)
	setField(p, ".modal", "log-save-name", "Round3 renamed")
	p.clickText(".modal button", "Save")
	p.waitFor(`document.querySelector("#log-save-name") === null`)
	if _, body := consoleDo(t, http.MethodGet, "/api/resources/logging-saved-queries?project="+project, ""); !strings.Contains(body, "Round3 renamed") {
		t.Errorf("after Edit, the saved query = %s", body)
	}
	p.eval(`(() => { const q = document.querySelector("#log-query"); q.value = ""; return true; })()`, nil)
	p.eval(`document.querySelector("#log-query-library").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-library-tab-saved") !== null`)
	p.eval(`document.querySelector("#log-library-tab-saved").click(), true`, nil)
	p.waitFor(`[...document.querySelectorAll(".modal .query-library-item h3")].some((h) => h.textContent === "Round3 renamed")`)
	p.eval(`[...document.querySelectorAll(".modal .query-library-item")].find((li) => li.textContent.includes("Round3 renamed")).querySelector("button").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-query").value === "severity>=ERROR" && document.querySelector("#log-library-tab-saved") === null`)

	q := "?scope=project&project=" + project
	auditScreens(t, []a11yPage{
		{"Logs Explorer", "/logs" + q, settled + ` && document.querySelector("#log-timeline .timeline-column") !== null`, ""},
		{"Query library dialog", "/logs" + q, settled + ` && document.querySelector("#log-query-library") !== null`,
			`document.querySelector("#log-query-library").click()`},
		{"Save query dialog", "/logs" + q, settled + ` && document.querySelector("#log-save-query") !== null`,
			`document.querySelector("#log-save-query").click()`},
		{"Log scopes", "/logs/log-scopes?project=" + project, settled + ` && document.querySelector("#view table") !== null`, ""},
		{"Create log scope", "/logs/log-scopes/create?project=" + project, settled + ` && document.querySelector("form.create-page") !== null`, ""},
	})
}

// TestLoggingScopesAndRowMenus (#1863 round 3), in the storage shard: the
// Log scopes list has _Default marked Default; Create log scope takes Add
// projects and Name log scope and ends in Create log scope; a row's More
// is Set as default, Edit scope, Delete scope, and Set as default moves
// the marker. The Log Router's row menu is View sink details, Edit sink,
// Disable sink, Delete sink, Troubleshoot sink, the last answering with
// its findings; a log-based metric's is Edit metric, View in Metrics
// Explorer, Delete metric.
func TestLoggingScopesAndRowMenus(t *testing.T) {
	needService(t, "logging-log-scopes")
	project := uniqueProject(t)
	scope := "projects/" + project + "/locations/global/logScopes/round3"
	sink := "projects/" + project + "/sinks/round3-sink"
	metric := "projects/" + project + "/metrics/round3_metric"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-log-scopes?project="+project+"&name="+url.QueryEscape(scope), "")
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&name="+url.QueryEscape(sink), "")
		consoleDo(t, http.MethodDelete, "/api/resources/logging-metrics?project="+project+"&name="+url.QueryEscape(metric), "")
	})
	p := open(t)
	p.navigate("/logs/log-scopes?project=" + project)
	def := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect projects/"+project+"/locations/global/logScopes/_Default")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null && document.querySelector(%q).textContent.includes("Default")`, def, def))

	p.navigate("/logs/log-scopes/create?project=" + project)
	p.waitFor(`document.querySelector("form.create-page #f-id") !== null`)
	var heads string
	p.eval(`[...document.querySelectorAll("form.create-page .form-section-title, form.create-page .create-step-title")].map((h) => h.textContent.trim()).filter(Boolean).join("|")`, &heads)
	for _, h := range []string{"Projects", "Log views", "Name log scope"} {
		if !strings.Contains(heads, h) {
			t.Errorf("Create log scope's sections = %q, want %s", heads, h)
		}
	}
	p.clickText("form.create-page button", "Add projects")
	p.eval(fmt.Sprintf(`(() => { const i = document.querySelector("form.create-page .group-row input[type=text]"); i.value = %q; i.dispatchEvent(new Event("input")); return true; })()`, project), nil)
	setField(p, "form.create-page", "f-id", "round3")
	var submit string
	p.eval(`document.querySelector('form.create-page button[type="submit"]').textContent.trim()`, &submit)
	if submit != "Create log scope" {
		t.Errorf("the submit = %q", submit)
	}
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"]`)))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+scope)
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/log-scopes" && document.querySelector(%q) !== null`, row))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+scope))))
	var items string
	p.eval(menuItems, &items)
	if items != "Set as default|Edit scope|Delete scope" {
		t.Errorf("a log scope's More = %q", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Set as default")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null && [...document.querySelector(%q).cells].some((c) => c.textContent.trim() === "Default") && ![...document.querySelector(%q).cells].some((c) => c.textContent.trim() === "Default")`, row, row, def))

	sinkBody, _ := json.Marshal(map[string]string{"name": "round3-sink", "service": "Pub/Sub topic", "destination": "projects/" + project + "/topics/t", "filter": "severity>=ERROR"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-sinks?project="+project, string(sinkBody)); code != http.StatusOK {
		t.Fatalf("create the sink = %d %s", code, out)
	}
	p.navigate("/logs/sinks?project=" + project)
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+sink))))
	p.eval(menuItems, &items)
	if items != "View sink details|Edit sink|Disable sink|Delete sink|Troubleshoot sink" {
		t.Errorf("a sink's More actions = %q", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Troubleshoot sink")
	p.waitFor(`document.querySelector("#action-result") !== null && document.querySelector("#action-result").textContent.includes("the inclusion filter parses")`)
	p.clickText("#action-result button", "Close")

	metricBody, _ := json.Marshal(map[string]string{"type": "Counter", "name": "round3_metric", "filter": "severity>=ERROR"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-metrics?project="+project, string(metricBody)); code != http.StatusOK {
		t.Fatalf("create the metric = %d %s", code, out)
	}
	p.navigate("/logs/metrics?project=" + project)
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+metric))))
	p.eval(menuItems, &items)
	if items != "Edit metric|View in Metrics Explorer|Delete metric" {
		t.Errorf("a metric's menu = %q", items)
	}
}
