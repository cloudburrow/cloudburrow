package logging

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestObservabilityScopes (#2020): the _Default scope starts on the
// project's _Default log scope; patch sets a held log scope and refuses a
// missing one, another project's and another location; the 501 for the
// API's other collections; and deleting the default log scope resets it.
func TestObservabilityScopes(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	const path = "/v1/projects/proj-1/locations/global/scopes/_Default"
	get := func() ObservabilityScope {
		t.Helper()
		code, body := a.CallObservability(http.MethodGet, path, nil)
		var s ObservabilityScope
		if code != http.StatusOK || json.Unmarshal(body, &s) != nil {
			t.Fatalf("get = %d %s", code, body)
		}
		return s
	}
	if s := get(); s.LogScope != "//logging.googleapis.com/projects/proj-1/locations/global/logScopes/_Default" || s.UpdateTime != "" ||
		s.TraceScope != "projects/proj-1/locations/global/traceScopes/_Default" {
		t.Errorf("initial scope %+v", s)
	}
	if code, body := a.CallExtra(http.MethodPost, "/v2/projects/proj-1/locations/global/logScopes?logScopeId=two",
		[]byte(`{"resourceNames":["projects/proj-1","projects/proj-2"]}`)); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, _ := a.CallExtra(http.MethodPost, "/v2/projects/proj-1/locations/global/logScopes?logScopeId=six",
		[]byte(`{"resourceNames":["projects/proj-a","projects/proj-b","projects/proj-c","projects/proj-d","projects/proj-e","projects/proj-f"]}`)); code != http.StatusBadRequest {
		t.Errorf("a scope of six projects = %d", code)
	}
	if code, _ := a.CallExtra(http.MethodPost, "/v2/projects/proj-1/locations/global/logScopes?logScopeId=_Default",
		[]byte(`{"resourceNames":["projects/proj-1"]}`)); code != http.StatusBadRequest {
		t.Errorf("creating _Default = %d", code)
	}
	if code, body := a.CallObservability(http.MethodPost, "/v1/projects/proj-1/locations/global/traceScopes?traceScopeId=t",
		[]byte(`{"resourceNames":["projects/proj-1"]}`)); code != http.StatusOK {
		t.Fatalf("create trace scope = %d %s", code, body)
	}
	for body, want := range map[string]int{
		`{"traceScope":"projects/proj-1/locations/global/traceScopes/none"}`:                      http.StatusNotFound,
		`{"logScope":"//logging.googleapis.com/projects/proj-1/locations/global/logScopes/none"}`: http.StatusNotFound,
		`{"logScope":"//logging.googleapis.com/projects/proj-2/locations/global/logScopes/two"}`:  http.StatusBadRequest,
		`{"logScope":"projects/proj-1/locations/global/logScopes/two"}`:                           http.StatusBadRequest,
		`{"traceScope":"projects/proj-1/locations/global/traceScopes/t"}`:                         http.StatusOK,
		`{"logScope":"//logging.googleapis.com/projects/proj-1/locations/global/logScopes/two"}`:  http.StatusOK,
	} {
		if code, out := a.CallObservability(http.MethodPatch, path, []byte(body)); code != want {
			t.Errorf("patch %s = %d %s; want %d", body, code, out, want)
		}
	}
	if code, _ := a.CallObservability(http.MethodPatch, path+"?updateMask=name", []byte(`{}`)); code != http.StatusBadRequest {
		t.Errorf("an unknown mask path = %d", code)
	}
	s := get()
	if !strings.HasSuffix(s.LogScope, "/logScopes/two") || s.TraceScope != "projects/proj-1/locations/global/traceScopes/t" || s.UpdateTime == "" {
		t.Errorf("after patch %+v", s)
	}
	for p, want := range map[string]int{
		"/v1/projects/proj-1/locations/us-central1/scopes/_Default": http.StatusBadRequest,
		"/v1/projects/proj-1/locations/global/scopes/other":         http.StatusNotFound,
		"/v1/projects/proj-1/locations/global/settings":             http.StatusOK, // served since #2117
		"/v1/projects/proj-1/locations/global/widgets":              http.StatusNotFound,
	} {
		if code, _ := a.CallObservability(http.MethodGet, p, nil); code != want {
			t.Errorf("GET %s = %d; want %d", p, code, want)
		}
	}
	if code, body := a.CallExtra(http.MethodDelete, "/v2/projects/proj-1/locations/global/logScopes/two", nil); code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, body)
	}
	if s := get(); !strings.HasSuffix(s.LogScope, "/logScopes/_Default") {
		t.Errorf("after deleting the default log scope %+v", s)
	}
}
