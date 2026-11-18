package bigqueryfront

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
)

// TestIAMCheck (#1767): a request with a token the host gave is held to
// the account's roles on the job's project and the data's project or
// dataset (its access entries); one without, or with a token the
// instance did not mint, is served as before.
func TestIAMCheck(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	s := &iamState{}
	put := func(m BigQueryIAM) {
		b, _ := json.Marshal(m)
		rec := httptest.NewRecorder()
		s.check(rec, httptest.NewRequest(http.MethodPut, BigQueryIAMPath, bytes.NewReader(b)), nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("PUT: %d", rec.Code)
		}
	}
	token := BigQueryIAMToken{Hash: hex.EncodeToString(sum[:]), Email: "sa@p.iam.gserviceaccount.com", Expires: time.Now().Add(time.Hour)}
	put(BigQueryIAM{Tokens: []BigQueryIAMToken{token}})
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects/other/datasets/shared") {
			_, _ = w.Write([]byte(`{"access":[{"role":"READER","userByEmail":"SA@p.iam.gserviceaccount.com"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	reached := false
	h := s.handler(engine, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	do := func(auth, method, path, body string) (int, string) {
		reached = false
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if reached {
			return http.StatusOK, ""
		}
		return rec.Code, rec.Body.String()
	}
	query := func(sql string) string {
		return `{"configuration":{"query":{"query":"` + sql + `"}}}`
	}
	jobs := "/bigquery/v2/projects/p/jobs"
	for _, c := range []struct {
		name, auth, method, path, body string
		roles                          map[string][]string
		code                           int
		msg                            string
	}{
		{"no token", "", http.MethodPost, jobs, query("SELECT 1"), nil, 200, ""},
		{"a token not minted", "other", http.MethodPost, jobs, query("SELECT 1"), nil, 200, ""},
		{"no role", "tok", http.MethodPost, jobs, query("SELECT 1"), nil, 403, "User does not have bigquery.jobs.create permission in project p."},
		{"jobUser", "tok", http.MethodPost, jobs, query("SELECT 1"), map[string][]string{"p": {"roles/bigquery.jobUser"}}, 200, ""},
		{"another project's table", "tok", http.MethodPost, jobs, query("SELECT * FROM `other.d.t`"),
			map[string][]string{"p": {"roles/bigquery.jobUser"}}, 403, "Permission bigquery.tables.getData denied on table other:d.t"},
		{"dataViewer on the data's project", "tok", http.MethodPost, jobs, query("SELECT * FROM `other.d.t`"),
			map[string][]string{"p": {"roles/bigquery.jobUser"}, "other": {"roles/bigquery.dataViewer"}}, 200, ""},
		{"a dataset's READER entry", "tok", http.MethodPost, jobs, query("SELECT * FROM other.shared.t"),
			map[string][]string{"p": {"roles/bigquery.jobUser"}}, 200, ""},
		{"tabledata.list", "tok", http.MethodGet, "/bigquery/v2/projects/other/datasets/d/tables/t/data", "",
			map[string][]string{"p": {"roles/owner"}}, 403, "Permission bigquery.tables.getData denied on table other:d.t"},
		{"datasets.insert", "tok", http.MethodPost, "/bigquery/v2/projects/other/datasets", `{}`,
			map[string][]string{"other": {"roles/bigquery.dataViewer"}}, 403, "bigquery.datasets.create permission in project other"},
	} {
		put(BigQueryIAM{Tokens: []BigQueryIAMToken{token}, Roles: map[string]map[string][]string{token.Email: c.roles}})
		code, body := do(c.auth, c.method, c.path, c.body)
		if code != c.code || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, body, c.code, c.msg)
		}
	}
}

// TestIAMCheckRest (#1774): jobs.get, jobs.list and jobs.cancel with the
// creator rule, routines, models, row access policies, a table's own IAM
// policy and its methods, conditional bindings, and editor's exclusions.
func TestIAMCheckRest(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	email := "sa@p.iam.gserviceaccount.com"
	token := BigQueryIAMToken{Hash: hex.EncodeToString(sum[:]), Email: email, Expires: time.Now().Add(time.Hour)}
	policies := &iamPolicies{}
	s := &iamState{policies: policies}
	put := func(roles map[string][]string, conds map[string][]BigQueryIAMCondition) {
		b, _ := json.Marshal(BigQueryIAM{Tokens: []BigQueryIAMToken{token}, Roles: map[string]map[string][]string{email: roles},
			Conditional: map[string]map[string][]BigQueryIAMCondition{email: conds}})
		rec := httptest.NewRecorder()
		s.check(rec, httptest.NewRequest(http.MethodPut, BigQueryIAMPath, bytes.NewReader(b)), nil)
	}
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tables/granted") {
			_, _ = w.Write([]byte(`{"creationTime":"7"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := policies.set(iamKey("p", "d", "tables", "granted"), "7", &iampb.SetIamPolicyRequest{Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.dataViewer", Members: []string{"serviceAccount:" + email}}}}}); err != nil {
		t.Fatal(err)
	}
	var restBody string
	h := s.handler(engine, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(restBody))
	}))
	do := func(method, path string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	base := "/bigquery/v2/projects/p"
	// The caller runs job "mine"; "theirs" is someone else's.
	put(map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil)
	restBody = `{"jobReference":{"projectId":"p","jobId":"mine"}}`
	if code, body := do(http.MethodPost, base+"/jobs"); code != 200 {
		t.Fatalf("jobs.insert: %d %s", code, body)
	}
	restBody = `{"jobs":[{"id":"p:US.mine","jobReference":{"projectId":"p","jobId":"mine"},"configuration":{}},` +
		`{"id":"p:US.theirs","jobReference":{"projectId":"p","jobId":"theirs"},"configuration":{"query":{}},"state":"DONE"}]}`
	for _, c := range []struct {
		name, method, path string
		roles              map[string][]string
		conds              map[string][]BigQueryIAMCondition
		code               int
		msg                string
	}{
		{"own job", http.MethodGet, base + "/jobs/mine?location=US", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 200, ""},
		{"another's job", http.MethodGet, base + "/jobs/theirs?location=US", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 403,
			"Access Denied: Job p:US.theirs: User does not have bigquery.jobs.get permission for job p:US.theirs."},
		{"another's job, resourceViewer", http.MethodGet, base + "/jobs/theirs", map[string][]string{"p": {"roles/bigquery.resourceViewer"}}, nil, 200, ""},
		{"another's results", http.MethodGet, base + "/queries/theirs", map[string][]string{"p": {"roles/bigquery.user"}}, nil, 403, "bigquery.jobs.get"},
		{"cancel own", http.MethodPost, base + "/jobs/mine/cancel", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 200, ""},
		{"cancel another's", http.MethodPost, base + "/jobs/theirs/cancel", map[string][]string{"p": {"roles/editor"}}, nil, 403, "bigquery.jobs.update"},
		{"delete", http.MethodDelete, base + "/jobs/mine/delete", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 403, "bigquery.jobs.delete"},
		{"list own", http.MethodGet, base + "/jobs", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 200, `"jobs":[{"configuration":{}`},
		{"list, no role", http.MethodGet, base + "/jobs", nil, nil, 403, "bigquery.jobs.list permission in project p"},
		{"list all, jobUser", http.MethodGet, base + "/jobs?allUsers=true", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 403,
			"bigquery.jobs.listAll permission in project p"},
		{"list all, user: redacted", http.MethodGet, base + "/jobs?allUsers=true", map[string][]string{"p": {"roles/bigquery.user"}}, nil, 200,
			`{"id":"p:US.theirs","jobReference":{"projectId":"p","jobId":"theirs"},"state":"DONE"}]`},
		{"list all, resourceViewer", http.MethodGet, base + "/jobs?allUsers=true", map[string][]string{"p": {"roles/bigquery.resourceViewer"}}, nil, 200,
			`"configuration":{"query":{}}`},
		{"routines.list", http.MethodGet, base + "/datasets/d/routines", map[string][]string{"p": {"roles/bigquery.jobUser"}}, nil, 403,
			"Permission bigquery.routines.list denied on dataset p:d"},
		{"routines.get", http.MethodGet, base + "/datasets/d/routines/r", map[string][]string{"p": {"roles/bigquery.user"}}, nil, 403,
			"Access Denied: Routine p:d.r: Permission bigquery.routines.get denied on routine p:d.r"},
		{"routines.delete, dataViewer", http.MethodDelete, base + "/datasets/d/routines/r", map[string][]string{"p": {"roles/bigquery.dataViewer"}}, nil, 403,
			"bigquery.routines.delete"},
		{"routines.get, dataViewer", http.MethodGet, base + "/datasets/d/routines/r", map[string][]string{"p": {"roles/bigquery.dataViewer"}}, nil, 200, ""},
		{"models.get", http.MethodGet, base + "/datasets/d/models/m", map[string][]string{"p": {"roles/bigquery.user"}}, nil, 403,
			"Access Denied: Model p:d.m: Permission bigquery.models.getMetadata denied on model p:d.m"},
		{"rowAccessPolicies.list", http.MethodGet, base + "/datasets/d/tables/t/rowAccessPolicies", map[string][]string{"p": {"roles/bigquery.dataViewer"}}, nil, 403,
			"Permission bigquery.rowAccessPolicies.list denied on table p:d.t"},
		{"rowAccessPolicies.list, viewer", http.MethodGet, base + "/datasets/d/tables/t/rowAccessPolicies", map[string][]string{"p": {"roles/viewer"}}, nil, 200, ""},
		{"tables.setIamPolicy, dataEditor", http.MethodPost, base + "/datasets/d/tables/t:setIamPolicy", map[string][]string{"p": {"roles/bigquery.dataEditor"}}, nil, 403,
			"Permission bigquery.tables.setIamPolicy denied on table p:d.t"},
		{"tables.getIamPolicy, dataEditor", http.MethodPost, base + "/datasets/d/tables/t:getIamPolicy", map[string][]string{"p": {"roles/bigquery.dataEditor"}}, nil, 200, ""},
		{"a table's own policy", http.MethodGet, base + "/datasets/d/tables/granted/data", nil, nil, 200, ""},
		{"not another table", http.MethodGet, base + "/datasets/d/tables/t/data", nil, nil, 403, "bigquery.tables.getData"},
		{"a condition that holds", http.MethodGet, base + "/datasets/d/tables/t/data", nil,
			map[string][]BigQueryIAMCondition{"p": {{Role: "roles/bigquery.dataViewer", Expression: `resource.name.startsWith("projects/p/datasets/d/")`}}}, 200, ""},
		{"a condition that does not", http.MethodGet, base + "/datasets/e/tables/t/data", nil,
			map[string][]BigQueryIAMCondition{"p": {{Role: "roles/bigquery.dataViewer", Expression: `resource.name.startsWith("projects/p/datasets/d/")`}}}, 403, "bigquery.tables.getData"},
		{"an expired condition", http.MethodGet, base + "/datasets/d/tables/t/data", nil,
			map[string][]BigQueryIAMCondition{"p": {{Role: "roles/bigquery.dataViewer", Expression: `request.time < timestamp("2020-01-01T00:00:00Z")`}}}, 403, "bigquery.tables.getData"},
	} {
		put(c.roles, c.conds)
		code, body := do(c.method, c.path)
		if code != c.code || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, body, c.code, c.msg)
		}
	}
}

// TestIAMProjectsList (#1774): a minted caller's projects.list names the
// projects it holds a role on.
func TestIAMProjectsList(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	email := "sa@p.iam.gserviceaccount.com"
	s := &iamState{iam: &BigQueryIAM{Tokens: []BigQueryIAMToken{{Hash: hex.EncodeToString(sum[:]), Email: email, Expires: time.Now().Add(time.Hour)}},
		Roles:       map[string]map[string][]string{email: {"a": {"roles/bigquery.user"}}},
		Conditional: map[string]map[string][]BigQueryIAMCondition{email: {"c": {{Role: "roles/viewer", Expression: "true"}}}}}}
	h := s.handler(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"kind":"bigquery#projectList","projects":[{"id":"a","projectReference":{"projectId":"a"}},` +
			`{"id":"b","projectReference":{"projectId":"b"}},{"id":"c","projectReference":{"projectId":"c"}}],"totalItems":3}`))
	}))
	for auth, want := range map[string]string{"tok": `"projects":[{"id":"a"`, "": `"totalItems":3`} {
		req := httptest.NewRequest(http.MethodGet, "/bigquery/v2/projects", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, want) || (auth != "" && (strings.Contains(body, `"id":"b"`) || !strings.Contains(body, `"id":"c"`) ||
			!strings.Contains(body, `"totalItems":2`))) {
			t.Errorf("projects.list with token %q: %s", auth, body)
		}
	}
}
