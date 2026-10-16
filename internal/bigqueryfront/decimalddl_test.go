package bigqueryfront

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestReadDecimalColumns(t *testing.T) {
	sql := "CREATE TABLE ds.t (id INT64, a NUMERIC(5, 2), b DECIMAL(3) NOT NULL, " +
		"c BIGNUMERIC OPTIONS (description = 'x', rounding_mode = 'ROUND_HALF_EVEN'), s STRUCT<x NUMERIC(2)>, d STRING(4))"
	d, msg, ok := parseDDLOptions(sql)
	if !ok || msg != "" || d.invalid != "" || !d.sets() {
		t.Fatalf("parseDDLOptions: ok %v, msg %q, invalid %q", ok, msg, d.invalid)
	}
	want := []decimalColumn{{name: "a", precision: "5", scale: "2"}, {name: "b", precision: "3"},
		{name: "c", rounding: "ROUND_HALF_EVEN"}}
	if !reflect.DeepEqual(d.decimals, want) {
		t.Errorf("decimals %+v, want %+v", d.decimals, want)
	}
	d, _, _ = parseDDLOptions("CREATE TABLE ds.t (a NUMERIC(2) OPTIONS (rounding_mode = 'UP'))")
	if !strings.Contains(d.invalid, "rounding_mode") {
		t.Errorf("an invalid rounding_mode: %q", d.invalid)
	}
	if _, _, ok := parseDDLOptions("CREATE TABLE ds.t (a NUMERIC, b INT64)"); ok {
		t.Error("a column list with no parameterized decimal is left to the engine")
	}

	meta := map[string]json.RawMessage{"schema": json.RawMessage(`{"fields":[{"name":"A","type":"NUMERIC"},{"name":"c","type":"BIGNUMERIC"}]}`)}
	putDecimalColumns(meta, want)
	if got := string(meta["schema"]); got != `{"fields":[{"name":"A","precision":"5","scale":"2","type":"NUMERIC"},`+
		`{"name":"c","roundingMode":"ROUND_HALF_EVEN","type":"BIGNUMERIC"}]}` {
		t.Errorf("schema %s", got)
	}
}

func TestDecimalOverflow(t *testing.T) {
	fields := []field{
		{Name: "a", Type: "NUMERIC", Precision: "2"},
		{Name: "e", Type: "NUMERIC", Precision: "2", RoundingMode: "ROUND_HALF_EVEN"},
		{Name: "s", Type: "BIGNUMERIC", Precision: "4", Scale: "2"},
		{Name: "n", Type: "NUMERIC"},
	}
	for _, c := range []struct {
		row       map[string]any
		tableMode string
		bad       []string
	}{
		{map[string]any{"a": "99.4", "e": "98.5", "s": "-99.994", "n": "1e20"}, "", nil},
		{map[string]any{"a": "99.5"}, "", []string{"a"}},
		{map[string]any{"a": "99.5"}, "ROUND_HALF_EVEN", []string{"a"}},
		{map[string]any{"e": "100", "s": json.Number("-99.995")}, "", []string{"e", "s"}},
	} {
		errs := decimalOverflow(fields, c.row, c.tableMode)
		var got []string
		for _, e := range errs {
			if e.Reason != "invalid" || !strings.Contains(e.Message, "OUT_OF_RANGE") {
				t.Errorf("%v: error %+v", c.row, e)
			}
			got = append(got, e.Location)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, c.bad) {
			t.Errorf("%v (table mode %q): refused %v, want %v", c.row, c.tableMode, got, c.bad)
		}
	}
	if s := scratchSchema(fields); s.Fields[1].Precision != "" || s.Fields[1].RoundingMode != "" || fields[1].Precision != "2" {
		t.Errorf("scratchSchema: %+v (source %+v)", s.Fields[1], fields[1])
	}
}
