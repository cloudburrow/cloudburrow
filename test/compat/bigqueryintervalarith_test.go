//go:build compat

package compat

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/memory"
	"google.golang.org/grpc/codes"
)

// TestBigQueryIntervalArithmetic (#1437): INTERVAL arithmetic as BigQuery
// documents it (operators, "Interval arithmetic operators"; interval
// functions): INTERVAL + and - INTERVAL, * and / INT64, unary minus;
// DATE, DATETIME and TIMESTAMP + and - INTERVAL, the month first and a day
// past the month's end its last day; the difference of two DATEs,
// DATETIMEs or TIMESTAMPs as an INTERVAL; JUSTIFY_DAYS, JUSTIFY_HOURS,
// JUSTIFY_INTERVAL, MAKE_INTERVAL and EXTRACT from an INTERVAL. Each part,
// months, days and the time part, keeps its own sign, and a result out of
// INTERVAL's range is an error. Arithmetic on a column and an INTERVAL
// query parameter reads back as an INTERVAL. Measured first through the
// front: INTERVAL 1 DAY + INTERVAL 2 HOUR failed 400 "unsupported add
// operator for interval value" (as did -, *, / and unary minus), and DATE
// '2021-01-31' + INTERVAL 1 MONTH was 2021-03-03. Fixed in the engine
// CloudBurrow builds (googlesqlite patch 0035).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryIntervalArithmetic(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, tc := range []struct{ expr, want string }{
		{"INTERVAL 1 DAY + INTERVAL 2 HOUR", "0-0 1 2:0:0"},
		{"INTERVAL '1-2 3 4:5:6' YEAR TO SECOND + INTERVAL '0-11 30 20:0:0' YEAR TO SECOND", "2-1 33 24:5:6"},
		{"INTERVAL 1 DAY - INTERVAL 2 HOUR", "0-0 1 -2:0:0"},
		{"INTERVAL 1 YEAR - INTERVAL 13 MONTH", "-0-1 0 0:0:0"},
		{"INTERVAL 1 DAY * 3", "0-0 3 0:0:0"},
		{"3 * INTERVAL '1-1 1 1:1:1.5' YEAR TO SECOND", "3-3 3 3:3:4.500"},
		{"INTERVAL 10 MONTH / 3", "0-3 10 0:0:0"},
		{"INTERVAL 1 DAY / 7", "0-0 0 3:25:42.857142"},
		{"-(INTERVAL '1-2 -3 4:5:6' YEAR TO SECOND)", "-1-2 3 -4:5:6"},
		{"DATE '2021-01-31' + INTERVAL 1 MONTH", "2021-02-28 00:00:00"},
		{"INTERVAL 1 MONTH + DATE '2020-01-31'", "2020-02-29 00:00:00"},
		{"DATE '2021-03-31' - INTERVAL 1 MONTH", "2021-02-28 00:00:00"},
		{"DATETIME '2021-01-31 23:00:00' + INTERVAL '0-1 1 2:0:0' YEAR TO SECOND", "2021-03-02 01:00:00"},
		{"TIMESTAMP '2021-01-31 00:00:00+00' + INTERVAL 1 MONTH", "2021-02-28 00:00:00+00"},
		{"TIMESTAMP '2021-05-02 00:01:02.345+00' + INTERVAL 25 HOUR", "2021-05-03 01:01:02.345+00"},
		{"DATE '2021-05-20' - DATE '2020-04-19'", "0-0 396 0:0:0"},
		{"DATETIME '2021-06-01 12:34:56.789' - DATETIME '2021-05-31 00:00:00'", "0-0 0 36:34:56.789"},
		{"TIMESTAMP '2021-06-01 12:34:56.789' - TIMESTAMP '2021-05-31 00:00:00'", "0-0 0 36:34:56.789"},
		{"JUSTIFY_DAYS(INTERVAL -65 DAY)", "-0-2 -5 0:0:0"},
		{"JUSTIFY_DAYS(INTERVAL 370 DAY)", "1-0 10 0:0:0"},
		{"JUSTIFY_HOURS(INTERVAL -25 HOUR)", "0-0 -1 -1:0:0"},
		{"JUSTIFY_HOURS(INTERVAL 125 MINUTE)", "0-0 0 2:5:0"},
		{"JUSTIFY_INTERVAL(INTERVAL '29 49:00:00' DAY TO SECOND)", "0-1 1 1:0:0"},
		{"MAKE_INTERVAL(1, 6, 15)", "1-6 15 0:0:0"},
		{"MAKE_INTERVAL(1, minute => 72, hour => 10)", "1-0 0 11:12:0"},
		{"EXTRACT(MINUTE FROM MAKE_INTERVAL(minute => 72))", "12"},
		{"EXTRACT(HOUR FROM INTERVAL 25 HOUR)", "25"},
		{"EXTRACT(MONTH FROM INTERVAL '-1-2 -3 -4:5:6.789999' YEAR TO SECOND)", "-2"},
		{"EXTRACT(MILLISECOND FROM INTERVAL '1-2 3 4:5:6.789999' YEAR TO SECOND)", "789"},
		{"EXTRACT(MICROSECOND FROM INTERVAL '1-2 3 4:5:6.789999' YEAR TO SECOND)", "789999"},
	} {
		row, err := oneValue(ctx, c, "SELECT CAST("+tc.expr+" AS STRING)", false)
		if err != nil || len(row) != 1 || row[0] != tc.want {
			t.Errorf("SELECT %s: %v %v, want %s", tc.expr, row, err, tc.want)
		}
	}
	for _, tc := range []struct{ expr, want string }{
		{"INTERVAL 10000 YEAR + INTERVAL 1 MONTH", "out of range"},
		{"INTERVAL 5000 YEAR * 3", "Interval overflow during multiplication"},
		{"INTERVAL 1 DAY / 0", "Interval division by zero"},
		{"MAKE_INTERVAL(year => 10001)", "out of range"},
		{"DATE '9999-12-31' + INTERVAL 1 DAY", "overflow"},
	} {
		row, err := oneValue(ctx, c, "SELECT CAST("+tc.expr+" AS STRING)", false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("SELECT %s: %v %v, want an error with %q", tc.expr, row, err, tc.want)
		}
	}

	// A column and a parameter, through a query job; the result is read
	// as the client's INTERVAL.
	if err := bqRun(ctx, c, "CREATE TABLE "+ds.DatasetID+".ivarith AS SELECT 1 AS id, INTERVAL '1-2 3 4:0:0' YEAR TO SECOND AS v, "+
		"DATE '2021-01-31' AS d", true); err != nil {
		t.Fatal(err)
	}
	p := bigquery.QueryParameter{Name: "p", Value: &bigquery.IntervalValue{Days: 1, Hours: -1}}
	row, err := oneValue(ctx, c, "SELECT v * 2 + @p, -v, d + v, JUSTIFY_INTERVAL(v + @p * 30) FROM "+ds.DatasetID+".ivarith", true, p)
	if err != nil || len(row) != 4 {
		t.Fatalf("arithmetic on a column and a parameter: %v %v", row, err)
	}
	got := []string{row[0].(*bigquery.IntervalValue).String(), row[1].(*bigquery.IntervalValue).String(),
		fmt.Sprint(row[2]), row[3].(*bigquery.IntervalValue).String()}
	want := []string{"2-4 7 7:0:0", "-1-2 -3 -4:0:0", "2022-04-03T04:00:00", "1-3 1 22:0:0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("arithmetic on a column and a parameter: %q, want %q", got, want)
	}
}

// TestBigQueryStorageWriteArrowIntervalStrings (#1437): an INTERVAL column
// written in arrow_rows as Utf8 is read as the proto path reads a string
// (#1424: the canonical format or the literal's syntax), stored canonical;
// a value that is not an interval, or is out of INTERVAL's range, fails
// the append with row_errors naming its row, and nothing of that append
// is written. Before #1437 such a value was passed to the emulator
// unchecked. (No negative month part under a year: the Go client's
// IntervalValue reads -0-1 back as 0-1.)
//
// covers: bigquery.tabledata.list
func TestBigQueryStorageWriteArrowIntervalStrings(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds, _ := twoDatasets(t, h, c)
	if err := bqRun(h.Context(), c, "CREATE TABLE "+ds.DatasetID+".ivs (i INT64, s INTERVAL)", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	tbl := ds.Table("ivs")
	raw, err := bqstorage.NewBigQueryWriteClient(h.Context(), storageWriteOptions(h)...)
	if err != nil {
		t.Fatalf("NewBigQueryWriteClient: %v", err)
	}
	defer raw.Close()
	appendRows := func(vals ...string) (rowErrors []int64, code codes.Code) {
		t.Helper()
		b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{
			{Name: "i", Type: arrow.PrimitiveTypes.Int64}, {Name: "s", Type: arrow.BinaryTypes.String, Nullable: true}}, nil))
		defer b.Release()
		for i, v := range vals {
			b.Field(0).(*array.Int64Builder).Append(int64(i + 1))
			b.Field(1).(*array.StringBuilder).Append(v)
		}
		schema, batch := arrowMessagesOf(t, b.NewRecord())
		ar, err := raw.AppendRows(h.Context())
		if err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		defer ar.CloseSend()
		if err := ar.Send(arrowAppend(writeTablePath(tbl)+"/streams/_default", schema, batch, nil)); err != nil {
			t.Fatalf("send: %v", err)
		}
		resp, err := ar.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		for _, re := range resp.GetRowErrors() {
			rowErrors = append(rowErrors, re.GetIndex())
		}
		return rowErrors, codes.Code(resp.GetError().GetCode())
	}
	if rowErrors, code := appendRows("1-2 3 4:5:6", "INTERVAL 5 DAY", "INTERVAL '1-2' YEAR TO MONTH", "-1-1 0 -1:0:0"); code != codes.OK || len(rowErrors) != 0 {
		t.Fatalf("append of good INTERVAL strings: %s, row errors %v", code, rowErrors)
	}
	for name, bad := range map[string]string{"not an interval": "five days", "out of range": "10001-0 0 0:0:0"} {
		if rowErrors, code := appendRows("0-0 1 0:0:0", bad); code != codes.InvalidArgument || !reflect.DeepEqual(rowErrors, []int64{1}) {
			t.Errorf("%s (%q): %s, row errors %v; want INVALID_ARGUMENT, row 1", name, bad, code, rowErrors)
		}
	}
	got := readRows(t, tbl.Read(h.Context()), "Table.Read")
	want := []string{"1|1-2 3 4:5:6", "2|0-0 5 0:0:0", "3|1-2 0 0:0:0", "4|-1-1 0 -1:0:0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read: %q, want %q", got, want)
	}
}
