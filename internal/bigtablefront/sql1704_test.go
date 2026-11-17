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
