//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestWorkflowsThroughTheBrowser (#1986, #2004), in the run shard, whose
// instance serves Workflows. Reached from the drawer's Integration services
// row: Create workflow with Google's fields, its source typed into the
// editor, whose syntax is highlighted and whose graph is drawn beside it as
// it changes; the workflow's Source tab shows the source highlighted beside
// the graph; Execute with an input; then on the Executions tab the
// execution's page with Google's Summary (its input and output), Steps
// (the visualization, each step marked succeeded, over the step entries)
// and Logs (the execution's entries, sys.log's among them). The console API
// reads each result back from the server the SDK calls.
func TestWorkflowsThroughTheBrowser(t *testing.T) {
	needService(t, "workflows")
	project := uniqueProject(t)
	id := "browser-wf"
	name := "projects/" + project + "/locations/us-east1/workflows/" + id
	detail := func(path ...string) map[string]any {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/workflows?"+q.Encode(), "")
		var d map[string]any
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("read %v through the console API = %d (%v): %s", path, code, err, body)
		}
		return d
	}
	const source = "main:\n  params: [input]\n  steps:\n    - log:\n        call: sys.log\n        args:\n          text: ${\"doubling \" + string(input.n)}\n" +
		"    - check:\n        switch:\n          - condition: ${input.n < 0}\n            raise: negative\n    - double:\n        return: ${input.n * 2}\n"

	p := open(t)
	p.navigateByDrawer("Integration services", "/workflows", "Workflows")
	p.navigate("/workflows?project=" + project)
	p.clickText("#view button", "Create workflow")
	// Google's fields, the editor last.
	p.waitFor(`["f-workflowId", "f-location", "f-description", "f-serviceAccount", "f-callLogLevel", "f-executionHistoryLevel",
		"f-userEnvVars", "f-labels", "f-sourceContents"].every((id) => document.getElementById(id) !== null) &&
		[...document.querySelectorAll('#view input[type="radio"]')].some((r) => r.value === "cmek")`)
	p.setField("#f-sourceContents", source)
	// Highlighted as it is typed, and drawn beside it.
	p.waitFor(`[...document.querySelectorAll("#view .wf-highlight .tok-key")].some((k) => k.textContent === "switch") &&
		[...document.querySelectorAll("#view .wf-highlight .tok-expr")].some((k) => k.textContent === "${input.n * 2}") &&
		["log", "check", "double"].every((n) => [...document.querySelectorAll("#view .wf-graph .wf-name")].some((t) => t.textContent === n))`)
	// Google's Triggers section (#2011): Add new trigger, Cloud Scheduler.
	p.clickText("#view button", "Add new trigger: Cloud Scheduler")
	p.waitFor(`document.querySelector('#view [aria-label="Name 1"]') !== null`)
	for label, v := range map[string]string{"Name 1": "browser-wf-every-day", "Frequency 1": "0 9 * * *",
		"Workflow's argument 1": `{"n": 5}`, "Service account 1": "invoker@" + project + ".iam.gserviceaccount.com"} {
		p.setField(fmt.Sprintf(`#view [aria-label=%q]`, label), v)
	}
	p.submitForm(map[string]string{"workflowId": id, "location": "us-east1", "description": "from the browser",
		"callLogLevel": "LOG_ALL_CALLS", "executionHistoryLevel": "EXECUTION_HISTORY_DETAILED"}, "workflowId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/workflows" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, name))
	if got := fmt.Sprint(detail(name)["sections"]); !strings.Contains(got, "All calls") || !strings.Contains(got, "Detailed") {
		t.Errorf("the workflow's Details read back as %s; want All calls and Detailed", got)
	}

	// The trigger is a Cloud Scheduler job that runs the workflow, on its
	// Triggers tab.
	var triggers string
	for _, s := range detail(name)["sections"].([]any) {
		if sec := s.(map[string]any); sec["id"] == "triggers" {
			b, _ := json.Marshal(sec)
			triggers = string(b)
		}
	}
	if !strings.Contains(triggers, "browser-wf-every-day") || !strings.Contains(triggers, "Cloud Scheduler") || !strings.Contains(triggers, "0 9 * * *") {
		t.Errorf("the Triggers tab = %s", triggers)
	}
	p.follow(name)
	p.waitFor(`location.pathname.endsWith("browser-wf")`)
	p.clickText(`#view [role="tab"]`, "Triggers")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-wf-every-day"))`)
	p.clickText(`#view [role="tab"]`, "Source")
	p.waitFor(`[...document.querySelectorAll("#view .wf-highlight .tok-expr")].some((k) => k.textContent === "${input.n * 2}") &&
		document.querySelectorAll("#view .wf-graph .wf-node").length === 3`)
	// Execute with its own Execution history level, over the workflow's
	// Detailed (#2011).
	p.pageAction("Execute", map[string]string{"argument": `{"n": 21}`, "executionHistoryLevel": "EXECUTION_HISTORY_BASIC"}, "")

	// The execution's ID is the console API's; its page is reached from the
	// Executions tab.
	var execID string
	for _, s := range detail(name)["sections"].([]any) {
		sec := s.(map[string]any)
		if sec["id"] != "executions" {
			continue
		}
		items, _ := sec["listing"].(map[string]any)["items"].([]any)
		if len(items) == 1 {
			execID, _ = items[0].(map[string]any)["name"].(string)
		}
	}
	if execID == "" {
		t.Fatalf("Execute made no execution: %v", detail(name)["sections"])
	}
	p.navigate("/workflows?project=" + project)
	p.follow(name)
	p.clickText(`#view [role="tab"]`, "Executions")
	p.follow(execID)
	p.waitFor(`location.pathname.includes("executions")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("SUCCEEDED")`)
	p.waitFor(`[...document.querySelectorAll('#view [role="tab"]')].map((t) => t.textContent.trim()).join(",") === "Summary,Steps,Logs"`)
	p.waitFor(`document.querySelector("#view").textContent.includes("Execution duration") &&
		document.querySelector("#view").textContent.includes("42")`)
	p.clickText(`#view [role="tab"]`, "Steps")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("double"))`)
	// The visualization marks the steps that ran.
	p.waitFor(`document.querySelector("#view details.wf-visualization[open] .wf-graph") !== null &&
		document.querySelectorAll("#view .wf-node.is-succeeded").length === 3`)
	// Google's step detail panel (#2011): the check entry, between log and
	// double; Next moves the panel to double. Basic history keeps no
	// variables.
	p.waitFor(`document.querySelector("#wf-step-entry") !== null`)
	p.eval(`(() => { const s = document.querySelector("#wf-step-entry");
		s.value = [...s.options].find((o) => o.text.startsWith("check ")).value;
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelector(".wf-step-body h3").textContent === "check" &&
		[...document.querySelectorAll(".wf-step-nav .wf-step-link")].map((b) => b.textContent.split(" ")[0]).join(",") === "log,double" &&
		document.querySelector(".wf-step-body").textContent.includes("recorded with the Detailed")`)
	p.eval(`(() => { [...document.querySelectorAll(".wf-step-nav .wf-step-link")].find((b) => b.textContent.startsWith("double ")).click(); return true; })()`, nil)
	p.waitFor(`document.querySelector(".wf-step-body h3").textContent === "double" && document.activeElement.id === "wf-step-entry"`)
	p.clickText(`#view [role="tab"]`, "Logs")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("doubling 21"))`)
	ed := detail(name, name+"/executions/"+execID)
	if got := fmt.Sprint(ed["summary"]); !strings.Contains(got, "SUCCEEDED") {
		t.Errorf("the execution reads %s through the console API", got)
	}
	if got := fmt.Sprint(ed["sections"]); !strings.Contains(got, "label:Execution history level value:Basic") {
		t.Errorf("the execution's Summary has no Basic history level: %s", got)
	}
}

// TestWorkflowsScreensMeetWCAG (#2004): the Workflows list, Create
// workflow with its editor and graph, a workflow's Source tab, and an
// execution's Summary, Steps (with the visualization) and Logs tabs.
func TestWorkflowsScreensMeetWCAG(t *testing.T) {
	needService(t, "workflows")
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/workflows/a11y-wf"
	src := "main:\n  steps:\n    - init:\n        assign:\n          - x: 2\n    - loop:\n        for:\n          value: v\n          in: [1, 2]\n" +
		"          steps:\n            - add:\n                assign:\n                  - x: ${x + v}\n    - done:\n        return: ${x}\n"
	b, _ := json.Marshal(map[string]string{"workflowId": "a11y-wf", "location": "us-central1", "encryption": "google",
		"callLogLevel": "LOG_ALL_CALLS", "executionHistoryLevel": "EXECUTION_HISTORY_BASIC", "sourceContents": src})
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/workflows?project="+project, string(b)); code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}
	b, _ = json.Marshal(map[string]any{"Path": []string{name}, "Action": "execute",
		"Values": map[string]string{"executionHistoryLevel": "EXECUTION_HISTORY_DETAILED"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/workflows?project="+project, string(b)); code != http.StatusOK {
		t.Fatalf("execute = %d: %s", code, body)
	}
	var exec string
	for deadline := time.Now().Add(30 * time.Second); exec == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		q := url.Values{"project": {project}, "name": {name}}
		_, body := consoleDo(t, http.MethodGet, "/api/detail/workflows?"+q.Encode(), "")
		var d struct {
			Sections []struct {
				ID      string
				Listing struct {
					Items []struct{ Name, Status string }
				}
			}
		}
		_ = json.Unmarshal([]byte(body), &d)
		for _, s := range d.Sections {
			if s.ID == "executions" && len(s.Listing.Items) == 1 && s.Listing.Items[0].Status == "SUCCEEDED" {
				exec = s.Listing.Items[0].Name
			}
		}
	}
	if exec == "" {
		t.Fatal("the execution did not succeed")
	}
	q := "?project=" + url.QueryEscape(project)
	// A resource's address is its path, each segment escaped (detailHref).
	wf := "/workflows/" + url.PathEscape(name)
	run := wf + "/" + url.PathEscape(name+"/executions/"+exec)
	auditScreens(t, []a11yPage{
		{"Workflows", "/workflows" + q, rowReady(name), ""},
		{"Create workflow", "/workflows/create" + q, settled + ` && document.querySelector("#view .wf-graph") !== null`, ""},
		{"Source tab", wf + q + "&tab=source", settled + ` && document.querySelector("#view .wf-graph") !== null`, ""},
		{"Execution summary", run + q, settled + ` && document.querySelector("#view").textContent.includes("Execution duration")`, ""},
		{"Execution steps", run + q + "&tab=steps", settled + ` && document.querySelector("#view .wf-node.is-succeeded") !== null`, ""},
		// The step detail panel on the loop's entry, with its children and
		// (detailed history) its variables (#2011).
		{"Step detail panel", run + q + "&tab=steps", settled + ` && document.querySelector("#wf-step-entry") !== null`,
			`const s = document.querySelector("#wf-step-entry"); s.value = [...s.options].find((o) => o.text.startsWith("loop ")).value;
			s.dispatchEvent(new Event("change", { bubbles: true }))`},
		{"Create workflow triggers", "/workflows/create" + q, settled + ` && document.querySelector("#view .wf-graph") !== null`,
			`[...document.querySelectorAll("#view button")].filter((b) => b.textContent.startsWith("Add new trigger")).forEach((b) => b.click())`},
		{"Triggers tab", wf + q + "&tab=triggers", settled, ""},
		{"Execution logs", run + q + "&tab=logs", settled + ` && document.querySelector("#view tbody tr") !== null`, ""},
	})
}
