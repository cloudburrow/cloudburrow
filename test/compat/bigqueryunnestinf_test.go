//go:build compat

package compat

import (
	"math"
	"testing"
)

// TestBigQueryUnnestInfinity (#1204): UNNEST of an array literal holding an
// infinite FLOAT64 failed "json: unsupported value: +Inf". BigQuery returns
// the infinities as rows (googlesqlite patch 0022).
func TestBigQueryUnnestInfinity(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		got, err := scriptRows(ctx, c, "SELECT x FROM UNNEST([CAST('inf' AS FLOAT64), CAST('-inf' AS FLOAT64), 1.5]) AS x WITH OFFSET o ORDER BY o", insert)
		if err != nil {
			t.Fatalf("insert %v: %v", insert, err)
		}
		want := []float64{math.Inf(1), math.Inf(-1), 1.5}
		if len(got) != len(want) {
			t.Fatalf("insert %v: %v, want %v", insert, got, want)
		}
		for i, row := range got {
			if f, ok := row[0].(float64); !ok || f != want[i] {
				t.Errorf("insert %v: row %d is %v, want %v", insert, i, row[0], want[i])
			}
		}
	}
}
