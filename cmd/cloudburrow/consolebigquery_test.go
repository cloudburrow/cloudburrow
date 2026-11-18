package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheBigQueryEditorRunsOnlyOneSelect.
//
// BigQuery has no read-only transaction and the emulator's dry run calls every
// statement SELECT, so the editor's guard is lexical (#698). It is an
// allowlist — the statement must begin with SELECT or WITH and be the only
// one — and quoted text and comments are not read as keywords or separators.
func TestTheBigQueryEditorRunsOnlyOneSelect(t *testing.T) {
	for _, stmt := range []string{
		"SELECT 1",
		"  select * from orders",
		"SELECT 1;",
		"SELECT 1;  ;\n-- trailing comment",
		"WITH t AS (SELECT 1 AS x) SELECT x FROM t",
		"(SELECT 1) UNION ALL (SELECT 2)",
		"-- why\nSELECT 1",
		"# why\nSELECT 1",
		"/* DELETE FROM t; */ SELECT 1",
		"SELECT 'a; DELETE FROM t' AS s",
		`SELECT "x;y", r'\'; DROP', b'z;' FROM t`,
		"SELECT '''multi\n; line''' AS s",
		"SELECT * FROM `my-project.ds.t;x`",
	} {
		if err := readOnlyBigQuery(stmt); err != nil {
			t.Errorf("refused %q: %v", stmt, err)
		}
	}
	for _, stmt := range []string{
		"",
		"   ",
		"-- only a comment",
		"DELETE FROM t WHERE true",
		"insert into t values (1)",
		"UPDATE t SET x = 1 WHERE true",
		"MERGE t USING s ON false WHEN NOT MATCHED THEN INSERT ROW",
		"CREATE TABLE t (x INT64)",
		"DROP TABLE t",
		"TRUNCATE TABLE t",
		"EXPORT DATA OPTIONS(uri='gs://b/*') AS SELECT 1",
		"DECLARE x INT64",
		"CALL p()",
		"SELECT 1; DELETE FROM t WHERE true",
		"SELECT 1; SELECT 2",
		"/* SELECT */ DELETE FROM t",
		"SELECT 'unterminated",
		"SELECT 1 /* unterminated",
	} {
		if err := readOnlyBigQuery(stmt); err == nil {
			t.Errorf("accepted %q", stmt)
		}
	}
}

// TestBigQueryOtherProjectsAreRead (#1764).
//
// BigQuery serves every project of the instance's Resource Manager, so the
// project chosen in the toolbar is the one read, not the instance's
// default; one BigQuery answers 404 "project P is not found" gets a prompt
// that says why, not the 404 as an error and not an empty table that says
// the project has no datasets (#698).
func TestBigQueryOtherProjectsAreRead(t *testing.T) {
	ctx := context.Background()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"project other-project is not found","errors":[{"reason":"notFound","message":"project other-project is not found"}]}}`))
	}))
	defer srv.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}

	list, err := p.List(ctx, "other-project")
	if err != nil {
		t.Fatal(err)
	}
	if list.Unavailable != "" || len(list.Items) != 0 {
		t.Errorf("a project BigQuery does not serve: unavailable %q, %d rows", list.Unavailable, len(list.Items))
	}
	if !strings.Contains(list.Prompt, `"other-project"`) || !strings.Contains(list.Prompt, "Resource Manager") {
		t.Errorf("the prompt does not say why: %q", list.Prompt)
	}
	if len(paths) == 0 || !strings.Contains(paths[0], "/projects/other-project/datasets") {
		t.Errorf("the chosen project was not read: %v", paths)
	}

	if _, err := p.Query(ctx, "other-project", []string{"ds"}, "SELECT 1"); err == nil {
		t.Error("a query in a project BigQuery does not serve succeeded")
	}
	for _, path := range paths {
		if strings.Contains(path, "served-project") {
			t.Errorf("the instance's project was read instead of the chosen one: %s", path)
		}
	}

	if list, _ := p.List(ctx, ""); list.Prompt == "" || list.Unavailable != "" {
		t.Errorf("no project selected: %+v", list)
	}

	// The Job history screen and a job's page say so too, not "listing
	// jobs: project other-project is not found".
	jobs := bigqueryJobsProvider{bq: p}
	if list, err := jobs.List(ctx, "other-project"); err != nil || list.Unavailable != "" || len(list.Items) != 0 ||
		!strings.Contains(list.Prompt, `"other-project"`) || !strings.Contains(list.Prompt, "Resource Manager") {
		t.Errorf("Job history of a project BigQuery does not serve = %+v, %v; want the prompt", list, err)
	}
	if d, err := jobs.Detail(ctx, "other-project", []string{"j"}); err != nil || d.Unavailable != "" ||
		!strings.Contains(d.Prompt, `"other-project"`) {
		t.Errorf("a job's page in a project BigQuery does not serve = %+v, %v; want the prompt", d, err)
	}
}
