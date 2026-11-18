package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestCheckConnectionUse (#1765): bigquery.connections.use on the
// connection's policy or the project's, and roles/aiplatform.user for the
// connection's service account on the project, read from Resource Manager.
func TestCheckConnectionUse(t *testing.T) {
	const sa = "bqcx-1-ab12@gcp-sa-bigquery-condel.iam.gserviceaccount.com"
	caller := "serviceAccount:" + callerIdentity("p")
	var project []policyBinding
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p:getIamPolicy" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bindings": project})
	}))
	defer rm.Close()
	f := front{resourceManager: rm.URL}
	cn := &bqConnection{Project: "1", Location: "us", ID: "c", Fields: map[string]json.RawMessage{
		"cloudResource": json.RawMessage(`{"serviceAccountId":"` + sa + `"}`)}}
	ctx := context.Background()
	if code, msg := f.checkConnectionUse(ctx, "p", "p", cn); code != 403 || !strings.Contains(msg, "bigquery.connections.use") {
		t.Errorf("no grants: %d %s", code, msg)
	}
	// A conditional binding grants nothing; neither does a role without
	// the permission.
	project = []policyBinding{{Role: "roles/bigquery.connectionUser", Members: []string{caller}, Condition: json.RawMessage(`{"expression":"true"}`)},
		{Role: "roles/bigquery.dataViewer", Members: []string{caller}}}
	if code, _ := f.checkConnectionUse(ctx, "p", "p", cn); code != 403 {
		t.Errorf("conditional or unrelated binding: %d", code)
	}
	// On the connection's own policy: then the service account's role.
	cn.Policy = json.RawMessage(`{"bindings":[{"role":"roles/bigquery.connectionUser","members":["` + caller + `"]}]}`)
	if code, msg := f.checkConnectionUse(ctx, "p", "p", cn); code != 400 || !strings.Contains(msg, sa) || !strings.Contains(msg, "Vertex AI user role") {
		t.Errorf("no aiplatform.user: %d %s", code, msg)
	}
	project = append(project, policyBinding{Role: "roles/aiplatform.user", Members: []string{"serviceAccount:" + sa}})
	if code, msg := f.checkConnectionUse(ctx, "p", "p", cn); code != 0 {
		t.Errorf("granted: %d %s", code, msg)
	}
	// On the project's policy instead.
	cn.Policy = nil
	project = append(project, policyBinding{Role: "roles/bigquery.admin", Members: []string{caller}})
	if code, msg := f.checkConnectionUse(ctx, "p", "p", cn); code != 0 {
		t.Errorf("granted on the project: %d %s", code, msg)
	}
	// Without Resource Manager nothing can be read: 501.
	if code, _ := (front{}).checkConnectionUse(ctx, "p", "p", cn); code != 501 {
		t.Errorf("no Resource Manager: %d", code)
	}
}

// TestConnectionTestIamPermissions (#1770): testIamPermissions answers
// bigquery.connections.use as checkConnectionUse decides it, through the
// connection's policy or its project's; the other bigquery.connections.*
// permissions, checked nowhere, are held.
func TestConnectionTestIamPermissions(t *testing.T) {
	caller := "serviceAccount:" + callerIdentity("p")
	var project []policyBinding
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p:getIamPolicy" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bindings": project})
	}))
	defer rm.Close()
	c := &bqConnections{resourceManager: rm.URL}
	numbers := &projectNumbers{byID: map[string]int64{"p": 42}}
	do := func(method, target, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		if !c.serve(w, httptest.NewRequest(method, target, strings.NewReader(body)), numbers) {
			t.Fatalf("%s %s not served", method, target)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/projects/p/locations/us/connections"
	if code, res := do(http.MethodPost, base+"?connectionId=ai", `{"cloudResource":{}}`); code != 200 {
		t.Fatalf("create: %d %v", code, res)
	}
	test := func(target string) []any {
		t.Helper()
		code, res := do(http.MethodPost, target+":testIamPermissions", `{"permissions":["bigquery.connections.use","bigquery.connections.get","bigquery.tables.get"]}`)
		if code != 200 {
			t.Fatalf("testIamPermissions: %d %v", code, res)
		}
		perms, _ := res["permissions"].([]any)
		return perms
	}
	if got := test(base + "/ai"); len(got) != 1 || got[0] != "bigquery.connections.get" {
		t.Errorf("with no binding: %v", got)
	}
	project = []policyBinding{{Role: "roles/bigquery.connectionUser", Members: []string{caller}}}
	if got := test(base + "/ai"); len(got) != 2 || got[0] != "bigquery.connections.use" {
		t.Errorf("granted on the project: %v", got)
	}
	// By the project's number, as the client names the connection.
	if got := test("/v1/projects/42/locations/us/connections/ai"); len(got) != 2 {
		t.Errorf("granted on the project, by its number: %v", got)
	}
	project = nil
	if code, _ := do(http.MethodPost, base+"/ai:setIamPolicy",
		`{"policy":{"bindings":[{"role":"roles/bigquery.connectionUser","members":["`+caller+`"]}]}}`); code != 200 {
		t.Fatalf("setIamPolicy: %d", code)
	}
	if got := test(base + "/ai"); len(got) != 2 {
		t.Errorf("granted on the connection: %v", got)
	}
	// By a number the host did not send, derived from the project ID.
	numbers.byID = nil
	if code, _ := do(http.MethodPost, base+"?connectionId=ai2", `{"cloudResource":{}}`); code != 200 {
		t.Fatalf("create ai2: %d", code)
	}
	if code, _ := do(http.MethodPost, base+"/ai2:setIamPolicy",
		`{"policy":{"bindings":[{"role":"roles/bigquery.connectionUser","members":["`+caller+`"]}]}}`); code != 200 {
		t.Fatalf("setIamPolicy ai2: %d", code)
	}
	derived := strconv.FormatInt(numbers.of("p"), 10)
	if got := test("/v1/projects/" + derived + "/locations/us/connections/ai2"); len(got) != 2 {
		t.Errorf("granted on the connection, by the derived number: %v", got)
	}
}
