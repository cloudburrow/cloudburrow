package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// iamEmulator is an emulator with tables only: GET answers each with its
// creationTime, DELETE removes it.
type iamEmulator struct {
	mu      sync.Mutex
	created map[string]string
}

func (e *iamEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	c, ok := e.created[id]
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "notFound", "Not found: Table "+id)
	case r.Method == http.MethodDelete:
		delete(e.created, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"creationTime": c, "type": "TABLE"})
	}
}

func iamCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	b, _ := io.ReadAll(w.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return w.Code, m
}

// A table's policy (#1231) is stored with an etag, a stale etag is
// refused 409, conditions are 501, it survives a restart of the front,
// and it goes with its table: on a delete, or when the table is made again.
func TestTableIAMPolicyIsStoredAndGoesWithItsTable(t *testing.T) {
	emu := &iamEmulator{created: map[string]string{"t": "100"}}
	dir := t.TempDir()
	wrap := func() http.Handler { return Wrap(emu, func(o *options) { o.stateDir = dir }) }
	h := wrap()
	const tb = "/bigquery/v2/projects/p/datasets/d/tables/t"

	code, empty := iamCall(t, h, "POST", tb+":getIamPolicy", `{"options":{"requestedPolicyVersion":3}}`)
	if code != http.StatusOK || empty["etag"] != "ACAB" || empty["bindings"] != nil {
		t.Fatalf("an unset policy = %d %v", code, empty)
	}
	set := `{"policy":{"etag":"ACAB","bindings":[{"role":"roles/bigquery.dataViewer","members":["user:a@example.com"]}]}}`
	code, got := iamCall(t, h, "POST", tb+":setIamPolicy", set)
	if code != http.StatusOK || got["etag"] == "ACAB" || len(got["bindings"].([]any)) != 1 {
		t.Fatalf("setIamPolicy = %d %v", code, got)
	}
	if code, _ := iamCall(t, h, "POST", tb+":setIamPolicy", set); code != http.StatusConflict {
		t.Errorf("a stale etag answered %d, want 409", code)
	}
	if code, m := iamCall(t, h, "POST", tb+":setIamPolicy",
		`{"policy":{"version":3,"bindings":[{"role":"r","members":["allUsers"],"condition":{"expression":"true"}}]}}`); code != http.StatusNotImplemented ||
		!strings.Contains(m["error"].(map[string]any)["message"].(string), "condition") {
		t.Errorf("a condition answered %d %v, want 501 naming it", code, m)
	}
	code, perms := iamCall(t, h, "POST", tb+":testIamPermissions", `{"permissions":["bigquery.tables.get","bigquery.tables.getData"]}`)
	if code != http.StatusOK || len(perms["permissions"].([]any)) != 2 {
		t.Errorf("testIamPermissions = %d %v", code, perms)
	}
	if code, perms := iamCall(t, h, "POST", "/bigquery/v2/projects/p/datasets/d/tables/missing:testIamPermissions", `{"permissions":["bigquery.tables.get"]}`); code != http.StatusOK || perms["permissions"] != nil {
		t.Errorf("testIamPermissions of a missing table = %d %v, want an empty set", code, perms)
	}
	if code, _ := iamCall(t, h, "POST", "/bigquery/v2/projects/p/datasets/d/tables/missing:getIamPolicy", `{}`); code != http.StatusNotFound {
		t.Errorf("getIamPolicy of a missing table = %d, want 404", code)
	}

	// A restart of the front keeps it.
	h = wrap()
	if _, after := iamCall(t, h, "POST", tb+":getIamPolicy", `{}`); after["etag"] != got["etag"] {
		t.Errorf("after a restart = %v, want %v", after, got)
	}
	// The table made again starts with none.
	emu.mu.Lock()
	emu.created["t"] = "200"
	emu.mu.Unlock()
	if _, after := iamCall(t, h, "POST", tb+":getIamPolicy", `{}`); after["etag"] != "ACAB" {
		t.Errorf("a table made again kept %v", after)
	}
	// A delete drops it.
	iamCall(t, h, "POST", tb+":setIamPolicy", set)
	if code, _ := iamCall(t, h, "DELETE", tb, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	emu.mu.Lock()
	emu.created["t"] = "200" // the same creationTime: only the delete dropped it
	emu.mu.Unlock()
	if _, after := iamCall(t, h, "POST", tb+":getIamPolicy", `{}`); after["etag"] != "ACAB" {
		t.Errorf("a deleted table's policy was kept: %v", after)
	}
}

// A row access policy's IAM policy is its grantees, as
// roles/bigquery.filteredDataViewer (#1231).
func TestRowPolicyIAMIsItsGrantees(t *testing.T) {
	pol := rowPolicy{Grantees: []string{"allUsers", "user:a@example.com"}, Etag: "e1"}
	p := rowPolicyIAM(pol)
	if len(p.GetBindings()) != 1 || p.GetBindings()[0].GetRole() != filteredDataViewer ||
		strings.Join(p.GetBindings()[0].GetMembers(), ",") != "allUsers,user:a@example.com" || string(p.GetEtag()) != "e1" {
		t.Errorf("rowPolicyIAM = %v", p)
	}
	if p := rowPolicyIAM(rowPolicy{Etag: "e2"}); len(p.GetBindings()) != 0 {
		t.Errorf("no grantees = %v", p)
	}
}
