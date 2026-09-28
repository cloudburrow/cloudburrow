package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQuerySchemaEditorFollowsBigQuerysRules (#854).
//
// Create table's schema arrives as the editor's rows. The emulator accepts a
// field name with a space and answers two fields of one name with a 500 the
// client retries until its deadline, so the rules are BigQuery's and are
// applied here: a name is a letter or underscore then letters, digits or
// underscores, names are case-insensitive, and a type and a mode are one of
// the ones the editor offers. An empty mode is NULLABLE, as BigQuery's is.
func TestBigQuerySchemaEditorFollowsBigQuerysRules(t *testing.T) {
	schema, err := parseSchemaField(`[{"name":"id","type":"INTEGER","mode":"REQUIRED"},
		{"name":"tags","type":"string","mode":"REPEATED"},{"name":"_note","type":"JSON","mode":""}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		{Name: "_note", Type: bigquery.JSONFieldType},
	}
	if !reflect.DeepEqual(schema, want) {
		t.Errorf("parsed %+v, want %+v", schema, want)
	}
	for raw, why := range map[string]string{
		``:                                 "at least one field",
		`[]`:                               "at least one field",
		`{}`:                               "JSON array",
		`[{"name":"a b","type":"STRING"}]`: "not a field name",
		`[{"name":"1a","type":"STRING"}]`:  "not a field name",
		`[{"name":"x","type":"STRING"},{"name":"X","type":"INTEGER"}]`:  "named twice",
		`[{"name":"x","type":"RECORD"}]`:                                "is not one of",
		`[{"name":"x","type":"STRING","mode":"OPTIONAL"}]`:              "mode",
		`[{"name":"` + strings.Repeat("a", 301) + `","type":"STRING"}]`: "not a field name",
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
}

// TestBigQueryRowsAreCheckedBeforeAnyIsSent (#854).
//
// The emulator stores what it cannot read back — an unparseable element of a
// REPEATED INTEGER leaves the table unreadable, "x" becomes a NUMERIC 0 — and
// inserts the good rows of a batch with a bad one, where BigQuery inserts
// none. So every value is parsed as its column's type before anything is sent,
// a missing REQUIRED value and an unknown field are refused, and the values
// sent are the ones insertAll's JSON takes.
func TestBigQueryRowsAreCheckedBeforeAnyIsSent(t *testing.T) {
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "name", Type: bigquery.StringFieldType},
		{Name: "score", Type: bigquery.FloatFieldType},
		{Name: "price", Type: bigquery.NumericFieldType},
		{Name: "ok", Type: bigquery.BooleanFieldType},
		{Name: "at", Type: bigquery.TimestampFieldType},
		{Name: "day", Type: bigquery.DateFieldType},
		{Name: "clock", Type: bigquery.TimeFieldType},
		{Name: "local", Type: bigquery.DateTimeFieldType},
		{Name: "blob", Type: bigquery.BytesFieldType},
		{Name: "doc", Type: bigquery.JSONFieldType},
		{Name: "where", Type: bigquery.GeographyFieldType},
		{Name: "nums", Type: bigquery.IntegerFieldType, Repeated: true},
		{Name: "addr", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "city", Type: bigquery.StringFieldType, Required: true}}},
	}
	rows, err := decodeRows(`{"id": 1, "name": "a", "score": 2.5, "price": "1.25", "ok": true,` +
		` "at": "2026-09-27T15:04:05Z", "day": "2026-09-27", "clock": "15:04:05", "local": "2026-09-27T15:04:05",` +
		` "blob": "aGk=", "doc": {"k": [1, 2]}, "where": "POINT(1 2)", "nums": [1, "2"], "addr": {"city": "x"}}` +
		"\n\n" + `{"ID": "7", "score": 3, "price": 4.5, "name": null}`)
	if err != nil {
		t.Fatal(err)
	}
	first, err := bigqueryRow(schema, rows[0])
	if err != nil {
		t.Fatal(err)
	}
	want := jsonRow{
		"id": int64(1), "name": "a", "score": 2.5, "price": "1.25", "ok": true,
		"at": "2026-09-27T15:04:05Z", "day": "2026-09-27", "clock": "15:04:05", "local": "2026-09-27T15:04:05",
		"blob": "aGk=", "doc": `{"k":[1,2]}`, "where": "POINT(1 2)",
		"nums": []bigquery.Value{int64(1), int64(2)},
		"addr": map[string]bigquery.Value{"city": "x"},
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("row 1 is sent as %#v\nwant %#v", first, want)
	}
	// Field names are case-insensitive, a string holds an INTEGER, and a
	// null is no value rather than a value.
	second, err := bigqueryRow(schema, rows[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := (jsonRow{"id": int64(7), "score": float64(3), "price": "4.5"}); !reflect.DeepEqual(second, want) {
		t.Errorf("row 2 is sent as %#v, want %#v", second, want)
	}

	for raw, why := range map[string]string{
		`{"name": "no id"}`:                      "id is REQUIRED",
		`{"id": null}`:                           "id is REQUIRED",
		`{"id": 1, "nosuch": 2}`:                 "no such field: nosuch",
		`{"id": "x"}`:                            "not a 64-bit integer",
		`{"id": 1.5}`:                            "not a 64-bit integer",
		`{"id": 1, "nums": ["notanumber"]}`:      "nums[0]",
		`{"id": 1, "nums": [null]}`:              "holds no NULL",
		`{"id": 1, "nums": 3}`:                   "JSON array",
		`{"id": 1, "price": "x"}`:                "not a decimal",
		`{"id": 1, "price": "1/3"}`:              "not a decimal",
		`{"id": 1, "ok": "yes"}`:                 "not true or false",
		`{"id": 1, "at": "yesterday"}`:           "RFC 3339",
		`{"id": 1, "day": "27/09/2026"}`:         "2026-09-27",
		`{"id": 1, "clock": "3pm"}`:              "15:04:05",
		`{"id": 1, "blob": "!!"}`:                "not base64",
		`{"id": 1, "name": 5}`:                   "not a JSON string",
		`{"id": 1, "addr": {}}`:                  "addr: city is REQUIRED",
		`{"id": 1, "addr": "x"}`:                 "not a JSON object",
		`{"id": 1, "score": "many"}`:             "not a number",
		`{"id": 1, "local": "2026-09-27T25:00"}`: "2026-09-27T15:04:05",
	} {
		rows, err := decodeRows(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if _, err := bigqueryRow(schema, rows[0]); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("row %s = %v, want an error naming %q", raw, err, why)
		}
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
	// In the served project the dataset page offers Create table and a
	// Delete dataset that asks for the name back and says the tables go; a
	// table page, Insert rows and Delete table.
	ids := func(path ...string) (out []string) {
		for _, a := range p.DetailActions(ctx, "served-project", path) {
			out = append(out, a.ID)
			if strings.HasPrefix(a.ID, "delete") && (!a.Destructive || !a.Leaves || len(a.Fields) > 0) {
				t.Errorf("%s is offered as %+v; want destructive, leaving the page, confirmed by name", a.ID, a)
			}
		}
		return out
	}
	if got := ids("d"); !reflect.DeepEqual(got, []string{"createtable", "deletedataset"}) {
		t.Errorf("a dataset page offers %v", got)
	}
	if got := ids("d", "t"); !reflect.DeepEqual(got, []string{"insertrows", "deletetable"}) {
		t.Errorf("a table page offers %v", got)
	}
	// Refused on the served project's form before the emulator, which would
	// accept them: a hyphen in a dataset ID, "!" in a table ID.
	if _, err := p.Create(ctx, "served-project", map[string]string{"datasetId": "bad-name"}); err == nil ||
		!strings.Contains(err.Error(), "not a dataset ID") {
		t.Errorf("dataset ID bad-name = %v", err)
	}
	if err := p.ActAt(ctx, "served-project", []string{"d"}, "createtable",
		map[string]string{"tableId": "bad name!", "schema": `[{"name":"x","type":"STRING"}]`}); err == nil ||
		!strings.Contains(err.Error(), "not a table ID") {
		t.Errorf("table ID \"bad name!\" = %v", err)
	}
}
