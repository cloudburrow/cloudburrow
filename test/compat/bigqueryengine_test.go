//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// Engine bugs of the pinned goccy/bigquery-emulator v0.8.1 that the build
// CloudBurrow makes (#1061, third_party/bigquery-emulator) fixes and no
// other test names.

// TestBigQueryInfinityInsideCompositeValues (#1077), official Go client: an
// infinity or a NaN a statement computes inside an ARRAY or a STRUCT is
// selected, and written by INSERT … SELECT and read back; so are infinities
// in a RECORD and a REPEATED column of a JSON load. The pinned
// engine JSON-encoded composite values with no room for an infinity, and
// failed "json: unsupported value: +Inf" (measured, #1077).
func TestBigQueryInfinityInsideCompositeValues(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	if got, want := q("SELECT ARRAY_LENGTH([CAST('inf' AS FLOAT64), IEEE_DIVIDE(0, 0)]), IS_INF(STRUCT(IEEE_DIVIDE(-1, 0) AS x).x)"),
		"[[2 true]]"; got != want {
		t.Errorf("composites in a SELECT = %s, want %s", got, want)
	}
	for _, sql := range []string{
		"CREATE TABLE DS.comp (id INT64, a ARRAY<FLOAT64>, s STRUCT<x FLOAT64>)",
		"INSERT INTO DS.comp (id, a, s) SELECT 4, [CAST('inf' AS FLOAT64), IEEE_DIVIDE(0, 0), 1.5], STRUCT(IEEE_DIVIDE(-1, 0) AS x)",
	} {
		if err := bqRun(ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+"."), true); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// A JSON load of infinities in a RECORD and a REPEATED column, which the
	// pinned emulator refused, 400 "json: unsupported value" (#1077).
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "a", Type: bigquery.FloatFieldType, Repeated: true},
		{Name: "s", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.FloatFieldType}}},
	}
	if err := runLoad(ctx, ds.Table("comp").LoaderFrom(jsonSource(`{"id":5,"a":["-Infinity",2],"s":{"x":"Infinity"}}`+"\n", schema))); err != nil {
		t.Fatalf("JSON load of infinities in a RECORD and a REPEATED column: %v", err)
	}
	if got, want := q("SELECT id, IS_INF(a[OFFSET(0)]) AND a[OFFSET(0)] < 0, a[OFFSET(1)], IS_INF(s.x) AND s.x > 0 FROM DS.comp WHERE id = 5"),
		"[[5 true 2 true]]"; got != want {
		t.Errorf("the loaded composites read back %s, want %s", got, want)
	}
	if got, want := q("SELECT id, IS_INF(a[OFFSET(0)]) AND a[OFFSET(0)] > 0, IS_NAN(a[OFFSET(1)]), a[OFFSET(2)], IS_INF(s.x) AND s.x < 0 FROM DS.comp WHERE id = 4"),
		"[[4 true true 1.5 true]]"; got != want {
		t.Errorf("the composites read back %s, want %s", got, want)
	}
}

// TestBigQueryUnaryMinus, official Go client: the - operator on a column,
// not only on a literal, which the pinned engine failed "no such function:
// googlesqlite_unary_minus" (measured, #1061), for INT64, FLOAT64 and
// NUMERIC, and the negation of the least INT64 is an error.
func TestBigQueryUnaryMinus(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	if got, want := bqValue(ctx, c, "SELECT -a, -b, CAST(-c AS STRING) FROM (SELECT 3 AS a, 1.5 AS b, NUMERIC '2.25' AS c)"),
		"[-3 -1.5 -2.25]"; got != want {
		t.Errorf("negated columns = %s, want %s", got, want)
	}
	if got := bqValue(ctx, c, "SELECT -a FROM (SELECT -9223372036854775808 AS a)"); !strings.HasPrefix(got, "error:") {
		t.Errorf("-(least INT64) = %s, want an error", got)
	}
}
