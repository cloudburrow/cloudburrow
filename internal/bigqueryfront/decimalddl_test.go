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
		{name: "c", rounding: "ROUND_HALF_EVEN"}, {name: "s", fields: []decimalColumn{{name: "x", precision: "2"}}}}
	if !reflect.DeepEqual(d.decimals, want) {
		t.Errorf("decimals %+v, want %+v", d.decimals, want)
	}
	d, _, _ = parseDDLOptions("CREATE TABLE ds.t (a NUMERIC(2) OPTIONS (rounding_mode = 'UP'))")
	if !strings.Contains(d.invalid, "rounding_mode") {
		t.Errorf("an invalid rounding_mode: %q", d.invalid)
	}
	for _, bad := range []string{
		"CREATE TABLE ds.t (s STRUCT<a NUMERIC(2) OPTIONS (rounding_mode = 'UP')>)",
		"CREATE TABLE ds.t (s ARRAY<STRUCT<a STRING OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')>>)",
	} {
		if d, _, _ := parseDDLOptions(bad); !strings.Contains(d.invalid, "rounding_mode") {
			t.Errorf("%s: invalid %q", bad, d.invalid)
		}
	}
	// #1409: a STRUCT's fields, and an ARRAY<STRUCT>'s, at any depth.
	d, _, _ = parseDDLOptions("CREATE TABLE ds.t (s STRUCT<a NUMERIC(4, 1) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'), " +
		"t STRUCT<b BIGNUMERIC(3)>>, r ARRAY<STRUCT<c NUMERIC(2)>>, n ARRAY<NUMERIC(2)>)")
	nested := []decimalColumn{
		{name: "s", fields: []decimalColumn{{name: "a", precision: "4", scale: "1", rounding: "ROUND_HALF_EVEN"},
			{name: "t", fields: []decimalColumn{{name: "b", precision: "3"}}}}},
		{name: "r", fields: []decimalColumn{{name: "c", precision: "2"}}},
		{name: "n", precision: "2"},
	}
	if d.invalid != "" || !reflect.DeepEqual(d.decimals, nested) {
		t.Errorf("nested decimals %+v (invalid %q), want %+v", d.decimals, d.invalid, nested)
	}
	nmeta := map[string]json.RawMessage{"schema": json.RawMessage(`{"fields":[{"name":"s","type":"RECORD","fields":[` +
		`{"name":"a","type":"NUMERIC"},{"name":"t","type":"RECORD","fields":[{"name":"b","type":"BIGNUMERIC"}]}]}]}`)}
	putDecimalColumns(nmeta, nested)
	if got := string(nmeta["schema"]); got != `{"fields":[{"fields":[{"name":"a","precision":"4","roundingMode":"ROUND_HALF_EVEN","scale":"1","type":"NUMERIC"},`+
		`{"fields":[{"name":"b","precision":"3","type":"BIGNUMERIC"}],"name":"t","type":"RECORD"}],"name":"s","type":"RECORD"}]}` {
		t.Errorf("nested schema %s", got)
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
	// #1409: a STRUCT's field, an ARRAY's element, an ARRAY<STRUCT>'s field.
	nested := []field{
		{Name: "s", Type: "RECORD", Fields: []field{{Name: "a", Type: "NUMERIC", Precision: "2", RoundingMode: "ROUND_HALF_EVEN"}}},
		{Name: "r", Type: "NUMERIC", Mode: "REPEATED", Precision: "2"},
		{Name: "x", Type: "STRUCT", Mode: "REPEATED", Fields: []field{{Name: "e", Type: "NUMERIC", Precision: "2"}}},
	}
	for _, c := range []struct {
		row map[string]any
		bad []string
	}{
		{map[string]any{"s": map[string]any{"a": "98.5"}, "r": []any{"99.4", "-99.4"}, "x": []any{map[string]any{"e": "1"}}}, nil},
		{map[string]any{"s": map[string]any{"a": "99.5"}}, []string{"s.a"}},
		{map[string]any{"r": []any{"1", "99.5"}}, []string{"r"}},
		{map[string]any{"x": []any{map[string]any{"e": "1"}, map[string]any{"e": "-99.5"}}}, []string{"x.e"}},
	} {
		var got []string
		for _, e := range decimalOverflow(nested, c.row, "") {
			got = append(got, e.Location)
		}
		if !reflect.DeepEqual(got, c.bad) {
			t.Errorf("%v: refused %v, want %v", c.row, got, c.bad)
		}
	}
	if s := scratchSchema(nested); s.Fields[0].Fields[0].Precision != "" || s.Fields[2].Fields[0].Precision != "" || nested[0].Fields[0].Precision != "2" {
		t.Errorf("scratchSchema of nested fields: %+v (source %+v)", s.Fields, nested)
	}
	if s := scratchSchema(fields); s.Fields[1].Precision != "" || s.Fields[1].RoundingMode != "" || fields[1].Precision != "2" {
		t.Errorf("scratchSchema: %+v (source %+v)", s.Fields[1], fields[1])
	}
}

// TestWithoutFieldRounding (#1409): a nested NUMERIC's parameters and a
// STRUCT field's OPTIONS (rounding_mode) are cut from what the engine is
// sent; a column's, and a field's description, are kept.
func TestWithoutFieldRounding(t *testing.T) {
	sql := "CREATE TABLE ds.t (a NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'), " +
		"s STRUCT<e NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'), d STRING OPTIONS (description = 'x')>, " +
		"x ARRAY<STRUCT<e NUMERIC(2) OPTIONS(rounding_mode='ROUND_HALF_AWAY_FROM_ZERO')>>, r ARRAY<BIGDECIMAL(4, 2)>)"
	d, _, ok := parseDDLOptions(sql)
	want := "CREATE TABLE ds.t (a NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'), " +
		"s STRUCT<e NUMERIC , d STRING OPTIONS (description = 'x')>, x ARRAY<STRUCT<e NUMERIC >>, r ARRAY<BIGDECIMAL>)"
	if !ok || d.sent != want {
		t.Errorf("sent %q, want %q", d.sent, want)
	}
	if d, _, _ := parseDDLOptions("CREATE TABLE ds.t (a NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'))"); d.sent != "" {
		t.Errorf("a column's rounding_mode: sent %q", d.sent)
	}
}
