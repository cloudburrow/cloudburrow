//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestLoggingConsoleRound6 (#2032), on an instance serving Cloud Logging,
// Cloud Monitoring and BigQuery: the Logs Explorer's Fields pane has the
// label dimensions Google's System metadata has where the entries carry
// their labels, Workload / Service from k8s-pod/app and service.name, a
// value adding its own label's clause; and a Log Analytics chart saved to
// a dashboard is drawn there, its SQL run through Log Analytics, as a bar
// chart of the results, and a table saved as a Table. Then the pages
// through the WCAG 2.1 AA audit.
func TestLoggingConsoleRound6(t *testing.T) {
	needService(t, "logging-analytics")
	needService(t, "monitoring-dashboards")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	monAddr := endpoint(t, envMonitoring)
	project := uniqueProject(t)
	id := "round6"
	bucket := "projects/" + project + "/locations/global/buckets/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/logging-sinks?project="+project+"&name="+url.QueryEscape("projects/"+project+"/sinks/round6-sink"), "")
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
	sk, _ := json.Marshal(map[string]string{"name": "round6-sink", "service": "Cloud Logging bucket", "destination": bucket, "filter": ""})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/logging-sinks?project="+project, string(sk)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create the sink = %d %s", code, out)
	}
	b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
		"entries": []map[string]any{
			{"severity": "INFO", "textPayload": "a", "labels": map[string]string{"k8s-pod/app": "frontend"}},
			{"severity": "INFO", "textPayload": "b", "labels": map[string]string{"k8s-pod/app": "frontend"}},
			{"severity": "ERROR", "textPayload": "c", "labels": map[string]string{"service.name": "checkout"}},
			{"severity": "ERROR", "textPayload": "d"},
			{"severity": "ERROR", "textPayload": "e"},
		}})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
		t.Fatalf("entries:write = %d: %s", code, out)
	}

	// The Fields pane's label dimension.
	p := open(t)
	p.navigate("/logs?scope=project&project=" + project)
	const dim = `([...document.querySelectorAll("#log-fields-system .log-field-group")].find((g) => g.dataset.field === 'labels."k8s-pod/app"') || null)`
	p.waitFor(dim + ` !== null`)
	var system string
	p.eval(`[...document.querySelectorAll("#log-fields-system .log-field-group h4")].map((h) => h.textContent).join("|")`, &system)
	if system != "Resource type|Severity|Workload / Service|Log name|Project ID" {
		t.Errorf("System metadata = %q, want Workload / Service after Resource type and Severity", system)
	}
	var values string
	p.eval(dim+`.querySelectorAll(".log-field-value").length + ":" + [...`+dim+`.querySelectorAll(".log-field-value")].map((v) => v.dataset.value + "=" + v.querySelector(".log-field-count").textContent).join(",")`, &values)
	if values != "2:frontend=2,checkout=1" {
		t.Errorf("Workload / Service = %q, want frontend (2) and checkout (1)", values)
	}
	p.eval(dim+`.querySelector('.log-field-value[data-value="checkout"]').click(), true`, nil)
	p.waitFor(`document.querySelector("#log-query").value.includes('labels."service.name"="checkout"')`)
	// Narrowed to the one entry: the dimension then holds checkout alone.
	p.waitFor(dim + ` !== null && ` + dim + `.querySelectorAll(".log-field-value").length === 1`)

	// A Log Analytics chart and table saved to a dashboard, then drawn.
	ref := project + ".global." + id + "._AllLogs"
	sql := "SELECT severity, COUNT(*) AS n FROM `" + ref + "` GROUP BY severity ORDER BY severity"
	saveTo := func(dashboard, name, title, kind string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{}, "Action": "save-chart-to-dashboard",
			"Values": map[string]string{"dashboard": dashboard, "dashboardName": name, "title": title, "kind": kind, "sql": sql}})
		if code, out := consoleDo(t, http.MethodPost, "/api/actions/logging-analytics?project="+project, string(body)); code != http.StatusOK {
			t.Fatalf("Save to dashboard = %d %s", code, out)
		}
	}
	saveTo("New dashboard", "Severities", "By severity", "Bar")
	code, body := do(t, http.MethodGet, "http://"+monAddr+"/v1/projects/"+project+"/dashboards", "", "")
	var l struct{ Dashboards []struct{ Name string } }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &l) != nil || len(l.Dashboards) != 1 {
		t.Fatalf("dashboards.list = %d %s", code, body)
	}
	dash := l.Dashboards[0].Name[strings.LastIndex(l.Dashboards[0].Name, "/")+1:]
	saveTo(dash, "", "Severity table", "Table")

	page := "/monitoring/dashboards/" + dash + "?project=" + project
	p.navigate(page + "&tab=chart-1")
	p.waitFor(`document.querySelector("#view svg.bq-chart") !== null`)
	var label string
	var bars int
	p.eval(`document.querySelector("#view svg.bq-chart").getAttribute("aria-label")`, &label)
	p.eval(`document.querySelectorAll("#view svg.bq-chart rect[fill='#1a73e8']").length`, &bars)
	if label != "Bar chart of n by severity, 2 points" || bars != 2 {
		t.Errorf("the dashboard's chart = %q with %d bars, want a bar chart of n by severity, ERROR and INFO", label, bars)
	}
	var tip string
	p.eval(`[...document.querySelectorAll("#view svg.bq-chart rect title")].map((t) => t.textContent).join("|")`, &tip)
	if tip != "ERROR: n 3|INFO: n 2" {
		t.Errorf("the bars = %q, want ERROR: n 3|INFO: n 2", tip)
	}
	p.navigate(page + "&tab=chart-2")
	p.waitFor(`document.querySelector("#view table.analytics-widget-table") !== null`)
	var cells string
	p.eval(`[...document.querySelectorAll("#view table.analytics-widget-table tbody td")].map((td) => td.textContent).join(",")`, &cells)
	if cells != "ERROR,3,INFO,2" {
		t.Errorf("the dashboard's table = %q, want ERROR,3,INFO,2", cells)
	}
	// The Widgets tab lists each with its query and how many rows it read.
	p.navigate(page)
	p.waitFor(`document.querySelector("#view").textContent.includes("Log Analytics query: SELECT severity") &&
		document.querySelector("#view").textContent.includes("(2 rows)")`)

	auditScreens(t, []a11yPage{
		{"Logs Explorer with a label dimension", "/logs?scope=project&project=" + project, settled + ` && ` + dim + ` !== null`, ""},
		{"Dashboard's Log Analytics chart", page + "&tab=chart-1", settled + ` && document.querySelector("#view svg.bq-chart") !== null`, ""},
		{"Dashboard's Log Analytics table", page + "&tab=chart-2", settled + ` && document.querySelector("#view table.analytics-widget-table") !== null`, ""},
	})
}
