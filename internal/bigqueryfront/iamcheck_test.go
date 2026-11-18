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
