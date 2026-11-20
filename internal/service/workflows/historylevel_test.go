package workflows

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestExecutionHistoryLevel: the JSON API's Execution.executionHistoryLevel
// overrides the workflow's for one execution; detailed history records each
// step entry's variables and a for loop's expected iterations, basic does
// not; every entry carries its navigationInfo.
func TestExecutionHistoryLevel(t *testing.T) {
	s, err := NewServer("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	wc, _, base := startServer(t, s)
	w := deploy(t, wc, "levels", `main:
  steps:
    - init:
        assign:
          - total: 0
    - loop:
        for:
          value: v
          in: [1, 2, 3]
          steps:
            - add:
                assign:
                  - total: ${total + v}
    - done:
        return: ${total}
`)
	post := func(body string) map[string]any {
		t.Helper()
		resp, err := http.Post(base+"/v1/"+w.GetName()+"/executions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create %s: %d %v", body, resp.StatusCode, out)
		}
		return out
	}
	get := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(base + "/v1/" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	detailed := post(`{"argument": "{}", "executionHistoryLevel": "EXECUTION_HISTORY_DETAILED"}`)
	if detailed["executionHistoryLevel"] != "EXECUTION_HISTORY_DETAILED" {
		t.Fatalf("create answered %v", detailed)
	}
	basic := post(`{}`)
	if _, ok := basic["executionHistoryLevel"]; ok {
		t.Fatalf("an execution naming no level answered %v", basic)
	}
	for _, e := range []map[string]any{detailed, basic} {
		s.Wait(e["name"].(string))
	}
	if got := get(detailed["name"].(string)); got["executionHistoryLevel"] != "EXECUTION_HISTORY_DETAILED" || got["result"] != "6" {
		t.Fatalf("get answered %v", got)
	}
	list := get(w.GetName() + "/executions")
	n := 0
	for _, e := range list["executions"].([]any) {
		if e.(map[string]any)["executionHistoryLevel"] == "EXECUTION_HISTORY_DETAILED" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("list: %d executions detailed, want 1: %v", n, list)
	}
	steps := func(name string) []map[string]any {
		var out []map[string]any
		for _, e := range get(name + "/stepEntries")["stepEntries"].([]any) {
			out = append(out, e.(map[string]any))
		}
		return out
	}
	var loop map[string]any
	for _, e := range steps(detailed["name"].(string)) {
		if e["step"] == "loop" {
			loop = e
		}
		if e["step"] == "done" {
			vars := e["variableData"].(map[string]any)["variables"].(map[string]any)
			if vars["total"] != float64(6) {
				t.Fatalf("done's variables: %v", vars)
			}
		}
	}
	meta := loop["stepEntryMetadata"].(map[string]any)
	if meta["expectedIteration"] != "3" || meta["progressType"] != "PROGRESS_TYPE_FOR" || meta["progressNumber"] != "3" {
		t.Fatalf("loop's metadata: %v", meta)
	}
	nav := loop["navigationInfo"].(map[string]any)
	if len(nav["children"].([]any)) != 3 || nav["previous"] != "1" || nav["next"] == nil {
		t.Fatalf("loop's navigationInfo: %v", nav)
	}
	for _, e := range steps(basic["name"].(string)) {
		if _, ok := e["variableData"]; ok {
			t.Fatalf("basic history recorded variables: %v", e)
		}
		if e["step"] == "loop" {
			if _, ok := e["stepEntryMetadata"].(map[string]any)["expectedIteration"]; ok {
				t.Fatalf("basic history recorded expected iterations: %v", e)
			}
		}
	}
	resp, err := http.Post(base+"/v1/"+w.GetName()+"/executions", "application/json", strings.NewReader(`{"executionHistoryLevel": "FULL"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown level answered %d", resp.StatusCode)
	}
}
