package firestoreadmin

import (
	"math"
	"testing"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The pipeline functions of #1951, each against the examples of Google's
// function reference (firebase.google.com/docs/firestore/pipelines/
// functions/*, read 2026-11-20) and the rules it states.

func fnv(name string, args ...*firestorepb.Value) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_FunctionValue{FunctionValue: &firestorepb.Function{Name: name, Args: args}}}
}

func fld(path string) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_FieldReferenceValue{FieldReferenceValue: path}}
}

func vref(name string) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_VariableReferenceValue{VariableReferenceValue: name}}
}

func ints(ns ...int64) *firestorepb.Value {
	out := make([]*firestorepb.Value, len(ns))
	for i, n := range ns {
		out[i] = intValue(n)
	}
	return arrayValue(out)
}

func strs(ss ...string) *firestorepb.Value {
	out := make([]*firestorepb.Value, len(ss))
	for i, s := range ss {
		out[i] = stringValue(s)
	}
	return arrayValue(out)
}

func arr(vs ...*firestorepb.Value) *firestorepb.Value { return arrayValue(vs) }

func ts(s string) *firestorepb.Value {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return timestampValue(t)
}

func regexV(pattern, options string) *firestorepb.Value {
	return mapValue(map[string]*firestorepb.Value{"__regex__": mapValue(map[string]*firestorepb.Value{
		"pattern": stringValue(pattern), "options": stringValue(options)})})
}

func geoV(lat, lng float64) *firestorepb.Value {
	return &firestorepb.Value{ValueType: &firestorepb.Value_GeoPointValue{GeoPointValue: &latlng.LatLng{Latitude: lat, Longitude: lng}}}
}

// absentV marks a want of absent (nil); errV a want of an expression error.
var absentV = &firestorepb.Value{}
var errV = stringValue("\x00error")

type fnCase struct {
	name string
	expr *firestorepb.Value
	want *firestorepb.Value
}

func runFnCases(t *testing.T, p *pipelineRun, r row, cases []fnCase) {
	t.Helper()
	for _, c := range cases {
		got, err := p.eval(c.expr, r)
		switch {
		case c.want == errV:
			if err == nil || !isEvalError(err) {
				t.Errorf("%s = %v, %v; want an expression error", c.name, got, err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want == absentV:
			if got != nil {
				t.Errorf("%s = %v, want absent", c.name, got)
			}
		case !proto.Equal(got, c.want):
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPipelineRegexFunctions(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"s": stringValue("Sometext MATCH"), "n": intValue(1)}}
	runFnCases(t, p, r, []fnCase{
		{"regex_find plain", fnv("regex_find", fld("s"), stringValue("text")), stringValue("text")},
		{"regex_find regex i", fnv("regex_find", fld("s"), regexV("match", "i")), stringValue("MATCH")},
		{"regex_find regex i group", fnv("regex_find", fld("s"), regexV("(match)", "i")), stringValue("MATCH")},
		{"regex_find whole match, not the group", fnv("regex_find", stringValue("a1b2"), stringValue("[a-z]([0-9])")), stringValue("a1")},
		{"regex_find two groups", fnv("regex_find", stringValue("a1b2"), stringValue("([a-z])([0-9])")), stringValue("a1")},
		{"regex_find no match", fnv("regex_find", stringValue("Sometext match"), regexV("MATCH", "")), nullValue()},
		{"regex_find absent", fnv("regex_find", fld("missing"), stringValue("x")), nullValue()},
		{"regex_find null", fnv("regex_find", nullValue(), stringValue("x")), nullValue()},
		{"regex_find not a string", fnv("regex_find", fld("n"), stringValue("x")), errV},
		{"regex_find invalid", fnv("regex_find", fld("s"), stringValue("(")), errV},
		{"regex_find not RE2 (backreference)", fnv("regex_find", fld("s"), stringValue(`(a)\1`)), errV},
		{"regex_find bad option", fnv("regex_find", fld("s"), regexV("x", "x")), errV},
		{"regex_find s option", fnv("regex_find", stringValue("a\nb"), regexV("a.b", "s")), stringValue("a\nb")},
		{"regex_find m option", fnv("regex_find", stringValue("x\nab"), regexV("^ab$", "m")), stringValue("ab")},
		{"regex_find_all one", fnv("regex_find_all", stringValue("one two three two"), stringValue("one")), strs("one")},
		{"regex_find_all i", fnv("regex_find_all", stringValue("one TWO three Two"), regexV("two", "i")), strs("TWO", "Two")},
		{"regex_find_all none", fnv("regex_find_all", stringValue("one two three two"), regexV("TWO", "")), strs()},
		{"regex_find_all absent", fnv("regex_find_all", fld("missing"), stringValue("x")), nullValue()},
		{"regex_find_all invalid", fnv("regex_find_all", fld("s"), stringValue("[")), errV},
		{"regex_contains regex", fnv("regex_contains", fld("s"), regexV("match", "i")), boolValue(true)},
		{"regex_match regex", fnv("regex_match", fld("s"), regexV("some.*", "i")), boolValue(true)},
		{"regex_match anchored", fnv("regex_match", fld("s"), stringValue("some")), boolValue(false)},
	})
}

func TestPipelineArrayFunctions(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"a": ints(1, 2, 3, 4, 5), "s": stringValue("x")}}
	gt1 := fnv("greater_than", vref("x"), intValue(1))
	runFnCases(t, p, r, []fnCase{
		{"array_first_n 3", fnv("array_first_n", fld("a"), intValue(3)), ints(1, 2, 3)},
		{"array_first_n past the end", fnv("array_first_n", ints(1, 2), intValue(3)), ints(1, 2)},
		{"array_first_n 0", fnv("array_first_n", ints(1, 2, 3), intValue(0)), ints()},
		{"array_first_n negative", fnv("array_first_n", ints(1, 2, 3), intValue(-1)), errV},
		{"array_first_n absent", fnv("array_first_n", fld("missing"), intValue(1)), nullValue()},
		{"array_first_n not an array", fnv("array_first_n", fld("s"), intValue(1)), errV},
		{"array_last_n 3", fnv("array_last_n", fld("a"), intValue(3)), ints(3, 4, 5)},
		{"array_last_n past the end", fnv("array_last_n", ints(1, 2), intValue(3)), ints(1, 2)},
		{"array_last_n 0", fnv("array_last_n", ints(1, 2, 3), intValue(0)), ints()},
		{"array_last_n negative", fnv("array_last_n", ints(1, 2, 3), intValue(-2)), errV},
		{"array_index_of_all", fnv("array_index_of_all", ints(1, 2, 3, 2), intValue(2)), ints(1, 3)},
		{"array_index_of_all none", fnv("array_index_of_all", ints(1, 2, 3), intValue(4)), ints()},
		{"array_index_of_all null", fnv("array_index_of_all", arr(intValue(1), nullValue(), intValue(3), nullValue()), nullValue()), ints(1, 3)},
		{"array_index_of_all across number types", fnv("array_index_of_all", ints(1, 2), doubleValue(2)), ints(1)},
		{"minimum_n", fnv("minimum_n", ints(1, 5, 2, 4, 3), intValue(3)), ints(1, 2, 3)},
		{"minimum_n nulls ignored", fnv("minimum_n", arr(intValue(5), nullValue(), intValue(1)), intValue(3)), ints(1, 5)},
		{"minimum_n negative", fnv("minimum_n", ints(1), intValue(-1)), errV},
		{"maximum_n", fnv("maximum_n", ints(1, 5, 2, 4, 3), intValue(3)), ints(5, 4, 3)},
		{"maximum_n nulls ignored", fnv("maximum_n", arr(intValue(1), nullValue(), intValue(5)), intValue(3)), ints(5, 1)},
		{"maximum_n negative", fnv("maximum_n", ints(1), intValue(-1)), errV},
		{"offset", fnv("offset", fld("a"), intValue(1)), intValue(2)},
		{"offset negative", fnv("offset", fld("a"), intValue(-1)), intValue(5)},
		{"offset out of bounds", fnv("offset", fld("a"), intValue(5)), absentV},
		{"offset not an array", fnv("offset", fld("s"), intValue(0)), absentV},
		{"offset absent", fnv("offset", fld("missing"), intValue(0)), absentV},
		{"array_filter", fnv("array_filter", ints(1, 2, 3), stringValue("x"), gt1), ints(2, 3)},
		{"array_filter null", fnv("array_filter", arr(intValue(1), nullValue(), intValue(3)), stringValue("x"), gt1), ints(3)},
		{"array_filter strings", fnv("array_filter", strs("a", "b", "c"), stringValue("x"), fnv("not_equal", vref("x"), stringValue("b"))), strs("a", "c")},
		{"array_filter empty", fnv("array_filter", ints(), stringValue("x"), boolValue(true)), ints()},
		{"array_filter null predicate dropped", fnv("array_filter", ints(1, 2), stringValue("x"), nullValue()), ints()},
		{"array_filter non-boolean predicate", fnv("array_filter", ints(1, 2), stringValue("x"), intValue(1)), errV},
		{"array_filter absent", fnv("array_filter", fld("missing"), stringValue("x"), gt1), nullValue()},
		{"array_filter sees the row", fnv("array_filter", ints(1, 6), stringValue("x"), fnv("greater_than", vref("x"), fnv("array_length", fld("a")))), ints(6)},
		{"array_transform x*2", fnv("array_transform", ints(1, 2, 3), stringValue("x"), fnv("multiply", vref("x"), intValue(2))), ints(2, 4, 6)},
		{"array_transform x+1", fnv("array_transform", ints(1, 2, 3), stringValue("x"), fnv("add", vref("x"), intValue(1))), ints(2, 3, 4)},
		{"array_transform x+i", fnv("array_transform", ints(10, 20), stringValue("x"), stringValue("i"), fnv("add", vref("x"), vref("i"))), ints(10, 21)},
		{"array_transform empty", fnv("array_transform", ints(), stringValue("x"), intValue(1)), ints()},
		{"array_transform absent result is null", fnv("array_transform", ints(1), stringValue("x"), fld("missing")), arr(nullValue())},
		{"array_transform not an array", fnv("array_transform", fld("s"), stringValue("x"), vref("x")), errV},
	})
	// A lambda's parameter must be a constant name.
	if _, err := p.eval(fnv("array_filter", ints(1), intValue(3), boolValue(true)), r); status.Code(err) != codes.InvalidArgument {
		t.Errorf("array_filter with a non-name parameter: %v, want INVALID_ARGUMENT", err)
	}
}

func TestPipelineTimestampFunctions(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"n": intValue(1)}}
	runFnCases(t, p, r, []fnCase{
		{"trunc second", fnv("timestamp_trunc", ts("2000-01-01T10:20:30.123456Z"), stringValue("second")), ts("2000-01-01T10:20:30Z")},
		{"trunc millisecond", fnv("timestamp_trunc", ts("2000-01-01T10:20:30.123456Z"), stringValue("millisecond")), ts("2000-01-01T10:20:30.123Z")},
		{"trunc microsecond", fnv("timestamp_trunc", ts("2000-01-01T10:20:30.123456Z"), stringValue("microsecond")), ts("2000-01-01T10:20:30.123456Z")},
		{"trunc minute", fnv("timestamp_trunc", ts("2000-01-01T10:20:30Z"), stringValue("minute")), ts("2000-01-01T10:20:00Z")},
		{"trunc hour", fnv("timestamp_trunc", ts("2000-01-01T10:20:30Z"), stringValue("hour")), ts("2000-01-01T10:00:00Z")},
		{"trunc day", fnv("timestamp_trunc", ts("1997-05-31T04:30:30Z"), stringValue("day")), ts("1997-05-31T00:00:00Z")},
		{"trunc day LA", fnv("timestamp_trunc", ts("1997-05-31T04:30:30Z"), stringValue("day"), stringValue("America/Los_Angeles")), ts("1997-05-30T07:00:00Z")},
		{"trunc week(friday)", fnv("timestamp_trunc", ts("2001-03-16T04:00:00Z"), stringValue("week(friday)")), ts("2001-03-16T00:00:00Z")},
		{"trunc week (Sunday)", fnv("timestamp_trunc", ts("2001-03-16T04:00:00Z"), stringValue("week")), ts("2001-03-11T00:00:00Z")},
		{"trunc week(monday)", fnv("timestamp_trunc", ts("2001-03-16T04:00:00Z"), stringValue("week(monday)")), ts("2001-03-12T00:00:00Z")},
		{"trunc month GMT offset", fnv("timestamp_trunc", ts("2026-01-24T20:00:00Z"), stringValue("month"), stringValue("GMT+06:32:43")), ts("2026-01-01T06:32:43Z")},
		{"trunc month", fnv("timestamp_trunc", ts("2026-01-24T20:00:00Z"), stringValue("month")), ts("2026-01-01T00:00:00Z")},
		{"trunc quarter", fnv("timestamp_trunc", ts("2026-05-24T20:00:00Z"), stringValue("quarter")), ts("2026-04-01T00:00:00Z")},
		{"trunc year", fnv("timestamp_trunc", ts("2026-05-24T20:00:00Z"), stringValue("year")), ts("2026-01-01T00:00:00Z")},
		{"trunc isoyear", fnv("timestamp_trunc", ts("2027-01-01T12:00:00Z"), stringValue("isoyear")), ts("2025-12-29T00:00:00Z")},
		{"trunc isoweek", fnv("timestamp_trunc", ts("2027-01-01T12:00:00Z"), stringValue("isoweek")), ts("2026-12-28T00:00:00Z")},
		{"trunc bad granularity", fnv("timestamp_trunc", ts("2027-01-01T12:00:00Z"), stringValue("decade")), errV},
		{"trunc bad zone", fnv("timestamp_trunc", ts("2027-01-01T12:00:00Z"), stringValue("day"), stringValue("Mars/Olympus")), errV},
		{"trunc absent", fnv("timestamp_trunc", fld("missing"), stringValue("day")), nullValue()},
		{"trunc not a timestamp", fnv("timestamp_trunc", fld("n"), stringValue("day")), errV},
		{"diff second", fnv("timestamp_diff", ts("2026-07-04T00:01:00Z"), ts("2026-07-04T00:00:00Z"), stringValue("second")), intValue(60)},
		{"diff day negative", fnv("timestamp_diff", ts("2026-07-04T00:00:00Z"), ts("2026-07-05T00:00:00Z"), stringValue("day")), intValue(-1)},
		{"diff minute truncates", fnv("timestamp_diff", ts("2026-07-04T00:00:59Z"), ts("2026-07-04T00:00:00Z"), stringValue("minute")), intValue(0)},
		{"diff microsecond across millennia", fnv("timestamp_diff", ts("9999-12-31T23:59:59.999999Z"), ts("0001-01-01T00:00:00Z"), stringValue("microsecond")), intValue(315537897599999999)},
		{"diff day across millennia", fnv("timestamp_diff", ts("9999-12-31T00:00:00Z"), ts("0001-01-01T00:00:00Z"), stringValue("day")), intValue(3652058)},
		{"diff bad unit", fnv("timestamp_diff", ts("2026-07-04T00:00:00Z"), ts("2026-07-04T00:00:00Z"), stringValue("week")), errV},
		{"diff absent start", fnv("timestamp_diff", ts("2026-07-04T00:00:00Z"), fld("missing"), stringValue("day")), nullValue()},
		{"extract year", fnv("timestamp_extract", ts("2025-02-20T10:20:30Z"), stringValue("year")), intValue(2025)},
		{"extract day", fnv("timestamp_extract", ts("2025-02-20T10:20:30Z"), stringValue("day")), intValue(20)},
		{"extract year Tokyo", fnv("timestamp_extract", ts("2025-12-31T23:59:59Z"), stringValue("year"), stringValue("Asia/Tokyo")), intValue(2026)},
		{"extract dayofweek Thursday", fnv("timestamp_extract", ts("2025-02-20T10:20:30Z"), stringValue("dayofweek")), intValue(5)},
		{"extract dayofyear", fnv("timestamp_extract", ts("2025-02-20T10:20:30Z"), stringValue("dayofyear")), intValue(51)},
		{"extract quarter", fnv("timestamp_extract", ts("2025-02-20T10:20:30Z"), stringValue("quarter")), intValue(1)},
		{"extract microsecond", fnv("timestamp_extract", ts("2025-02-20T10:20:30.123456Z"), stringValue("microsecond")), intValue(123456)},
		{"extract millisecond", fnv("timestamp_extract", ts("2025-02-20T10:20:30.123456Z"), stringValue("millisecond")), intValue(123)},
		// 2025 begins on a Wednesday: its first Sunday, 5 January, starts week 1.
		{"extract week before the first Sunday", fnv("timestamp_extract", ts("2025-01-04T10:00:00Z"), stringValue("week")), intValue(0)},
		{"extract week first Sunday", fnv("timestamp_extract", ts("2025-01-05T10:00:00Z"), stringValue("week")), intValue(1)},
		{"extract week(monday)", fnv("timestamp_extract", ts("2025-01-06T10:00:00Z"), stringValue("week(monday)")), intValue(1)},
		{"extract isoweek", fnv("timestamp_extract", ts("2024-12-30T10:00:00Z"), stringValue("isoweek")), intValue(1)},
		{"extract isoyear", fnv("timestamp_extract", ts("2024-12-30T10:00:00Z"), stringValue("isoyear")), intValue(2025)},
		{"extract hour GMT-5 (POSIX sign: east)", fnv("timestamp_extract", ts("2025-02-20T10:00:00Z"), stringValue("hour"), stringValue("GMT-5")), intValue(15)},
		{"extract bad part", fnv("timestamp_extract", ts("2025-02-20T10:00:00Z"), stringValue("century")), errV},
		{"extract absent", fnv("timestamp_extract", fld("missing"), stringValue("year")), nullValue()},
	})
}

func TestPipelineGeneralFunctions(t *testing.T) {
	p := &pipelineRun{}
	r := row{fields: map[string]*firestorepb.Value{"x": intValue(3), "m": mapValue(map[string]*firestorepb.Value{"foo": intValue(2)})}}
	sw := func(x int64) *firestorepb.Value {
		return fnv("switch_on", fnv("equal", intValue(x), intValue(1)), stringValue("one"),
			fnv("equal", intValue(x), intValue(2)), stringValue("two"), stringValue("other"))
	}
	runFnCases(t, p, r, []fnCase{
		{"cmp 1 1", fnv("cmp", intValue(1), intValue(1)), intValue(0)},
		{"cmp 1.0 1", fnv("cmp", doubleValue(1), intValue(1)), intValue(0)},
		{"cmp -1.0 1", fnv("cmp", doubleValue(-1), intValue(1)), intValue(-1)},
		{"cmp 42.5 foo", fnv("cmp", doubleValue(42.5), stringValue("foo")), intValue(-1)},
		{"cmp null null", fnv("cmp", nullValue(), nullValue()), intValue(0)},
		{"cmp null absent", fnv("cmp", nullValue(), fld("missing")), intValue(0)},
		{"cmp NaN 1", fnv("cmp", doubleValue(math.NaN()), intValue(1)), intValue(-1)},
		{"cmp b a", fnv("cmp", stringValue("b"), stringValue("a")), intValue(1)},
		{"switch_on one", sw(1), stringValue("one")},
		{"switch_on two", sw(2), stringValue("two")},
		{"switch_on default", sw(3), stringValue("other")},
		{"switch_on no default", fnv("switch_on", boolValue(false), intValue(1)), errV},
		{"switch_on null condition", fnv("switch_on", nullValue(), intValue(1), intValue(0)), intValue(0)},
		{"switch_on error caught", fnv("if_error", fnv("switch_on", boolValue(false), intValue(1)), stringValue("none")), stringValue("none")},
		{"error", fnv("error", stringValue("no condition matched")), errV},
		{"error in switch_on", fnv("switch_on", boolValue(true), intValue(1), fnv("error", stringValue("no condition matched"))), intValue(1)},
		{"is_error error", fnv("is_error", fnv("error", stringValue("x"))), boolValue(true)},
		{"map_entries empty", fnv("map_entries", mapValue(map[string]*firestorepb.Value{})), arr()},
		{"map_entries one", fnv("map_entries", fld("m")), arr(mapValue(map[string]*firestorepb.Value{"k": stringValue("foo"), "v": intValue(2)}))},
		{"map_entries absent", fnv("map_entries", fld("missing")), nullValue()},
		{"map_entries not a map", fnv("map_entries", fld("x")), errV},
		{"current_document", fnv("current_document"), mapValue(r.fields)},
		{"current_context", fnv("current_context"), mapValue(r.fields)},
	})
	for i := 0; i < 100; i++ {
		v, err := p.eval(fnv("rand"), r)
		f, ok := v.GetValueType().(*firestorepb.Value_DoubleValue)
		if err != nil || !ok || f.DoubleValue < 0 || f.DoubleValue >= 1 {
			t.Fatalf("rand() = %v, %v; want a float64 in [0, 1)", v, err)
		}
	}
	if _, err := p.eval(fnv("rand", intValue(1)), r); status.Code(err) != codes.InvalidArgument {
		t.Errorf("rand(1): %v, want INVALID_ARGUMENT", err)
	}
	if _, err := p.eval(fnv("score"), r); status.Code(err) != codes.Unimplemented {
		t.Errorf("score(): %v, want UNIMPLEMENTED", err)
	}
}

func TestPipelineGeoDistance(t *testing.T) {
	paris, london := geoV(48.8566, 2.3522), geoV(51.5074, -0.1278)
	r := row{fields: map[string]*firestorepb.Value{"loc": paris, "n": intValue(1)}}
	// Outside a search stage it is refused, as the client documents.
	if _, err := (&pipelineRun{}).eval(fnv("geo_distance", fld("loc"), london), r); status.Code(err) != codes.InvalidArgument {
		t.Errorf("geo_distance outside search: %v, want INVALID_ARGUMENT", err)
	}
	p := &pipelineRun{inSearch: true}
	got, err := p.eval(fnv("geo_distance", fld("loc"), london), r)
	if err != nil || math.Abs(got.GetDoubleValue()-343556.6) > 1 {
		t.Errorf("geo_distance Paris London = %v, %v; want 343556.6 m", got, err)
	}
	runFnCases(t, p, r, []fnCase{
		{"geo_distance same point", fnv("geo_distance", fld("loc"), paris), doubleValue(0)},
		{"geo_distance absent", fnv("geo_distance", fld("missing"), paris), nullValue()},
		{"geo_distance not a point", fnv("geo_distance", fld("n"), paris), errV},
	})
	// The search stage serves it.
	rows, err := p.search(map[string]*firestorepb.Value{"query": fnv("less_than_or_equal", fnv("geo_distance", fld("loc"), london), intValue(400000))},
		[]row{r, {fields: map[string]*firestorepb.Value{"loc": geoV(0, 0)}}})
	if err != nil || len(rows) != 1 {
		t.Errorf("search within 400 km of London = %d rows, %v; want 1", len(rows), err)
	}
}

func TestGMTOffsetZone(t *testing.T) {
	for _, c := range []struct {
		name string
		off  int
		ok   bool
	}{
		{"GMT+06:32:43", -(6*3600 + 32*60 + 43), true},
		{"GMT-5", 5 * 3600, true},
		{"GMT+05:30", -(5*3600 + 30*60), true},
		{"GMT", 0, false},
		{"GMT+", 0, false},
		{"GMT+5:61", 0, false},
		{"GMT+1:2:3:4", 0, false},
		{"UTC+5", 0, false},
	} {
		loc, ok := gmtOffsetZone(c.name)
		if ok != c.ok {
			t.Errorf("gmtOffsetZone(%q) ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok {
			if _, off := time.Unix(0, 0).In(loc).Zone(); off != c.off {
				t.Errorf("gmtOffsetZone(%q) offset = %d, want %d", c.name, off, c.off)
			}
		}
	}
}
