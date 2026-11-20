//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestLoggingExplorerAsGoogleLaysItOut (#1863), in the storage shard, whose
// instance serves Cloud Logging. Cloud Logging's menu is in Google's order;
// the Logs Explorer's query editor takes the Logging query language and Run
// query applies it, refusing what it cannot evaluate on the pane; the
// Severity menu writes its clause; the Fields pane counts values and adds
// one to the query; an entry expands to its fields, with Expand nested
// fields, Copy link and Hide matching entries; the Timeline collapses; the
// time range is in the address; and the query's Actions open Create metric
// with the query as its filter. Log Analytics says it is not available
// locally and Logs Dashboard counts the entries.
func TestLoggingExplorerAsGoogleLaysItOut(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	write := func(log, severity, text string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/" + log, "resource": map[string]string{"type": "global"},
			"entries": []map[string]string{{"severity": severity, "textPayload": text}}})
		if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
			t.Fatalf("entries:write = %d: %s", code, out)
		}
	}
	shown := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === %q)`, text)
	}
	write("query-app", "INFO", "q-info")
	write("query-app", "ERROR", "q-error")
	write("other-app", "WARNING", "q-other")

	p := open(t)
	setQuery := func(q string) {
		t.Helper()
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector("#log-query"); e.value = %q; document.querySelector("#log-run-query").click(); return true; })()`, q), nil)
	}
	p.navigate("/logs?scope=project&project=" + project)
	p.waitFor(shown("q-info") + ` && ` + shown("q-error") + ` && ` + shown("q-other"))

	// The menu, in Google's order.
	var menu []string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).filter(Boolean)`, &menu)
	want := []string{"Logs Explorer", "Log Analytics", "Logs Dashboard", "Log-based Metrics", "Log Router", "Logs Storage"}
	if len(menu) < len(want) {
		t.Fatalf("Cloud Logging's menu is %q, want it to start %q", menu, want)
	}
	for i, w := range want {
		if !strings.Contains(menu[i], w) {
			t.Errorf("Cloud Logging's menu is %q, want it to start %q", menu, want)
			break
		}
	}

	// The query pane: Search all fields, Resource, Log name, Severity, the
	// editor and Run query.
	for _, sel := range []string{"#contains", "#log-resource-type", "#log-name", "#severity", "#log-query", "#log-run-query", "#log-time-range", "#log-fields", "#log-timeline"} {
		var there bool
		p.eval(fmt.Sprintf(`document.querySelector(%q) !== null`, sel), &there)
		if !there {
			t.Errorf("the Logs Explorer has no %s", sel)
		}
	}
	setQuery("severity>=ERROR")
	p.waitFor(shown("q-error") + ` && !` + shown("q-info") + ` && !` + shown("q-other"))
	p.waitFor(`location.search.includes("query=severity")`)

	// A query with an unmatched parenthesis, which the language's syntax
	// checklist names (logging-query-language, Syntax issues), is refused
	// on the pane, naming where. (severity>=ERROR), which this used before
	// #2026, is in the language and runs.
	setQuery("(severity>=ERROR")
	p.waitFor(`!document.querySelector("#log-query-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("#log-query-error").textContent`, &refusal)
	if !strings.Contains(refusal, "syntax error") || !strings.Contains(refusal, "<EOF>") {
		t.Errorf("the refused query says %q, want why", refusal)
	}
	p.forgive()

	// The Severity menu writes its clause into the editor.
	setQuery("")
	p.waitFor(shown("q-info"))
	p.setField("#severity", "WARNING")
	p.waitFor(`document.querySelector("#log-query").value === "severity>=WARNING"`)
	p.waitFor(shown("q-other") + ` && !` + shown("q-info"))

	// The Fields pane: Log name values with their counts; choosing one
	// adds it to the query.
	setQuery("")
	logName := "projects/" + project + "/logs/query-app"
	value := fmt.Sprintf(`[...document.querySelectorAll("#log-fields [data-field='logName'] .log-field-value")].find((b) => b.dataset.value === %q)`, logName)
	p.waitFor(value + ` !== undefined && ` + value + `.querySelector(".log-field-count").textContent === "2"`)
	p.eval(value+`.click(), true`, nil)
	p.waitFor(fmt.Sprintf(`document.querySelector("#log-query").value === %q`, `logName="`+logName+`"`))
	p.waitFor(shown("q-info") + ` && ` + shown("q-error") + ` && !` + shown("q-other"))

	// An entry expands to its fields; Hide matching entries adds the
	// negated clause.
	row := `[...document.querySelectorAll("#view tr.log-entry")].find((r) => [...r.cells].some((c) => c.textContent === "q-error"))`
	p.eval(row+`.querySelector(".log-expand").click(), true`, nil)
	p.waitFor(`document.querySelector("#view tr.log-detail") !== null`)
	var actions []string
	p.eval(`[...document.querySelectorAll("#view tr.log-detail .log-detail-actions button")].map((b) => b.textContent)`, &actions)
	for _, w := range []string{"Expand nested fields", "Collapse nested fields", "Copy link", "Copy as JSON"} {
		if !strings.Contains(strings.Join(actions, "|"), w) {
			t.Errorf("an expanded entry offers %q, want %q", actions, w)
		}
	}
	leaf := `document.querySelector("#view tr.log-detail [data-field='textPayload']")`
	p.waitFor(leaf + ` !== null`)
	p.eval(leaf+`.querySelector(".overflow-trigger").click(), true`, nil)
	p.clickText(`#view tr.log-detail [data-field='textPayload'] .overflow-item`, "Hide matching entries")
	p.waitFor(`document.querySelector("#log-query").value.includes('-textPayload="q-error"')`)
	p.waitFor(shown("q-info") + ` && !` + shown("q-error"))

	// The Timeline collapses; the time range is carried in the address.
	p.eval(`document.querySelector("#log-timeline-toggle").click(), true`, nil)
	p.waitFor(`document.querySelector("#log-timeline-toggle").textContent === "Expand timeline"`)
	p.setField("#log-time-range", "15m")
	p.waitFor(`location.search.includes("timeRange=15m")`)

	// Actions > Create metric opens Create metric with the query as its
	// filter.
	setQuery("severity>=ERROR")
	p.waitFor(shown("q-error"))
	p.eval(`document.querySelector('.log-results-head [aria-label="Actions"]').click(), true`, nil)
	var items []string
	p.eval(`[...document.querySelector('.log-results-head [aria-label="Actions"]').closest(".overflow").querySelectorAll(".overflow-item")].map((b) => b.textContent)`, &items)
	// Round 2 (#1863): Create log alert (disabled without Cloud
	// Monitoring) and Highlight in results, in Google's menu.
	if strings.Join(items, "|") != "Create metric|Create sink|Create log alert|Highlight in results|Download" {
		t.Errorf("the query's Actions are %q, want Create metric, Create sink, Create log alert, Highlight in results, Download", items)
	}
	p.clickText(".log-results-head .overflow-item", "Create metric")
	p.waitFor(`location.pathname === "/logs/metrics/create" && document.querySelector("#f-filter") !== null`)
	p.waitFor(`document.querySelector("#f-filter").value === "severity>=ERROR"`)

	// Log Analytics is served wherever BigQuery is, with or without a
	// linked dataset (#2021), and is not available locally elsewhere.
	p.navigate("/logs/analytics?project=" + project)
	if serviceServed(t, "logging-analytics") {
		p.waitFor(`[...document.querySelectorAll("#view .log-views h2")].some((h) => h.textContent === "Log views")`)
	} else {
		p.waitFor(`document.querySelector("#view").textContent.includes("Not available locally")`)
	}
	p.navigate("/logs/dashboard?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Log entries by severity")`)
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent === "ERROR")`)
}
