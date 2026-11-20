package firestoreadmin

import (
	"math"
	"testing"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
)

// The pipeline semantics of #1990, each against the example tables of
// Google's function reference (firebase.google.com/docs/firestore/
// pipelines/functions/{logical,comparison,array,aggregate}-functions,
// read 2026-11-20). Cases Google leaves open are marked UNVERIFIED.

var (
	tru, fls = boolValue(true), boolValue(false)
	absentF  = fld("missing")
	nan      = doubleValue(math.NaN())
)

// isNaN wraps e so a NaN result compares as TRUE.
func isNaN(e *firestorepb.Value) *firestorepb.Value { return fnv("is_nan", e) }

func TestPipelineThreeValuedLogic(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"n": intValue(1)}}
	null := nullValue()
	runFnCases(t, p, r, []fnCase{
		// and, logical-functions' table.
		{"and T T", fnv("and", tru, tru), tru},
		{"and F T", fnv("and", fls, tru), fls},
		{"and NULL T", fnv("and", null, tru), null},
		{"and ABSENT T", fnv("and", absentF, tru), null},
		{"and NULL F", fnv("and", null, fls), fls},
		{"and F ABSENT", fnv("and", fls, absentF), fls},
		{"and T T NULL", fnv("and", tru, tru, null), null},
		// or.
		{"or T T", fnv("or", tru, tru), tru},
		{"or F T", fnv("or", fls, tru), tru},
		{"or NULL T", fnv("or", null, tru), tru},
		{"or ABSENT T", fnv("or", absentF, tru), tru},
		{"or NULL F", fnv("or", null, fls), null},
		{"or F ABSENT", fnv("or", fls, absentF), null},
		// xor.
		{"xor T T", fnv("xor", tru, tru), fls},
		{"xor F F", fnv("xor", fls, fls), fls},
		{"xor F T", fnv("xor", fls, tru), tru},
		{"xor NULL T", fnv("xor", null, tru), null},
		{"xor ABSENT T", fnv("xor", absentF, tru), null},
		{"xor NULL F", fnv("xor", null, fls), null},
		{"xor F ABSENT", fnv("xor", fls, absentF), null},
		// nor.
		{"nor T T", fnv("nor", tru, tru), fls},
		{"nor F T", fnv("nor", fls, tru), fls},
		{"nor F F", fnv("nor", fls, fls), tru},
		{"nor NULL T", fnv("nor", null, tru), fls},
		{"nor ABSENT T", fnv("nor", absentF, tru), fls},
		{"nor NULL F", fnv("nor", null, fls), null},
		{"nor F ABSENT", fnv("nor", fls, absentF), null},
		// conditional treats NULL and ABSENT as FALSE.
		{"conditional NULL", fnv("conditional", null, intValue(1), intValue(0)), intValue(0)},
		{"conditional ABSENT", fnv("conditional", absentF, intValue(1), intValue(0)), intValue(0)},
		{"conditional and(NULL, T)", fnv("conditional", fnv("and", null, tru), intValue(1), intValue(0)), intValue(0)},
		// UNVERIFIED (re-read 2026-11-20): not(NULL), not(ABSENT) and non-boolean
		// inputs, which Google does not document.
		{"not T", fnv("not", tru), fls},
		{"not NULL", fnv("not", null), null},
		{"not ABSENT", fnv("not", absentF), null},
		{"not 1", fnv("not", intValue(1)), errV},
		{"and 1 T", fnv("and", intValue(1), tru), errV},
		{"xor 'a' T", fnv("xor", stringValue("a"), tru), errV},
		// The first deciding argument stops evaluation.
		{"and F error", fnv("and", fls, fnv("error", stringValue("x"))), fls},
		{"or T error", fnv("or", tru, fnv("error", stringValue("x"))), tru},
	})
	// where keeps only TRUE: a NULL condition drops the row.
	rows, err := p.stage(&firestorepb.Pipeline_Stage{Name: "where", Args: []*firestorepb.Value{fnv("and", fld("missing"), tru)}}, []row{r})
	if err != nil || len(rows) != 0 {
		t.Errorf("where and(ABSENT, TRUE) = %d rows, %v; want 0", len(rows), err)
	}
	rows, err = p.stage(&firestorepb.Pipeline_Stage{Name: "where", Args: []*firestorepb.Value{fnv("or", fld("missing"), tru)}}, []row{r})
	if err != nil || len(rows) != 1 {
		t.Errorf("where or(ABSENT, TRUE) = %d rows, %v; want 1", len(rows), err)
	}
}

func TestPipelineNaNAndComparisons(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"n": intValue(1)}}
	null := nullValue()
	runFnCases(t, p, r, []fnCase{
		// equal and not_equal, comparison-functions' tables.
		{"equal 1 1", fnv("equal", intValue(1), intValue(1)), tru},
		{"equal 1.0 1", fnv("equal", doubleValue(1), intValue(1)), tru},
		{"equal -1.0 1", fnv("equal", doubleValue(-1), intValue(1)), fls},
		{"equal NaN NaN", fnv("equal", nan, nan), tru},
		{"equal NULL NULL", fnv("equal", null, null), tru},
		{"equal NULL ABSENT", fnv("equal", null, absentF), fls},
		{"not_equal 1 1", fnv("not_equal", intValue(1), intValue(1)), fls},
		{"not_equal 1.0 1", fnv("not_equal", doubleValue(1), intValue(1)), fls},
		{"not_equal -1.0 1", fnv("not_equal", doubleValue(-1), intValue(1)), tru},
		{"not_equal NaN 0", fnv("not_equal", nan, intValue(0)), tru},
		{"not_equal NaN NaN", fnv("not_equal", nan, nan), fls},
		{"not_equal NULL NULL", fnv("not_equal", null, null), fls},
		{"not_equal NULL ABSENT", fnv("not_equal", null, absentF), tru},
		// Firestore's value order: 0.0 and -0.0 are one value; NaN is a
		// number that equals no other.
		{"equal 0.0 -0.0", fnv("equal", doubleValue(0), doubleValue(math.Copysign(0, -1))), tru},
		{"equal NaN 0", fnv("equal", nan, intValue(0)), fls},
		{"equal [NaN] [NaN]", fnv("equal", arr(nan), arr(nan)), tru},
		{"equal {a: NaN} {a: NaN}", fnv("equal", mapValue(map[string]*firestorepb.Value{"a": nan}), mapValue(map[string]*firestorepb.Value{"a": nan})), tru},
		// The ordered comparisons, comparison-functions' tables.
		{"less_than 1 0.0", fnv("less_than", intValue(1), doubleValue(0)), fls},
		{"less_than 1 2", fnv("less_than", intValue(1), intValue(2)), tru},
		{"less_than foo 0", fnv("less_than", stringValue("foo"), intValue(0)), fls},
		{"less_than 0 foo", fnv("less_than", intValue(0), stringValue("foo")), fls},
		{"less_than NaN 0", fnv("less_than", nan, intValue(0)), fls},
		{"less_than 0 NaN", fnv("less_than", intValue(0), nan), fls},
		{"less_than NULL NULL", fnv("less_than", null, null), fls},
		{"less_than_or_equal 1 1", fnv("less_than_or_equal", intValue(1), intValue(1)), tru},
		{"less_than_or_equal NaN 0", fnv("less_than_or_equal", nan, intValue(0)), fls},
		{"less_than_or_equal 0 NaN", fnv("less_than_or_equal", intValue(0), nan), fls},
		{"less_than_or_equal NULL NULL", fnv("less_than_or_equal", null, null), tru},
		{"greater_than 1 0.0", fnv("greater_than", intValue(1), doubleValue(0)), tru},
		{"greater_than NaN 0", fnv("greater_than", nan, intValue(0)), fls},
		{"greater_than 0 NaN", fnv("greater_than", intValue(0), nan), fls},
		{"greater_than NULL NULL", fnv("greater_than", null, null), fls},
		{"greater_than_or_equal 1 0.0", fnv("greater_than_or_equal", intValue(1), doubleValue(0)), tru},
		{"greater_than_or_equal NaN 0", fnv("greater_than_or_equal", nan, intValue(0)), fls},
		{"greater_than_or_equal NULL NULL", fnv("greater_than_or_equal", null, null), tru},
		// UNVERIFIED (re-read 2026-11-20): Google gives no NaN-with-NaN row for the
		// ordered comparisons; NaN is incomparable there, so all are FALSE.
		{"less_than_or_equal NaN NaN", fnv("less_than_or_equal", nan, nan), fls},
		{"greater_than_or_equal NaN NaN", fnv("greater_than_or_equal", nan, nan), fls},
		// cmp follows the sort order, where NaN equals NaN and sorts
		// below every other number.
		{"cmp NaN NaN", fnv("cmp", nan, nan), intValue(0)},
		{"cmp NaN -Inf", fnv("cmp", nan, doubleValue(math.Inf(-1))), intValue(-1)},
		// equal_any and not_equal_any, logical-functions' tables.
		{"equal_any 0 [1,2,3]", fnv("equal_any", intValue(0), ints(1, 2, 3)), fls},
		{"equal_any 2 [1,2,3]", fnv("equal_any", intValue(2), ints(1, 2, 3)), tru},
		{"equal_any NULL [1,2,3]", fnv("equal_any", null, ints(1, 2, 3)), fls},
		{"equal_any NULL [1,NULL]", fnv("equal_any", null, arr(intValue(1), null)), tru},
		{"equal_any ABSENT [1,NULL]", fnv("equal_any", absentF, arr(intValue(1), null)), fls},
		{"equal_any NaN [1,NaN,3]", fnv("equal_any", nan, arr(intValue(1), nan, intValue(3))), tru},
		{"not_equal_any 0 [1,2,3]", fnv("not_equal_any", intValue(0), ints(1, 2, 3)), tru},
		{"not_equal_any NULL [1,NULL]", fnv("not_equal_any", null, arr(intValue(1), null)), fls},
		{"not_equal_any ABSENT [1,NULL]", fnv("not_equal_any", absentF, arr(intValue(1), null)), tru},
		{"not_equal_any NaN [1,NaN,3]", fnv("not_equal_any", nan, arr(intValue(1), nan, intValue(3))), fls},
		// The array functions use the same equality.
		{"array_contains_all [NaN] [NaN]", fnv("array_contains_all", arr(nan), arr(nan)), tru},
		{"array_contains [1,NaN] NaN", fnv("array_contains", arr(intValue(1), nan), nan), tru},
		{"array_index_of_all [NaN,1,NaN] NaN", fnv("array_index_of_all", arr(nan, intValue(1), nan), nan), ints(0, 2)},
	})
}

func TestPipelineScalarMaxMinSum(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"n": intValue(1)}}
	null := nullValue()
	runFnCases(t, p, r, []fnCase{
		// maximum and minimum of a series, logical-functions' tables.
		{"maximum F T", fnv("maximum", fls, tru), tru},
		{"maximum F -10", fnv("maximum", fls, intValue(-10)), intValue(-10)},
		{"maximum 0.0 -5", fnv("maximum", doubleValue(0), intValue(-5)), doubleValue(0)},
		{"maximum foo bar", fnv("maximum", stringValue("foo"), stringValue("bar")), stringValue("foo")},
		{"maximum foo [foo]", fnv("maximum", stringValue("foo"), strs("foo")), strs("foo")},
		{"maximum ABSENT ABSENT", fnv("maximum", absentF, absentF), null},
		{"maximum NULL NULL", fnv("maximum", null, null), null},
		{"maximum NULL 3 ABSENT", fnv("maximum", null, intValue(3), absentF), intValue(3)},
		{"minimum F T", fnv("minimum", fls, tru), fls},
		{"minimum F -10", fnv("minimum", fls, intValue(-10)), fls},
		{"minimum 0.0 -5", fnv("minimum", doubleValue(0), intValue(-5)), intValue(-5)},
		{"minimum foo bar", fnv("minimum", stringValue("foo"), stringValue("bar")), stringValue("bar")},
		{"minimum foo [foo]", fnv("minimum", stringValue("foo"), strs("foo")), stringValue("foo")},
		{"minimum ABSENT ABSENT", fnv("minimum", absentF, absentF), null},
		{"minimum NULL NULL", fnv("minimum", null, null), null},
		// maximum and minimum of one array, array-functions' tables.
		{"maximum [1,5,2]", fnv("maximum", ints(1, 5, 2)), intValue(5)},
		{"maximum [null,null]", fnv("maximum", arr(null, null)), null},
		{"maximum []", fnv("maximum", arr()), null},
		{"minimum [5,null,1]", fnv("minimum", arr(intValue(5), null, intValue(1))), intValue(1)},
		{"minimum []", fnv("minimum", arr()), null},
		// UNVERIFIED (re-read 2026-11-20): one non-array argument, and NaN, which
		// sorts below every other number.
		{"maximum 7", fnv("maximum", intValue(7)), intValue(7)},
		{"maximum NULL", fnv("maximum", null), null},
		{"maximum NaN 1", fnv("maximum", nan, intValue(1)), intValue(1)},
		{"minimum [3,NaN]", isNaN(fnv("minimum", arr(intValue(3), nan))), tru},
		// sum of an array, array-functions' table and rules.
		{"sum [1,a,2]", fnv("sum", arr(intValue(1), stringValue("a"), intValue(2))), intValue(3)},
		{"sum [1,NaN]", isNaN(fnv("sum", arr(intValue(1), nan))), tru},
		{"sum [a]", fnv("sum", strs("a")), null},
		{"sum []", fnv("sum", arr()), null},
		{"sum [MAX,1]", fnv("sum", ints(math.MaxInt64, 1)), errV},
		{"sum [MAX,1.0]", fnv("sum", arr(intValue(math.MaxInt64), doubleValue(1))), doubleValue(9.223372036854776e+18)},
		{"sum [1.5,MAX,1]", fnv("sum", arr(doubleValue(1.5), intValue(math.MaxInt64), intValue(1))), doubleValue(1.5 + float64(math.MaxInt64) + 1)},
		{"sum [MIN,-1]", fnv("sum", ints(math.MinInt64, -1)), errV},
		{"sum [MAX,MAX] floats", fnv("sum", arr(doubleValue(math.MaxFloat64), doubleValue(math.MaxFloat64))), doubleValue(math.Inf(1))},
		{"sum [1,2.5]", fnv("sum", arr(intValue(1), doubleValue(2.5))), doubleValue(3.5)},
		// UNVERIFIED (re-read 2026-11-20): sum of NULL, ABSENT or a non-array.
		{"sum NULL", fnv("sum", null), null},
		{"sum ABSENT", fnv("sum", absentF), null},
		{"sum 1", fnv("sum", intValue(1)), errV},
	})
}

// The aggregates under the same equality and order: count_distinct and
// array_agg_distinct see 1L and 1.0, 0.0 and -0.0, and NaN and NaN as one
// value; sum and average make NaN of a NaN (aggregate-functions).
func TestPipelineAggregateSemantics(t *testing.T) {
	p := &pipelineRun{}
	var rows []row
	for _, v := range []*firestorepb.Value{intValue(1), doubleValue(1), doubleValue(0), doubleValue(math.Copysign(0, -1)),
		nan, doubleValue(math.Float64frombits(0x7ff8000000000001)), nullValue(), nil, arr(intValue(1)), arr(doubleValue(1))} {
		f := map[string]*firestorepb.Value{}
		if v != nil {
			f["v"] = v
		}
		rows = append(rows, row{fields: f})
	}
	acc := func(name string, rs []row) *firestorepb.Value {
		t.Helper()
		v, err := p.accumulate(&firestorepb.Function{Name: name, Args: []*firestorepb.Value{fld("v")}}, rs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return v
	}
	// 1 and 1.0; 0.0 and -0.0; the NaNs; [1] and [1.0].
	if got := acc("count_distinct", rows).GetIntegerValue(); got != 4 {
		t.Errorf("count_distinct = %d, want 4", got)
	}
	// The same four, and NULL (for NULL and ABSENT).
	if got := len(acc("array_agg_distinct", rows).GetArrayValue().GetValues()); got != 5 {
		t.Errorf("array_agg_distinct has %d values, want 5", got)
	}
	if got := acc("sum", rows[:6]).GetDoubleValue(); !math.IsNaN(got) {
		t.Errorf("sum with NaN = %v, want NaN", got)
	}
	if got := acc("average", rows[:6]).GetDoubleValue(); !math.IsNaN(got) {
		t.Errorf("average with NaN = %v, want NaN", got)
	}
	// Overflow converts to float64 in the aggregate (unlike the array
	// sum): "An INTEGER will be converted to a DOUBLE if it cannot be
	// represented as an INTEGER".
	big := []row{{fields: map[string]*firestorepb.Value{"v": intValue(math.MaxInt64)}}, {fields: map[string]*firestorepb.Value{"v": intValue(1)}}}
	if got := acc("sum", big); got.GetDoubleValue() != 9.223372036854776e+18 {
		t.Errorf("sum MAX + 1 = %v, want 9.223372036854776e+18", got)
	}
	// UNVERIFIED (re-read 2026-11-20): minimum and maximum place NaN below every
	// other number, as Firestore's value order does.
	if got := acc("minimum", rows[:6]).GetDoubleValue(); !math.IsNaN(got) {
		t.Errorf("minimum with NaN = %v, want NaN", got)
	}
	if got := acc("maximum", rows[:6]); got.GetIntegerValue() != 1 && got.GetDoubleValue() != 1 {
		t.Errorf("maximum with NaN = %v, want 1", got)
	}
	// 2^53 and 2^53 + 1 stay two values.
	two := []row{{fields: map[string]*firestorepb.Value{"v": intValue(1 << 53)}}, {fields: map[string]*firestorepb.Value{"v": intValue(1<<53 + 1)}}}
	if got := acc("count_distinct", two).GetIntegerValue(); got != 2 {
		t.Errorf("count_distinct 2^53, 2^53+1 = %d, want 2", got)
	}
}
