package bigtablefront

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/civil"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// show is a result value as the tests compare it.
func show(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case civil.Date:
		return x.String()
	case bigtable.Struct:
		var parts []string
		for i := 0; i < x.Len(); i++ {
			var f any
			_ = x.GetByIndex(i, &f)
			parts = append(parts, show(f))
		}
		return "(" + strings.Join(parts, ",") + ")"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return "NULL"
		}
		return show(rv.Elem().Interface())
	case reflect.Slice:
		var parts []string
		for i := 0; i < rv.Len(); i++ {
			parts = append(parts, show(rv.Index(i).Interface()))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case reflect.Map:
		var parts []string
		for _, k := range rv.MapKeys() {
			name := fmt.Sprint(k.Interface())
			if b, err := base64.StdEncoding.DecodeString(name); err == nil {
				name = string(b)
			}
			parts = append(parts, name+":"+show(rv.MapIndex(k).Interface()))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprint(v)
}

// sqlFixture is an instance with the reference's UNPACK example table,
// "report" (https://cloud.google.com/bigtable/docs/reference/sql/table-functions-built-in#unpack),
// and a query runner answering each row as its columns' shown values.
func sqlFixture(t *testing.T) (func(sql string, params map[string]bigtable.SQLType, bind map[string]any) [][]string, func(sql string) error) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	opt := option.WithGRPCConn(dial(t, addr))
	ac, err := bigtable.NewAdminClient(ctx, "p", "sql-inst", opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "sql-inst", opt)
	if err != nil {
		t.Fatal(err)
	}
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "sql-inst", DisplayName: "sql instance", ClusterId: "sql-cluster", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatal(err)
	}
	seedReport(ctx, t, ac, c)
	seedCounters(ctx, t, ac, c)
	run := func(sql string, params map[string]bigtable.SQLType, bind map[string]any) [][]string {
		t.Helper()
		ps, err := c.PrepareStatement(ctx, sql, params)
		if err != nil {
			t.Fatalf("PrepareStatement(%s): %v", sql, err)
		}
		bs, err := ps.Bind(bind)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var out [][]string
		if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool {
			var row []string
			for i := range r.Metadata.Columns {
				var v any
				if err := r.GetByIndex(i, &v); err != nil {
					t.Fatalf("%s: GetByIndex(%d): %v", sql, i, err)
				}
				row = append(row, show(v))
			}
			out = append(out, row)
			return true
		}); err != nil {
			t.Fatalf("Execute(%s): %v", sql, err)
		}
		return out
	}
	// prepare prepares and runs sql, answering its error.
	prepare := func(sql string) error {
		ps, err := c.PrepareStatement(ctx, sql, nil)
		if err != nil {
			return err
		}
		bs, err := ps.Bind(nil)
		if err != nil {
			return err
		}
		return bs.Execute(ctx, func(bigtable.ResultRow) bool { return true })
	}
	return run, prepare
}

// seedReport writes the reference's UNPACK example: Dept1 and Dept2, each
// with expenses and sales at four timestamps on 2025-04-01.
func seedReport(ctx context.Context, t *testing.T, ac *bigtable.AdminClient, c *bigtable.Client) {
	t.Helper()
	if err := ac.CreateTable(ctx, "report"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "report", "report"); err != nil {
		t.Fatal(err)
	}
	at := func(h int) bigtable.Timestamp {
		return bigtable.Time(time.Date(2025, 4, 1, h, 0, 0, 0, time.UTC))
	}
	for key, cells := range map[string][][3]any{
		"Dept1": {{"expenses", 21, "3"}, {"expenses", 18, "2"}, {"sales", 22, "12"}, {"sales", 20, "10"}},
		"Dept2": {{"expenses", 19, "2"}, {"expenses", 18, "1"}, {"sales", 23, "9"}, {"sales", 19, "5"}},
	} {
		m := bigtable.NewMutation()
		for _, cl := range cells {
			m.Set("report", cl[0].(string), at(cl[1].(int)), []byte(cl[2].(string)))
		}
		if err := c.Open("report").Apply(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}
}

// seedCounters writes an aggregate family, a Sum of INT64, the reference's
// counters (https://cloud.google.com/bigtable/docs/create-update-counters).
func seedCounters(ctx context.Context, t *testing.T, ac *bigtable.AdminClient, c *bigtable.Client) {
	t.Helper()
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "counters", ColumnFamilies: map[string]bigtable.Family{
		"c": {ValueType: bigtable.AggregateType{Input: bigtable.Int64Type{}, Aggregator: bigtable.SumAggregator{}}},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{3, 4} {
		m := bigtable.NewMutation()
		m.AddIntToCell("c", "views", bigtable.Timestamp(0), n)
		if err := c.Open("counters").Apply(ctx, "page#1", m); err != nil {
			t.Fatal(err)
		}
	}
}

func joined(rows [][]string) string {
	var out []string
	for _, r := range rows {
		out = append(out, strings.Join(r, " "))
	}
	return strings.Join(out, "\n")
}

func expectRows(t *testing.T, what string, got [][]string, want string) {
	t.Helper()
	if g := joined(got); g != want {
		t.Errorf("%s =\n%s\nwant\n%s", what, g, want)
	}
}

// TestSQLUnpack runs the reference's UNPACK examples
// (https://cloud.google.com/bigtable/docs/reference/sql/table-functions-built-in#unpack).
func TestSQLUnpack(t *testing.T) {
	run, prepare := sqlFixture(t)
	expectRows(t, "UNPACK report", run("SELECT * FROM UNPACK((SELECT _key, report FROM report(with_history => true)))", nil, nil),
		`Dept1 {sales:12} 2025-04-01T22:00:00Z
Dept1 {expenses:3} 2025-04-01T21:00:00Z
Dept1 {sales:10} 2025-04-01T20:00:00Z
Dept1 {expenses:2} 2025-04-01T18:00:00Z
Dept2 {sales:9} 2025-04-01T23:00:00Z
Dept2 {expenses:2,sales:5} 2025-04-01T19:00:00Z
Dept2 {expenses:1} 2025-04-01T18:00:00Z`)
	expectRows(t, "UNPACK a qualifier", run("SELECT _key, _timestamp, sales FROM UNPACK((SELECT _key, report['sales'] AS sales FROM report(with_history => true)))", nil, nil),
		`Dept1 2025-04-01T22:00:00Z 12
Dept1 2025-04-01T20:00:00Z 10
Dept2 2025-04-01T23:00:00Z 9
Dept2 2025-04-01T19:00:00Z 5`)
	expectRows(t, "UNPACK the family, select a qualifier", run("SELECT _key, _timestamp, report['sales'] AS sales FROM UNPACK((SELECT _key, report FROM report(with_history => true)))", nil, nil),
		`Dept1 2025-04-01T22:00:00Z 12
Dept1 2025-04-01T21:00:00Z NULL
Dept1 2025-04-01T20:00:00Z 10
Dept1 2025-04-01T18:00:00Z NULL
Dept2 2025-04-01T23:00:00Z 9
Dept2 2025-04-01T19:00:00Z 5
Dept2 2025-04-01T18:00:00Z NULL`)
	expectRows(t, "no timestamps", run("SELECT _key, _timestamp FROM UNPACK((SELECT _key FROM report(with_history => true)))", nil, nil),
		"Dept1 NULL\nDept2 NULL")
	expectRows(t, "UNPACK(TABLE ...)", run("SELECT _key, _timestamp FROM UNPACK(TABLE report(with_history => true, latest_n => 1)) WHERE _key = @k", map[string]bigtable.SQLType{"k": bigtable.BytesSQLType{}}, map[string]any{"k": []byte("Dept1")}),
		"Dept1 2025-04-01T22:00:00Z\nDept1 2025-04-01T21:00:00Z")
	// The reference's time-series pattern: a parameter inside, an aggregate
	// over _timestamp outside.
	expectRows(t, "per hour", run("SELECT FORMAT_TIMESTAMP('%H', _timestamp) AS h, COUNT(sales) AS n FROM UNPACK((SELECT report['sales'] AS sales FROM report(with_history => true, after => @t) WHERE _key = @k)) GROUP BY h",
		map[string]bigtable.SQLType{"k": bigtable.BytesSQLType{}, "t": bigtable.TimestampSQLType{}},
		map[string]any{"k": []byte("Dept2"), "t": time.Date(2025, 4, 1, 18, 30, 0, 0, time.UTC)}),
		"23 1\n19 1")
	expectRows(t, "before_or_equal", run("SELECT report['sales'] FROM report(with_history => true, before_or_equal => TIMESTAMP '2025-04-01 20:00:00+00') WHERE _key = 'Dept1'", nil, nil),
		"[(2025-04-01T20:00:00Z,10)]")
	for sql, want := range map[string]codes.Code{
		"SELECT _timestamp FROM report": codes.InvalidArgument,
		"SELECT * FROM UNPACK(report)":  codes.InvalidArgument,
		"SELECT * FROM report(with_history => true, as_of => TIMESTAMP '2025-04-01')": codes.InvalidArgument,
	} {
		if err := prepare(sql); status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
}

// TestSQLScalars runs the reference's examples of the scalar functions
// #1704 adds (string_functions, date_functions, mathematical_functions).
func TestSQLScalars(t *testing.T) {
	run, prepare := sqlFixture(t)
	for sql, want := range map[string]string{
		"SELECT SOUNDEX('Ashcraft'), SOUNDEX('Robert'), SOUNDEX('Rupert'), SOUNDEX('Tymczak'), SOUNDEX('Pfister'), SOUNDEX('é')":                                                     "A261 R163 R163 T522 P236 ",
		"SELECT FORMAT('%d', 10), FORMAT('|%10d|', 11), FORMAT('+%010d+', 12), FORMAT(\"%'d\", 123456789)":                                                                           "10 |        11| +0000000012+ 123,456,789",
		"SELECT FORMAT('-%s-', 'abcd efg'), FORMAT('%f %E', 1.1, 2.2), FORMAT('%t', DATE '2015-09-01')":                                                                              "-abcd efg- 1.100000 2.200000E+00 2015-09-01",
		"SELECT FORMAT('%t', TIMESTAMP '2015-09-01 12:34:56 America/Los_Angeles'), FORMAT('00-%t-00', NULL), FORMAT('%i', NULL)":                                                     "2015-09-01 19:34:56+00 00-NULL-00 NULL",
		"SELECT FORMAT('%g %g %T %T', 392.65, 39265000.0, 'x', b'ab'), FORMAT(\"%'x\", 12345678), FORMAT(\"%'o\", 55555)":                                                            "392.65 3.9265e+07 \"x\" b\"ab\" bc:614e 15,4403",
		"SELECT FORMAT('%.*f|%-5s|', 2, 3.14159, 'ab'), FORMAT('%t', [1, 2])":                                                                                                        "3.14|ab   | [1, 2]",
		"SELECT ASCII('abcd'), ASCII(''), CHR(65), CHR(0), UNICODE('âbcd'), INITCAP('Hello World-everyone!')":                                                                        "97 0 A  226 Hello World-Everyone!",
		"SELECT INITCAP('Apples1oranges2pears', '12'), INSTR('banana', 'an', 1, 2), INSTR('banana', 'an', -1, 1), TRANSLATE('This is a cookie', 'sco', 'zku')":                       "Apples1Oranges2Pears 4 4 Thiz iz a kuukie",
		"SELECT TO_VECTOR32(b'\\x3f\\xc0\\x00\\x00\\x42\\xc8\\x00\\x00'), TO_VECTOR64(b'\\x40\\x5e\\xdc\\xcc\\xcc\\xcc\\xcc\\xcd')":                                                  "[1.5,100] [123.45]",
		"SELECT ROUND(COSINE_DISTANCE([1.0, 2.0], [3.0, 4.0]), 6), EUCLIDEAN_DISTANCE([1.0, 2.0], [3.0, 4.0])":                                                                       "0.01613 2.8284271247461903",
		"SELECT DATE(2016, 12, 25), DATE(TIMESTAMP '2016-12-25 05:30:00+07', 'America/Los_Angeles'), DATE_ADD(DATE '2008-12-25', INTERVAL 5 DAY)":                                    "2016-12-25 2016-12-24 2008-12-30",
		"SELECT DATE_SUB(DATE '2008-12-25', INTERVAL 5 DAY), DATE_ADD(DATE '2008-01-31', INTERVAL 1 MONTH), DATE_DIFF(DATE '2010-07-07', DATE '2008-12-25', DAY)":                    "2008-12-20 2008-02-29 559",
		"SELECT DATE_DIFF(DATE '2023-11-30', DATE '2020-01-01', YEAR), DATE_DIFF(DATE '2020-01-01', DATE '2023-11-30', YEAR), DATE_DIFF(DATE '2017-10-15', DATE '2017-10-14', WEEK)": "3 -3 1",
		"SELECT DATE_TRUNC(DATE '2008-12-25', MONTH), DATE_TRUNC(DATE '2017-11-05', WEEK(MONDAY)), DATE_TRUNC(DATE '2015-06-15', ISOYEAR)":                                           "2008-12-01 2017-10-30 2014-12-29",
		"SELECT LAST_DAY(DATE '2008-11-25'), LAST_DAY(DATE '2008-11-25', YEAR), LAST_DAY(DATE '2008-11-10', WEEK(SUNDAY)), LAST_DAY(DATE '2008-11-10', WEEK(MONDAY))":                "2008-11-30 2008-12-31 2008-11-15 2008-11-16",
		"SELECT FORMAT_DATE('%b-%d-%Y', DATE '2008-12-25'), PARSE_DATE('%A %b %e %Y', 'Thursday Dec 25 2008'), PARSE_DATE('%F', '2000-12-30')":                                       "Dec-25-2008 2008-12-25 2000-12-30",
		"SELECT UNIX_DATE(DATE '2008-12-25'), DATE_FROM_UNIX_DATE(14238), PARSE_TIMESTAMP('%a %b %e %I:%M:%S %Y', 'Thu Dec 25 07:30:00 2008')":                                       "14238 2008-12-25 2008-12-25T07:30:00Z",
		"SELECT SAFE_ADD(9223372036854775807, 1), SAFE_SUBTRACT(5, 4), SAFE_MULTIPLY(20, 4), SAFE_NEGATE(1), ROUND(ATAN2(1, 1) * 4, 6), ACOSH(1.0)":                                  "NULL 1 80 -1 3.141593 0",
		"SELECT TIMESTAMP_TRUNC(TIMESTAMP '2017-11-06 00:00:00+00', ISOWEEK), TIMESTAMP_TRUNC(TIMESTAMP '2017-11-06 00:00:00+00', WEEK(MONDAY))":                                     "2017-11-06T00:00:00Z 2017-11-06T00:00:00Z",
	} {
		got := run(sql, nil, nil)
		if len(got) != 1 || strings.Join(got[0], " ") != want {
			t.Errorf("%s =\n%v\nwant\n%s", sql, got, want)
		}
	}
	if got := run("SELECT CURRENT_DATE = CURRENT_DATE(), RAND() < 1.0", nil, nil); joined(got) != "true true" {
		t.Errorf("CURRENT_DATE, RAND = %v", got)
	}
	for sql, want := range map[string]codes.Code{
		"SELECT ACOS(2)":         codes.InvalidArgument,
		"SELECT FORMAT('%s', 1)": codes.InvalidArgument,
		"SELECT COSINE_DISTANCE([1.0], [CAST(1 AS FLOAT32)])": codes.InvalidArgument,
		"SELECT PARSE_DATE('%Q', '1')":                        codes.Unimplemented,
	} {
		if err := prepare(sql); status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
}

// TestSQLAggregatesAndStructs runs the statistical and approximate
// aggregates and ARRAY_CONCAT_AGG over the report table, and the STRUCT
// constructors (data-types#constructing_a_struct).
func TestSQLAggregatesAndStructs(t *testing.T) {
	run, prepare := sqlFixture(t)
	for sql, want := range map[string]string{
		"SELECT ROUND(STDDEV(CAST(CAST(report['sales'] AS STRING) AS INT64)), 6), ROUND(VAR_POP(CAST(CAST(report['sales'] AS STRING) AS INT64)), 6) FROM report": "2.12132 2.25",
		"SELECT STDDEV_SAMP(1), VARIANCE(1), STDDEV_POP(1), COVAR_POP(1, 2), COVAR_SAMP(1, 2), CORR(1, 2)":                                                       "NULL NULL 0 0 NULL NULL",
		"SELECT APPROX_COUNT_DISTINCT(report['sales']), APPROX_QUANTILES(CAST(CAST(report['sales'] AS STRING) AS INT64), 2) FROM report":                         "2 [9,9,12]",
		"SELECT APPROX_TOP_COUNT(_key, 1), APPROX_TOP_SUM(_key, 2, 5), ARRAY_CONCAT_AGG(MAP_KEYS(report)) FROM report":                                           "[(Dept1,1)] [(Dept1,2),(Dept2,2)] [expenses,sales,expenses,sales]",
		// The official Go client cannot read a STRUCT with an anonymous field
		// ("StructSQLType field at index 0 must have a name"), so those are
		// read through FORMAT.
		"SELECT STRUCT(1 AS a, 'abc' AS b).b, FORMAT('%t %T', STRUCT(1, 2, 3), (1, 'x')), STRUCT<x INT64, y FLOAT64, d DATE>(1, 2, '2011-05-05'), STRUCT(1 AS a, 'abc' AS b)": "abc (1, 2, 3) (1, \"x\") (1,2,2011-05-05) (1,abc)",
		"SELECT STRUCT(_key, 5 AS n).`_key` FROM report WHERE _key = 'Dept1'":                                                                                                 "Dept1",
		"SELECT c['views'], c['views'] > 5 FROM counters":                                                                                                                     "7 true",
		"SELECT ROUND(COSINE_DISTANCE([(1, 1.0), (2, 2.0)], [(2, 4.0), (1, 3.0)]), 6)":                                                                                        "0.01613",
	} {
		got := run(sql, nil, nil)
		if len(got) != 1 || strings.Join(got[0], " ") != want {
			t.Errorf("%s =\n%v\nwant\n%s", sql, got, want)
		}
	}
	for sql, want := range map[string]codes.Code{
		"SELECT APPROX_QUANTILES(_key, _key) FROM report": codes.InvalidArgument,
		"SELECT STRUCT<x INT64>(5 AS x)":                  codes.InvalidArgument,
		"SELECT STRUCT<x INT64>('a')":                     codes.InvalidArgument,
		"SELECT MIN_BY(_key, _key) FROM report":           codes.Unimplemented,
	} {
		if err := prepare(sql); status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
}

// TestSQLAggregateFamily reads an aggregate family's typed values: a Sum
// of INT64 is MAP<BYTES, INT64>, with history its cells' INT64 values.
func TestSQLAggregateFamily(t *testing.T) {
	run, _ := sqlFixture(t)
	expectRows(t, "counter", run("SELECT c['views'], c['views'] > 5, c FROM counters", nil, nil), "7 true {views:7}")
	expectRows(t, "counter history", run("SELECT c['views'] FROM counters(with_history => true)", nil, nil), "[(1970-01-01T00:00:00Z,7)]")
	expectRows(t, "unpacked counter", run("SELECT _timestamp, v FROM UNPACK((SELECT c['views'] AS v FROM counters(with_history => true)))", nil, nil), "1970-01-01T00:00:00Z 7")
}

// TestSQLWindowFunctions runs window function calls (window-function-calls,
// numbering_functions, navigation_functions) over the reference's report
// table's sales, unpacked: Dept1 12 and 10, Dept2 9 and 5.
func TestSQLWindowFunctions(t *testing.T) {
	run, prepare := sqlFixture(t)
	const v = "CAST(CAST(s AS STRING) AS INT64)"
	from := " FROM UNPACK((SELECT _key, report['sales'] AS s FROM report(with_history => true)))"
	q := func(sql string) string { return strings.ReplaceAll(sql, "$V", v) }
	expectRows(t, "windows", run(q("SELECT _key, $V AS v, ROW_NUMBER() OVER (PARTITION BY _key ORDER BY $V) AS rn, RANK() OVER (ORDER BY $V DESC) AS r, "+
		"SUM($V) OVER (PARTITION BY _key) AS tot, SUM($V) OVER (ORDER BY $V ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) AS mov, "+
		"LAG($V) OVER (PARTITION BY _key ORDER BY $V) AS prev, FIRST_VALUE($V) OVER w AS top, NTILE(2) OVER (ORDER BY $V) AS tile, "+
		"CUME_DIST() OVER (ORDER BY $V) AS cd, PERCENTILE_CONT($V, 0.5) OVER () AS med, "+
		"COUNT(*) OVER (ORDER BY $V RANGE BETWEEN 2 PRECEDING AND 2 FOLLOWING) AS near"+from+
		" WINDOW w AS (PARTITION BY _key ORDER BY $V DESC)"), nil, nil),
		`Dept1 12 2 1 22 22 10 12 2 1 9.5 2
Dept1 10 1 2 22 19 NULL 12 2 0.75 9.5 3
Dept2 9 2 3 14 14 5 9 1 0.5 9.5 2
Dept2 5 1 4 14 5 NULL 9 1 0.25 9.5 1`)
	expectRows(t, "after GROUP BY", run(q("SELECT _key, SUM($V) AS t, RANK() OVER (ORDER BY SUM($V) DESC) AS r, LEAD(_key, 1, 'none') OVER (ORDER BY _key) AS nxt"+from+" GROUP BY _key"), nil, nil),
		"Dept1 22 1 Dept2\nDept2 14 2 none")
	expectRows(t, "frames", run(q("SELECT $V, LAST_VALUE($V) OVER (ORDER BY $V ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING), NTH_VALUE($V, 2) OVER (ORDER BY $V ROWS UNBOUNDED PRECEDING), "+
		"DENSE_RANK() OVER (ORDER BY _key), PERCENT_RANK() OVER (ORDER BY $V), PERCENTILE_DISC($V, 0.5) OVER (PARTITION BY _key)"+from), nil, nil),
		`12 12 9 1 1 10
10 12 9 1 0.6666666666666666 10
9 12 9 2 0.3333333333333333 5
5 12 NULL 2 0 5`)
	for sql, want := range map[string]codes.Code{
		"SELECT RANK() OVER () FROM report":                                                  codes.InvalidArgument,
		"SELECT RANK() FROM report":                                                          codes.InvalidArgument,
		"SELECT _key FROM report WHERE ROW_NUMBER() OVER () = 1":                             codes.InvalidArgument,
		"SELECT SUM(COUNT(*) OVER ()) FROM report":                                           codes.InvalidArgument,
		"SELECT RANK() OVER (ORDER BY _key ROWS 1 PRECEDING) FROM report":                    codes.InvalidArgument,
		"SELECT COUNT(DISTINCT _key) OVER () FROM report":                                    codes.InvalidArgument,
		"SELECT ROW_NUMBER() OVER w FROM report":                                             codes.InvalidArgument,
		"SELECT ROW_NUMBER() OVER (w ORDER BY _key) FROM report WINDOW w AS (ORDER BY _key)": codes.InvalidArgument,
	} {
		if err := prepare(sql); status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
}

// TestSQLArrayAndCodePointFunctions runs the reference's examples of the
// array and code point functions #1704 adds.
func TestSQLArrayAndCodePointFunctions(t *testing.T) {
	run, _ := sqlFixture(t)
	for sql, want := range map[string]string{
		"SELECT ARRAY_INCLUDES_ALL([1, 2, 3, 4, 5], [3, 4, 7]), ARRAY_INCLUDES_ALL([1, 2, 3, 4, 5], [4, 3]), ARRAY_INCLUDES_ANY([1, 2, 3, 4, 5], [3, 4, 7]), ARRAY_INCLUDES_ANY([1, 2, 3, 4, 5], [9, 7])":                                                                                                        "false true true false",
		"SELECT ARRAY_IS_DISTINCT([1, 2, 3]), ARRAY_IS_DISTINCT([1, 1, 1]), ARRAY_LAST_N([1, 2, 3, 4, 5], 2), ARRAY_LAST_N([1, 2], 5)":                                                                                                                                                                           "true false [4,5] [1,2]",
		"SELECT ARRAY_SLICE(['a', 'b', 'c', 'd', 'e'], 1, 3), ARRAY_SLICE(['a', 'b', 'c', 'd', 'e'], -1, 3), ARRAY_SLICE(['a', 'b', 'c', 'd', 'e'], 1, -3), ARRAY_SLICE(['a', 'b', 'c', 'd', 'e'], -20, 2), ARRAY_SLICE(['a', 'b', 'c', 'd', 'e'], 1, 30)":                                                       "[b,c,d] [] [b,c] [a,b,c] [b,c,d,e]",
		"SELECT GENERATE_ARRAY(1, 5), GENERATE_ARRAY(0, 10, 3), GENERATE_ARRAY(10, 0, -3), GENERATE_ARRAY(0, 1, 0.25), GENERATE_ARRAY(4, 4), GENERATE_ARRAY(4, 3)":                                                                                                                                               "[1,2,3,4,5] [0,3,6,9] [10,7,4,1] [0,0.25,0.5,0.75,1] [4] []",
		"SELECT GENERATE_DATE_ARRAY(DATE '2016-10-05', DATE '2016-10-08'), GENERATE_DATE_ARRAY(DATE '2016-01-01', DATE '2016-12-31', INTERVAL 2 MONTH)":                                                                                                                                                          "[2016-10-05,2016-10-06,2016-10-07,2016-10-08] [2016-01-01,2016-03-01,2016-05-01,2016-07-01,2016-09-01,2016-11-01]",
		"SELECT GENERATE_TIMESTAMP_ARRAY(TIMESTAMP '2016-10-05 00:00:00+00', TIMESTAMP '2016-10-07 00:00:00+00', INTERVAL 1 DAY)":                                                                                                                                                                                "[2016-10-05T00:00:00Z,2016-10-06T00:00:00Z,2016-10-07T00:00:00Z]",
		"SELECT TO_CODE_POINTS('foo'), TO_CODE_POINTS(b'\\x66\\x6f'), CODE_POINTS_TO_STRING([65, 255, 513, 1024]), CODE_POINTS_TO_BYTES([65, 98, 67, 100]), OCTET_LENGTH('абв'), CHARACTER_LENGTH('абв')":                                                                                                        "[102,111,111] [102,111] AÿȁЀ AbCd 6 3",
		`SELECT JSON_QUERY('{"class": {"students": [{"name": "Jane"}]}}', '$'), JSON_QUERY('{"class": {"students": [{"name": "Jane"}]}}', '$.class.students[0]'), JSON_QUERY('null', '$'), JSON_EXTRACT('{"a.b": {"c": 1}}', "$['a.b'].c")`:                                                                      `{"class":{"students":[{"name":"Jane"}]}} {"name":"Jane"} NULL 1`,
		`SELECT JSON_QUERY_ARRAY('[1, 2, 3]'), JSON_QUERY_ARRAY('{"a.b": {"c": ["world"]}}', '$."a.b".c'), TO_JSON_STRING(STRUCT(1 AS id, [10,20] AS coordinates)), TO_JSON_STRING(9007199254740993), TO_JSON_STRING(b'Google'), TO_JSON_STRING(TIMESTAMP '2017-03-06 12:34:56.789012+00'), TO_JSON_STRING(1.0)`: `[1,2,3] ["world"] {"id":1,"coordinates":[10,20]} "9007199254740993" "R29vZ2xl" "2017-03-06T12:34:56.789012Z" 1`,
		"SELECT REGEXP_INSTR('ab@cd-ef', '@[^-]*'), REGEXP_INSTR('abc-ef', '@[^-]*'), REGEXP_INSTR('a@cd-ef b@cd-ef', '@[^-]*', 3), REGEXP_INSTR('a@cd-ef b@cd-ef c@cd-ef', '@[^-]*', 1, 3), REGEXP_INSTR('a@cd-ef', '@[^-]*', 1, 1, 1)":                                                                         "3 0 10 18 5",
		"SELECT IFERROR(1, 0), IFERROR(1 / 0, -1), ISERROR(1 / 0), ISERROR(1), NULLIFERROR(1 / 0), NULLIFERROR(2)":                                                                                                                                                                                               "1 -1 true false NULL 2",
		"SELECT TO_BASE32(b'abcde\\xFF'), FROM_BASE32('MFRGGZDF74======')": "MFRGGZDF74====== abcde\xff",
	} {
		got := run(sql, nil, nil)
		if len(got) != 1 || strings.Join(got[0], " ") != want {
			t.Errorf("%s =\n%v\nwant\n%s", sql, got, want)
		}
	}
}

// TestSQLLambdasAndNormalize runs array_functions' examples of the lambda
// functions and string_functions' of NORMALIZE and NORMALIZE_AND_CASEFOLD;
// GROUPING, MIN_BY and MAX_BY, which GoogleSQL for Bigtable does not list,
// are refused saying so.
func TestSQLLambdasAndNormalize(t *testing.T) {
	run, prepare := sqlFixture(t)
	for sql, want := range map[string]string{
		"SELECT ARRAY_FILTER([1 ,2, 3], e -> e > 1) AS a1, ARRAY_FILTER([0, 2, 3], (e, i) -> e > i) AS a2":                                          "[2,3] [2,3]",
		"SELECT ARRAY_TRANSFORM([1, 4, 3], e -> e + 1) AS a1, ARRAY_TRANSFORM([1, 4, 3], (e, i) -> e + i) AS a2":                                    "[2,5,4] [1,5,5]",
		"SELECT ARRAY_OFFSET([1, 4, 4, 4, 6], 4), ARRAY_OFFSET([1, 4, 4, 4, 6], 4, 'FIRST'), ARRAY_OFFSET([1, 4, 4, 4, 6], 4, 'LAST')":              "1 1 3",
		"SELECT ARRAY_OFFSET([1, 4, 4, 4, 6], e -> e = 4, 'LAST'), ARRAY_OFFSET([1, 4, 4, 4, 6], e -> e > 2 AND e < 5, 'LAST')":                     "3 3",
		"SELECT ARRAY_OFFSET([1, 4, 4, 4, 6], 5), ARRAY_OFFSET([1, 4, 4, 4, 6], e -> e > 7), ARRAY_OFFSET([1, 4, 6], 4, LAST)":                      "NULL NULL 1",
		"SELECT ARRAY_OFFSETS([1, 4, 4, 4, 6, 4], 4), ARRAY_OFFSETS([1, 4, 7, 3, 6, 4], e -> e > 2 AND e < 5)":                                      "[1,2,3,5] [1,3,5]",
		"SELECT ARRAY_OFFSETS([1, 4, 4, 4, 6], 5), ARRAY_OFFSETS([1, 4, 4, 4, 6], e -> e > 7), ARRAY_FILTER(CAST(NULL AS ARRAY<INT64>), e -> true)": "[] [] NULL",
		// a lambda's alias shadows a column; its body reads the row's columns.
		"SELECT ARRAY_TRANSFORM(['a', 'b'], _key -> CONCAT(_key, '!')), ARRAY_FILTER([_key, 'x'], e -> e = _key) FROM report WHERE _key = 'Dept1'":                          "[a!,b!] [Dept1]",
		"SELECT ARRAY_TRANSFORM([STRUCT(1 AS a), STRUCT(2 AS a)], s -> s.a * 10), ARRAY_TRANSFORM([[1, 2], [3]], x -> ARRAY_LENGTH(ARRAY_FILTER(x, y -> y > 1)))":           "[10,20] [1,1]",
		"SELECT NORMALIZE('\\u00ea') = NORMALIZE('\\u0065\\u0302'), NORMALIZE('Raha\\u2004Mahan', NFKC), NORMALIZE('Raha Mahan', NFKC)":                                     "true Raha Mahan Raha Mahan",
		"SELECT NORMALIZE('The red barn') = NORMALIZE('The Red Barn'), NORMALIZE_AND_CASEFOLD('The red barn') = NORMALIZE_AND_CASEFOLD('The Red Barn')":                     "false true",
		"SELECT NORMALIZE_AND_CASEFOLD('\\u2168', NFD) = NORMALIZE_AND_CASEFOLD('IX', NFD), NORMALIZE_AND_CASEFOLD('\\u2168', NFC) = NORMALIZE_AND_CASEFOLD('IX', NFC)":     "false false",
		"SELECT NORMALIZE_AND_CASEFOLD('\\u2168', NFKD) = NORMALIZE_AND_CASEFOLD('IX', NFKD), NORMALIZE_AND_CASEFOLD('\\u2168', NFKC) = NORMALIZE_AND_CASEFOLD('IX', NFKC)": "true true",
		"SELECT NORMALIZE('\\u0041\\u030A', NFD) = NORMALIZE('\\u00C5', NFD), NORMALIZE_AND_CASEFOLD('\\u0041\\u030A', NFKC) = NORMALIZE_AND_CASEFOLD('\\u00C5', NFKC)":     "true true",
	} {
		got := run(sql, nil, nil)
		if len(got) != 1 || strings.Join(got[0], " ") != want {
			t.Errorf("%s =\n%v\nwant\n%s", sql, got, want)
		}
	}
	for sql, want := range map[string]codes.Code{
		"SELECT ARRAY_FILTER([1, 2], e -> e + 1)":               codes.InvalidArgument,
		"SELECT ARRAY_OFFSET([1, 2], (e, i) -> e > i)":          codes.InvalidArgument,
		"SELECT ARRAY_FILTER([1, 2], (e, e) -> true)":           codes.InvalidArgument,
		"SELECT ARRAY_LENGTH(e -> e)":                           codes.InvalidArgument,
		"SELECT ARRAY_OFFSET([1, 2], 'a')":                      codes.InvalidArgument,
		"SELECT NORMALIZE('a', 'NFC')":                          codes.InvalidArgument,
		"SELECT NORMALIZE('a', NFX)":                            codes.InvalidArgument,
		"SELECT ARRAY_FILTER([1, 2], e -> COUNT(e) > 0)":        codes.InvalidArgument,
		"SELECT MIN_BY(_key, _key) FROM report":                 codes.Unimplemented,
		"SELECT MAX_BY(_key, _key) FROM report":                 codes.Unimplemented,
		"SELECT _key, GROUPING(_key) FROM report GROUP BY _key": codes.Unimplemented,
		"SELECT ST_REGIONSTATS(ST_GEOGPOINT(1, 2), 'x')":        codes.Unimplemented,
	} {
		err := prepare(sql)
		if status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
		if strings.Contains(sql, "_BY") && !strings.Contains(fmt.Sprint(err), "not in GoogleSQL for Bigtable") {
			t.Errorf("%s = %v, want it to say it is not in GoogleSQL for Bigtable", sql, err)
		}
	}
	if err := prepare("SELECT ARRAY_OFFSET([1], 1, 'MIDDLE')"); !strings.Contains(fmt.Sprint(err), "'FIRST' or 'LAST'") {
		t.Errorf("ARRAY_OFFSET 'MIDDLE' = %v", err)
	}
}
