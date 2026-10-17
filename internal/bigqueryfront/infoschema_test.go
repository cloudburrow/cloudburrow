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

// TestRowsSubqueryUnionsAreCapped (#1374): past termsPerUnion arrays, the
// UNION ALL is nested, so no compound SELECT has more terms than SQLite
// takes, whatever the number of rows.
func TestRowsSubqueryUnionsAreCapped(t *testing.T) {
	n := rowsPerArray*termsPerUnion*termsPerUnion + 7
	rows := make([][]string, n)
	for i := range rows {
		rows[i] = []string{"1"}
	}
	got := rowsSubquery("STRUCT<n INT64>", rows)
	if c := strings.Count(got, "(1)"); c != n {
		t.Errorf("%d rows, want %d", c, n)
	}
	// Each subquery's terms: split at each "(" that opens a UNION ALL.
	depth, terms := 0, []int{0}
	for i := 0; i < len(got); i++ {
		switch {
		case got[i] == '(':
			depth++
			terms = append(terms, 0)
		case got[i] == ')':
			if terms[len(terms)-1]+1 > termsPerUnion {
				t.Fatalf("a UNION ALL of %d terms", terms[len(terms)-1]+1)
			}
			terms = terms[:len(terms)-1]
			depth--
		case strings.HasPrefix(got[i:], " UNION ALL "):
			terms[len(terms)-1]++
		}
	}
	if depth != 0 {
		t.Errorf("unbalanced: %d", depth)
	}
	if !strings.Contains(got, "SELECT * FROM (SELECT * FROM (SELECT * FROM UNNEST(") {
		t.Errorf("not nested twice: %.200s", got)
	}
}

// structArity is the number of top-level fields of a STRUCT<...> type.
func structArity(typ string) int {
	body := strings.TrimSuffix(strings.TrimPrefix(typ, "STRUCT<"), ">")
	n, depth := 1, 0
	for _, r := range body {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				n++
			}
		}
	}
	return n
}

// TestColumnsRowsArity (#1490): every COLUMNS and COLUMN_FIELD_PATHS row,
// of an ingestion-time partitioned table's pseudo-columns too, has a
// value per field of the view's row type. A pseudo-column row one short
// failed COLUMNS 400 "Array element type STRUCT<STRING, ...> does not
// coerce" for every dataset holding such a table.
func TestColumnsRowsArity(t *testing.T) {
	tables := []infoTable{
		{"tableId": "ing", "timePartitioning": map[string]any{"type": "DAY"},
			"schema": map[string]any{"fields": []any{map[string]any{"name": "x", "type": "INTEGER"}}}},
		{"tableId": "hourly", "timePartitioning": map[string]any{"type": "HOUR"}},
		{"tableId": "nested", "timePartitioning": map[string]any{"type": "DAY", "field": "d"},
			"clustering": map[string]any{"fields": []any{"g"}},
			"schema": map[string]any{"fields": []any{
				map[string]any{"name": "d", "type": "DATE", "mode": "REQUIRED"},
				map[string]any{"name": "g", "type": "GEOGRAPHY"},
				map[string]any{"name": "j", "type": "JSON"},
				map[string]any{"name": "r", "type": "RECORD", "mode": "REPEATED", "fields": []any{
					map[string]any{"name": "a", "type": "STRING", "collation": "und:ci"}}},
			}}},
	}
	for name, v := range map[string]struct {
		columns string
		rows    func(project, dataset string, t infoTable) [][]string
	}{"COLUMNS": {columnsColumns, columnsRows}, "COLUMN_FIELD_PATHS": {fieldPathsColumns, fieldPathsRows}} {
		want := structArity(v.columns)
		for _, tb := range tables {
			for _, row := range v.rows("p", "d", tb) {
				if len(row) != want {
					t.Errorf("%s of %s: %d values, want %d: %v", name, tb["tableId"], len(row), want, row)
				}
			}
		}
	}
	if n := len(columnsRows("p", "d", tables[0])); n != 3 {
		t.Errorf("COLUMNS of an ingestion-time DAY table: %d rows, want x, _PARTITIONTIME and _PARTITIONDATE", n)
	}
}
