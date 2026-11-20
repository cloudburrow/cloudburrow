package iameval

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestSchedulerWorkflowsPermissionsExist (#1879): every permission the
// Cloud Scheduler and Workflows tables check is one Google's permission
// index names, so a typo cannot deny what no role can grant.
func TestSchedulerWorkflowsPermissionsExist(t *testing.T) {
	names := iamroles.Names()
	var perms []string
	for _, svc := range []*Service{Scheduler, Workflows} {
		for full, m := range svc.Methods {
			if m.Permission == "" {
				t.Errorf("%s has no permission", full)
			}
			perms = append(perms, m.Permission)
		}
	}
	for _, m := range WorkflowsJSON {
		perms = append(perms, m.Permission)
	}
	for _, p := range perms {
		if _, ok := slices.BinarySearch(names, p); !ok {
			t.Errorf("%s is not in Google's permission index", p)
		}
	}
}

// TestWorkflowsJSONOnlyMethods (#1879): a step entries read without
// workflows.stepEntries.list is Google's 403 envelope, and a path that is
// none of them is served.
func TestWorkflowsJSONOnlyMethods(t *testing.T) {
	e := &Enforcer{Eval: &Evaluator{Source: fakeSource{}}, On: func() bool { return true },
		CallerOf: func(string) Caller { return Caller{Email: sa} }}
	ran := false
	h := e.HTTPPaths(Workflows, WorkflowsJSON, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	exec := "projects/p/locations/us-central1/workflows/w/executions/e"
	for _, c := range []struct{ method, path, perm, resource string }{
		{http.MethodGet, "/v1/" + exec + "/stepEntries", "workflows.stepEntries.list", exec},
		{http.MethodGet, "/v1/" + exec + "/stepEntries/3", "workflows.stepEntries.get", exec + "/stepEntries/3"},
		{http.MethodGet, "/v1/" + exec + "/callbacks", "workflows.callbacks.list", exec},
		{http.MethodPost, "/v1/" + exec + "/callbacks/cb", "workflows.callbacks.send", exec},
		{http.MethodGet, "/v1/" + exec + ":exportData", "workflows.executions.get", exec},
		{http.MethodPost, "/v1/" + exec + ":deleteExecutionHistory", "workflows.executions.cancel", exec},
		{http.MethodPost, "/v1/projects/p/locations/us-central1/workflows/w:triggerPubsubExecution", "workflows.executions.create",
			"projects/p/locations/us-central1/workflows/w"},
	} {
		ran = false
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Authorization", "Bearer x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var body struct{ Error struct{ Message string } }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		want := "Permission '" + c.perm + "' denied on resource '" + c.resource + "' (or it may not exist)."
		if ran || w.Code != http.StatusForbidden || body.Error.Message != want {
			t.Errorf("%s %s: %d %s ran=%v, want %q", c.method, c.path, w.Code, w.Body, ran, want)
		}
	}
	ran = false
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/"+exec, nil))
	if !ran {
		t.Fatal("a bound method's path was not passed on")
	}
}
