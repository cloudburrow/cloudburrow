//go:build compat

package compat

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
)

// TestBigQuerySelectListParameters (#1814), through the official Go client:
// a positional (`SELECT ? AS a`) or named (`SELECT @p AS a`) query
// parameter of each scalar type alone in the select list is a column of
// the parameter's type and reads back as sent. BigQuery's scalar parameter
// types are BIGNUMERIC, BOOL, BYTES, DATE, DATETIME, FLOAT64, GEOGRAPHY,
// INT64, INTERVAL, NUMERIC, STRING, TIME and TIMESTAMP
// (https://cloud.google.com/bigquery/docs/parameterized-queries), and JSON;
// the result schema names them STRING, INTEGER, FLOAT, BOOLEAN, BYTES,
// NUMERIC, BIGNUMERIC, DATE, TIME, DATETIME, TIMESTAMP, GEOGRAPHY, JSON and
// INTERVAL (TableFieldSchema.type). Measured first on Landing 147's engine:
// `SELECT ? AS a` with a STRING failed 400 "strconv.ParseInt ... invalid
// syntax", and a positional FLOAT64 or BOOL read back an INTEGER (3, 1):
// the engine left positional parameters undeclared, and one with no type
// from its context is an INT64 (googlesqlite patch 0097 declares them).
//
// NULL, ARRAY and STRUCT parameters (#1816): a NULL of each of those types
// is a column of its type (a QueryParameterValue's type is the parameter's,
// whatever its value, QueryParameter.parameterType), an ARRAY a REPEATED
// column of its element type ("ARRAY<T>" is a REPEATED T in a result
// schema, TableFieldSchema.mode) and a STRUCT a RECORD of its fields'
// types, in their order (TableFieldSchema.fields); each reads back as sent,
// a NULL ARRAY as an empty one, as BigQuery returns it ("BigQuery
// translates a NULL ARRAY into an empty ARRAY in the query result",
// https://cloud.google.com/bigquery/docs/arrays#array_nulls). Measured
// first through the CLI on #1814's engine: a NULL STRING, DATE or NUMERIC
// was an INTEGER (the engine left a NULL undeclared); the front built
// ARRAYs and STRUCTs of those types in the query from STRINGs (#1078,
// #1082, #1108), which the engine now types as declared (googlesqlite 0099,
// bigquery-emulator 0100).
func TestBigQuerySelectListParameters(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	typed := func(kind string, v any) *bigquery.QueryParameterValue {
		return &bigquery.QueryParameterValue{Type: bigquery.StandardSQLDataType{TypeKind: kind}, Value: v}
	}
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	date := civil.Date{Year: 2024, Month: 1, Day: 2}
	clock := civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 123456000}
	bignum, _ := new(big.Rat).SetString("1.25000000001")
	interval := &bigquery.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6}
	for _, tc := range []struct {
		kind  string
		value any // the parameter's Go value
		field bigquery.FieldType
		want  any // what reads back
	}{
		{"STRING", "hello", bigquery.StringFieldType, "hello"},
		{"STRING (empty)", "", bigquery.StringFieldType, ""},
		{"INT64", int64(-42), bigquery.IntegerFieldType, int64(-42)},
		{"FLOAT64", 3.5, bigquery.FloatFieldType, 3.5},
		{"BOOL", true, bigquery.BooleanFieldType, true},
		{"BYTES", []byte{0xff, 0x00, 0x61}, bigquery.BytesFieldType, []byte{0xff, 0x00, 0x61}},
		{"NUMERIC", big.NewRat(12345, 100), bigquery.NumericFieldType, big.NewRat(12345, 100)},
		{"BIGNUMERIC", typed("BIGNUMERIC", "1.25000000001"), bigquery.BigNumericFieldType, bignum},
		{"DATE", date, bigquery.DateFieldType, date},
		{"TIME", clock, bigquery.TimeFieldType, clock},
		{"DATETIME", civil.DateTime{Date: date, Time: clock}, bigquery.DateTimeFieldType, civil.DateTime{Date: date, Time: clock}},
		{"TIMESTAMP", ts, bigquery.TimestampFieldType, ts},
		{"GEOGRAPHY", typed("GEOGRAPHY", "POINT(1 2)"), bigquery.GeographyFieldType, "POINT(1 2)"},
		{"JSON", typed("JSON", `{"a":1}`), bigquery.JSONFieldType, `{"a":1}`},
		{"INTERVAL", interval, bigquery.IntervalFieldType, interval},
	} {
		for _, named := range []bool{false, true} {
			sql, p := "SELECT ? AS a", bigquery.QueryParameter{Value: tc.value}
			if named {
				sql, p.Name = "SELECT @p AS a", "p"
			}
			q := c.Query(sql)
			q.Parameters = []bigquery.QueryParameter{p}
			it, err := q.Read(ctx)
			if err != nil {
				t.Errorf("%s with a %s: %v", sql, tc.kind, err)
				continue
			}
			var row []bigquery.Value
			if err := it.Next(&row); err != nil {
				t.Errorf("%s with a %s: %v", sql, tc.kind, err)
				continue
			}
			if len(it.Schema) != 1 || it.Schema[0].Name != "a" || it.Schema[0].Type != tc.field {
				t.Errorf("%s with a %s: schema %v, want one column a of %s", sql, tc.kind, schemaTypes(it.Schema), tc.field)
			}
			if fmt.Sprint(row[0]) != fmt.Sprint(tc.want) {
				t.Errorf("%s with a %s = %v (%T), want %v", sql, tc.kind, row[0], row[0], tc.want)
			}
		}
	}

	// NULLs of each type, ARRAYs and STRUCTs (#1816).
	null := func(kind string) *bigquery.QueryParameterValue {
		// The client sends {"value": null}.
		return &bigquery.QueryParameterValue{Type: bigquery.StandardSQLDataType{TypeKind: kind}, Value: bigquery.NullString{}}
	}
	type inner struct {
		N *big.Rat
		D civil.Date
	}
	type rec struct {
		S  string
		B  []byte
		I  inner
		L  []civil.Date
		TS time.Time
	}
	d2 := civil.Date{Year: 2024, Month: 1, Day: 3}
	rv := rec{S: "s", B: []byte{0xff, 0x00, 0x61}, I: inner{N: big.NewRat(5, 4), D: date}, L: []civil.Date{date, d2}, TS: ts}
	elem := func(kind string) *bigquery.StandardSQLDataType { return &bigquery.StandardSQLDataType{TypeKind: kind} }
	geogs := &bigquery.QueryParameterValue{Type: bigquery.StandardSQLDataType{TypeKind: "ARRAY", ArrayElementType: elem("GEOGRAPHY")},
		ArrayValue: []bigquery.QueryParameterValue{{Value: "POINT(1 2)"}, {Value: "POINT(3 4)"}}}
	jsons := &bigquery.QueryParameterValue{Type: bigquery.StandardSQLDataType{TypeKind: "ARRAY", ArrayElementType: elem("JSON")},
		ArrayValue: []bigquery.QueryParameterValue{{Value: `{"a":1}`}}}
	for _, tc := range []struct {
		kind   string
		value  any
		schema string // the column's type, mode and fields
		want   string // fmt.Sprint of what reads back
	}{
		{"NULL STRING", null("STRING"), "STRING", "<nil>"},
		{"NULL INT64", null("INT64"), "INTEGER", "<nil>"},
		{"NULL FLOAT64", null("FLOAT64"), "FLOAT", "<nil>"},
		{"NULL BOOL", null("BOOL"), "BOOLEAN", "<nil>"},
		{"NULL BYTES", null("BYTES"), "BYTES", "<nil>"},
		{"NULL NUMERIC", null("NUMERIC"), "NUMERIC", "<nil>"},
		{"NULL BIGNUMERIC", null("BIGNUMERIC"), "BIGNUMERIC", "<nil>"},
		{"NULL DATE", null("DATE"), "DATE", "<nil>"},
		{"NULL TIME", null("TIME"), "TIME", "<nil>"},
		{"NULL DATETIME", null("DATETIME"), "DATETIME", "<nil>"},
		{"NULL TIMESTAMP", null("TIMESTAMP"), "TIMESTAMP", "<nil>"},
		{"NULL GEOGRAPHY", null("GEOGRAPHY"), "GEOGRAPHY", "<nil>"},
		{"NULL JSON", null("JSON"), "JSON", "<nil>"},
		{"NULL INTERVAL", null("INTERVAL"), "INTERVAL", "<nil>"},
		{"ARRAY<STRING>", []string{"a", "b"}, "STRING REPEATED", "[a b]"},
		{"ARRAY<INT64>", []int64{1, -2}, "INTEGER REPEATED", "[1 -2]"},
		{"ARRAY<FLOAT64>", []float64{0.5}, "FLOAT REPEATED", "[0.5]"},
		{"ARRAY<BOOL>", []bool{true, false}, "BOOLEAN REPEATED", "[true false]"},
		{"ARRAY<BYTES>", [][]byte{{0xff, 0x00, 0x61}}, "BYTES REPEATED", "[[255 0 97]]"},
		{"ARRAY<NUMERIC>", []*big.Rat{big.NewRat(12345, 100)}, "NUMERIC REPEATED", "[2469/20]"},
		{"ARRAY<DATE>", []civil.Date{date, d2}, "DATE REPEATED", "[2024-01-02 2024-01-03]"},
		{"ARRAY<TIME>", []civil.Time{clock}, "TIME REPEATED", fmt.Sprint([]civil.Time{clock})},
		{"ARRAY<DATETIME>", []civil.DateTime{{Date: date, Time: clock}}, "DATETIME REPEATED",
			fmt.Sprint([]civil.DateTime{{Date: date, Time: clock}})},
		{"ARRAY<TIMESTAMP>", []time.Time{ts}, "TIMESTAMP REPEATED", fmt.Sprint([]time.Time{ts})},
		{"ARRAY<GEOGRAPHY>", geogs, "GEOGRAPHY REPEATED", "[POINT(1 2) POINT(3 4)]"},
		{"ARRAY<JSON>", jsons, "JSON REPEATED", `[{"a":1}]`},
		{"ARRAY<INTERVAL>", []*bigquery.IntervalValue{interval}, "INTERVAL REPEATED", fmt.Sprint([]*bigquery.IntervalValue{interval})},
		{"empty ARRAY<DATE>", []civil.Date{}, "DATE REPEATED", "[]"},
		{"STRUCT", rv, "RECORD(S STRING, B BYTES, I RECORD(N NUMERIC, D DATE), L DATE REPEATED, TS TIMESTAMP)",
			fmt.Sprint([]any{"s", []byte{0xff, 0x00, 0x61}, []any{big.NewRat(5, 4), date}, []any{date, d2}, ts})},
		{"ARRAY<STRUCT>", []inner{{N: big.NewRat(3, 1), D: d2}}, "RECORD REPEATED(N NUMERIC, D DATE)", "[[3/1 2024-01-03]]"},
		{"NULL STRUCT", (*inner)(nil), "RECORD(N NUMERIC, D DATE)", "<nil>"},
	} {
		for _, named := range []bool{false, true} {
			sql, p := "SELECT ? AS a", bigquery.QueryParameter{Value: tc.value}
			if named {
				sql, p.Name = "SELECT @p AS a", "p"
			}
			q := c.Query(sql)
			q.Parameters = []bigquery.QueryParameter{p}
			it, err := q.Read(ctx)
			if err != nil {
				t.Errorf("%s with a %s: %v", sql, tc.kind, err)
				continue
			}
			var row []bigquery.Value
			if err := it.Next(&row); err != nil {
				t.Errorf("%s with a %s: %v", sql, tc.kind, err)
				continue
			}
			if len(it.Schema) != 1 || it.Schema[0].Name != "a" || fieldSchemaText(it.Schema[0]) != tc.schema {
				t.Errorf("%s with a %s: schema %v, want one column a of %s", sql, tc.kind, fieldSchemaText(it.Schema[0]), tc.schema)
			}
			if got := fmt.Sprint(row[0]); got != tc.want {
				t.Errorf("%s with a %s = %s (%T), want %s", sql, tc.kind, got, row[0], tc.want)
			}
		}
	}
	// A STRUCT's fields and an ARRAY's elements in expressions of their
	// types, and positional NULLs among other parameters.
	q := c.Query("SELECT @p.I.N + 1 AS n, DATE_ADD(@p.L[OFFSET(1)], INTERVAL 1 DAY) AS d, TO_HEX(@p.B) AS h, @z IS NULL AS z")
	q.Parameters = []bigquery.QueryParameter{{Name: "p", Value: rv}, {Name: "z", Value: (*inner)(nil)}}
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("fields of a STRUCT: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("fields of a STRUCT: %v", err)
	}
	if got := fmt.Sprint(schemaTypes(it.Schema), row); got != "[NUMERIC DATE STRING BOOLEAN] [9/4 2024-01-04 ff0061 true]" {
		t.Errorf("fields of a STRUCT: %s", got)
	}
	q = c.Query("SELECT ? AS s, ? AS d, ? AS i")
	q.Parameters = []bigquery.QueryParameter{{Value: null("STRING")}, {Value: null("DATE")}, {Value: int64(7)}}
	if it, err = q.Read(ctx); err != nil {
		t.Fatalf("positional NULLs: %v", err)
	}
	if err := it.Next(&row); err != nil {
		t.Fatalf("positional NULLs: %v", err)
	}
	if got := fmt.Sprint(schemaTypes(it.Schema), row); got != "[STRING DATE INTEGER] [<nil> <nil> 7]" {
		t.Errorf("positional NULLs: %s", got)
	}

	// Several positional parameters of different types, in their order.
	q = c.Query("SELECT ? AS s, ? AS f, ? AS b, ? AS i")
	q.Parameters = []bigquery.QueryParameter{{Value: "x"}, {Value: 2.5}, {Value: false}, {Value: int64(7)}}
	if it, err = q.Read(ctx); err != nil {
		t.Fatalf("four positional parameters: %v", err)
	}
	if err := it.Next(&row); err != nil {
		t.Fatalf("four positional parameters: %v", err)
	}
	if got := fmt.Sprint(schemaTypes(it.Schema), row); got != "[STRING FLOAT BOOLEAN INTEGER] [x 2.5 false 7]" {
		t.Errorf("four positional parameters: %s", got)
	}
}

// fieldSchemaText is f's type, " REPEATED" when it is, and its fields'
// names and types in their order.
func fieldSchemaText(f *bigquery.FieldSchema) string {
	s := string(f.Type)
	if f.Repeated {
		s += " REPEATED"
	}
	if len(f.Schema) > 0 {
		parts := make([]string, len(f.Schema))
		for i, sub := range f.Schema {
			parts[i] = sub.Name + " " + fieldSchemaText(sub)
		}
		s += "(" + strings.Join(parts, ", ") + ")"
	}
	return s
}
