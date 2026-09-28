//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// oneValue runs sql through jobs.query, or (insert) as a query job, with
// params, and returns its one row, failing a query that takes longer than
// 20 s (a 500 the client retries).
func oneValue(ctx context.Context, c *bigquery.Client, sql string, insert bool, params ...bigquery.QueryParameter) ([]bigquery.Value, error) {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	q := c.Query(sql)
	q.Parameters = params
	var it *bigquery.RowIterator
	var err error
	if insert {
		var job *bigquery.Job
		if job, err = q.Run(qctx); err == nil {
			it, err = job.Read(qctx)
		}
	} else {
		it, err = q.Read(qctx)
	}
	if err != nil {
		return nil, err
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		return nil, err
	}
	if err := it.Next(&[]bigquery.Value{}); !errors.Is(err, iterator.Done) {
		return nil, fmt.Errorf("more than one row: %v", err)
	}
	return row, nil
}

// TestBigQueryNullArgumentsOfOtherFunctions (#1121): in BigQuery "If an
// operand is NULL, the function result is NULL" unless the function says
// otherwise (function call rules); GENERATE_DATE_ARRAY and
// GENERATE_TIMESTAMP_ARRAY return "a NULL array" if any argument is NULL
// (array functions); JSON_OBJECT: "If json_key is NULL, an error is
// produced"; IN returns FALSE for an empty set, TRUE for a match, NULL if a
// comparison with NULL decides, FALSE otherwise (operators). Each call
// below reads so through jobs.query and a query job, promptly; a call with
// its optional arguments left out still reads its value. Read first in
// the pinned emulator's engine (goccy/googlesqlite v0.3.1) and found by
// calling each of its functions with a NULL argument: these panicked (the
// emulator answered 500, which the client retries), returned an error, or
// (IN) returned FALSE. Fixed in the engine CloudBurrow builds (googlesqlite
// patch 0011).
func TestBigQueryNullArgumentsOfOtherFunctions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	const p = "ST_GEOGPOINT(1, 2)"
	for _, insert := range []bool{false, true} {
		for _, expr := range []string{
			"GENERATE_DATE_ARRAY('2020-01-01', '2020-01-03', INTERVAL CAST(NULL AS INT64) DAY)",
			"GENERATE_DATE_ARRAY(CAST(NULL AS DATE), '2020-01-03')",
			"GENERATE_TIMESTAMP_ARRAY('2020-01-01', '2020-01-02', INTERVAL CAST(NULL AS INT64) HOUR)",
			"MAKE_INTERVAL(1, CAST(NULL AS INT64))",
			"INTERVAL CAST(NULL AS INT64) DAY",
			"JUSTIFY_DAYS(CAST(NULL AS INTERVAL))",
			"JUSTIFY_HOURS(CAST(NULL AS INTERVAL))",
			"JUSTIFY_INTERVAL(CAST(NULL AS INTERVAL))",
			"JSON_TYPE(CAST(NULL AS JSON))",
			"ST_BUFFER(" + p + ", CAST(NULL AS FLOAT64))",
			"ST_BUFFERWITHTOLERANCE(" + p + ", CAST(NULL AS FLOAT64), 1)",
			"ST_INTERSECTSBOX(" + p + ", CAST(NULL AS FLOAT64), 0, 10, 10)",
			"ST_HAUSDORFFDWITHIN(" + p + ", " + p + ", CAST(NULL AS FLOAT64))",
			"ST_DISTANCE(" + p + ", CAST(NULL AS GEOGRAPHY))",
			"NET.IP_TRUNC(CAST(NULL AS BYTES), 8)",
			"NET.IPV4_FROM_INT64(CAST(NULL AS INT64))",
			"NET.PUBLIC_SUFFIX(CAST(NULL AS STRING))",
			"CAST(NULL AS INT64) IN UNNEST([1, 2])",
			"1 IN UNNEST([2, NULL])",
			"1 NOT IN UNNEST([2, NULL])",
			"1 IN (2, NULL)",
		} {
			row, err := oneValue(ctx, c, "SELECT ("+expr+") IS NULL", insert)
			if err != nil || len(row) != 1 || row[0] != true {
				t.Errorf("%s (query job %v): %v %v, want NULL", expr, insert, row, err)
			}
		}
		for _, tc := range []struct {
			expr string
			want bigquery.Value
		}{
			{"ARRAY_LENGTH(GENERATE_DATE_ARRAY('2020-01-01', '2020-01-03'))", int64(3)},
			{"CAST(MAKE_INTERVAL(hour => 10) AS STRING)", "0-0 0 10:0:0"},
			{"ST_BUFFER(" + p + ", 10) IS NULL", false},
			{"ST_INTERSECTSBOX(" + p + ", 0, 0, 10, 10)", true},
			{"ST_DISTANCE(" + p + ", " + p + ")", float64(0)},
			{"NULLIF(1, NULL)", int64(1)},
			{"1 IN UNNEST([NULL, 1])", true},
			{"1 IN UNNEST([2, 3])", false},
			{"CAST(NULL AS INT64) IN UNNEST(CAST([] AS ARRAY<INT64>))", false},
			{"1 IN (NULL, 1)", true},
			{"TO_JSON_STRING(JSON_SET(JSON '{\"a\":1}', '$.a', CAST(NULL AS INT64)))", `{"a":null}`},
		} {
			row, err := oneValue(ctx, c, "SELECT "+tc.expr, insert)
			if err != nil || len(row) != 1 || row[0] != tc.want {
				t.Errorf("%s (query job %v): %v %v, want %v", tc.expr, insert, row, err, tc.want)
			}
		}
		// An error in BigQuery too: a prompt 400 that names it, not a 500.
		start := time.Now()
		_, err := oneValue(ctx, c, "SELECT JSON_OBJECT(CAST(NULL AS STRING), 1)", insert)
		var ge *googleapi.Error
		if err == nil || !errors.As(err, &ge) || ge.Code != 400 || !strings.Contains(err.Error(), "key is NULL") || time.Since(start) > 15*time.Second {
			t.Errorf("JSON_OBJECT of a NULL key (query job %v): %v after %v, want a prompt 400 \"a key is NULL\"", insert, err, time.Since(start))
		}
	}
}

// TestBigQueryIntervalComparison (#1120, #1126): INTERVAL is a comparable
// type in BigQuery ("All data types are supported except for: GEOGRAPHY
// JSON ARRAY", comparable data types), so = and < of two INTERVALs, of a
// column and a parameter, read their value; GoogleSQL compares intervals
// by length, a month counting 30 days and a day 24 hours
// (googlesql/public/interval_value.h). A negative interval of less than an
// hour keeps its sign (canonical format [sign]Y-M [sign]D [sign]H:M:S[.F],
// interval type), stored and read back, and INTERVAL n MILLISECOND and
// MICROSECOND are taken. Measured first through the front on the pinned
// emulator: SELECT INTERVAL 1 DAY = INTERVAL 1 DAY failed 400 "unsupported
// eq operator for interval value"; read in its engine: INTERVAL -1 SECOND
// was written 0-0 0 0:0:1, and INTERVAL 1500 MILLISECOND failed "unexpected
// interval part". Fixed in the engine CloudBurrow builds (googlesqlite
// patches 0009 and 0010).
func TestBigQueryIntervalComparison(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, tc := range []struct {
		expr string
		want bigquery.Value
	}{
		{"INTERVAL 1 DAY = INTERVAL 1 DAY", true},
		{"INTERVAL 1 DAY = INTERVAL 2 DAY", false},
		{"INTERVAL 1 MONTH = INTERVAL 30 DAY", true},
		{"INTERVAL 1 DAY = INTERVAL 24 HOUR", true},
		{"INTERVAL 1 DAY < INTERVAL 25 HOUR", true},
		{"INTERVAL -1 SECOND < INTERVAL 0 SECOND", true},
		{"INTERVAL 2 HOUR >= INTERVAL 120 MINUTE", true},
		{"CAST(INTERVAL -1 SECOND AS STRING)", "0-0 0 -0:0:1"},
		{"CAST(MAKE_INTERVAL(second => -61) AS STRING)", "0-0 0 -0:1:1"},
		{"CAST(INTERVAL -1 MONTH AS STRING)", "-0-1 0 0:0:0"},
		{"CAST(INTERVAL 1500 MILLISECOND AS STRING)", "0-0 0 0:0:1.5"},
		{"INTERVAL 1 MICROSECOND > INTERVAL 0 SECOND", true},
	} {
		row, err := oneValue(ctx, c, "SELECT "+tc.expr, false)
		if err != nil || len(row) != 1 || row[0] != tc.want {
			t.Errorf("SELECT %s: %v %v, want %v", tc.expr, row, err, tc.want)
		}
	}
	tbl := ds.Table("iv")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "v", Type: bigquery.IntervalFieldType}}}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".iv (id, v) VALUES (1, INTERVAL 1 DAY), (2, INTERVAL 24 HOUR), "+
		"(3, INTERVAL 2 DAY), (4, INTERVAL -1 SECOND)", true); err != nil {
		t.Fatal(err)
	}
	p := bigquery.QueryParameter{Name: "p", Value: &bigquery.IntervalValue{Days: 1}}
	for _, tc := range []struct{ where, want string }{
		{"v = @p", "[[1] [2]]"},
		{"v > @p", "[[3]]"},
		{"v < INTERVAL 0 SECOND", "[[4]]"},
	} {
		q := c.Query("SELECT id FROM " + ds.DatasetID + ".iv WHERE " + tc.where + " ORDER BY id")
		if strings.Contains(tc.where, "@p") {
			q.Parameters = []bigquery.QueryParameter{p}
		}
		it, err := q.Read(ctx)
		if err != nil {
			t.Errorf("WHERE %s: %v", tc.where, err)
			continue
		}
		var rows [][]bigquery.Value
		for {
			var row []bigquery.Value
			if err := it.Next(&row); errors.Is(err, iterator.Done) {
				break
			} else if err != nil {
				t.Fatalf("WHERE %s: %v", tc.where, err)
			}
			rows = append(rows, row)
		}
		if fmt.Sprint(rows) != tc.want {
			t.Errorf("WHERE %s: %v, want %s", tc.where, rows, tc.want)
		}
	}
}

// TestBigQueryRepeatedFieldLeftOutIsAnEmptyArray (#1124): in BigQuery a
// stored array is never NULL: "each array is written as an empty array"
// (array type). So a REPEATED column a streamed row (Inserter.Put of a
// row without it) or a JSON load's record leaves out reads as an empty
// array: not NULL, of length 0, ARRAY_TO_STRING ” , and [] in
// tabledata.list. Measured first through the front on the pinned
// emulator: `tags IS NULL, ARRAY_LENGTH(tags), ARRAY_TO_STRING(tags, ',')`
// of the streamed row read true, NULL, NULL. Fixed in the emulator
// CloudBurrow builds (bigquery-emulator patch 0002).
func TestBigQueryRepeatedFieldLeftOutIsAnEmptyArray(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
	}
	tbl := ds.Table("rep")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Inserter().Put(ctx, []*bigquery.ValuesSaver{
		{Schema: bigquery.Schema{schema[0]}, InsertID: "1", Row: []bigquery.Value{int64(1)}},
		{Schema: schema, InsertID: "2", Row: []bigquery.Value{int64(2), []bigquery.Value{"a", "b"}}},
	}); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	src := bigquery.NewReaderSource(strings.NewReader("{\"id\":3}\n"))
	src.SourceFormat = bigquery.JSON
	job, err := tbl.LoaderFrom(src).Run(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("load: %v %v", err, st.Err())
	}
	got := queryRows(t, ctx, c, "", "", "SELECT id, tags IS NULL, ARRAY_LENGTH(tags), ARRAY_TO_STRING(tags, ',') FROM "+
		ds.DatasetID+".rep")
	if want := []string{"1|false|0|", "2|false|2|a,b", "3|false|0|"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the rows: %v, want %v", got, want)
	}
	it := tbl.Read(ctx)
	var listed []string
	for {
		var row []bigquery.Value
		if err := it.Next(&row); errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			t.Fatalf("tabledata.list: %v", err)
		}
		listed = append(listed, fmt.Sprint(row))
	}
	if want := []string{"[1 []]", "[2 [a b]]", "[3 []]"}; !reflect.DeepEqual(sortedCopy(listed), want) {
		t.Errorf("tabledata.list: %v, want %v", listed, want)
	}
}

// sortedCopy is s sorted.
func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// TestBigQueryGeographyWKT (#1119): BigQuery writes a GEOGRAPHY's WKT with
// no space before a parenthesis, POINT(1 2), LINESTRING(1 2, 3 4),
// GEOMETRYCOLLECTION(POINT(0 0), LINESTRING(1 2, 2 1)), and an empty one as
// GEOMETRYCOLLECTION EMPTY (ST_GEOGFROMTEXT('POINT EMPTY') reads
// GEOMETRYCOLLECTION EMPTY): every example in the geography functions
// reference. So a computed geography, a GEOGRAPHY parameter, a streamed
// and a stored value read so, through jobs.query and tabledata.list.
// Measured first through the front on the pinned emulator: SELECT
// ST_GEOGPOINT(1, 2) read POINT (1 2), and so did a parameter POINT(1 2).
// Fixed in the engine CloudBurrow builds (googlesqlite patch 0012).
func TestBigQueryGeographyWKT(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, tc := range []struct{ expr, want string }{
		{"ST_GEOGPOINT(1, 2)", "POINT(1 2)"},
		{"ST_GEOGFROMTEXT('POINT (1 2)')", "POINT(1 2)"},
		{"ST_ASTEXT(ST_GEOGFROMTEXT('LINESTRING(1 2, 3 4)'))", "LINESTRING(1 2, 3 4)"},
		{"ST_GEOGFROMTEXT('GEOMETRYCOLLECTION(POINT(0 0), LINESTRING(1 2, 2 1))')", "GEOMETRYCOLLECTION(POINT(0 0), LINESTRING(1 2, 2 1))"},
		{"ST_GEOGFROMTEXT('POINT EMPTY')", "GEOMETRYCOLLECTION EMPTY"},
	} {
		row, err := oneValue(ctx, c, "SELECT "+tc.expr, false)
		if err != nil || len(row) != 1 || row[0] != tc.want {
			t.Errorf("SELECT %s: %v %v, want %q", tc.expr, row, err, tc.want)
		}
	}
	row, err := oneValue(ctx, c, "SELECT @g", false, bigquery.QueryParameter{Name: "g",
		Value: &bigquery.QueryParameterValue{Type: bigquery.StandardSQLDataType{TypeKind: "GEOGRAPHY"}, Value: "POINT(1 2)"}})
	if err != nil || len(row) != 1 || row[0] != "POINT(1 2)" {
		t.Errorf("SELECT @g: %v %v, want POINT(1 2)", row, err)
	}
	tbl := ds.Table("geo")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "g", Type: bigquery.GeographyFieldType}}}); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: bigquery.Schema{{Name: "g", Type: bigquery.GeographyFieldType}},
		InsertID: "1", Row: []bigquery.Value{"POINT (3 4)"}}}); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".geo (g) VALUES (ST_GEOGPOINT(5, 6))", true); err != nil {
		t.Fatal(err)
	}
	it := tbl.Read(ctx)
	var listed []string
	for {
		var r []bigquery.Value
		if err := it.Next(&r); errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			t.Fatalf("tabledata.list: %v", err)
		}
		listed = append(listed, fmt.Sprint(r[0]))
	}
	if want := []string{"POINT(3 4)", "POINT(5 6)"}; !reflect.DeepEqual(sortedCopy(listed), want) {
		t.Errorf("tabledata.list: %v, want %v", listed, want)
	}
}
