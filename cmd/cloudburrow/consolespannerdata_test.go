package main

import (
	"bytes"
	"encoding/json"
	"testing"

	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
)

// The Data tab's types in both dialects, and an ARRAY's JSON text as
// Spanner's wire values (#1859).
func TestSpannerDataTabTypes(t *testing.T) {
	for in, want := range map[string]string{
		"INT64": "INT64", "STRING(MAX)": "STRING", "ARRAY<INT64>": "ARRAY<INT64>", "ARRAY<STRING(64)>": "ARRAY<STRING>",
		"bigint": "INT64", "character varying(20)": "STRING", "character varying[]": "ARRAY<STRING>",
		"timestamp with time zone": "TIMESTAMP", "numeric": "NUMERIC/PG_NUMERIC", "NUMERIC": "NUMERIC", "jsonb": "JSON/PG_JSONB",
		"PROTO<x.Y>": "", "ARRAY<ARRAY<INT64>>": "", "STRUCT<a INT64>": "",
	} {
		got := ""
		if ty := spannerColumnType(in); ty != nil {
			got = ty.GetCode().String()
			if ty.GetCode() == sppb.TypeCode_ARRAY {
				got = "ARRAY<" + ty.GetArrayElementType().GetCode().String() + ">"
			}
			if a := ty.GetTypeAnnotation(); a != sppb.TypeAnnotationCode_TYPE_ANNOTATION_CODE_UNSPECIFIED {
				got += "/" + a.String()
			}
		}
		if got != want {
			t.Errorf("spannerColumnType(%q) = %q, want %q", in, got, want)
		}
	}
	v, err := spannerWireValue(spannerColumnType("ARRAY<INT64>"), `[1, "2", null]`)
	if err != nil {
		t.Fatal(err)
	}
	if b := compactJSON(t, v); b != `["1","2",null]` {
		t.Errorf("ARRAY<INT64> [1, \"2\", null] = %s", b)
	}
	if _, err := spannerWireValue(spannerColumnType("ARRAY<INT64>"), `[1.5]`); err == nil {
		t.Error("ARRAY<INT64> [1.5] was accepted")
	}
	if _, err := spannerWireValue(spannerColumnType("ARRAY<BOOL>"), `true`); err == nil {
		t.Error("an ARRAY that is not a JSON array was accepted")
	}
	if v, err := spannerWireValue(spannerColumnType("ARRAY<FLOAT64>"), `[0.5, "NaN"]`); err != nil {
		t.Error(err)
	} else if b := compactJSON(t, v); b != `[0.5,"NaN"]` {
		t.Errorf("ARRAY<FLOAT64> = %s", b)
	}
	d := spannerDialect{pg: true}
	if d.quote(`a"b`) != `"a""b"` || (spannerDialect{}).quote("a") != "`a`" {
		t.Error("identifiers are not quoted in the dialect")
	}
	b := &spannerBinder{d: d, params: map[string]any{}}
	if b.bind("x", 1) != "$1" || b.bind("y", 2) != "$2" || b.params["p2"] != 2 {
		t.Errorf("PostgreSQL parameters are %v", b.params)
	}
}

func compactJSON(t *testing.T, v interface{ MarshalJSON() ([]byte, error) }) string {
	t.Helper()
	b, err := v.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.String()
}
