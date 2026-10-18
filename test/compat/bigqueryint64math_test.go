//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryInt64MathExact (#1599): GoogleSQL's ABS, MOD, SAFE_ADD,
// SAFE_SUBTRACT, SAFE_MULTIPLY, SAFE_NEGATE, GREATEST and LEAST of INT64s
// are INT64s (the mathematical functions reference's return type tables),
// so exact at the INT64 edges, where the engine's float64 lost the low
// digits above 2^53 (googlesqlite patches 0067, MOD, and 0075, ABS and
// the SAFE_ functions). The SAFE_ functions are NULL on overflow
// ("returns NULL if overflow occurs"), INT64 +, - and * fail on it, and
// ABS of the INT64 minimum is an
// error ("Returns an error if the argument is an integer and the output
// value can't be represented as the same type"): 400 invalidQuery, its
// text "int64 overflow: ABS(-9223372036854775808)" UNVERIFIED (docs
// re-read 2026-10-17:
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/mathematical_functions).
// The hash functions page's sampling idiom, ABS(FARM_FINGERPRINT) modulo
// 100, now picks by the exact remainder.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInt64MathExact(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	// From columns, so the analyzer folds nothing.
	const sql = `SELECT
  ABS(a) AS abs_a, ABS(b) AS abs_b, ABS(mx) AS abs_max, ABS(-mx) AS abs_minus_max, ABS(z) AS abs_zero,
  MOD(b, 100) AS mod_b, MOD(mx, 3) AS mod_max, MOD(mn, -1) AS mod_min, MOD(mn, 10) AS mod_min_10,
  MOD(ABS(b), 100) AS mod_abs_b, ABS(MOD(b, 100)) AS abs_mod_b,
  SAFE_ADD(mx, 1) AS add_over, SAFE_ADD(a, 2) AS add_exact,
  SAFE_SUBTRACT(mn, 1) AS sub_over, SAFE_SUBTRACT(-1, mn) AS sub_edge,
  SAFE_MULTIPLY(3037000499, s) AS mul_exact, SAFE_MULTIPLY(mn, -1) AS mul_over,
  SAFE_NEGATE(mn) AS neg_over, SAFE_NEGATE(a) AS neg_a,
  GREATEST(-a, -a - 1) AS greatest, LEAST(-a, -a - 1) AS least, SIGN(mn) AS sign_min
FROM (SELECT -9007199254740993 AS a, -6889379130834334227 AS b, 9223372036854775807 AS mx,
  -9223372036854775808 AS mn, 0 AS z, 3037000499 AS s)`
	want := map[string]bigquery.Value{
		"abs_a": int64(9007199254740993), "abs_b": int64(6889379130834334227), "abs_max": int64(9223372036854775807),
		"abs_minus_max": int64(9223372036854775807), "abs_zero": int64(0),
		"mod_b": int64(-27), "mod_max": int64(1), "mod_min": int64(0), "mod_min_10": int64(-8),
		"mod_abs_b": int64(27), "abs_mod_b": int64(27),
		"add_over": nil, "add_exact": int64(-9007199254740991),
		"sub_over": nil, "sub_edge": int64(9223372036854775807),
		"mul_exact": int64(9223372030926249001), "mul_over": nil,
		"neg_over": nil, "neg_a": int64(9007199254740993),
		"greatest": int64(9007199254740993), "least": int64(9007199254740992), "sign_min": int64(-1),
	}
	check := func(what string, rows []map[string]bigquery.Value) {
		t.Helper()
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", what, len(rows))
		}
		for k, v := range want {
			if got, ok := rows[0][k]; !ok || got != v {
				t.Errorf("%s: %s = %v (%T); want %v", what, k, got, got, v)
			}
		}
	}
	check("jobs.query", mlQuery(t, h, c, sql))
	job, err := c.Query(sql).Run(ctx)
	if err != nil {
		t.Fatalf("jobs.insert: %v", err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatalf("Job.Wait: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("Job.Read: %v", err)
	}
	row := map[string]bigquery.Value{}
	if err := it.Next(&row); err != nil {
		t.Fatalf("Job.Read Next: %v", err)
	}
	check("jobs.insert", []map[string]bigquery.Value{row})

	// The sampling idiom: MOD(ABS(FARM_FINGERPRINT(s)), 100) is the exact
	// remainder, that of DIV, which was always exact.
	n := mlQuery(t, h, c, `SELECT COUNTIF(MOD(ABS(f), 100) != ABS(f - DIV(f, 100) * 100)) AS bad, COUNT(*) AS n
FROM (SELECT FARM_FINGERPRINT(CAST(x AS STRING)) AS f FROM UNNEST(GENERATE_ARRAY(1, 2000)) AS x)`)
	if len(n) != 1 || n[0]["bad"] != int64(0) || n[0]["n"] != int64(2000) {
		t.Errorf("MOD(ABS(FARM_FINGERPRINT), 100) against DIV: %v; want no row apart", n)
	}

	// SAFE_MULTIPLY is its own function, never the safe form of *, which
	// shares its engine name (googlesqlite patch 0075): NULL at each edge.
	edges := mlQuery(t, h, c, `SELECT SAFE_MULTIPLY(x, 2) AS a, SAFE_MULTIPLY(-1, x) AS b, SAFE_MULTIPLY(y, y) AS c,
  SAFE_MULTIPLY(3037000500, 3037000500 + z) AS d, SAFE_ADD(y, y) AS e, SAFE_SUBTRACT(x, y) AS f
FROM (SELECT -9223372036854775808 AS x, 9223372036854775807 AS y, 0 AS z)`)
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		if len(edges) != 1 || edges[0][k] != nil {
			t.Errorf("SAFE_ overflow %s: %v; want NULL", k, edges)
		}
	}

	// +, - and * of INT64s that overflow fail (the operators reference:
	// "All operators will throw an error if the computation result
	// overflows"), the text GoogleSQL's, UNVERIFIED against BigQuery (docs
	// re-read 2026-10-17:
	// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/operators).
	for q, msg := range map[string]string{
		"SELECT x + 1 AS v FROM UNNEST([9223372036854775807]) AS x":  "int64 overflow: 9223372036854775807 + 1",
		"SELECT x - 1 AS v FROM UNNEST([-9223372036854775808]) AS x": "int64 overflow: -9223372036854775808 - 1",
		"SELECT x * 2 AS v FROM UNNEST([9223372036854775807]) AS x":  "int64 overflow: 9223372036854775807 * 2",
	} {
		_, err := c.Query(q).Read(ctx)
		if got := failedQueryError(t, q, err); got.code != 400 || got.reason != "invalidQuery" || !strings.Contains(got.message, msg) {
			t.Errorf("%s: %v; want 400 invalidQuery %q", q, got, msg)
		}
	}

	// ABS of the INT64 minimum fails.
	const overflow = "SELECT ABS(x) AS a FROM UNNEST([-9223372036854775808]) AS x"
	_, err = c.Query(overflow).Read(ctx)
	if got := failedQueryError(t, "jobs.query", err); got.code != 400 || got.reason != "invalidQuery" ||
		!strings.Contains(got.message, "int64 overflow: ABS(-9223372036854775808)") {
		t.Errorf("ABS(INT64 minimum), jobs.query: %v; want 400 invalidQuery int64 overflow", got)
	}
	job, err = c.Query(overflow).Run(ctx)
	if err != nil {
		t.Fatalf("jobs.insert of the overflow: %v", err)
	}
	_, err = job.Wait(ctx)
	if got := failedQueryError(t, "Job.Wait", err); got.code != 400 || got.reason != "invalidQuery" ||
		!strings.Contains(got.message, "int64 overflow") {
		t.Errorf("ABS(INT64 minimum), Job.Wait: %v; want 400 invalidQuery int64 overflow", got)
	}
}
