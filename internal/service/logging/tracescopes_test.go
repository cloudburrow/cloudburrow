package logging

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestTraceScopes (#2108): _Default holds the project and can't be changed
// or deleted; a trace scope is created, listed after _Default, patched and
// deleted; the documented limits are enforced; and deleting the default
// trace scope makes _Default the default again.
func TestTraceScopes(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	const base = "/v1/projects/proj-1/locations/global/traceScopes"
	call := func(method, path, body string) (int, TraceScope) {
		t.Helper()
		code, out := a.CallObservability(method, path, []byte(body))
		var ts TraceScope
		_ = json.Unmarshal(out, &ts)
		return code, ts
	}
	if code, ts := call(http.MethodGet, base+"/_Default", ""); code != http.StatusOK || len(ts.ResourceNames) != 1 || ts.ResourceNames[0] != "projects/proj-1" {
		t.Errorf("_Default = %d %+v", code, ts)
	}
	if code, _ := call(http.MethodPatch, base+"/_Default", `{"description":"x"}`); code != http.StatusBadRequest {
		t.Errorf("patching _Default = %d, want 400 (FAILED_PRECONDITION)", code)
	}
	if code, _ := call(http.MethodDelete, base+"/_Default", ""); code != http.StatusBadRequest {
		t.Errorf("deleting _Default = %d", code)
	}
	code, ts := call(http.MethodPost, base+"?traceScopeId=team", `{"resourceNames":["projects/proj-1","projects/proj-2"],"description":"both"}`)
	if code != http.StatusOK || ts.Name != "projects/proj-1/locations/global/traceScopes/team" || ts.CreateTime == "" {
		t.Fatalf("create = %d %+v", code, ts)
	}
	if code, _ := call(http.MethodPost, base+"?traceScopeId=team", `{"resourceNames":["projects/proj-1"]}`); code != http.StatusConflict {
		t.Errorf("a second create = %d", code)
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf("%q", fmt.Sprintf("projects/p%d", i))
	}
	for body, want := range map[string]int{
		`{"resourceNames":[]}`:                                http.StatusBadRequest,
		`{"resourceNames":["folders/1"]}`:                     http.StatusBadRequest,
		`{"resourceNames":[` + strings.Join(many, ",") + `]}`: http.StatusBadRequest,
		`{"resourceNames":["projects/proj-1"],"description":"` + strings.Repeat("d", 8001) + `"}`: http.StatusBadRequest,
	} {
		if code, _ := call(http.MethodPost, base+"?traceScopeId=bad", body); code != want {
			t.Errorf("create %.60s = %d, want %d", body, code, want)
		}
	}
	code, out := a.CallObservability(http.MethodGet, base, nil)
	if code != http.StatusOK || !strings.Contains(string(out), `traceScopes/_Default"`) || !strings.Contains(string(out), "traceScopes/team") ||
		strings.Index(string(out), "_Default") > strings.Index(string(out), "team") {
		t.Errorf("list = %d %s", code, out)
	}
	if code, ts := call(http.MethodPatch, base+"/team?updateMask=resourceNames", `{"resourceNames":["projects/proj-3"]}`); code != http.StatusOK ||
		len(ts.ResourceNames) != 1 || ts.Description != "both" {
		t.Errorf("patch = %d %+v", code, ts)
	}
	scope := "/v1/projects/proj-1/locations/global/scopes/_Default"
	if code, out := a.CallObservability(http.MethodPatch, scope+"?updateMask=traceScope", []byte(`{"traceScope":"projects/proj-1/locations/global/traceScopes/team"}`)); code != http.StatusOK {
		t.Fatalf("set default = %d %s", code, out)
	}
	if code, _ := call(http.MethodDelete, base+"/team", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	if code, _ := call(http.MethodGet, base+"/team", ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d", code)
	}
	_, raw := a.CallObservability(http.MethodGet, scope, nil)
	if !strings.Contains(string(raw), "traceScopes/_Default") {
		t.Errorf("the default after deleting it: %s", raw)
	}
	for i := 0; i < MaxTraceScopes-1; i++ {
		if code, _ := call(http.MethodPost, fmt.Sprintf("%s?traceScopeId=s%d", base, i), `{"resourceNames":["projects/proj-1"]}`); code != http.StatusOK {
			t.Fatalf("scope %d = %d", i, code)
		}
	}
	if code, _ := call(http.MethodPost, base+"?traceScopeId=over", `{"resourceNames":["projects/proj-1"]}`); code != http.StatusTooManyRequests {
		t.Errorf("the 101st trace scope = %d, want 429", code)
	}
}
