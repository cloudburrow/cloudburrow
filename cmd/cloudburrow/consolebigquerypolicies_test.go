package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBigQueryRowAccessPoliciesTab (#1218): a table's Row access policies
// tab lists its policies, each offering Delete policy, and names the caller
// CloudBurrow assumes; Create row access policy sends insert, or update with
// Replace; Delete all row access policies sends one batchDelete of every
// policy; the Permissions tab (#1231) reads and writes tables.getIamPolicy
// and setIamPolicy, and routines' on a routine's page.
func TestBigQueryRowAccessPoliciesTab(t *testing.T) {
	var sent []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/bigquery/v2")
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, r.Method+" "+path+" "+string(body))
		switch {
		case strings.HasSuffix(path, ":getIamPolicy"):
			_, _ = io.WriteString(w, `{"version":1,"etag":"ACAB","bindings":[{"role":"roles/bigquery.dataViewer","members":["user:a@b.c"]}]}`)
		case strings.HasSuffix(path, ":setIamPolicy"):
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && path == "/projects/p/datasets/d/tables/t/rowAccessPolicies":
			_, _ = io.WriteString(w, `{"rowAccessPolicies":[{"rowAccessPolicyReference":{"projectId":"p","datasetId":"d","tableId":"t","policyId":"eu"},`+
				`"filterPredicate":"region = 'eu'","grantees":["allUsers"]}]}`)
		case strings.HasPrefix(path, "/projects/p/datasets/d/tables/t/rowAccessPolicies"):
			if r.Method == http.MethodDelete || strings.HasSuffix(path, ":batchDelete") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()

	sec := p.rowPoliciesSection(ctx, "d", "t")
	if sec.ID != "rowaccesspolicies" || len(sec.Listing.Items) != 1 {
		t.Fatalf("the tab = %+v", sec)
	}
	row := sec.Listing.Items[0]
	if row.Name != "eu" || row.Fields["Filter"] != "region = 'eu'" || row.Fields["Grantees"] != "allUsers" ||
		strings.Join(row.ActsOn, "/") != "d/t/rowAccessPolicies/eu" || row.Actions[0].ID != actDeleteRowPolicy {
		t.Errorf("the eu row = %+v", row)
	}
	if !strings.Contains(sec.Listing.Note, "cloudburrow-local@p.iam.gserviceaccount.com") {
		t.Errorf("the tab does not name the assumed caller: %q", sec.Listing.Note)
	}
	for _, path := range [][]string{{"d", "t"}, {"d", "routines", "f"}} {
		if p.PolicyOn(ctx, path) == nil {
			t.Errorf("%v has no Permissions tab", path)
		}
		sent = nil
		pol, err := p.GetPolicy(ctx, "p", path)
		if err != nil || pol.Etag != "ACAB" || len(pol.Bindings) != 1 || pol.Bindings[0].Members[0] != "user:a@b.c" {
			t.Errorf("GetPolicy(%v) = %+v, %v", path, pol, err)
		}
		pol.Bindings[0].Members = append(pol.Bindings[0].Members, "allUsers")
		if err := p.SetPolicy(ctx, "p", path, pol); err != nil {
			t.Errorf("SetPolicy(%v): %v", path, err)
		}
		resource := "/projects/p/datasets/d/tables/t"
		if len(path) == 3 {
			resource = "/projects/p/datasets/d/routines/f"
		}
		if last := sent[len(sent)-1]; !strings.HasPrefix(last, "POST "+resource+":setIamPolicy ") || !strings.Contains(last, `"etag":"ACAB"`) ||
			!strings.Contains(last, `"allUsers"`) {
			t.Errorf("SetPolicy(%v) sent %q", path, last)
		}
	}
	if p.PolicyOn(ctx, []string{"d"}) != nil || p.PolicyOn(ctx, []string{"d", "t", "rowAccessPolicies", "eu"}) != nil {
		t.Error("a dataset or a row access policy row has the PolicyEditor tab")
	}
	if acts := p.DetailActions(ctx, "p", []string{"d", "t", "rowAccessPolicies", "eu"}); len(acts) != 1 || acts[0].ID != actDeleteRowPolicy {
		t.Errorf("a policy row's actions = %+v", acts)
	}

	sent = nil
	for _, c := range []struct {
		action string
		path   []string
		values map[string]string
		want   string
	}{
		{actCreateRowPolicy, []string{"d", "t"}, map[string]string{"policyId": "us", "filter": "region = 'us'", "grantees": "allUsers\nuser:a@b.c"}, "POST /projects/p/datasets/d/tables/t/rowAccessPolicies "},
		{actCreateRowPolicy, []string{"d", "t"}, map[string]string{"policyId": "us", "filter": "TRUE", "replace": "true"}, "PUT /projects/p/datasets/d/tables/t/rowAccessPolicies/us "},
		{actDeleteRowPolicy, []string{"d", "t", "rowAccessPolicies", "us"}, nil, "DELETE /projects/p/datasets/d/tables/t/rowAccessPolicies/us "},
		{actDeleteAllRowPolicies, []string{"d", "t"}, nil, "POST /projects/p/datasets/d/tables/t/rowAccessPolicies:batchDelete "},
	} {
		sent = nil
		if err := p.ActAt(ctx, "p", c.path, c.action, c.values); err != nil {
			t.Fatalf("%s: %v", c.action, err)
		}
		last := sent[len(sent)-1]
		if !strings.HasPrefix(last, c.want) {
			t.Errorf("%s sent %q, want %q", c.action, last, c.want)
		}
		if c.action == actCreateRowPolicy && c.values["replace"] == "" {
			var pol struct {
				Grantees []string `json:"grantees"`
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(last, c.want)), &pol)
			if strings.Join(pol.Grantees, ",") != "allUsers,user:a@b.c" {
				t.Errorf("Create sent grantees %v", pol.Grantees)
			}
		}
		if c.action == actDeleteAllRowPolicies && !strings.Contains(last, `"policyIds":["eu"]`) {
			t.Errorf("Delete all sent %q", last)
		}
	}
}
