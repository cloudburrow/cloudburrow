package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestTheBigQueryReadWriteEditorRefusesDMLAndLoneQueries (#994).
//
// Read-write mode sends DDL and scripts and leaves what they may do to the
// API. Before anything is sent it refuses plain DML — alone, in a script, in
// a BEGIN block or in an exception handler, naming #1008 — and a text that
// is only queries, which belongs in Read-only; a keyword inside quotes or a
// comment is not a statement.
func TestTheBigQueryReadWriteEditorRefusesDMLAndLoneQueries(t *testing.T) {
	for _, stmt := range []string{
		"CREATE SCHEMA s",
		"CREATE TABLE t AS SELECT 1 AS a",
		"CREATE OR REPLACE VIEW v AS SELECT 1 AS a",
		"CREATE TABLE IF NOT EXISTS t (x INT64)",
		"DECLARE x INT64 DEFAULT 1;\nCREATE TABLE t AS SELECT x AS a;\nSELECT * FROM t;",
		"DROP TABLE t",
		"ALTER TABLE t ADD COLUMN c STRING",
		"CREATE TABLE t AS SELECT 'INSERT INTO u' AS s -- ; DELETE FROM u",
		"BEGIN TRANSACTION; COMMIT TRANSACTION",
		"SELECT 'unterminated",
	} {
		if err := writableBigQuery(stmt); err != nil {
			t.Errorf("refused %q: %v", stmt, err)
		}
	}
	for stmt, want := range map[string]string{
		"INSERT INTO t (x) VALUES (1)":                                                 "INSERT is DML",
		"update t set x = 1 where true":                                                "UPDATE is DML",
		"DELETE FROM t WHERE true":                                                     "DELETE is DML",
		"MERGE t USING s ON false WHEN NOT MATCHED THEN INSERT ROW":                    "MERGE is DML",
		"TRUNCATE TABLE t":                                                             "TRUNCATE is DML",
		"DECLARE x INT64;\nINSERT INTO t (x) VALUES (x)":                               "(#1008)",
		"BEGIN\n  DELETE FROM t WHERE true;\nEND":                                      "DELETE is DML",
		"BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN UPDATE t SET x = 1 WHERE true; END": "UPDATE is DML",
		"SELECT * FROM t":                                                              "switch the editor to Read-only",
		"WITH a AS (SELECT 1) SELECT * FROM a;":                                        "switch the editor to Read-only",
		"(SELECT 1)":                                                                   "switch the editor to Read-only",
		"":                                                                             "a statement is required",
		"-- nothing":                                                                   "a statement is required",
		strings.Repeat("-", maxBigQueryScriptBytes+1):                                  "at most",
	} {
		if err := writableBigQuery(stmt); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40q = %v, want a refusal naming %q", stmt, err, want)
		}
	}
	// Read-only mode sends a write to the switch.
	if err := readOnlyBigQuery("CREATE TABLE t AS SELECT 1 AS a"); err == nil || !strings.Contains(err.Error(), "Switch it to Read-write") {
		t.Errorf("read-only CREATE = %v, want it sent to Read-write", err)
	}
}

// TestBigQueryWriteReportSendsJobsQuery (#994).
//
// A read-write statement goes out as jobs.query, GoogleSQL, with the page's
// dataset as the default, and is answered with a sentence naming the job;
// the API's refusal is its message alone. The mode is offered on a dataset's
// and a table's page, naming the project and the dataset, and nowhere else.
func TestBigQueryWriteReportSendsJobsQuery(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/projects/served-project/queries") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found: `+r.URL.Path+`"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sent = append(sent, body)
		mu.Unlock()
		if strings.Contains(body["query"].(string), "nosuch") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Table not found: nosuch","errors":[{"reason":"invalid","message":"Table not found: nosuch"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"bigquery#queryResponse","jobComplete":true,"totalRows":"0",`+
			`"jobReference":{"projectId":"served-project","jobId":"job_1"}}`)
	})
	srv := httptest.NewServer(emulator)
	t.Cleanup(srv.Close)
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}
	ctx := context.Background()

	msg, err := p.WriteReport(ctx, "served-project", []string{"ds", "t"}, "CREATE SCHEMA other;")
	if err != nil || msg != "The statement ran as job job_1." {
		t.Fatalf("CREATE SCHEMA = %q, %v", msg, err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d requests, want one jobs.query", len(sent))
	}
	got := sent[0]
	ds, _ := got["defaultDataset"].(map[string]any)
	if got["query"] != "CREATE SCHEMA other;" || got["useLegacySql"] != false || ds["datasetId"] != "ds" || ds["projectId"] != "served-project" {
		t.Errorf("jobs.query was sent %v", got)
	}
	if _, err := p.WriteReport(ctx, "served-project", []string{"ds"}, "CREATE TABLE a AS SELECT * FROM nosuch"); err == nil ||
		err.Error() != "Table not found: nosuch" {
		t.Errorf("a refused statement = %v, want the API's message alone", err)
	}
	if _, err := p.WriteReport(ctx, "served-project", []string{"ds"}, "DELETE FROM t WHERE true"); err == nil || len(sent) != 2 {
		t.Errorf("a DELETE = %v after %d requests, want it refused unsent", err, len(sent))
	}
	if _, err := p.WriteReport(ctx, "other", []string{"ds"}, "CREATE SCHEMA x"); err == nil || !strings.Contains(err.Error(), `"served-project"`) {
		t.Errorf("another project = %v, want the one-project refusal", err)
	}

	for _, path := range [][]string{{"ds"}, {"ds", "t"}} {
		spec := p.WriteSpec(path)
		if spec == nil || spec.Label != "Read-write" || spec.Run != "Run statement" || spec.Confirm == "" ||
			!strings.Contains(spec.Target, "served-project") || !strings.Contains(spec.Target, "ds") {
			t.Errorf("WriteSpec(%v) = %+v", path, spec)
		}
	}
	for _, path := range [][]string{nil, {"ds", "t", "x"}} {
		if spec := p.WriteSpec(path); spec != nil {
			t.Errorf("WriteSpec(%v) = %+v, want none", path, spec)
		}
	}
}

// TestBigQueryFieldNamesAreFlexible (#994).
//
// The schema editors take what the API takes since #861 and #881: BigQuery's
// flexible column names, a space and & % = + : ' < > # | included, at most
// 300 characters; not a dot, an exclamation mark or a name of 301.
func TestBigQueryFieldNamesAreFlexible(t *testing.T) {
	re := regexp.MustCompile(bigqueryFieldNamePattern)
	for _, name := range []string{"first name", "a-b", "_x", "1st", "préférence", "a&b|c<d>", "x:y'z#%=+", strings.Repeat("a", 300)} {
		if !re.MatchString(name) {
			t.Errorf("%q is refused", name)
		}
	}
	for _, name := range []string{"", "a.b", "x!", "a/b", "(a)", "a,b", strings.Repeat("a", 301)} {
		if re.MatchString(name) {
			t.Errorf("%.20q is accepted", name)
		}
	}
	for _, fields := range [][]string{fieldsWithSchema(bigqueryTableFields()), fieldsWithSchema(bigqueryLoadFields(true))} {
		for _, pattern := range fields {
			if pattern != bigqueryFieldNamePattern {
				t.Errorf("a schema editor carries %q", pattern)
			}
		}
	}
}

func fieldsWithSchema(fields []console.Field) []string {
	var out []string
	for _, f := range fields {
		if f.Type == "schema" {
			out = append(out, f.Pattern)
		}
	}
	return out
}

// TestBigQueryCreateViewAndEditTable (#994).
//
// Create table with the VIEW type sends tables.insert with the view's query
// as GoogleSQL and no schema. Edit table sends one tables.patch holding every
// label the form holds, so a label left as it was survives the emulator's
// replacing the map (#1009), and the new description; it refuses, unsent,
// removing a label, clearing the description and a form with no change.
func TestBigQueryCreateViewAndEditTable(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tables/t"):
			_, _ = io.WriteString(w, `{"tableReference":{"projectId":"served-project","datasetId":"d","tableId":"t"},`+
				`"type":"TABLE","description":"old","labels":{"team":"data","env":"dev"},`+
				`"schema":{"fields":[{"name":"id","type":"INTEGER"}]}}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
		case strings.HasSuffix(r.URL.Path, "/queries"):
			// The front runs a view's query alone to read its columns (#916).
			_, _ = io.WriteString(w, `{"jobComplete":true,"totalRows":"0","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`)
		default:
			bodies = append(bodies, r.Method+" "+string(b))
			_, _ = io.WriteString(w, `{"tableReference":{"projectId":"served-project","datasetId":"d","tableId":"t"}}`)
		}
	})
	srv := httptest.NewServer(bigqueryfront.Wrap(emulator))
	t.Cleanup(srv.Close)
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}
	ctx := context.Background()
	act := func(path []string, action string, values map[string]string) error {
		return p.ActAt(ctx, "served-project", path, action, values)
	}

	if err := act([]string{"d"}, "createtable", map[string]string{"tableId": "v", "tableType": "VIEW",
		"viewQuery": "SELECT 1 AS a"}); err != nil {
		t.Fatalf("Create table as a view: %v", err)
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "POST ") {
		t.Fatalf("sent %v", bodies)
	}
	var view struct {
		View struct {
			Query        string
			UseLegacySQL *bool `json:"useLegacySql"`
		}
		Schema any
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(bodies[0], "POST ")), &view)
	if view.View.Query != "SELECT 1 AS a" || view.View.UseLegacySQL == nil || *view.View.UseLegacySQL || view.Schema != nil {
		t.Errorf("the view was sent as %s", bodies[0])
	}
	for values, want := range map[string]map[string]string{
		"a view needs its query":           {"tableId": "v", "tableType": "VIEW"},
		"leave the schema empty":           {"tableId": "v", "tableType": "VIEW", "viewQuery": "SELECT 1", "schema": `[{"name":"a","type":"STRING"}]`},
		"choose VIEW":                      {"tableId": "v", "viewQuery": "SELECT 1", "schema": `[{"name":"a","type":"STRING"}]`},
		"a table needs at least one field": {"tableId": "v", "tableType": "TABLE"},
		"unknown table type":               {"tableId": "v", "tableType": "EXTERNAL"},
	} {
		if err := act([]string{"d"}, "createtable", want); err == nil || !strings.Contains(err.Error(), values) {
			t.Errorf("Create table %v = %v, want %q", want, err, values)
		}
	}

	bodies = nil
	if err := act([]string{"d", "t"}, "edittable", map[string]string{"description": "new",
		"labels": `{"team":"ops","env":"dev"}`}); err != nil {
		t.Fatalf("Edit table: %v", err)
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "PATCH ") {
		t.Fatalf("Edit table sent %v, want one tables.patch", bodies)
	}
	var patch struct {
		Description string
		Labels      map[string]string
		Schema      any
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(bodies[0], "PATCH ")), &patch)
	if patch.Description != "new" || len(patch.Labels) != 2 || patch.Labels["team"] != "ops" || patch.Labels["env"] != "dev" || patch.Schema != nil {
		t.Errorf("Edit table sent %s; want the description and both labels, and no schema", bodies[0])
	}
	bodies = nil
	for want, values := range map[string]map[string]string{
		"removing a label (env)":   {"description": "old", "labels": `{"team":"data"}`},
		"clearing the description": {"description": "", "labels": `{"team":"data","env":"dev"}`},
		"nothing to change":        {"description": "old", "labels": `{"team":"data","env":"dev"}`},
		"labels":                   {"description": "old", "labels": `not json`},
	} {
		if err := act([]string{"d", "t"}, "edittable", values); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Edit table %v = %v, want %q", values, err, want)
		}
	}
	if len(bodies) != 0 {
		t.Errorf("refused edits were sent: %v", bodies)
	}
	fields := bigqueryEditFields(&bigquery.TableMetadata{Description: "old", Labels: map[string]string{"a": "1"}})
	if len(fields) != 2 || fields[0].Default != "old" || fields[1].Default != `{"a":"1"}` {
		t.Errorf("Edit table's form is %+v, want the description and labels as they are", fields)
	}
}

// TestBigQueryInsertRowsOptions (#994).
//
// Insert rows' two checkboxes reach insertAll. With Skip invalid rows the
// valid rows are written and the answer lists the invalid one by the number
// the form gave it, in the API's words (the front's), with how many were
// inserted; with Ignore unknown values a field the table lacks is dropped
// rather than refused.
func TestBigQueryInsertRowsOptions(t *testing.T) {
	var mu sync.Mutex
	var inserted []string
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tables/t"):
			_, _ = io.WriteString(w, `{"tableReference":{"projectId":"served-project","datasetId":"d","tableId":"t"},`+
				`"schema":{"fields":[{"name":"id","type":"INTEGER","mode":"REQUIRED"}]}}`)
		case strings.HasSuffix(r.URL.Path, "/insertAll"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			inserted = append(inserted, string(b))
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
		}
	})
	srv := httptest.NewServer(bigqueryfront.Wrap(emulator))
	t.Cleanup(srv.Close)
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}
	ctx := context.Background()

	res, err := p.ActAtResult(ctx, "served-project", []string{"d", "t"}, "insertrows", map[string]string{
		"skipInvalidRows": "true", "rows": `{"id": 1}` + "\n" + `{}` + "\n" + `{"id": 3}`})
	if err != nil {
		t.Fatalf("Insert rows with Skip invalid rows: %v", err)
	}
	if res == nil || len(res.Items) != 1 || res.Items[0].Name != "2" ||
		!strings.Contains(res.Items[0].Fields["Skipped because"], "Missing required field: id.") ||
		res.Note != "Inserted 2 of 3 rows; 1 skipped as invalid." {
		t.Errorf("the answer is %+v", res)
	}
	if len(inserted) != 1 || strings.Count(inserted[0], `"id"`) != 2 {
		t.Errorf("the emulator was sent %v, want the two valid rows", inserted)
	}

	if _, err := p.ActAtResult(ctx, "served-project", []string{"d", "t"}, "insertrows",
		map[string]string{"rows": `{"id": 4, "nosuch": 1}`}); err == nil || !strings.Contains(err.Error(), "no such field: nosuch") {
		t.Errorf("an unknown field = %v, want it refused", err)
	}
	res, err = p.ActAtResult(ctx, "served-project", []string{"d", "t"}, "insertrows",
		map[string]string{"ignoreUnknownValues": "true", "rows": `{"id": 4, "nosuch": 1}`})
	if err != nil || res != nil || len(inserted) != 2 || strings.Contains(inserted[1], "nosuch") {
		t.Errorf("Ignore unknown values = %+v, %v, sent %v; want the row without the field", res, err, inserted)
	}
}
