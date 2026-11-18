//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLImportedTFLiteGaps (#1766): imported TENSORFLOW_LITE
// models of what internal/tflitemodel first refused, through the official
// bigquery and storage Go clients, against LiteRT's own predictions
// (ai-edge-litert 2.3.0; internal/tflitemodel/testdata/gen_conv.py and
// gen_quant.py): CONV_2D, DEPTHWISE_CONV_2D, MAX_POOL_2D, AVERAGE_POOL_2D
// and GATHER (to 1e-4); a full integer (int8) model, its ARRAY<INT64>
// input and output, within two steps of its output's scale; and STRING
// tensors, a STRING input reshaped and a STRING constant gathered, which
// models.get names STRING.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLImportedTFLiteGaps(t *testing.T) {
	h := New(t)
	// Cloud Storage under the emulators shard's own name for it, as
	// TestBigQueryMLImportedXGBoostModel (#1761), so the Cloud Storage suite
	// stays in the storage shard and this test runs in emulators.
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	dir := filepath.Join("..", "..", "internal", "tflitemodel", "testdata")
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, m := range []string{"conv", "qint8", "strings"} {
		w := bh.Object("tflg/" + m + ".tflite").NewWriter(ctx)
		if _, err := w.Write(read(m + ".tflite")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", m, err)
		}
	}
	type litert struct {
		Inputs  map[string]any `json:"inputs"`
		Outputs map[string]any `json:"outputs"`
		Tol     float64        `json:"tol"`
	}
	load := func(f string) map[string]litert {
		var out map[string]litert
		if err := json.Unmarshal(read(f), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	conv, quant, strs := load("litert_conv.json")["conv"], load("litert_quant.json")["qint8"], load("litert_strings.json")["strings"]
	// flat is batch item 0 of a value, flattened.
	var flat func(v any) []any
	flat = func(v any) []any {
		a, ok := v.([]any)
		if !ok {
			return []any{v}
		}
		var out []any
		for _, e := range a {
			out = append(out, flat(e)...)
		}
		return out
	}
	first := func(v any) []any { return flat(v.([]any)[0]) }
	arr := func(typ string, vs []any) string {
		var s []string
		for _, v := range vs {
			switch x := v.(type) {
			case string:
				s = append(s, fmt.Sprintf("%q", x))
			default:
				s = append(s, fmt.Sprint(x))
			}
		}
		return "ARRAY<" + typ + ">[" + strings.Join(s, ", ") + "]"
	}
	name := mlDataset(t, h, c, map[string]string{
		"rows": fmt.Sprintf("SELECT %s AS image, %s AS ids, %s AS qimage, %s AS words, %s AS sids",
			arr("FLOAT64", first(conv.Inputs["image"])), arr("INT64", first(conv.Inputs["ids"])), arr("INT64", first(quant.Inputs["image"])),
			arr("STRING", first(strs.Inputs["words"])), arr("INT64", first(strs.Inputs["ids"]))),
	})
	create := func(m string) {
		t.Helper()
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tflg/%s.tflite')", name, m, b, m)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m, err)
		}
	}
	nums := func(model, col string, got bigquery.Value, want []any, tol func(float64) float64) {
		t.Helper()
		gs, _ := got.([]bigquery.Value)
		if len(gs) != len(want) {
			t.Fatalf("%s %s: %d values, LiteRT %d: %v", model, col, len(gs), len(want), got)
		}
		for k, w := range want {
			wf := w.(float64)
			var g float64
			switch x := gs[k].(type) {
			case float64:
				g = x
			case int64:
				g = float64(x)
			}
			if math.Abs(g-wf) > tol(wf) {
				t.Errorf("%s %s[%d] = %v, LiteRT %v", model, col, k, gs[k], wf)
			}
		}
	}

	create("conv")
	r := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.conv`, (SELECT image, ids FROM `%[1]s.rows`))", name))
	if len(r) != 1 {
		t.Fatalf("conv: %d rows", len(r))
	}
	for _, col := range []string{"depthwise", "maxpool", "avgpool", "strided", "embed"} {
		nums("conv", col, r[0][col], first(conv.Outputs[col]), func(w float64) float64 { return 1e-4 * math.Max(1, math.Abs(w)) })
	}

	create("qint8")
	r = mlQuery(t, h, c, fmt.Sprintf("SELECT logits FROM ML.PREDICT(MODEL `%s.qint8`, (SELECT qimage AS image FROM `%[1]s.rows`))", name))
	if len(r) != 1 {
		t.Fatalf("qint8: %d rows", len(r))
	}
	nums("qint8", "logits", r[0]["logits"], first(quant.Outputs["logits"]), func(float64) float64 { return quant.Tol })

	create("strings")
	md, err := c.Dataset(name).Model("strings").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fc, _ := md.RawFeatureColumns()
	types := map[string]string{}
	for _, f := range fc {
		types[f.Name] = string(f.Type.ArrayElementType.TypeKind)
	}
	if types["words"] != "STRING" {
		t.Errorf("feature columns %+v", types)
	}
	r = mlQuery(t, h, c, fmt.Sprintf("SELECT names, cols FROM ML.PREDICT(MODEL `%s.strings`, (SELECT words, sids AS ids FROM `%[1]s.rows`))", name))
	if len(r) != 1 {
		t.Fatalf("strings: %d rows", len(r))
	}
	for _, col := range []string{"names", "cols"} {
		var got []any
		for _, v := range r[0][col].([]bigquery.Value) {
			got = append(got, v)
		}
		if want := first(strs.Outputs[col]); !reflect.DeepEqual(got, want) {
			t.Errorf("strings %s = %v, LiteRT %v", col, got, want)
		}
	}
}
