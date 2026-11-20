package bigqueryfront

import (
	"fmt"
	"strings"
	"testing"
)

// BigQuery's 10,000 columns per table count nested ones (#2008).
func TestCheckSchemaColumnLimit(t *testing.T) {
	wide := func(n int) []field {
		out := make([]field, n)
		for i := range out {
			out[i] = field{Name: fmt.Sprintf("c%d", i), Type: "INT64"}
		}
		return out
	}
	if got := checkSchema(wide(10000), ""); got != "" {
		t.Errorf("10,000 columns: %q", got)
	}
	if got := checkSchema(wide(10001), ""); !strings.Contains(got, "has 10001 columns") || !strings.Contains(got, "up to 10000 columns") {
		t.Errorf("10,001 columns: %q", got)
	}
	nested := append(wide(9999), field{Name: "r", Type: "RECORD", Fields: wide(1)})
	if got := checkSchema(nested, ""); !strings.Contains(got, "has 10001 columns") {
		t.Errorf("9,999 columns and a RECORD of one: %q", got)
	}
	if n := countColumns(nested); n != 10001 {
		t.Errorf("countColumns = %d, want 10001", n)
	}
}

// A window stays Stamped only while every append to it was (#2007).
func TestStreamWindowStamped(t *testing.T) {
	var s streamBuffers
	s.addRows(streamWindow{Table: "t", Rows: 1, Stamped: true}, "k")
	if w := s.wins["k"]; w == nil || !w.Stamped || w.Rows != 1 {
		t.Fatalf("first append: %+v", w)
	}
	s.addRows(streamWindow{Table: "t", Rows: 2, Stamped: false}, "k")
	if w := s.wins["k"]; w.Stamped || w.Rows != 3 {
		t.Fatalf("an unstamped append: %+v", w)
	}
	s.addRows(streamWindow{Table: "t", Rows: 1, Stamped: true}, "k")
	if s.wins["k"].Stamped {
		t.Fatal("a stamped append made the window stamped again")
	}
}
