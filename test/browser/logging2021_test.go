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
)

// TestLoggingConsoleRound4 (#2021), on an instance serving Cloud Logging,
// Cloud Monitoring and BigQuery: Log Analytics queries a bucket upgraded
// to it that has no linked dataset, and charts the result; the Log
// Router's Volume column counts what the sink routed, and its row menu
// ends in View sink log volume and error rates, which opens Metrics
// Explorer on logging.googleapis.com/exports/byte_count; Logs Storage
// shows the month's ingestion and Create usage alert makes a Cloud
// Monitoring policy; the Query library's Queries provided by Google Cloud
// runs one of Google's queries. Then the pages and dialogs through the
// WCAG 2.1 AA audit.
func TestLoggingConsoleRound4(t *testing.T) {
	needService(t, "logging-analytics")
	needService(t, "monitoring-alerting")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	id := "round4"
	bucket := "projects/" + project + "/locations/global/buckets/" + id
	sink := "projects/" + project + "/sinks/round4-sink"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&name="+url.QueryEscape(sink), "")
		consoleDo(t, http.MethodDelete, "/api/resources/logging-buckets?project="+project+"&name="+url.QueryEscape(bucket), "")
		_, body := consoleDo(t, http.MethodGet, "/api/resources/monitoring-alerting?project="+project, "")
		var l struct{ Items []struct{ Name string } }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			consoleDo(t, http.MethodDelete, "/api/resources/monitoring-alerting?project="+project+"&name="+url.QueryEscape(it.Name), "")
		}
	})
	create, _ := json.Marshal(map[string]string{"name": id, "location": "global", "retention": "30", "analytics": "true"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-buckets?project="+project, string(create)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create the bucket = %d %s", code, out)
	}
	sk, _ := json.Marshal(map[string]string{"name": "round4-sink", "service": "Cloud Logging bucket", "destination": bucket, "filter": "severity>=ERROR"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-sinks?project="+project, string(sk)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create the sink = %d %s", code, out)
	}
	b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
		"entries": []map[string]any{{"severity": "ERROR", "textPayload": "round4-a"}, {"severity": "ERROR", "textPayload": "round4-b"},
			{"severity": "INFO", "textPayload": "round4-info"}}})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
		t.Fatalf("entries:write = %d: %s", code, out)
	}

	// Log Analytics, with no link.
	p := open(t)
	p.navigate("/logs/analytics?project=" + project)
	ref := project + ".global." + id + "._AllLogs"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".log-view-list button")].some((b) => b.textContent === %q)`, ref))
	p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#view textarea.query-editor"); e.value = %q; return true; })()`,
		"SELECT severity, COUNT(*) AS n FROM `"+ref+"` GROUP BY severity"), nil)
	p.clickText("#view .query-pane button", "Run query")
	p.waitFor(`(document.querySelector("#view .query-results") || { textContent: "" }).textContent.match(/row|No rows/) !== null || !document.querySelector("#view .query-pane .form-error").hidden`)
	var failure, results string
	p.eval(`(() => { const e = document.querySelector("#view .query-pane .form-error"); return e.hidden ? "" : e.textContent; })()`, &failure)
	if failure != "" {
		t.Fatalf("Run query over a bucket with no link failed: %s", failure)
	}
	p.eval(`document.querySelector("#view .query-results").textContent`, &results)
	if !strings.Contains(results, "ERROR") || !strings.Contains(results, "2") || strings.Contains(results, "INFO") {
		t.Errorf("the results = %q; want ERROR 2 and no INFO (the sink routed only errors)", results)
	}
	p.waitFor(`!document.querySelector("#log-analytics-chart").hidden && document.querySelector("#log-analytics-chart svg[aria-label]") !== null`)
	for _, u := range p.unnamedControls() {
		t.Errorf("Log Analytics with its chart: an unnamed %s", u)
	}

	// The Log Router's Volume and row menu.
	p.navigate("/logs/sinks?project=" + project)
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+sink)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	var heads, volume string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).join("|")`, &heads)
	if !strings.Contains(heads, "Last updated|Volume") {
		t.Errorf("the Log Router's columns = %q; want Volume after Last updated", heads)
	}
	p.eval(fmt.Sprintf(`(() => { const hs = [...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim());
		return document.querySelector(%q).cells[hs.indexOf("Volume")].textContent.trim(); })()`, row), &volume)
	if volume == "" || volume == "0 B" || !strings.HasSuffix(volume, "B") {
		t.Errorf("the sink's Volume = %q; want the bytes it routed", volume)
	}
	p.eval(fmt.Sprintf(`document.querySelector(%q).click(), true`, `button[aria-label="Actions for `+sink+`"]`), nil)
	var items string
	p.eval(menuItems, &items)
	if !strings.HasSuffix(items, "|Troubleshoot sink|View sink log volume and error rates") {
		t.Errorf("the sink's More actions = %q", items)
	}
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "View sink log volume and error rates")
	p.waitFor(`location.pathname === "/monitoring/metrics-explorer" && new URL(location.href).searchParams.get("metric") === "logging.googleapis.com/exports/byte_count"`)

	// Logs Storage's summary and Create usage alert.
	p.navigate("/logs/storage?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Current month ingestion") && document.querySelector("#list-action-usage-alert") !== null`)
	var summary string
	p.eval(`document.querySelector("#view").textContent`, &summary)
	for _, s := range []string{"Previous month ingestion", "Projected ingestion by EOM", "Current month billable storage"} {
		if !strings.Contains(summary, s) {
			t.Errorf("the Logs Storage summary has no %s", s)
		}
	}
	p.eval(`document.querySelector("#list-action-usage-alert").click(), true`, nil)
	p.waitFor(`document.querySelector(".modal #f-threshold") !== null`)
	setField(p, ".modal", "f-threshold", "1")
	p.clickText(".modal button", "Create policy")
	p.waitFor(`document.querySelector(".modal #f-threshold") === null`)
	if _, body := consoleDo(t, http.MethodGet, "/api/resources/monitoring-alerting?project="+project, ""); !strings.Contains(body, "Logs ingestion usage") {
		t.Errorf("after Create usage alert, the policies = %s", body)
	}

	// Queries provided by Google Cloud.
	p.navigate("/logs?scope=project&project=" + project)
	p.waitFor(`document.querySelector("#log-query-library") !== null`)
	p.eval(`document.querySelector("#log-query-library").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-library-tab-google") !== null`)
	p.eval(`document.querySelector("#log-library-tab-google").click(), true`, nil)
	p.waitFor(`[...document.querySelectorAll(".modal .query-library-heading")].some((h) => h.textContent === "Cloud Storage")`)
	p.eval(`[...document.querySelectorAll(".modal .query-library-item")].find((li) => li.querySelector("h4").textContent === "Cloud Storage bucket logs").querySelector("button").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-query").value === 'resource.type="gcs_bucket" AND resource.labels.bucket_name="BUCKET_NAME"' && document.querySelector("#log-library-tab-google") === null`)

	auditScreens(t, []a11yPage{
		{"Log Analytics", "/logs/analytics?project=" + project, settled + ` && document.querySelector(".log-view-list") !== null`, ""},
		{"Log Router", "/logs/sinks?project=" + project, settled + fmt.Sprintf(` && document.querySelector(%q) !== null`, row), ""},
		{"Logs Storage", "/logs/storage?project=" + project, settled + ` && document.querySelector("#list-action-usage-alert") !== null`, ""},
		{"Create usage alert dialog", "/logs/storage?project=" + project, settled + ` && document.querySelector("#list-action-usage-alert") !== null`,
			`document.querySelector("#list-action-usage-alert").click()`},
		{"Queries provided by Google Cloud dialog", "/logs?scope=project&project=" + project, settled + ` && document.querySelector("#log-query-library") !== null`,
			`document.querySelector("#log-query-library").click(); document.querySelector("#log-library-tab-google").click()`},
	})
}
