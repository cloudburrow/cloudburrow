//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
)

// jsonEncodingDDL is a table of every type GoogleSQL's JSON encodings
// table gives an example of
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/json_functions#json_encodings).
const jsonEncodingDDL = `(i INT64, w INT64, f FLOAT64, n NUMERIC, bn BIGNUMERIC, s STRING, y BYTES, b BOOL, d DATE,
 dt DATETIME, tm TIME, ts TIMESTAMP, st STRUCT<x INT64, y BOOL, z DATE>, a ARRAY<BOOL>)`

// jsonEncodingRow is the TO_JSON_STRING of a row of jsonEncodingDDL with
// the documented examples' values, for i: 9007199254740993 is a string,
// 123.56 a string, a BOOL true, a DATE, DATETIME, TIME and TIMESTAMP
// quoted in ISO 8601, a BYTES its base64, as the table's examples are.
func jsonEncodingRow(i int) string {
	return fmt.Sprintf(`{"i":%d,"w":"9007199254740993","f":1.5,"n":"123.56","bn":-1,"s":"\"abc\"","y":"R29vZ2xl","b":true,`+
		`"d":"2017-03-06","dt":"2017-03-06T12:34:56.789012","tm":"12:34:56.789012","ts":"2017-03-06T12:34:56.789012Z",`+
		`"st":{"x":12,"y":true,"z":"2017-03-06"},"a":[true,false]}`, i)
}

// TestBigQueryToJSONStringOfStoredRows (#1116): TO_JSON_STRING and TO_JSON
// of a row read from a table encode each value as the JSON encodings table
// says, whether the row was written by DML, by tabledata.insertAll (the Go
// client's Inserter) or by a load job (NDJSON through the Go client's
// ReaderSource). Measured first on the emulator: a BOOL read from a table
// was written 1, and a DATE, DATETIME, TIME or TIMESTAMP without quotes,
// which is not JSON.
// covers: bigquery.jobs.query, bigquery.tabledata.insertAll, bigquery.jobs.insert
func TestBigQueryToJSONStringOfStoredRows(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, _ := twoDatasets(t, h, c)
	ctx := h.Context()
	tbl := ds.DatasetID + ".j"
	if err := bqRun(ctx, c, "CREATE TABLE "+tbl+" "+jsonEncodingDDL, false); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	// Row 1, by DML.
	if err := bqRun(ctx, c, "INSERT "+tbl+` VALUES (1, 9007199254740993, 1.5, NUMERIC '123.56', BIGNUMERIC '-1', '"abc"',
		b'Google', TRUE, DATE '2017-03-06', DATETIME '2017-03-06 12:34:56.789012', TIME '12:34:56.789012',
		TIMESTAMP '2017-03-06 12:34:56.789012', STRUCT(12, TRUE, DATE '2017-03-06'), [TRUE, FALSE])`, true); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	// Row 2, by tabledata.insertAll.
	md, err := ds.Table("j").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2017, 3, 6, 12, 34, 56, 789012000, time.UTC)
	d := civil.Date{Year: 2017, Month: 3, Day: 6}
	ct := civil.Time{Hour: 12, Minute: 34, Second: 56, Nanosecond: 789012000}
	// The wide INT64 is sent as its decimal text: sent as a JSON number,
	// insertAll stores 9007199254740993 as 9007199254740992 (#1129).
	row := []bigquery.Value{int64(2), "9007199254740993", 1.5, big.NewRat(12356, 100), big.NewRat(-1, 1), `"abc"`,
		[]byte("Google"), true, d, civil.DateTime{Date: d, Time: ct}, ct, ts, []bigquery.Value{int64(12), true, d},
		[]bigquery.Value{true, false}}
	if err := ds.Table("j").Inserter().Put(ctx, &bigquery.ValuesSaver{Schema: md.Schema, Row: row}); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	// Row 3, by a load job.
	var line bytes.Buffer
	_ = json.NewEncoder(&line).Encode(map[string]any{"i": 3, "w": "9007199254740993", "f": 1.5, "n": "123.56", "bn": "-1",
		"s": `"abc"`, "y": "R29vZ2xl", "b": true, "d": "2017-03-06", "dt": "2017-03-06 12:34:56.789012", "tm": "12:34:56.789012",
		"ts": "2017-03-06 12:34:56.789012 UTC", "st": map[string]any{"x": 12, "y": true, "z": "2017-03-06"}, "a": []bool{true, false}})
	src := bigquery.NewReaderSource(&line)
	src.SourceFormat = bigquery.JSON
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	job, err := ds.Table("j").LoaderFrom(src).Run(lctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, err := job.Wait(lctx); err != nil || st.Err() != nil {
		t.Fatalf("load: %v, %v", err, st.Err())
	}

	got := queryRows(t, ctx, c, project, "", "SELECT TO_JSON_STRING(t) FROM "+tbl+" AS t")
	want := []string{jsonEncodingRow(1), jsonEncodingRow(2), jsonEncodingRow(3)}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("TO_JSON_STRING of the rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, g := range got {
		if !json.Valid([]byte(g)) {
			t.Errorf("not JSON: %s", g)
		}
	}
	// TO_JSON is TO_JSON_STRING's encoding but for the wide numbers, which
	// it writes as numbers (stringify_wide_numbers is FALSE); its object
	// keys are compared as JSON.
	for i, g := range queryRows(t, ctx, c, project, "", "SELECT TO_JSON_STRING(TO_JSON(t)) FROM "+tbl+" AS t") {
		w := strings.NewReplacer(`"w":"9007199254740993"`, `"w":9007199254740993`, `"n":"123.56"`, `"n":123.56`).Replace(jsonEncodingRow(i + 1))
		if canonicalJSON(t, g) != canonicalJSON(t, w) {
			t.Errorf("TO_JSON of row %d: %s, want %s", i+1, g, w)
		}
	}
	for q, want := range map[string]string{
		"SELECT TO_JSON_STRING(b), TO_JSON_STRING(d), TO_JSON_STRING(st) FROM " + tbl + " WHERE i = 2":          `true|"2017-03-06"|{"x":12,"y":true,"z":"2017-03-06"}`,
		"SELECT TO_JSON_STRING(INTERVAL '10:20:30.52' HOUR TO SECOND), TO_JSON_STRING(CAST('+inf' AS FLOAT64))": `"PT10H20M30.52S"|"Infinity"`,
		"SELECT TO_JSON_STRING(RANGE<DATE> '[2024-07-24, 2024-07-25)'), TO_JSON_STRING(1.0)":                    `{"start":"2024-07-24","end":"2024-07-25"}|1`,
		"SELECT TO_JSON_STRING(TO_JSON(9007199254740993, stringify_wide_numbers => TRUE))":                      `"9007199254740993"`,
	} {
		if got := queryRows(t, ctx, c, project, "", q); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %q, want %s", q, got, want)
		}
	}
}

// canonicalJSON is s with its object keys sorted and its numbers as written.
func canonicalJSON(t *testing.T, s string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %s: %v", s, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
