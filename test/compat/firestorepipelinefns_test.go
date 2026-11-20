//go:build compat

package compat

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFirestorePipelineFunctions (#1951): each pipeline function the gaps
// inventory listed as refused, through the official Go client's pipeline
// API on an Enterprise database, against the examples of Google's function
// reference (firebase.google.com/docs/firestore/pipelines/functions/*,
// read 2026-11-20). error and current_context, which the Go client has no
// helper for, go through its generated client (apiv1) as raw functions.
//
// covers: google.firestore.v1.Firestore/ExecutePipeline
func TestFirestorePipelineFunctions(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	dbName := enterpriseDatabase(t, h, "piped-fns")
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	c, err := firestore.NewClientWithDatabase(ctx, h.Project(), "piped-fns")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Collection("f").Doc("f1").Set(ctx, map[string]any{
		"s": "Sometext MATCH", "words": "one TWO three Two", "code": "a1b2",
		"a": []int{1, 5, 2, 4, 3}, "nulls": []any{1, nil, 3, nil}, "sparse": []any{5, nil, 1},
		"m":  map[string]any{"foo": 2},
		"x":  3,
		"ts": time.Date(2001, 3, 16, 4, 0, 0, 0, time.UTC), "jan": time.Date(2026, 1, 24, 20, 0, 0, 0, time.UTC),
		"nye": time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	base := func() *firestore.Pipeline { return c.Pipeline().Collection("f") }
	one := func(what string, p *firestore.Pipeline) map[string]any {
		t.Helper()
		rs, err := p.Execute(ctx).Results().GetAll()
		if err != nil || len(rs) != 1 {
			t.Fatalf("%s = %d results, %v; want 1", what, len(rs), err)
		}
		return rs[0].Data()
	}
	want := func(what string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", what, got, want)
		}
	}
	refused := func(what string, p *firestore.Pipeline) {
		t.Helper()
		if _, err := p.Execute(ctx).Results().GetAll(); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v, want INVALID_ARGUMENT", what, err)
		}
	}
	x := firestore.Variable("x")

	// Regular expressions (RE2), a pattern as a string or a REGEX value.
	got := one("regex", base().Select(firestore.Fields(
		firestore.RegexFind("s", firestore.ConstantOf(firestore.BSONRegex{Pattern: "match", Options: "i"})).As("find_i"),
		firestore.RegexFind("code", "[a-z]([0-9])").As("find_whole"),
		firestore.RegexFind("s", firestore.ConstantOf(firestore.BSONRegex{Pattern: "MATCH"})).As("find_cs"),
		firestore.RegexFind("s", "nothing").As("find_none"),
		firestore.RegexFindAll("words", firestore.ConstantOf(firestore.BSONRegex{Pattern: "two", Options: "i"})).As("all_i"),
		firestore.RegexFindAll("words", "TWO").As("all_cs"),
		firestore.RegexFindAll("words", "four").As("all_none"))))
	want("regex_find regex(match, i)", got["find_i"], "MATCH")
	want("regex_find the whole match", got["find_whole"], "a1")
	want("regex_find case-sensitive", got["find_cs"], "MATCH")
	want("regex_find no match", got["find_none"], nil)
	want("regex_find_all regex(two, i)", got["all_i"], []any{"TWO", "Two"})
	want("regex_find_all case-sensitive", got["all_cs"], []any{"TWO"})
	want("regex_find_all none", got["all_none"], []any{})
	refused("regex_find invalid", base().Select(firestore.Fields(firestore.RegexFind("s", "(").As("r"))))

	// Arrays: a = [1,5,2,4,3], nulls = [1,null,3,null], sparse = [5,null,1].
	got = one("arrays", base().Select(firestore.Fields(
		firestore.ArrayFirstN("a", 3).As("first3"),
		firestore.ArrayFirstN("a", 9).As("first9"),
		firestore.ArrayLastN("a", 3).As("last3"),
		firestore.ArrayLastN("a", 0).As("last0"),
		firestore.ArrayIndexOfAll("nulls", nil).As("idx_null"),
		firestore.ArrayIndexOfAll("a", 7).As("idx_none"),
		firestore.ArrayMinimumN("a", 3).As("min3"),
		firestore.ArrayMinimumN("sparse", 3).As("min_sparse"),
		firestore.ArrayMaximumN("a", 3).As("max3"),
		firestore.ArrayMaximumN("sparse", 3).As("max_sparse"),
		firestore.Offset("a", 1).As("off1"),
		firestore.Offset("a", -1).As("off_last"),
		firestore.Offset("a", 9).As("off_out"),
		firestore.Offset("s", 0).As("off_not_array"))))
	want("array_first_n 3", got["first3"], []any{int64(1), int64(5), int64(2)})
	want("array_first_n past the end", got["first9"], []any{int64(1), int64(5), int64(2), int64(4), int64(3)})
	want("array_last_n 3", got["last3"], []any{int64(2), int64(4), int64(3)})
	want("array_last_n 0", got["last0"], []any{})
	want("array_index_of_all null", got["idx_null"], []any{int64(1), int64(3)})
	want("array_index_of_all none", got["idx_none"], []any{})
	want("minimum_n 3", got["min3"], []any{int64(1), int64(2), int64(3)})
	want("minimum_n nulls ignored", got["min_sparse"], []any{int64(1), int64(5)})
	want("maximum_n 3", got["max3"], []any{int64(5), int64(4), int64(3)})
	want("maximum_n nulls ignored", got["max_sparse"], []any{int64(5), int64(1)})
	want("offset 1", got["off1"], int64(5))
	want("offset -1", got["off_last"], int64(3))
	for _, k := range []string{"off_out", "off_not_array"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %#v, want absent", k, v)
		}
	}
	refused("array_first_n negative", base().Select(firestore.Fields(firestore.ArrayFirstN("a", -1).As("r"))))
	refused("maximum_n negative", base().Select(firestore.Fields(firestore.ArrayMaximumN("a", -1).As("r"))))

	// Lambdas.
	got = one("lambdas", base().Select(firestore.Fields(
		firestore.ArrayFilter("nulls", "x", firestore.GreaterThan(x, 1)).As("filter"),
		firestore.ArrayFilter("a", "x", firestore.GreaterThan(x, firestore.FieldOf("x"))).As("filter_row"),
		firestore.ArrayTransform("a", "x", firestore.Multiply(x, 2)).As("double"),
		firestore.ArrayTransformWithIndex(firestore.ConstantOf([]int{10, 20}), "x", "i", firestore.Add(x, firestore.Variable("i"))).As("plus_i"))))
	want("array_filter [1,null,3,null] x > 1", got["filter"], []any{int64(3)})
	want("array_filter x > the row's x", got["filter_row"], []any{int64(5), int64(4)})
	want("array_transform x * 2", got["double"], []any{int64(2), int64(10), int64(4), int64(8), int64(6)})
	want("array_transform [10,20] x + i", got["plus_i"], []any{int64(10), int64(21)})

	// Timestamps: ts is Friday 2001-03-16 04:00 UTC.
	got = one("timestamps", base().Select(firestore.Fields(
		firestore.TimestampTruncate("ts", "week(friday)").As("wfri"),
		firestore.TimestampTruncate("ts", "week").As("wsun"),
		firestore.TimestampTruncate("ts", "isoweek").As("iso"),
		firestore.TimestampTruncate("ts", "quarter").As("q"),
		firestore.TimestampTruncate("ts", "isoyear").As("isoyear"),
		firestore.TimestampTruncateWithTimezone("jan", "month", "GMT+06:32:43").As("gmt"),
		firestore.TimestampTruncateWithTimezone("ts", "day", "America/Los_Angeles").As("la"),
		firestore.TimestampExtract("ts", "week").As("week"),
		firestore.TimestampExtract("ts", "dayofyear").As("doy"),
		firestore.TimestampExtractWithTimezone("nye", "year", "Asia/Tokyo").As("tokyo"),
		firestore.TimestampDiff("ts", firestore.ConstantOf(time.Date(2001, 3, 17, 4, 0, 0, 0, time.UTC)), "day").As("diff_neg"),
		firestore.TimestampDiff("ts", firestore.ConstantOf(time.Date(2001, 3, 16, 3, 59, 1, 0, time.UTC)), "minute").As("diff_trunc"),
		firestore.TimestampDiff("ts", firestore.ConstantOf(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)), "microsecond").As("diff_long"))))
	wantTime := func(what string, got any, w time.Time) {
		t.Helper()
		if g, ok := got.(time.Time); !ok || !g.Equal(w) {
			t.Errorf("%s = %v, want %v", what, got, w)
		}
	}
	wantTime("timestamp_trunc week(friday)", got["wfri"], time.Date(2001, 3, 16, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc week", got["wsun"], time.Date(2001, 3, 11, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc isoweek", got["iso"], time.Date(2001, 3, 12, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc quarter", got["q"], time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc isoyear", got["isoyear"], time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc month GMT+06:32:43", got["gmt"], time.Date(2026, 1, 1, 6, 32, 43, 0, time.UTC))
	wantTime("timestamp_trunc day Los Angeles", got["la"], time.Date(2001, 3, 15, 8, 0, 0, 0, time.UTC))
	// 2001 began on a Monday: its first Sunday, 7 January, starts week 1.
	want("timestamp_extract week", got["week"], int64(10))
	want("timestamp_extract dayofyear", got["doy"], int64(75))
	want("timestamp_extract year Asia/Tokyo", got["tokyo"], int64(2026))
	want("timestamp_diff negative", got["diff_neg"], int64(-1))
	want("timestamp_diff truncates", got["diff_trunc"], int64(0))
	want("timestamp_diff over two millennia", got["diff_long"], int64(63120312000000000))
	refused("timestamp_trunc bad zone", base().Select(firestore.Fields(firestore.TimestampTruncateWithTimezone("ts", "day", "Mars/Olympus").As("r"))))

	// General: cmp across types, switch_on, rand, current_document and
	// map_entries.
	got = one("general", base().Select(firestore.Fields(
		firestore.Cmp(firestore.ConstantOf(1.0), 1).As("cmp_eq"),
		firestore.Cmp(firestore.ConstantOf(42.5), "foo").As("cmp_types"),
		firestore.Cmp(firestore.ConstantOfNull(), firestore.FieldOf("missing")).As("cmp_null_absent"),
		firestore.Cmp("x", 1).As("cmp_gt"),
		firestore.SwitchOn(firestore.Equal("x", 1), "one", firestore.Equal("x", 2), "two", "other").As("sw"),
		firestore.SwitchOn(firestore.Equal("x", 3), "three").As("sw_hit"),
		firestore.IfError(firestore.SwitchOn(firestore.Equal("x", 1), "one"), "none").As("sw_none"),
		firestore.Rand().As("rand"),
		firestore.MapEntries("m").As("entries"),
		firestore.MapGet(firestore.CurrentDocument(), "x").As("doc_x"))))
	want("cmp 1.0 1", got["cmp_eq"], int64(0))
	want("cmp 42.5 foo", got["cmp_types"], int64(-1))
	want("cmp null absent", got["cmp_null_absent"], int64(0))
	want("cmp 3 1", got["cmp_gt"], int64(1))
	want("switch_on default", got["sw"], "other")
	want("switch_on match", got["sw_hit"], "three")
	want("switch_on no match, no default, is an error", got["sw_none"], "none")
	if r, ok := got["rand"].(float64); !ok || r < 0 || r >= 1 {
		t.Errorf("rand = %#v, want a float64 in [0, 1)", got["rand"])
	}
	want("map_entries", got["entries"], []any{map[string]any{"k": "foo", "v": int64(2)}})
	want("current_document's x", got["doc_x"], int64(3))
	refused("switch_on with no match outside where", base().Select(firestore.Fields(firestore.SwitchOn(firestore.Equal("x", 1), "one").As("r"))))

	// error and current_context, as raw functions through the generated
	// client.
	gapic := firestoreGAPIC(t, h)
	fn := func(name string, args ...*firestorepb.Value) *firestorepb.Value {
		return &firestorepb.Value{ValueType: &firestorepb.Value_FunctionValue{FunctionValue: &firestorepb.Function{Name: name, Args: args}}}
	}
	str := func(s string) *firestorepb.Value {
		return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: s}}
	}
	selectOf := func(fields map[string]*firestorepb.Value) (*firestorepb.Document, error) {
		stream, err := gapic.ExecutePipeline(ctx, &firestorepb.ExecutePipelineRequest{
			Database: strings.TrimSuffix(dbName, "/"),
			PipelineType: &firestorepb.ExecutePipelineRequest_StructuredPipeline{StructuredPipeline: &firestorepb.StructuredPipeline{
				Pipeline: &firestorepb.Pipeline{Stages: []*firestorepb.Pipeline_Stage{
					{Name: "collection", Args: []*firestorepb.Value{{ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: "/f"}}}},
					{Name: "select", Args: []*firestorepb.Value{{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{Fields: fields}}}}},
				}}}},
		})
		if err != nil {
			return nil, err
		}
		var doc *firestorepb.Document
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return doc, nil
			}
			if err != nil {
				return nil, err
			}
			if len(resp.GetResults()) > 0 {
				doc = resp.GetResults()[0]
			}
		}
	}
	d, err := selectOf(map[string]*firestorepb.Value{
		"caught":  fn("if_error", fn("error", str("no condition matched")), str("caught")),
		"iserr":   fn("is_error", fn("error", str("boom"))),
		"ctx_x":   fn("map_get", fn("current_context"), str("x")),
		"sw_safe": fn("switch_on", fn("equal", &firestorepb.Value{ValueType: &firestorepb.Value_FieldReferenceValue{FieldReferenceValue: "x"}}, &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: 3}}), str("three"), fn("error", str("no condition matched"))),
	})
	if err != nil || d == nil {
		t.Fatalf("raw select of error and current_context = %v, %v", d, err)
	}
	if v := d.GetFields()["caught"].GetStringValue(); v != "caught" {
		t.Errorf("if_error(error(...), caught) = %v, want caught", d.GetFields()["caught"])
	}
	if v := d.GetFields()["iserr"].GetBooleanValue(); !v {
		t.Errorf("is_error(error(...)) = %v, want true", d.GetFields()["iserr"])
	}
	if v := d.GetFields()["ctx_x"].GetIntegerValue(); v != 3 {
		t.Errorf("map_get(current_context(), x) = %v, want 3", d.GetFields()["ctx_x"])
	}
	if v := d.GetFields()["sw_safe"].GetStringValue(); v != "three" {
		t.Errorf("switch_on(x == 3, three, error(...)) = %v, want three", d.GetFields()["sw_safe"])
	}
	_, err = selectOf(map[string]*firestorepb.Value{"e": fn("error", str("no condition matched"))})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "no condition matched") {
		t.Errorf("error(no condition matched) = %v, want INVALID_ARGUMENT with the message", err)
	}
}
