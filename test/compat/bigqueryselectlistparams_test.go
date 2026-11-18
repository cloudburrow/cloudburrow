//go:build compat

package compat

import (
	"fmt"
	"math/big"
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

	// Several positional parameters of different types, in their order.
	q := c.Query("SELECT ? AS s, ? AS f, ? AS b, ? AS i")
	q.Parameters = []bigquery.QueryParameter{{Value: "x"}, {Value: 2.5}, {Value: false}, {Value: int64(7)}}
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("four positional parameters: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("four positional parameters: %v", err)
	}
	if got := fmt.Sprint(schemaTypes(it.Schema), row); got != "[STRING FLOAT BOOLEAN INTEGER] [x 2.5 false 7]" {
		t.Errorf("four positional parameters: %s", got)
	}
}
