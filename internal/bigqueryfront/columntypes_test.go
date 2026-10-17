package bigqueryfront

import (
	"reflect"
	"testing"
)

// TestCheckRowRangeObjects (#1492): a RANGE given as an object of start
// and end, at the top level, REPEATED and in a RECORD, is written as its
// text; another key, or a bound not a string, is refused.
func TestCheckRowRangeObjects(t *testing.T) {
	elem := &struct {
		Type string `json:"type"`
	}{Type: "DATE"}
	fields := []field{
		{Name: "r", Type: "RANGE", RangeElementType: elem},
		{Name: "ra", Type: "RANGE", Mode: "REPEATED", RangeElementType: elem},
		{Name: "s", Type: "RECORD", Fields: []field{{Name: "x", Type: "RANGE", RangeElementType: elem}}},
	}
	out, errs := checkRow(fields, map[string]any{
		"r":  map[string]any{"start": "2020-01-01", "end": nil},
		"ra": []any{map[string]any{"end": "2021-01-01"}, "[2022-01-01, 2022-02-01)"},
		"s":  map[string]any{"x": map[string]any{"Start": "2023-01-01", "End": "2023-02-01"}},
	}, false)
	if len(errs) != 0 {
		t.Fatalf("errors %v", errs)
	}
	want := map[string]any{
		"r":  "[2020-01-01, UNBOUNDED)",
		"ra": []any{"[UNBOUNDED, 2021-01-01)", "[2022-01-01, 2022-02-01)"},
		"s":  map[string]any{"x": "[2023-01-01, 2023-02-01)"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("row %v, want %v", out, want)
	}
	for _, bad := range []any{map[string]any{"start": "2020-01-01", "middle": "x"}, map[string]any{"start": 3}} {
		if _, errs := checkRow(fields[:1], map[string]any{"r": bad}, false); len(errs) != 1 {
			t.Errorf("%v: errors %v, want one", bad, errs)
		}
	}
}

// TestCheckRowMaxLength (#1493): a STRING longer than its maxLength in
// characters, or a BYTES in bytes, is refused; one at the limit is not.
func TestCheckRowMaxLength(t *testing.T) {
	fields := []field{{Name: "s", Type: "STRING", MaxLength: "3"}, {Name: "b", Type: "BYTES", MaxLength: "2"}}
	for _, c := range []struct {
		row  map[string]any
		errs int
	}{
		{map[string]any{"s": "héé", "b": "AAE="}, 0},
		{map[string]any{"s": "four"}, 1},
		{map[string]any{"b": "AAEC"}, 1},
	} {
		if _, errs := checkRow(fields, c.row, false); len(errs) != c.errs {
			t.Errorf("%v: errors %v, want %d", c.row, errs, c.errs)
		}
	}
}
