//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryCastColumnToString (#1202): a value cast to STRING at run
// time, a column's, gives BigQuery's string, the one a literal's cast
// gives. A DATETIME column had a T, "2018-12-03T15:30:00"; TIME, DATETIME
// and INTERVAL sub-second digits were not in groups of three; FLOAT64 NaN
// and infinities were Go's "NaN" and "+Inf"; BYTES were base64.
func TestBigQueryCastColumnToString(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	cases := []struct{ lit, want string }{
		{"DATETIME '2018-12-03 15:30:00'", "2018-12-03 15:30:00"},
		{"DATETIME '2018-12-03 15:30:00.5'", "2018-12-03 15:30:00.500"},
		{"DATETIME '2018-12-03 15:30:00.123456'", "2018-12-03 15:30:00.123456"},
		{"TIME '15:30:00.5'", "15:30:00.500"},
		{"TIME '15:30:00'", "15:30:00"},
		{"TIMESTAMP '2018-12-03 15:30:00.5+00'", "2018-12-03 15:30:00.500+00"},
		{"DATE '2018-12-03'", "2018-12-03"},
		{"TRUE", "true"},
		{"42", "42"},
		{"1.5", "1.5"},
		{"1e20", "1e+20"},
		{"CAST('nan' AS FLOAT64)", "nan"},
		{"CAST('inf' AS FLOAT64)", "inf"},
		{"CAST('-inf' AS FLOAT64)", "-inf"},
		{"NUMERIC '1.50'", "1.5"},
		{"BIGNUMERIC '1.5000'", "1.5"},
		{"b'abc'", "abc"},
		{"INTERVAL '1-2 3 4:5:6.5' YEAR TO SECOND", "1-2 3 4:5:6.500"},
		{"RANGE<DATE> '[2020-01-01, 2020-02-01)'", "[2020-01-01, 2020-02-01)"},
	}
	var exprs []string
	for i, cs := range cases {
		// Folded by the analyzer, and cast at run time. A FLOAT64 that is
		// not finite is made at run time from a STRING: an array of one
		// fails apart (#1204).
		col := fmt.Sprintf("(SELECT CAST(x AS STRING) FROM UNNEST([%s]) AS x)", cs.lit)
		if s, ok := strings.CutPrefix(cs.lit, "CAST('"); ok && strings.HasSuffix(s, "' AS FLOAT64)") {
			col = fmt.Sprintf("(SELECT CAST(CAST(s AS FLOAT64) AS STRING) FROM UNNEST(['%s']) AS s)", strings.TrimSuffix(s, "' AS FLOAT64)"))
		}
		exprs = append(exprs, fmt.Sprintf("CAST(%s AS STRING) AS l%d", cs.lit, i), fmt.Sprintf("%s AS c%d", col, i))
	}
	it, err := c.Query("SELECT " + strings.Join(exprs, ", ")).Read(ctx)
	if err != nil {
		t.Fatalf("a query of casts to STRING: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("read the casts: %v", err)
	}
	for i, cs := range cases {
		if got := row[2*i]; got != cs.want {
			t.Errorf("CAST(%s AS STRING) = %#v, want %q", cs.lit, got, cs.want)
		}
		if got := row[2*i+1]; got != cs.want {
			t.Errorf("a column's CAST(%s AS STRING) = %#v, want %q", cs.lit, got, cs.want)
		}
	}
	// BYTES that are not UTF-8 are an error, and NULL with SAFE_CAST.
	if it, err := c.Query(`SELECT (SELECT CAST(x AS STRING) FROM UNNEST([b'\xff']) AS x)`).Read(ctx); err == nil {
		var r []bigquery.Value
		if err := it.Next(&r); err == nil {
			t.Errorf(`CAST(b'\xff' AS STRING) = %v, want an error`, r)
		}
	}
	it, err = c.Query(`SELECT (SELECT SAFE_CAST(x AS STRING) FROM UNNEST([b'\xff']) AS x)`).Read(ctx)
	if err != nil {
		t.Fatalf("SAFE_CAST of bytes that are not UTF-8: %v", err)
	}
	var vals []bigquery.Value
	if err := it.Next(&vals); err != nil || vals[0] != nil {
		t.Errorf(`SAFE_CAST(b'\xff' AS STRING) = %v, %v; want NULL`, vals, err)
	}
}
