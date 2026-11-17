//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/civil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBigtableSQLUnpackFunctionsAndAggregates (#1704) runs, through the
// official client's PrepareStatement, Bind and Execute, the parts of
// GoogleSQL for Bigtable #1511 left UNIMPLEMENTED, each against the
// reference's own examples: UNPACK and _timestamp over the reference's
// report table (table-functions-built-in#unpack), the temporal filter
// before_or_equal; FORMAT, SOUNDEX and the other string functions, the date
// functions and PARSE_TIMESTAMP, the vector functions, the trigonometric
// and SAFE_ functions; the statistical and approximate aggregates and
// ARRAY_CONCAT_AGG; STRUCT constructors; window functions; and an aggregate
// family's typed values; the lambda array functions (ARRAY_FILTER,
// ARRAY_TRANSFORM, ARRAY_OFFSET, ARRAY_OFFSETS) and NORMALIZE and
// NORMALIZE_AND_CASEFOLD.
//
// covers: google.bigtable.v2.Bigtable/PrepareQuery, google.bigtable.v2.Bigtable/ExecuteQuery
func TestBigtableSQLUnpackFunctionsAndAggregates(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // asks for the Bigtable endpoint first
	ctx := h.Context()
	const id = "cb-inst-sql1704"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "SQL 1704",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	c, err := bigtable.NewClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// The reference's UNPACK table: Dept1 and Dept2, expenses and sales.
	if err := ac.CreateTable(ctx, "report"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "report", "report"); err != nil {
		t.Fatal(err)
	}
	at := func(hr int) bigtable.Timestamp { return bigtable.Time(time.Date(2025, 4, 1, hr, 0, 0, 0, time.UTC)) }
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
	// An aggregate family, a Sum of INT64 (create-update-counters).
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

	run := func(sql string, params map[string]bigtable.SQLType, bind map[string]any) string {
		t.Helper()
		ps, err := c.PrepareStatement(ctx, sql, params)
		if err != nil {
			t.Fatalf("PrepareStatement(%s): %v", sql, err)
		}
		bs, err := ps.Bind(bind)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var rows []string
		if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool {
			var cols []string
			for i := range r.Metadata.Columns {
				var v any
				if err := r.GetByIndex(i, &v); err != nil {
					t.Fatalf("%s: GetByIndex(%d): %v", sql, i, err)
				}
				cols = append(cols, sqlShow1704(v))
			}
			rows = append(rows, strings.Join(cols, " "))
			return true
		}); err != nil {
			t.Fatalf("Execute(%s): %v", sql, err)
		}
		return strings.Join(rows, "\n")
	}
	expect := func(sql, want string) {
		t.Helper()
		if got := run(sql, nil, nil); got != want {
			t.Errorf("%s =\n%s\nwant\n%s", sql, got, want)
		}
	}

	// UNPACK, the reference's examples.
	expect("SELECT * FROM UNPACK((SELECT _key, report FROM report(with_history => true)))",
		`Dept1 {sales:12} 2025-04-01T22:00:00Z
Dept1 {expenses:3} 2025-04-01T21:00:00Z
Dept1 {sales:10} 2025-04-01T20:00:00Z
Dept1 {expenses:2} 2025-04-01T18:00:00Z
Dept2 {sales:9} 2025-04-01T23:00:00Z
Dept2 {expenses:2,sales:5} 2025-04-01T19:00:00Z
Dept2 {expenses:1} 2025-04-01T18:00:00Z`)
	expect("SELECT _key, _timestamp, report['sales'] AS sales FROM UNPACK((SELECT _key, report FROM report(with_history => true)))",
		`Dept1 2025-04-01T22:00:00Z 12
Dept1 2025-04-01T21:00:00Z NULL
Dept1 2025-04-01T20:00:00Z 10
Dept1 2025-04-01T18:00:00Z NULL
Dept2 2025-04-01T23:00:00Z 9
Dept2 2025-04-01T19:00:00Z 5
Dept2 2025-04-01T18:00:00Z NULL`)
	expect("SELECT _key, _timestamp FROM UNPACK((SELECT _key FROM report(with_history => true)))", "Dept1 NULL\nDept2 NULL")
	expect("SELECT _key, _timestamp FROM UNPACK(TABLE report(with_history => true, latest_n => 1)) WHERE _key = 'Dept1'",
		"Dept1 2025-04-01T22:00:00Z\nDept1 2025-04-01T21:00:00Z")
	// The reference's time-series pattern, with parameters inside UNPACK.
	if got := run("SELECT FORMAT_TIMESTAMP('%H', _timestamp) AS h, COUNT(sales) AS n FROM UNPACK((SELECT report['sales'] AS sales FROM report(with_history => true, after => @t) WHERE _key = @k)) GROUP BY h",
		map[string]bigtable.SQLType{"k": bigtable.BytesSQLType{}, "t": bigtable.TimestampSQLType{}},
		map[string]any{"k": []byte("Dept2"), "t": time.Date(2025, 4, 1, 18, 30, 0, 0, time.UTC)}); got != "23 1\n19 1" {
		t.Errorf("clicks per hour = %q", got)
	}
	expect("SELECT report['sales'] FROM report(with_history => true, before_or_equal => TIMESTAMP '2025-04-01 20:00:00+00') WHERE _key = 'Dept1'",
		"[(2025-04-01T20:00:00Z,10)]")

	// Scalar functions, the reference's examples.
	expect("SELECT SOUNDEX('Ashcraft'), FORMAT('|%10d|', 11), FORMAT(\"%'d\", 123456789), FORMAT('%f %E', 1.1, 2.2), FORMAT('%t', DATE '2015-09-01')",
		"A261 |        11| 123,456,789 1.100000 2.200000E+00 2015-09-01")
	expect("SELECT ASCII('abcd'), CHR(65), UNICODE('âbcd'), INITCAP('Hello World-everyone!'), INSTR('banana', 'an', 1, 2), TRANSLATE('This is a cookie', 'sco', 'zku')",
		"97 A 226 Hello World-Everyone! 4 Thiz iz a kuukie")
	expect("SELECT TO_VECTOR32(b'\\x3f\\xc0\\x00\\x00\\x42\\xc8\\x00\\x00'), ROUND(COSINE_DISTANCE([1.0, 2.0], [3.0, 4.0]), 6), EUCLIDEAN_DISTANCE([1.0, 2.0], [3.0, 4.0])",
		"[1.5,100] 0.01613 2.8284271247461903")
	expect("SELECT DATE(2016, 12, 25), DATE_ADD(DATE '2008-12-25', INTERVAL 5 DAY), DATE_DIFF(DATE '2010-07-07', DATE '2008-12-25', DAY), DATE_TRUNC(DATE '2017-11-05', WEEK(MONDAY)), LAST_DAY(DATE '2008-11-25')",
		"2016-12-25 2008-12-30 559 2017-10-30 2008-11-30")
	expect("SELECT FORMAT_DATE('%b-%d-%Y', DATE '2008-12-25'), PARSE_DATE('%A %b %e %Y', 'Thursday Dec 25 2008'), UNIX_DATE(DATE '2008-12-25'), PARSE_TIMESTAMP('%a %b %e %I:%M:%S %Y', 'Thu Dec 25 07:30:00 2008')",
		"Dec-25-2008 2008-12-25 14238 2008-12-25T07:30:00Z")
	expect("SELECT SAFE_ADD(9223372036854775807, 1), SAFE_NEGATE(1), ROUND(ATAN2(1, 1) * 4, 6), CURRENT_DATE = CURRENT_DATE()",
		"NULL -1 3.141593 true")

	expect("SELECT ARRAY_INCLUDES_ALL([1, 2, 3], [3, 1]), ARRAY_SLICE(['a', 'b', 'c', 'd', 'e'], 1, -3), GENERATE_ARRAY(0, 10, 3), GENERATE_DATE_ARRAY(DATE '2016-10-05', DATE '2016-10-07'), TO_CODE_POINTS('foo'), TO_BASE32(b'abcde\\xFF'), IFERROR(1 / 0, -1), ISERROR(1 / 0)",
		"true [b,c] [0,3,6,9] [2016-10-05,2016-10-06,2016-10-07] [102,111,111] MFRGGZDF74====== -1 true")

	expect(`SELECT JSON_QUERY('{"a": {"b": [{"n": "Jane"}]}}', '$.a.b[0]'), TO_JSON_STRING(STRUCT(1 AS id, [10, 20] AS coordinates)), REGEXP_INSTR('a@cd-ef b@cd-ef c@cd-ef', '@[^-]*', 1, 3)`,
		`{"n":"Jane"} {"id":1,"coordinates":[10,20]} 18`)

	// The lambda array functions and NORMALIZE, the reference's examples.
	expect("SELECT ARRAY_FILTER([1 ,2, 3], e -> e > 1), ARRAY_FILTER([0, 2, 3], (e, i) -> e > i), ARRAY_TRANSFORM([1, 4, 3], e -> e + 1), ARRAY_TRANSFORM([1, 4, 3], (e, i) -> e + i)",
		"[2,3] [2,3] [2,5,4] [1,5,5]")
	expect("SELECT ARRAY_OFFSET([1, 4, 4, 4, 6], 4), ARRAY_OFFSET([1, 4, 4, 4, 6], 4, 'LAST'), ARRAY_OFFSET([1, 4, 4, 4, 6], e -> e > 2 AND e < 5, 'LAST'), ARRAY_OFFSET([1, 4, 4, 4, 6], e -> e > 7), ARRAY_OFFSETS([1, 4, 4, 4, 6, 4], 4), ARRAY_OFFSETS([1, 4, 7, 3, 6, 4], e -> e > 2 AND e < 5), ARRAY_OFFSETS([1, 4, 4, 4, 6], 5)",
		"1 3 3 NULL [1,2,3,5] [1,3,5] []")
	expect("SELECT ARRAY_TRANSFORM(MAP_KEYS(report), k -> CONCAT(_key, '/', k)) FROM report WHERE _key = 'Dept2'", "[Dept2/expenses,Dept2/sales]")
	if got := run("SELECT ARRAY_FILTER(@a, e -> e > @n)",
		map[string]bigtable.SQLType{"a": bigtable.ArraySQLType{ElemType: bigtable.Int64SQLType{}}, "n": bigtable.Int64SQLType{}},
		map[string]any{"a": []any{int64(1), int64(5), int64(9)}, "n": int64(4)}); got != "[5,9]" {
		t.Errorf("ARRAY_FILTER with parameters = %q", got)
	}
	expect("SELECT NORMALIZE('\\u00ea') = NORMALIZE('\\u0065\\u0302'), NORMALIZE('Raha\\u2004Mahan', NFKC), NORMALIZE('The red barn') = NORMALIZE('The Red Barn'), NORMALIZE_AND_CASEFOLD('The red barn') = NORMALIZE_AND_CASEFOLD('The Red Barn'), NORMALIZE_AND_CASEFOLD('\\u2168', NFC) = NORMALIZE_AND_CASEFOLD('IX', NFC), NORMALIZE_AND_CASEFOLD('\\u2168', NFKD) = NORMALIZE_AND_CASEFOLD('IX', NFKD)",
		"true Raha Mahan false true false true")

	// Aggregates, over the latest sales (12, 9).
	expect("SELECT ROUND(STDDEV(CAST(CAST(report['sales'] AS STRING) AS INT64)), 6), VAR_POP(CAST(CAST(report['sales'] AS STRING) AS INT64)), APPROX_COUNT_DISTINCT(_key), APPROX_QUANTILES(CAST(CAST(report['sales'] AS STRING) AS INT64), 2) FROM report",
		"2.12132 2.25 2 [9,9,12]")
	expect("SELECT APPROX_TOP_COUNT(_key, 1), APPROX_TOP_SUM(_key, 2, 5), ARRAY_CONCAT_AGG(MAP_KEYS(report)) FROM report",
		"[(Dept1,1)] [(Dept1,2),(Dept2,2)] [expenses,sales,expenses,sales]")

	// STRUCT constructors. The Go client cannot read a STRUCT with an
	// anonymous field, so those are read through FORMAT.
	expect("SELECT STRUCT(1 AS a, 'abc' AS b).b, FORMAT('%t', (1, 'x')), STRUCT<x INT64, d DATE>(1, '2011-05-05')",
		"abc (1, x) (1,2011-05-05)")

	// Window function calls, over the sales unpacked (Dept1 12 and 10,
	// Dept2 9 and 5), and after GROUP BY.
	v := "CAST(CAST(s AS STRING) AS INT64)"
	from := " FROM UNPACK((SELECT _key, report['sales'] AS s FROM report(with_history => true)))"
	expect("SELECT _key, "+v+", ROW_NUMBER() OVER (PARTITION BY _key ORDER BY "+v+"), SUM("+v+") OVER (ORDER BY "+v+" ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), "+
		"LAG("+v+") OVER (PARTITION BY _key ORDER BY "+v+"), FIRST_VALUE("+v+") OVER w, CUME_DIST() OVER (ORDER BY "+v+"), PERCENTILE_CONT("+v+", 0.5) OVER ()"+from+
		" WINDOW w AS (PARTITION BY _key ORDER BY "+v+" DESC)",
		`Dept1 12 2 22 10 12 1 9.5
Dept1 10 1 19 NULL 12 0.75 9.5
Dept2 9 2 14 5 9 0.5 9.5
Dept2 5 1 5 NULL 9 0.25 9.5`)
	expect("SELECT _key, SUM("+v+") AS t, RANK() OVER (ORDER BY SUM("+v+") DESC) AS r"+from+" GROUP BY _key", "Dept1 22 1\nDept2 14 2")

	// An aggregate family's typed values.
	expect("SELECT c['views'], c['views'] > 5 FROM counters", "7 true")

	for sql, want := range map[string]codes.Code{
		"SELECT _timestamp FROM report":                         codes.InvalidArgument,
		"SELECT * FROM UNPACK(report)":                          codes.InvalidArgument,
		"SELECT STRUCT<x INT64>(5 AS x)":                        codes.InvalidArgument,
		"SELECT MIN_BY(_key, _key) FROM report":                 codes.Unimplemented,
		"SELECT MAX_BY(_key, _key) FROM report":                 codes.Unimplemented,
		"SELECT _key, GROUPING(_key) FROM report GROUP BY _key": codes.Unimplemented,
		"SELECT ARRAY_FILTER([1, 2], e -> e + 1)":               codes.InvalidArgument,
		"SELECT ST_BUFFER(_key, 1) FROM report":                 codes.Unimplemented,
		"SELECT COSINE_DISTANCE([1.0], [CAST(1 AS FLOAT32)])":   codes.InvalidArgument,
	} {
		_, err := c.PrepareStatement(ctx, sql, nil)
		if status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
}

// sqlShow1704 is a result value as the test compares it.
func sqlShow1704(v any) string {
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
			parts = append(parts, sqlShow1704(f))
		}
		return "(" + strings.Join(parts, ",") + ")"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return "NULL"
		}
		return sqlShow1704(rv.Elem().Interface())
	case reflect.Slice:
		var parts []string
		for i := 0; i < rv.Len(); i++ {
			parts = append(parts, sqlShow1704(rv.Index(i).Interface()))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case reflect.Map:
		var parts []string
		for _, k := range rv.MapKeys() {
			name := fmt.Sprint(k.Interface())
			if b, err := base64.StdEncoding.DecodeString(name); err == nil && name != "" {
				name = string(b)
			}
			parts = append(parts, name+":"+sqlShow1704(rv.MapIndex(k).Interface()))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprint(v)
}
