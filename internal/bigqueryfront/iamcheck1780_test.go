package bigqueryfront

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// TestIAMCheck1780 (#1780): a tag condition holds on the tags bound to a
// dataset, its resourceTags and the project's; a job's creator is kept
// across a restart of the front; a routine called by a two-part name and
// a procedure's body are checked, a one-part name is not a routine's.
func TestIAMCheck1780(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	email := "sa@p.iam.gserviceaccount.com"
	token := BigQueryIAMToken{Hash: hex.EncodeToString(sum[:]), Email: email, Expires: time.Now().Add(time.Hour)}
	env := iamcond.Tag{NamespacedKey: "p/env", ShortValue: "prod", KeyID: "tagKeys/1", ValueID: "tagValues/2"}
	dev := iamcond.Tag{NamespacedKey: "p/env", ShortValue: "dev", KeyID: "tagKeys/1", ValueID: "tagValues/3"}
	newState := func(dir string) (*iamState, http.Handler) {
		s := &iamState{}
		if dir != "" {
			s.keepCreators(jobCreatorsStateFile(dir), t.Logf)
		}
		engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
		rest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/datasets/rt"):
				_, _ = w.Write([]byte(`{"resourceTags":{"p/env":"prod"}}`))
			case strings.HasSuffix(r.URL.Path, "/routines/f"), strings.HasSuffix(r.URL.Path, "/routines/g"):
				_, _ = w.Write([]byte(`{"definitionBody":"x+1","language":"SQL"}`))
			case strings.HasSuffix(r.URL.Path, "/routines/proc"):
				_, _ = w.Write([]byte(`{"definitionBody":"BEGIN SELECT * FROM secret.t; END","routineType":"PROCEDURE"}`))
			case strings.HasSuffix(r.URL.Path, "/routines/js"):
				_, _ = w.Write([]byte(`{"definitionBody":"return 1; // FROM secret.t","language":"JAVASCRIPT"}`))
			case strings.Contains(r.URL.Path, "/routines/"):
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
				_, _ = w.Write([]byte(`{"jobReference":{"projectId":"p","jobId":"j1"}}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		})
		return s, s.handler(engine, rest)
	}
	put := func(s *iamState, m BigQueryIAM) {
		m.Tokens = []BigQueryIAMToken{token}
		b, _ := json.Marshal(m)
		rec := httptest.NewRecorder()
		s.check(rec, httptest.NewRequest(http.MethodPut, BigQueryIAMPath, bytes.NewReader(b)), nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("PUT: %d", rec.Code)
		}
	}
	do := func(h http.Handler, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	roles := func(r map[string][]string) map[string]map[string][]string {
		return map[string]map[string][]string{email: r}
	}
	cond := func(role, expr string) map[string]map[string][]BigQueryIAMCondition {
		return map[string]map[string][]BigQueryIAMCondition{email: {"p": {{Role: role, Expression: expr}}}}
	}

	// Tags.
	s, h := newState("")
	tagCond := cond("roles/bigquery.dataViewer", `resource.matchTag("p/env", "prod")`)
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}}), Conditional: tagCond,
		Tags: map[string][]iamcond.Tag{BigQueryTagPrefix + "projects/p/datasets/bound": {env}}})
	for _, c := range []struct {
		name, path string
		code       int
	}{
		{"a dataset bound to env=prod", "/bigquery/v2/projects/p/datasets/bound/tables/t/data", 200},
		{"its table, inherited", "/bigquery/v2/projects/p/datasets/bound/tables/t", 200},
		{"resourceTags env=prod", "/bigquery/v2/projects/p/datasets/rt/tables/t/data", 200},
		{"no tag", "/bigquery/v2/projects/p/datasets/plain/tables/t/data", 403},
	} {
		if code, body := do(h, http.MethodGet, c.path, ""); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}
	// A nearer value replaces the project's.
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}}), Conditional: tagCond,
		Tags: map[string][]iamcond.Tag{"projects/p": {env}, BigQueryTagPrefix + "projects/p/datasets/devds": {dev}}})
	if code, _ := do(h, http.MethodGet, "/bigquery/v2/projects/p/datasets/plain/tables/t/data", ""); code != 200 {
		t.Errorf("the project's env=prod: %d, want 200", code)
	}
	if code, _ := do(h, http.MethodGet, "/bigquery/v2/projects/p/datasets/devds/tables/t/data", ""); code != 403 {
		t.Errorf("a dataset's env=dev under a project's env=prod: %d, want 403", code)
	}

	// Routines by one- and two-part names, a procedure, a JavaScript UDF.
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}})})
	q := func(sql, def string) string {
		b, _ := json.Marshal(map[string]any{"query": sql, "useLegacySql": false, "defaultDataset": map[string]string{"datasetId": def}})
		return string(b)
	}
	queries := "/bigquery/v2/projects/p/queries"
	for _, c := range []struct {
		name, sql, def, msg string
	}{
		{"a two-part name", "SELECT d.f(1)", "", "bigquery.routines.get denied on routine p:d.f"},
		{"a one-part name is not a persistent routine's", "SELECT g(1)", "d", ""},
		{"a built-in", "SELECT SAFE.SUBSTR('a', 1), COUNT(1)", "d", ""},
	} {
		code, body := do(h, http.MethodPost, queries, q(c.sql, c.def))
		if c.msg == "" && code != 200 || c.msg != "" && (code != 403 || !strings.Contains(body, c.msg)) {
			t.Errorf("%s: %d %s, want %q", c.name, code, body, c.msg)
		}
	}
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}}),
		Conditional: cond("roles/bigquery.dataViewer", `resource.name.startsWith("projects/p/datasets/d")`)})
	if code, body := do(h, http.MethodPost, queries, q("CALL d.proc()", "")); code != 403 ||
		!strings.Contains(body, "bigquery.tables.getData denied on table p:secret.t") {
		t.Errorf("a procedure reading secret.t: %d %s, want 403 on p:secret.t", code, body)
	}
	if code, body := do(h, http.MethodPost, queries, q("SELECT d.js(1)", "")); code != 200 {
		t.Errorf("a JavaScript UDF: %d %s, want 200", code, body)
	}

	// A job's creator outlives a restart of the front.
	dir := t.TempDir()
	s, h = newState(dir)
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}})})
	if code, body := do(h, http.MethodPost, "/bigquery/v2/projects/p/jobs", `{"configuration":{"query":{"query":"SELECT 1"}}}`); code != 200 {
		t.Fatalf("jobs.insert: %d %s", code, body)
	}
	s, h = newState(dir)
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}})})
	if code, body := do(h, http.MethodGet, "/bigquery/v2/projects/p/jobs/j1", ""); code != 200 {
		t.Errorf("jobs.get of its own job after a restart: %d %s, want 200", code, body)
	}
	if code, _ := do(h, http.MethodGet, "/bigquery/v2/projects/p/jobs/other", ""); code != 403 {
		t.Errorf("jobs.get of another's job: %d, want 403", code)
	}
	s.resetCreators()
	if _, err := filepath.Glob(jobCreatorsStateFile(dir)); err != nil {
		t.Fatal(err)
	}
	s, h = newState(dir)
	put(s, BigQueryIAM{Roles: roles(map[string][]string{"p": {"roles/bigquery.jobUser"}})})
	if code, _ := do(h, http.MethodGet, "/bigquery/v2/projects/p/jobs/j1", ""); code != 403 {
		t.Errorf("jobs.get after the emulator's reset: %d, want 403", code)
	}
}
