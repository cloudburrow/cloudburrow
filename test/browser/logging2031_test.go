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

// TestLoggingConsoleRound5 (#2031), on an instance serving Cloud Logging,
// Cloud Monitoring and BigQuery: the Logs Explorer's Fields pane is split
// into System metadata and Frequent fields, the second listing the entries'
// json_payload fields, a value of which refines the query; the queries run
// are the user's recent queries, kept by Cloud Logging (recentQueries.list)
// rather than the browser, so they are listed after the browser's storage
// is cleared; Log Analytics' statements are kept too, and its chart is
// saved to a new and then to the same custom dashboard, a widget whose
// query is the SQL, read back through the Monitoring API. Then the pages
// and dialogs through the WCAG 2.1 AA audit.
func TestLoggingConsoleRound5(t *testing.T) {
	needService(t, "logging-analytics")
	needService(t, "monitoring-dashboards")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	monAddr := endpoint(t, envMonitoring)
	project := uniqueProject(t)
	id := "round5"
	bucket := "projects/" + project + "/locations/global/buckets/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-buckets?project="+project+"&name="+url.QueryEscape(bucket), "")
		_, body := consoleDo(t, http.MethodGet, "/api/resources/monitoring-dashboards?project="+project, "")
		var l struct{ Items []struct{ Name string } }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			consoleDo(t, http.MethodDelete, "/api/resources/monitoring-dashboards?project="+project+"&name="+url.QueryEscape(it.Name), "")
		}
	})
	create, _ := json.Marshal(map[string]string{"name": id, "location": "global", "retention": "30", "analytics": "true"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-buckets?project="+project, string(create)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create the bucket = %d %s", code, out)
	}
	sk, _ := json.Marshal(map[string]string{"name": "round5-sink", "service": "Cloud Logging bucket", "destination": bucket, "filter": ""})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-sinks?project="+project, string(sk)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create the sink = %d %s", code, out)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&name="+url.QueryEscape("projects/"+project+"/sinks/round5-sink"), "")
	})
	b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
		"entries": []map[string]any{
			{"severity": "INFO", "jsonPayload": map[string]any{"user": "alice", "request": map[string]any{"path": "/a"}}},
			{"severity": "INFO", "jsonPayload": map[string]any{"user": "alice", "request": map[string]any{"path": "/b"}}},
			{"severity": "ERROR", "jsonPayload": map[string]any{"user": "bob"}},
		}})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
		t.Fatalf("entries:write = %d: %s", code, out)
	}

	// The Fields pane's sections.
	p := open(t)
	p.navigate("/logs?scope=project&project=" + project)
	group := func(section, field string) string {
		return fmt.Sprintf(`document.querySelector('#%s .log-field-group[data-field=%q]')`, section, field)
	}
	p.waitFor(group("log-fields-frequent", "jsonPayload.user") + ` !== null`)
	var heads string
	p.eval(`[...document.querySelectorAll("#log-fields h3")].map((h) => h.textContent).join("|")`, &heads)
	if heads != "System metadata|Frequent fields" {
		t.Errorf("the Fields pane's sections = %q, want System metadata|Frequent fields", heads)
	}
	var system, frequent string
	p.eval(`[...document.querySelectorAll("#log-fields-system .log-field-group h4")].map((h) => h.textContent).join("|")`, &system)
	if !strings.HasPrefix(system, "Resource type|Severity|") {
		t.Errorf("System metadata = %q, want Resource type and Severity first", system)
	}
	p.eval(`[...document.querySelectorAll("#log-fields-frequent .log-field-group")].map((g) => g.dataset.field).join("|")`, &frequent)
	if frequent != "jsonPayload.user|jsonPayload.request.path" {
		t.Errorf("Frequent fields = %q, want jsonPayload.user (3 entries) then jsonPayload.request.path (2)", frequent)
	}
	var count string
	p.eval(group("log-fields-frequent", "jsonPayload.user")+`.querySelector('.log-field-value[data-value="alice"] .log-field-count').textContent`, &count)
	if count != "2" {
		t.Errorf("alice's count = %q, want 2", count)
	}
	p.eval(group("log-fields-frequent", "jsonPayload.user")+`.querySelector('.log-field-value[data-value="bob"]').click(), true`, nil)
	p.waitFor(`document.querySelector("#log-query").value.includes('jsonPayload.user="bob"')`)
	p.waitFor(`document.querySelectorAll("#view tbody tr.log-row, #view tbody tr[data-entry]").length === 1 || [...document.querySelectorAll("#view tbody td")].filter((td) => td.textContent === "ERROR").length >= 1`)

	// Recent queries are Cloud Logging's, not the browser's.
	p.eval(`(() => { const e = document.querySelector("#log-query"); e.value = "severity>=ERROR"; document.querySelector("#log-run-query").click(); return true; })()`, nil)
	recent := func() []string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/logs/recent-queries?project="+project+"&type=logging", "")
		if code != http.StatusOK {
			t.Fatalf("the recent queries = %d %s", code, body)
		}
		var l struct{ RecentQueries []struct{ Query string } }
		_ = json.Unmarshal([]byte(body), &l)
		var out []string
		for _, q := range l.RecentQueries {
			out = append(out, q.Query)
		}
		return out
	}
	p.waitFor(`document.querySelector("#log-query").value === "severity>=ERROR"`)
	deadline := 0
	for got := recent(); len(got) == 0 || got[0] != "severity>=ERROR"; got = recent() {
		if deadline++; deadline > 50 {
			t.Fatalf("recentQueries.list = %q, want severity>=ERROR first", got)
		}
		p.eval(`new Promise((r) => setTimeout(() => r(true), 100))`, nil)
	}
	if got := recent(); !strings.Contains(strings.Join(got, "\n"), `jsonPayload.user="bob"`) {
		t.Errorf("the recent queries = %q, want the Fields pane's query too", got)
	}
	p.eval(`localStorage.clear(), true`, nil)
	p.navigate("/logs?scope=project&project=" + project)
	p.waitFor(`document.querySelector("#log-query-library") !== null`)
	p.eval(`document.querySelector("#log-query-library").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-recent-queries") !== null`)
	var listed string
	p.eval(`document.querySelector("#log-recent-queries .query-library-item .query-library-text").textContent`, &listed)
	if listed != "severity>=ERROR" {
		t.Errorf("the Query library's first recent query = %q after the browser's storage was cleared, want severity>=ERROR", listed)
	}
	p.eval(`document.querySelector(".modal button[aria-label='Close'], .modal .modal-actions button") && document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })), true`, nil)

	// Log Analytics: the statement is kept, and its chart saved to a
	// dashboard.
	p.navigate("/logs/analytics?project=" + project)
	ref := project + ".global." + id + "._AllLogs"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".log-view-list button")].some((b) => b.textContent === %q)`, ref))
	sql := "SELECT severity, COUNT(*) AS n FROM `" + ref + "` GROUP BY severity"
	p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#view textarea.query-editor"); e.value = %q; return true; })()`, sql), nil)
	p.clickText("#view .query-pane button", "Run query")
	p.waitFor(`!document.querySelector("#log-analytics-chart").hidden && document.querySelector("#log-analytics-save-dashboard") !== null`)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#log-analytics-recent button")].some((b) => b.textContent === %q)`, sql))

	save := func(dashboard, name, title string) {
		t.Helper()
		p.eval(`document.querySelector("#log-analytics-save-dashboard").click(), true`, nil)
		p.waitFor(fmt.Sprintf(`document.querySelector(".modal #la-dash-choice") !== null && [...document.querySelectorAll(".modal #la-dash-choice option")].some((o) => o.textContent === %q)`, dashboard))
		p.eval(fmt.Sprintf(`(() => { const s = document.querySelector(".modal #la-dash-choice");
			s.value = [...s.options].find((o) => o.textContent === %q).value; s.dispatchEvent(new Event("change")); return true; })()`, dashboard), nil)
		if name != "" {
			setField(p, ".modal", "la-dash-name", name)
		}
		setField(p, ".modal", "la-dash-chart-title", title)
		p.clickText(".modal button", "Save")
		p.waitFor(`document.querySelector(".modal #la-dash-choice") === null`)
	}
	save("New dashboard", "Errors by severity", "By severity")
	type widget struct {
		Title   string
		XyChart *struct {
			DataSets []struct {
				PlotType        string
				TimeSeriesQuery struct{ OpsAnalyticsQuery struct{ SQL string } }
			}
		}
	}
	dashboards := func() []struct {
		Name, DisplayName string
		MosaicLayout      struct{ Tiles []struct{ Widget widget } }
	} {
		t.Helper()
		code, body := do(t, http.MethodGet, "http://"+monAddr+"/v1/projects/"+project+"/dashboards", "", "")
		if code != http.StatusOK {
			t.Fatalf("dashboards.list = %d %s", code, body)
		}
		var l struct {
			Dashboards []struct {
				Name, DisplayName string
				MosaicLayout      struct{ Tiles []struct{ Widget widget } }
			}
		}
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			t.Fatalf("dashboards.list: %v: %s", err, body)
		}
		return l.Dashboards
	}
	ds := dashboards()
	if len(ds) != 1 || ds[0].DisplayName != "Errors by severity" || len(ds[0].MosaicLayout.Tiles) != 1 {
		t.Fatalf("the dashboards = %+v, want Errors by severity with one tile", ds)
	}
	w := ds[0].MosaicLayout.Tiles[0].Widget
	if w.Title != "By severity" || w.XyChart == nil || len(w.XyChart.DataSets) != 1 ||
		w.XyChart.DataSets[0].TimeSeriesQuery.OpsAnalyticsQuery.SQL != sql || w.XyChart.DataSets[0].PlotType != "STACKED_BAR" {
		t.Errorf("the saved widget = %+v, want an xyChart whose query is the SQL", w)
	}
	save("Errors by severity", "", "Again")
	if ds := dashboards(); len(ds) != 1 || len(ds[0].MosaicLayout.Tiles) != 2 || ds[0].MosaicLayout.Tiles[1].Widget.Title != "Again" {
		t.Errorf("after saving to the same dashboard = %+v, want a second tile", ds)
	}
	dash := ds[0].Name[strings.LastIndex(ds[0].Name, "/")+1:]

	// The dashboard's page lists the chart's query.
	p.navigate("/monitoring/dashboards/" + dash + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Log Analytics query: SELECT severity")`)

	auditScreens(t, []a11yPage{
		{"Logs Explorer with its Fields pane", "/logs?scope=project&project=" + project, settled + ` && document.querySelector("#log-fields-frequent") !== null`, ""},
		{"Query library's Recent queries dialog", "/logs?scope=project&project=" + project, settled + ` && document.querySelector("#log-query-library") !== null`,
			`document.querySelector("#log-query-library").click()`},
		{"Log Analytics with its recent queries", "/logs/analytics?project=" + project, settled + ` && document.querySelector("#log-analytics-recent h2") !== null`, ""},
		// The ready check runs the query once, then waits for its chart.
		{"Save to dashboard dialog", "/logs/analytics?project=" + project + "&query=" + url.QueryEscape(sql), settled +
			` && (document.querySelector("#log-analytics-chart:not([hidden]) #log-analytics-save-dashboard") !== null ||
				(() => { const r = document.querySelector("#view .query-pane button.primary"); if (r && !r.dataset.ran) { r.dataset.ran = "1"; r.click(); } return false; })())`,
			`document.querySelector("#log-analytics-save-dashboard").click()`},
		{"Dashboard with a Log Analytics chart", "/monitoring/dashboards/" + dash + "?project=" + project, settled, ""},
	})
}
