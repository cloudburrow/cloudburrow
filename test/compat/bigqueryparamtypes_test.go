//go:build compat

package compat

import (
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"google.golang.org/api/iterator"
)

// TestBigQueryScalarParameterTypes (#1108): a top-level query parameter of
// each of NUMERIC, BIGNUMERIC, DATE, DATETIME, TIME, INTERVAL, GEOGRAPHY
// and JSON is of its type in the query, and reads back exactly as sent,
// as do a NULL of each, an ARRAY of each (with a NULL element) and a
// positional one; each, and a NULL of each, is inserted into a column of
// its type and compared with one; jobs.get of the INSERT shows the
// client's text and parameters. Measured
// first through the front with this client, on the pinned image: SELECT @p
// read back a STRING of the text for NUMERIC, BIGNUMERIC, DATE, TIME,
// GEOGRAPHY, JSON and INTERVAL, and a TIMESTAMP for DATETIME.
func TestBigQueryScalarParameterTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	typed := func(kind string, v any) *bigquery.QueryParameterValue {
		return &bigquery.QueryParameterValue{Type: bigquery.StandardSQLDataType{TypeKind: kind}, Value: v}
	}
	read := func(sql string, params ...bigquery.QueryParameter) ([][]bigquery.Value, bigquery.Schema) {
		t.Helper()
		q := c.Query(strings.ReplaceAll(sql, "DS.", ds.DatasetID+"."))
		q.Parameters = params
		it, err := q.Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var out [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return out, it.Schema
			}
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			out = append(out, row)
		}
	}
	bignum, _ := new(big.Rat).SetString("1.25000000001")
	// BigQuery writes a GEOGRAPHY as "POINT(1 2)" (#1119, googlesqlite
	// patch 0012).
	point := "POINT(1 2)"
	for _, tc := range []struct {
		kind  string
		field bigquery.FieldType
		value any // the parameter's Go value
		want  any // what reads back
		// eq compares a column of the type with the parameter.
		eq string
	}{
		{"NUMERIC", bigquery.NumericFieldType, big.NewRat(12345, 100), big.NewRat(12345, 100), "v = @p"},
		{"BIGNUMERIC", bigquery.BigNumericFieldType, typed("BIGNUMERIC", "1.25000000001"), bignum, "v = @p"},
		{"DATE", bigquery.DateFieldType, civil.Date{Year: 2020, Month: 1, Day: 2}, civil.Date{Year: 2020, Month: 1, Day: 2}, "v = @p"},
		{"DATETIME", bigquery.DateTimeFieldType,
			civil.DateTime{Date: civil.Date{Year: 2020, Month: 1, Day: 2}, Time: civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 6000}},
			civil.DateTime{Date: civil.Date{Year: 2020, Month: 1, Day: 2}, Time: civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 6000}},
			"v = @p"},
		{"TIME", bigquery.TimeFieldType, civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 6000},
			civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 6000}, "v = @p"},
		{"INTERVAL", bigquery.IntervalFieldType, &bigquery.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6},
			// = of INTERVALs since #1120 (googlesqlite patch 0010).
			&bigquery.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6}, "v = @p"},
		{"GEOGRAPHY", bigquery.GeographyFieldType, typed("GEOGRAPHY", "POINT(1 2)"), point, "ST_EQUALS(v, @p)"},
		{"JSON", bigquery.JSONFieldType, typed("JSON", `{"a":1}`), `{"a":1}`, "TO_JSON_STRING(v) = TO_JSON_STRING(@p)"},
	} {
		p := bigquery.QueryParameter{Name: "p", Value: tc.value}
		rows, schema := read("SELECT @p, @p IS NULL", p)
		if len(rows) != 1 || len(schema) != 2 || schema[0].Type != tc.field || fmt.Sprint(rows[0][0]) != fmt.Sprint(tc.want) ||
			rows[0][1] != false {
			t.Errorf("SELECT @p of a %s: %v of %v, want %v of %s", tc.kind, rows, schemaTypes(schema), tc.want, tc.field)
		}
		// Positional.
		rows, schema = read("SELECT ?", bigquery.QueryParameter{Value: tc.value})
		if len(rows) != 1 || len(schema) != 1 || schema[0].Type != tc.field || fmt.Sprint(rows[0][0]) != fmt.Sprint(tc.want) {
			t.Errorf("SELECT ? of a %s: %v of %v", tc.kind, rows, schemaTypes(schema))
		}
		// A NULL of the type.
		rows, schema = read("SELECT @p, @p IS NULL", bigquery.QueryParameter{Name: "p", Value: typed(tc.kind, bigquery.NullString{})})
		if len(rows) != 1 || len(schema) != 2 || schema[0].Type != tc.field || rows[0][0] != nil || rows[0][1] != true {
			t.Errorf("SELECT @p of a NULL %s: %v of %v", tc.kind, rows, schemaTypes(schema))
		}
		// An ARRAY of the type, with a NULL element.
		elem := tc.value
		if qv, ok := elem.(*bigquery.QueryParameterValue); ok {
			elem = qv.Value
		}
		arr := &bigquery.QueryParameterValue{
			Type:       bigquery.StandardSQLDataType{TypeKind: "ARRAY", ArrayElementType: &bigquery.StandardSQLDataType{TypeKind: tc.kind}},
			ArrayValue: []bigquery.QueryParameterValue{{Value: elem}, {Value: bigquery.NullString{}}, {Value: elem}},
		}
		rows, schema = read("SELECT ARRAY_LENGTH(@a), @a[OFFSET(0)], @a[OFFSET(1)] IS NULL, @a[OFFSET(2)]",
			bigquery.QueryParameter{Name: "a", Value: arr})
		if len(rows) != 1 || len(schema) != 4 || schema[1].Type != tc.field || rows[0][0] != int64(3) ||
			fmt.Sprint(rows[0][1]) != fmt.Sprint(tc.want) || rows[0][2] != true || fmt.Sprint(rows[0][3]) != fmt.Sprint(tc.want) {
			t.Errorf("an ARRAY<%s>: %v of %v", tc.kind, rows, schemaTypes(schema))
		}

		// Inserted into a column of the type, and compared with it.
		name := "t_" + strings.ToLower(tc.kind)
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType},
			{Name: "v", Type: tc.field}}}); err != nil {
			t.Fatal(err)
		}
		q := c.Query("INSERT INTO " + ds.DatasetID + "." + name + " (id, v) VALUES (1, @p), (2, @q)")
		q.Parameters = []bigquery.QueryParameter{p, {Name: "q", Value: tc.value}}
		job, err := q.Run(ctx)
		if err != nil {
			t.Errorf("INSERT of a %s: %v", tc.kind, err)
			continue
		}
		if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
			t.Errorf("INSERT of a %s: %v %v", tc.kind, err, st.Err())
			continue
		}
		rows, _ = read("SELECT id, v FROM DS."+name+" WHERE "+tc.eq+" ORDER BY id", p)
		if len(rows) != 2 || rows[0][0] != int64(1) || fmt.Sprint(rows[0][1]) != fmt.Sprint(tc.want) {
			t.Errorf("WHERE %s of a %s: %v", tc.eq, tc.kind, rows)
		}
		// A NULL inserted.
		nq := c.Query("INSERT INTO " + ds.DatasetID + "." + name + " (id, v) VALUES (3, @n)")
		nq.Parameters = []bigquery.QueryParameter{{Name: "n", Value: typed(tc.kind, bigquery.NullString{})}}
		if _, err := nq.Read(ctx); err != nil {
			t.Errorf("INSERT of a NULL %s: %v", tc.kind, err)
		}
		rows, _ = read("SELECT id FROM DS." + name + " WHERE v IS NULL")
		if fmt.Sprint(rows) != "[[3]]" {
			t.Errorf("the NULL %s inserted: %v", tc.kind, rows)
		}
		again, err := c.JobFromID(ctx, job.ID())
		if err != nil {
			t.Fatalf("jobs.get: %v", err)
		}
		if cfg, err := again.Config(); err != nil {
			t.Errorf("the job's configuration: %v", err)
		} else if qc, ok := cfg.(*bigquery.QueryConfig); !ok || strings.Contains(qc.Q, "CAST") || strings.Contains(qc.Q, "PARSE_JSON") ||
			strings.Contains(qc.Q, "ST_GEOGFROMTEXT") || len(qc.Parameters) != 2 || qc.Parameters[0].Name != "p" {
			t.Errorf("jobs.get of the INSERT of a %s: %+v", tc.kind, cfg)
		}
	}
}

func schemaTypes(s bigquery.Schema) []bigquery.FieldType {
	var out []bigquery.FieldType
	for _, f := range s {
		out = append(out, f.Type)
	}
	return out
}

// TestBigQueryRangeParameters (#1111), through the official Go client: a
// RANGE query parameter, at the top level, in an ARRAY or in a STRUCT, is
// 501 before anything runs. Measured first through the front on the
// pinned image: SELECT @r of a top-level RANGE<DATE> read back a NULL
// INTEGER, and a query's RANGE value (SELECT RANGE(DATE ..., DATE ...))
// came back with a schema field of no type, at the top level and in a
// STRUCT.
func TestBigQueryRangeParameters(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	r := &bigquery.RangeValue{Start: civil.Date{Year: 2020, Month: 1, Day: 1}, End: civil.Date{Year: 2020, Month: 2, Day: 1}}
	rangeType := &bigquery.StandardSQLDataType{TypeKind: "RANGE", RangeElementType: &bigquery.StandardSQLDataType{TypeKind: "DATE"}}
	for name, p := range map[string]bigquery.QueryParameter{
		"a top-level RANGE": {Name: "p", Value: &bigquery.QueryParameterValue{
			Type:  bigquery.StandardSQLDataType{TypeKind: "RANGE", RangeElementType: &bigquery.StandardSQLDataType{TypeKind: "DATE"}},
			Value: r,
		}},
		"a RANGE in a STRUCT": {Name: "p", Value: &bigquery.QueryParameterValue{
			Type: bigquery.StandardSQLDataType{TypeKind: "STRUCT", StructType: &bigquery.StandardSQLStructType{
				Fields: []*bigquery.StandardSQLField{{Name: "r", Type: rangeType}}}},
			StructValue: map[string]bigquery.QueryParameterValue{"r": {Type: *rangeType, Value: r}},
		}},
	} {
		q := c.Query("SELECT @p")
		q.Parameters = []bigquery.QueryParameter{p}
		_, err := q.Read(ctx)
		wantReason(t, name, err, 501, "notImplemented")
	}
}

// TestBigQueryRangeResults (#1111), through the official Go client: a
// RANGE value in a query's result reads back as a RangeValue of its
// element type, DATE, DATETIME or TIMESTAMP, an UNBOUNDED end as nil, a
// NULL RANGE as nil, and in an ARRAY. The emulator's schema gave a RANGE
// no type, which the client refused ("incomplete range schema"); the
// patched emulator gives the type RANGE and rangeElementType, and
// BigQuery's REST form of the bounds (bigquery-emulator patch 0008).
func TestBigQueryRangeResults(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	it, err := c.Query("SELECT RANGE(DATE '2020-01-01', DATE '2020-02-01') AS d, " +
		"RANGE(DATETIME '2020-01-01 03:04:05.25', NULL) AS dt, " +
		"RANGE(TIMESTAMP '2020-01-01 00:00:00 UTC', TIMESTAMP '2020-01-02 00:00:00.5 UTC') AS ts, " +
		"CAST(NULL AS RANGE<DATE>) AS n, [RANGE(DATE '2020-01-01', NULL)] AS a").Read(ctx)
	if err != nil {
		t.Fatalf("a query of RANGE values: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("read the RANGE values: %v", err)
	}
	for _, f := range it.Schema {
		if f.Type != bigquery.RangeFieldType || f.RangeElementType == nil {
			t.Errorf("column %s: type %q, element %v", f.Name, f.Type, f.RangeElementType)
		}
	}
	want := []bigquery.Value{
		&bigquery.RangeValue{Start: civil.Date{Year: 2020, Month: 1, Day: 1}, End: civil.Date{Year: 2020, Month: 2, Day: 1}},
		&bigquery.RangeValue{Start: civil.DateTime{Date: civil.Date{Year: 2020, Month: 1, Day: 1}, Time: civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 250000000}}},
		&bigquery.RangeValue{Start: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2020, 1, 2, 0, 0, 0, 500000000, time.UTC)},
		nil,
		[]bigquery.Value{&bigquery.RangeValue{Start: civil.Date{Year: 2020, Month: 1, Day: 1}}},
	}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("RANGE values %v, want %v", fmtRanges(row), fmtRanges(want))
	}
}

func fmtRanges(vs []bigquery.Value) []string {
	var out []string
	for _, v := range vs {
		if r, ok := v.(*bigquery.RangeValue); ok {
			out = append(out, fmt.Sprintf("[%v, %v)", r.Start, r.End))
			continue
		}
		out = append(out, fmt.Sprint(v))
	}
	return out
}
