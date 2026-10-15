package bigqueryfront

import (
	"strings"
	"testing"
)

// TestRowsSubqueryArraysAreCapped (#1201): rows are split into array
// literals of at most rowsPerArray elements, joined by UNION ALL, as the
// engine fails an array literal of more elements than SQLite lets a
// function take.
func TestRowsSubqueryArraysAreCapped(t *testing.T) {
	rows := make([][]string, 2*rowsPerArray+5)
	for i := range rows {
		rows[i] = []string{"1", "'a'"}
	}
	got := rowsSubquery("STRUCT<n INT64, s STRING>", rows)
	if n := strings.Count(got, "UNNEST("); n != 3 {
		t.Errorf("%d arrays, want 3", n)
	}
	if n := strings.Count(got, "(1, 'a')"); n != len(rows) {
		t.Errorf("%d rows, want %d", n, len(rows))
	}
	if got := rowsSubquery("STRUCT<n INT64>", nil); got != "(SELECT * FROM UNNEST(ARRAY<STRUCT<n INT64>>[]))" {
		t.Errorf("no rows: %s", got)
	}
}
