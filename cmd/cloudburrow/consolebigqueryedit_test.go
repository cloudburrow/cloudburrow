package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
)

// TestBigQuerySchemaEditorBuildsNestedSchemas (#854, #874).
//
// Create table's schema arrives as the editor's rows: each a name, a type and
// a mode, and a RECORD's nested rows as its fields, to any depth. An empty
// mode is NULLABLE, as BigQuery's is. The rules a schema must follow are the
// API's, which the front applies (TestBigQueryConsoleShowsTheAPIsRefusals),
// so they are not repeated here.
func TestBigQuerySchemaEditorBuildsNestedSchemas(t *testing.T) {
	schema, err := parseSchemaField(`[{"name":"id","type":"INTEGER","mode":"REQUIRED"},
		{"name":"tags","type":"string","mode":"REPEATED"},{"name":"_note","type":"JSON","mode":""},
		{"name":"addr","type":"RECORD","mode":"REPEATED","fields":[{"name":"city","type":"STRING","mode":"REQUIRED"},
			{"name":"geo","type":"RECORD","fields":[{"name":"lat","type":"FLOAT"}]}]}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		{Name: "_note", Type: bigquery.JSONFieldType},
		{Name: "addr", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{
			{Name: "city", Type: bigquery.StringFieldType, Required: true},
			{Name: "geo", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
				{Name: "lat", Type: bigquery.FloatFieldType}}},
		}},
	}
	if !reflect.DeepEqual(schema, want) {
		t.Errorf("parsed %+v, want %+v", schema, want)
	}
	for raw, why := range map[string]string{
		``:   "at least one field",
		`[]`: "at least one field",
		`{}`: "JSON array",
	} {
		if _, err := parseSchemaField(raw); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("schema %.60s = %v, want an error naming %q", raw, err, why)
		}
	}
	for _, f := range bigqueryTableFields() {
		if f.Name == "schema" && (f.Type != "schema" || !reflect.DeepEqual(f.Options, bigqueryColumnTypes) || f.Pattern != bigqueryFieldNamePattern) {
			t.Errorf("the schema field is %+v; want the editor with the column types and the name pattern", f)
		}
	}
	if !slices.Contains(bigqueryColumnTypes, "RECORD") {
		t.Errorf("the editor offers %v, without RECORD", bigqueryColumnTypes)
	}
}

// TestBigQueryRowsAreShapedForInsertAll (#854, #874).
//
// Each value is sent in the form insertAll's JSON takes for its column: an
// INTEGER written as a string as its number, a NUMERIC written as a number
// as its exact text, a JSON column's value as JSON text, a RECORD field by
// field and a REPEATED field element by element, with field names matched
// without case. Nothing is refused here: an unknown field and a value that
// does not convert go as written, for the front to refuse with BigQuery's
// reason.
func TestBigQueryRowsAreShapedForInsertAll(t *testing.T) {
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "name", Type: bigquery.StringFieldType},
		{Name: "score", Type: bigquery.FloatFieldType},
		{Name: "price", Type: bigquery.NumericFieldType},
		{Name: "doc", Type: bigquery.JSONFieldType},
		{Name: "nums", Type: bigquery.IntegerFieldType, Repeated: true},
		{Name: "addr", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{
			{Name: "city", Type: bigquery.StringFieldType, Required: true},
			{Name: "zip", Type: bigquery.IntegerFieldType},
			{Name: "meta", Type: bigquery.JSONFieldType}}},
	}
	rows, err := decodeRows(`{"ID": "7", "name": "a", "score": 2.5, "price": 1.25, "doc": {"k": [1, 2]},` +
		` "nums": [1, "2"], "addr": [{"CITY": "x", "zip": "10", "meta": true}]}` +
		"\n\n" + `{"id": "x", "nosuch": 1, "name": null, "nums": 3}`)
	if err != nil {
		t.Fatal(err)
	}
	want := jsonRow{
		"id": int64(7), "name": "a", "score": json.Number("2.5"), "price": "1.25", "doc": `{"k":[1,2]}`,
		"nums": []bigquery.Value{json.Number("1"), int64(2)},
		"addr": []bigquery.Value{map[string]bigquery.Value{"city": "x", "zip": int64(10), "meta": "true"}},
	}
	if got := bigqueryRow(schema, rows[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("row 1 is sent as %#v\nwant %#v", got, want)
	}
	want = jsonRow{"id": "x", "nosuch": json.Number("1"), "name": nil, "nums": json.Number("3")}
	if got := bigqueryRow(schema, rows[1]); !reflect.DeepEqual(got, want) {
		t.Errorf("row 2 is sent as %#v, want it as written: %#v", got, want)
	}

	// The two forms the rows are written in, and what is not a row.
	if rows, err := decodeRows(`[{"id": 1}, {"id": 2}]`); err != nil || len(rows) != 2 {
		t.Errorf("a JSON array of two rows = %d rows, %v", len(rows), err)
	}
	for raw, why := range map[string]string{
		"":                   "at least one row",
		"  \n ":              "at least one row",
		`[]`:                 "at least one row",
		`[1]`:                "row 1 is not a JSON object",
		`{"id": 1} {"id":2}`: "more than one JSON value",
		"{\"id\": 1}\n[":     "line 2 is not a JSON object",
		"null":               "null",
		`[{"id":1}] x`:       "nothing may follow",
		strings.Repeat("{}\n", bigqueryInsertLimit+1): "more than the 500",
	} {
		if _, err := decodeRows(raw); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("rows %.40q = %v, want an error naming %q", raw, err, why)
		}
	}
}

// TestBigQueryWritesStayInTheServedProject (#854).
//
// The emulator serves one project. A write aimed at any other is refused with
// the prompt that names the served one, before anything is sent — the
// endpoint here is a port nothing listens on — and no page of another project
// offers an action.
func TestBigQueryWritesStayInTheServedProject(t *testing.T) {
	ctx := context.Background()
	p := bigqueryProvider{endpoint: "127.0.0.1:1", project: "served-project"}
	checks := map[string]error{}
	_, checks["create dataset"] = p.Create(ctx, "other", map[string]string{"datasetId": "d"})
	checks["delete dataset"] = p.Delete(ctx, "other", "d")
	checks["create table"] = p.ActAt(ctx, "other", []string{"d"}, "createtable", nil)
	checks["insert rows"] = p.ActAt(ctx, "other", []string{"d", "t"}, "insertrows", nil)
	checks["delete table"] = p.ActAt(ctx, "other", []string{"d", "t"}, "deletetable", nil)
	checks["edit table"] = p.ActAt(ctx, "other", []string{"d", "t"}, "edittable", nil)
	_, checks["read-write statement"] = p.WriteReport(ctx, "other", []string{"d"}, "CREATE SCHEMA s")
	_, checks["load"] = p.ActAtResult(ctx, "other", []string{"d", "t"}, "load", map[string]string{"uris": "gs://b/o"})
	_, checks["export"] = p.ActAtResult(ctx, "other", []string{"d", "t"}, "export", map[string]string{"uri": "gs://b/o"})
	jobs := bigqueryJobsProvider{bq: p}
	checks["delete job"] = jobs.Delete(ctx, "other", "j")
	checks["cancel job"] = jobs.ActAt(ctx, "other", []string{"j"}, "cancel", nil)
	for what, err := range checks {
		if err == nil || !strings.Contains(err.Error(), `"served-project"`) {
			t.Errorf("%s in another project = %v, want the one-project refusal", what, err)
		}
	}
	for _, path := range [][]string{{"d"}, {"d", "t"}} {
		if got := p.DetailActions(ctx, "other", path); len(got) != 0 {
			t.Errorf("another project's %v offers %v", path, got)
		}
		if got := p.DetailActions(ctx, "", path); len(got) != 0 {
			t.Errorf("no project's %v offers %v", path, got)
		}
	}
	if got := jobs.DetailActions(ctx, "other", []string{"j"}); len(got) != 0 {
		t.Errorf("another project's job offers %v", got)
	}
	// In the served project the dataset page offers Create table, Load from
	// Cloud Storage and a Delete dataset that asks for the name back and says
	// the tables go; a table page, Insert rows, Load from Cloud Storage,
	// Export to Cloud Storage and Delete table (#993) — and no Edit table
	// (#994), whose form is drawn from a table that cannot be read here.
	ids := func(path ...string) (out []string) {
		for _, a := range p.DetailActions(ctx, "served-project", path) {
			out = append(out, a.ID)
			if strings.HasPrefix(a.ID, "delete") && (!a.Destructive || !a.Leaves || len(a.Fields) > 0) {
				t.Errorf("%s is offered as %+v; want destructive, leaving the page, confirmed by name", a.ID, a)
			}
		}
		return out
	}
	if got := ids("d"); !reflect.DeepEqual(got, []string{"createtable", "load", actLoadFile, "deletedataset"}) {
		t.Errorf("a dataset page offers %v", got)
	}
	if got := ids("d", "t"); !reflect.DeepEqual(got, []string{"insertrows", "load", actLoadFile, "export", "deletetable"}) {
		t.Errorf("a table page offers %v", got)
	}
}

// TestBigQueryConsoleShowsTheAPIsRefusals (#874).
//
// The console no longer checks what the API refuses; it shows the refusal in
// the API's words. Here the API is the validating front (internal/
// bigqueryfront) in front of a stand-in emulator that accepts anything, as
// the real one nearly does: a dataset that exists is "Already Exists", a
// hyphenated ID and a column named twice are refused naming the rule, and
// an insert with one row missing its REQUIRED value names that row and
// field — and not the valid row, which BigQuery reports only as "stopped" —
// with nothing written.
func TestBigQueryConsoleShowsTheAPIsRefusals(t *testing.T) {
	var inserts int
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/datasets/exists"):
			_, _ = io.WriteString(w, `{"datasetReference":{"projectId":"served-project","datasetId":"exists"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tables/t"):
			_, _ = io.WriteString(w, `{"tableReference":{"projectId":"served-project","datasetId":"d","tableId":"t"},`+
				`"schema":{"fields":[{"name":"id","type":"INTEGER","mode":"REQUIRED"},`+
				`{"name":"addr","type":"RECORD","fields":[{"name":"city","type":"STRING","mode":"REQUIRED"}]}]}}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
		default:
			if strings.HasSuffix(r.URL.Path, "/insertAll") {
				inserts++
			}
			_, _ = io.WriteString(w, `{}`)
		}
	})
	srv := httptest.NewServer(bigqueryfront.Wrap(emulator))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}

	if _, err := p.Create(ctx, "served-project", map[string]string{"datasetId": "exists"}); err == nil ||
		err.Error() != "Already Exists: Dataset served-project:exists" {
		t.Errorf("an existing dataset = %v, want the API's Already Exists", err)
	}
	if _, err := p.Create(ctx, "served-project", map[string]string{"datasetId": "bad-name"}); err == nil ||
		!strings.HasPrefix(err.Error(), `Invalid dataset ID "bad-name"`) {
		t.Errorf("dataset ID bad-name = %v, want the API's refusal", err)
	}
	if err := p.ActAt(ctx, "served-project", []string{"d"}, "createtable",
		map[string]string{"tableId": "bad name!", "schema": `[{"name":"x","type":"STRING"}]`}); err == nil ||
		!strings.HasPrefix(err.Error(), `Invalid table ID "bad name!"`) {
		t.Errorf("table ID \"bad name!\" = %v, want the API's refusal", err)
	}
	if err := p.ActAt(ctx, "served-project", []string{"d"}, "createtable", map[string]string{"tableId": "t2",
		"schema": `[{"name":"a","type":"RECORD","fields":[{"name":"x","type":"STRING"},{"name":"X","type":"STRING"}]}]`}); err == nil ||
		!strings.HasPrefix(err.Error(), "Field a.X already exists in schema") {
		t.Errorf("a nested column named twice = %v, want the API's refusal naming a.X", err)
	}
	err := p.ActAt(ctx, "served-project", []string{"d", "t"}, "insertrows",
		map[string]string{"rows": `{"id": 1, "addr": {"city": "x"}}` + "\n" + `{"id": 2, "addr": {}}`})
	if err == nil || err.Error() != "row 2: Missing required field: addr.city." {
		t.Errorf("a row missing a nested REQUIRED value = %v, want only row 2 named, with the field", err)
	}
	if inserts != 0 {
		t.Errorf("the refused insert reached the emulator %d times", inserts)
	}
	if err := p.ActAt(ctx, "served-project", []string{"d", "t"}, "insertrows",
		map[string]string{"rows": `{"id": "3", "addr": {"city": "y"}}`}); err != nil || inserts != 1 {
		t.Errorf("a valid row = %v after %d inserts, want it sent once", err, inserts)
	}
}
