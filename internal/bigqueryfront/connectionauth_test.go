package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
