//go:build compat

package compat

import (
	"testing"
)

// TestBigQueryFunctionStructOverGroupBy (#1560), official Go client: a SQL
// function, persistent or TEMP, whose body builds a STRUCT or an ARRAY
// over a scalar subquery: each element's index in a vocabulary over a
// GROUP BY (as BigQuery ML's encoders do), the same with a count, an ARRAY
// of STRUCT, a STRUCT over a window, and a nested STRUCT, in a function
// and in a table function. Measured first: a call failed "no such column:
// $col1" where the same expression outside a function worked (the engine
// wrote the body's columns by name; googlesqlite 0066). Each call returns
// what the expression does outside a function, through jobs.query and a
// query job alike.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryFunctionStructOverGroupBy(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := c.Dataset("fn_struct_1560")
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	d := ds.DatasetID

	index := "IFNULL((SELECT o + 1 FROM UNNEST(voc) AS w WITH OFFSET o WHERE w = e), 0)"
	indices := "ARRAY(SELECT i FROM (SELECT i, MIN(p) AS q FROM (SELECT " + index +
		" AS i, p FROM UNNEST(x) AS e WITH OFFSET p) GROUP BY i) ORDER BY q)"
	for _, tc := range []struct {
		name, params, body, args, bind, want string
	}{
		{"scalar_subquery_in_array", "", "(SELECT [i] FROM (SELECT (SELECT 7) AS i))", "", "", "[[7]]"},
		{"struct_over_group_by", "voc ARRAY<STRING>, x ARRAY<STRING>",
			"ARRAY(SELECT STRUCT(i AS index, 1.0 AS value) FROM UNNEST(" + indices + ") AS i WITH OFFSET p ORDER BY p)",
			"['a', 'b'], ['b', 'c', 'b']", "['a', 'b'] AS voc, ['b', 'c', 'b'] AS x", `[[{"index":2,"value":1},{"index":0,"value":1}]]`},
		{"struct_of_a_count_over_group_by", "voc ARRAY<STRING>, x ARRAY<STRING>",
			"ARRAY(SELECT STRUCT(i AS index, (SELECT COUNT(*) FROM UNNEST(x) AS e WHERE " + index + " = i) AS n) FROM UNNEST(" + indices + ") AS i ORDER BY i)",
			"['a', 'b'], ['b', 'c', 'b']", "['a', 'b'] AS voc, ['b', 'c', 'b'] AS x", `[[{"index":0,"n":1},{"index":2,"n":2}]]`},
		{"array_of_struct", "x ARRAY<STRING>",
			"ARRAY(SELECT STRUCT(v AS k, (SELECT COUNT(*) FROM UNNEST(x) AS w WHERE w = v) AS n) FROM UNNEST(x) AS v GROUP BY v ORDER BY v)",
			"['a', 'b', 'a']", "['a', 'b', 'a'] AS x", `[[{"k":"a","n":2},{"k":"b","n":1}]]`},
		{"struct_over_window", "x ARRAY<STRING>",
			"ARRAY(SELECT STRUCT(v AS k, (SELECT 1) + ROW_NUMBER() OVER (ORDER BY v) AS n) FROM UNNEST(x) AS v ORDER BY v)",
			"['b', 'a']", "['b', 'a'] AS x", `[[{"k":"a","n":2},{"k":"b","n":3}]]`},
		{"nested_struct", "x INT64",
			"(SELECT STRUCT(STRUCT(i AS b) AS a, [STRUCT(i AS c)] AS d) FROM (SELECT (SELECT x + 1) AS i))",
			"6", "6 AS x", `[{"a":{"b":7},"d":[{"c":7}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The expression outside a function: what each call must give.
			inline := tc.body
			if tc.bind != "" {
				inline = "(SELECT " + tc.body + " FROM (SELECT " + tc.bind + "))"
			}
			if got := bqValue(ctx, c, "SELECT TO_JSON_STRING("+inline+")"); got != tc.want {
				t.Fatalf("outside a function = %s, want %s", got, tc.want)
			}
			if err := bqRun(ctx, c, "CREATE FUNCTION "+d+"."+tc.name+"("+tc.params+") AS ("+tc.body+")", false); err != nil {
				t.Fatalf("CREATE FUNCTION: %v", err)
			}
			call := "SELECT TO_JSON_STRING(" + d + "." + tc.name + "(" + tc.args + "))"
			if got := bqValue(ctx, c, call); got != tc.want {
				t.Errorf("jobs.query %s = %s, want %s", call, got, tc.want)
			}
			if err := bqRun(ctx, c, call, true); err != nil {
				t.Errorf("query job %s: %v", call, err)
			}
			script := "CREATE TEMP FUNCTION tmp(" + tc.params + ") AS (" + tc.body + "); SELECT TO_JSON_STRING(tmp(" + tc.args + "))"
			if got := bqValue(ctx, c, script); got != tc.want {
				t.Errorf("TEMP function: %s = %s, want %s", script, got, tc.want)
			}
		})
	}

	// A table function's nested STRUCT of a scalar subquery.
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".nested_tf(x INT64) AS "+
		"SELECT STRUCT(STRUCT(i AS b) AS a, [STRUCT(i AS c)] AS d) AS s FROM (SELECT (SELECT x + 1) AS i)", false); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION: %v", err)
	}
	if got, want := bqValue(ctx, c, "SELECT TO_JSON_STRING(s) FROM "+d+".nested_tf(6)"), `[{"a":{"b":7},"d":[{"c":7}]}]`; got != want {
		t.Errorf("nested_tf(6) = %s, want %s", got, want)
	}
}
