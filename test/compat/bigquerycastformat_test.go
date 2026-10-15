//go:build compat

package compat

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
)

// TestBigQueryCastFormat (#1194, #1195): CAST's FORMAT clause, with BigQuery's
// format elements, and AT TIME ZONE. The emulator dropped both: a cast to
// STRING ignored the format, and a cast of a STRING to TIMESTAMP with one
// failed. Expected values are the format-elements reference's examples.
func TestBigQueryCastFormat(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	cases := []struct {
		expr string
		want bigquery.Value
	}{
		// BYTES and STRING.
		{`CAST(b'\x48\x65\x6c\x6c\x6f' AS STRING FORMAT 'ASCII')`, "Hello"},
		{`CAST(b'\x00\x01\xEF\xFF' AS STRING FORMAT 'HEX')`, "0001efff"},
		{`CAST(b'\x02\x11\x3B' AS STRING FORMAT 'BASE8')`, "00410473"},
		{`CAST(b'\xde\xad\xbe\xef' AS STRING FORMAT 'BASE64M')`, "3q2+7w=="},
		{`CAST('0001efff' AS BYTES FORMAT 'HEX')`, []byte{0x00, 0x01, 0xef, 0xff}},
		{`CAST('Hello' AS BYTES FORMAT 'ASCII')`, []byte("Hello")},
		// Dates and times as STRING.
		{"CAST(DATE '2018-01-30' AS STRING FORMAT 'YYYY')", "2018"},
		{"CAST(DATE '2018-01-30' AS STRING FORMAT 'MONTH')", "JANUARY"},
		{"CAST(DATE '2018-01-30' AS STRING FORMAT 'Mon DD, YYYY')", "Jan 30, 2018"},
		{"CAST(DATE '2020-12-31' AS STRING FORMAT 'Day')", "Thursday "},
		{"CAST(DATE '2018-02-03' AS STRING FORMAT 'DDD')", "034"},
		{"CAST(TIME '21:30:00' AS STRING FORMAT 'HH12 PM')", "09 PM"},
		{"CAST(TIME '21:30:25.16' AS STRING FORMAT 'HH24:MI:SS.FF2')", "21:30:25.16"},
		{"CAST(DATETIME '2018-01-30 01:02:03' AS STRING FORMAT 'YYYY-MM-DD SSSSS a.m.')", "2018-01-30 03723 a.m."},
		{"CAST(TIMESTAMP '2008-12-25 00:00:00+00:00' AS STRING FORMAT 'TZH')", "+00"},
		{"CAST(TIMESTAMP '2008-12-25 00:00:00+00:00' AS STRING FORMAT 'TZH' AT TIME ZONE 'Asia/Kolkata')", "+05"},
		{"CAST(TIMESTAMP '2008-12-25 00:00:00+00:00' AS STRING FORMAT 'TZM' AT TIME ZONE 'Asia/Kolkata')", "30"},
		{`CAST(TIMESTAMP '2020-01-01 05:00:00+00' AS STRING FORMAT 'YYYY-MM-DD HH24:MI "in NY"' AT TIME ZONE 'America/New_York')`, "2020-01-01 00:00 in NY"},
		{"(SELECT CAST(ts AS STRING FORMAT 'YYYY-MM-DD HH24:MI TZH:TZM' AT TIME ZONE 'Asia/Kolkata') FROM UNNEST([TIMESTAMP '2008-12-25 05:30:00+00']) AS ts)", "2008-12-25 11:00 +05:30"},
		// STRING as dates and times.
		{"CAST('18-12-03' AS DATE FORMAT 'YY-MM-DD')", civil.Date{Year: 2018, Month: 12, Day: 3}},
		{"CAST('DEC 03, 2018' AS DATE FORMAT 'MON DD, YYYY')", civil.Date{Year: 2018, Month: 12, Day: 3}},
		{"CAST('DECEMBER 03, 2018' AS DATE FORMAT 'MONTH DD, YYYY')", civil.Date{Year: 2018, Month: 12, Day: 3}},
		{"CAST('15:30' AS TIME FORMAT 'HH24:MI')", civil.Time{Hour: 15, Minute: 30}},
		{"CAST('03:30 P.M.' AS TIME FORMAT 'HH:MI A.M.')", civil.Time{Hour: 15, Minute: 30}},
		{"CAST('03723' AS TIME FORMAT 'SSSSS')", civil.Time{Hour: 1, Minute: 2, Second: 3}},
		{"CAST('2018-12-03 15:30' AS DATETIME FORMAT 'YYYY-MM-DD HH24:MI')", civil.DateTime{Date: civil.Date{Year: 2018, Month: 12, Day: 3}, Time: civil.Time{Hour: 15, Minute: 30}}},
		{"CAST('2020.06.03 00:00:53+00' AS TIMESTAMP FORMAT 'YYYY.MM.DD HH24:MI:SSTZH')", time.Date(2020, 6, 3, 0, 0, 53, 0, time.UTC)},
		{"CAST('2008-12-25 05:30:00+0530' AS TIMESTAMP FORMAT 'YYYY-MM-DD HH24:MI:SSTZHTZM')", time.Date(2008, 12, 25, 0, 0, 0, 0, time.UTC)},
		{"CAST('2020-01-01 00:00' AS TIMESTAMP FORMAT 'YYYY-MM-DD HH24:MI' AT TIME ZONE 'America/New_York')", time.Date(2020, 1, 1, 5, 0, 0, 0, time.UTC)},
		{"(SELECT CAST(s AS DATE FORMAT 'MM/DD/YYYY') FROM UNNEST(['12/03/2018']) AS s)", civil.Date{Year: 2018, Month: 12, Day: 3}},
		// Numbers as STRING.
		{"CAST(1234.56 AS STRING FORMAT '$999,999.999')", "   $1,234.560"},
		{"CAST(-12345.678 AS STRING FORMAT '$999,999.999')", " -$12,345.678"},
		{"CAST(1234567.89 AS STRING FORMAT '$999,999.999')", " $###,###.###"},
		{"CAST(12 AS STRING FORMAT '999')", "  12"},
		{"CAST(-12 AS STRING FORMAT '000.000')", "-012.000"},
		{"CAST(43981 AS STRING FORMAT '0X0x')", " ABcd"},
		{"CAST(12 AS STRING FORMAT 'S99')", "+12"},
		{"CAST(-12 AS STRING FORMAT '9999PR')", "  <12>"},
		{"CAST(-12 AS STRING FORMAT 'C999')", " -USD12"},
		{"CAST(299792458 AS STRING FORMAT 'S9.999EEEE')", "+2.998E+08"},
		{"CAST(12.5 AS STRING FORMAT '999999.000FM')", "12.5"},
		{"CAST(2021 AS STRING FORMAT 'RN')", "          MMXXI"},
		{"CAST(23.5 AS STRING FORMAT 'S000V00')", "+02350"},
		{"CAST(NUMERIC '1.23' AS STRING FORMAT 'B99.999S')", " 1.230+"},
		{"(SELECT CAST(n AS STRING FORMAT '999G999') FROM UNNEST([12345]) AS n)", "  12,345"},
		{"SAFE_CAST('nope' AS DATE FORMAT 'YYYY-MM-DD')", nil},
	}
	var exprs []string
	for i, cs := range cases {
		exprs = append(exprs, fmt.Sprintf("%s AS c%d", cs.expr, i))
	}
	it, err := c.Query("SELECT " + strings.Join(exprs, ", ")).Read(ctx)
	if err != nil {
		t.Fatalf("a query of casts with FORMAT: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("read the casts: %v", err)
	}
	for i, cs := range cases {
		got := row[i]
		if ts, ok := got.(time.Time); ok {
			got = ts.UTC()
		}
		if !reflect.DeepEqual(got, cs.want) {
			t.Errorf("%s = %#v, want %#v", cs.expr, row[i], cs.want)
		}
	}
	// An element that is not documented, or one the type lacks, is an
	// error, not ignored.
	for _, q := range []string{
		"SELECT CAST(12 AS STRING FORMAT 'TM9')",
		"SELECT CAST(DATE '2018-01-30' AS STRING FORMAT 'HH24')",
		"SELECT CAST('12:00' AS DATE FORMAT 'HH24:MI')",
		"SELECT CAST('nope' AS DATE FORMAT 'YYYY-MM-DD')",
	} {
		it, err := c.Query(q).Read(ctx)
		if err == nil {
			var r []bigquery.Value
			err = it.Next(&r)
			if err == nil {
				t.Errorf("%s = %v, want an error", q, r)
			}
		}
	}
}
