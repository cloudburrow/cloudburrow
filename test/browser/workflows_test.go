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

// TestWorkflowsThroughTheBrowser (#1986), in the run shard, whose instance
// serves Workflows. Reached from the drawer's Integration services row:
// Create workflow with its source in the editor, Execute on the
// workflow's page with an input, then on the Executions tab the
// execution's page with its Output and its Steps; the console API reads
// each result back from the server the SDK calls.
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

	p := open(t)
	p.navigateByDrawer("Integration services", "/workflows", "Workflows")
	p.navigate("/workflows?project=" + project)
	p.clickText("#view button", "Create workflow")
	p.submitForm(map[string]string{"workflowId": id, "location": "us-east1", "description": "from the browser",
		"sourceContents": "main:\n  params: [input]\n  steps:\n    - double:\n        return: ${input.n * 2}\n"}, "workflowId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/workflows" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, name))

	p.follow(name)
	p.waitFor(`location.pathname.endsWith("browser-wf")`)
	p.clickText(`#view [role="tab"]`, "Source")
	p.waitFor(`document.querySelector("#view").textContent.includes("input.n * 2")`)
	p.pageAction("Execute", map[string]string{"argument": `{"n": 21}`}, "")

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
	p.clickText(`#view [role="tab"]`, "Output")
	p.waitFor(`document.querySelector("#view").textContent.includes("42")`)
	p.clickText(`#view [role="tab"]`, "Steps")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("double"))`)
	if got := fmt.Sprint(detail(name, name+"/executions/"+execID)["summary"]); !strings.Contains(got, "SUCCEEDED") {
		t.Errorf("the execution reads %s through the console API", got)
	}
}
