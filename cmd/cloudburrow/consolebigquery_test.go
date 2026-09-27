package main

import (
	"context"
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

// TestBigQueryOtherProjectsArePromptsNotErrors.
//
// The emulator serves one project. Any other gets a prompt naming the one it
// serves — not the emulator's 404 as an error, and not an empty table that
// says the project has no datasets (#698). Nothing is sent: the endpoint here
// is a port nothing listens on, so a read would come back Unavailable.
func TestBigQueryOtherProjectsArePromptsNotErrors(t *testing.T) {
	ctx := context.Background()
	p := bigqueryProvider{endpoint: "127.0.0.1:1", project: "served-project"}

	list, err := p.List(ctx, "other-project")
	if err != nil {
		t.Fatal(err)
	}
	if list.Unavailable != "" || len(list.Items) != 0 {
		t.Errorf("another project was read: unavailable %q, %d rows", list.Unavailable, len(list.Items))
	}
	if !strings.Contains(list.Prompt, `"served-project"`) || !strings.Contains(list.Prompt, "one project") {
		t.Errorf("the prompt does not name the one project served: %q", list.Prompt)
	}

	d, err := p.Detail(ctx, "other-project", []string{"ds"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Prompt != list.Prompt || d.Unavailable != "" || len(d.Sections) != 0 {
		t.Errorf("a dataset of another project opened as %+v", d)
	}

	if _, err := p.Query(ctx, "other-project", []string{"ds"}, "SELECT 1"); err == nil ||
		!strings.Contains(err.Error(), "served-project") {
		t.Errorf("a query in another project = %v, want the one-project message", err)
	}

	if list, _ := p.List(ctx, ""); list.Prompt == "" || list.Unavailable != "" {
		t.Errorf("no project selected: %+v", list)
	}
}
