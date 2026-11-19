//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLImportedTFLiteOps3 (#1775): imported TENSORFLOW_LITE
// models of the builtin operators internal/tflitemodel/ops3.go adds,
// through the official bigquery and storage Go clients, against LiteRT's
// own predictions (ai-edge-litert 2.3.0; internal/tflitemodel/testdata/
// gen_ops3.py): ops3.tflite (TRANSPOSE_CONV, MIRROR_PAD, DEPTH_TO_SPACE,
// SPACE_TO_DEPTH, SPACE_TO_BATCH_ND, BATCH_TO_SPACE_ND, L2_NORMALIZATION,
// LOCAL_RESPONSE_NORMALIZATION, GATHER_ND, SCATTER_ND, ONE_HOT, RANGE,
// REVERSE_V2, BROADCAST_TO, CUMSUM, REDUCE_ANY, REDUCE_ALL, TOPK_V2, SIGN,
// ATAN2, BUCKETIZE, MATRIX_DIAG, SEGMENT_SUM and the UNSORTED_SEGMENT_*
// reductions) and lstm.tflite (Keras LSTM and bidirectional LSTM layers,
// which TensorFlow writes as WHILE loops, and PRELU), each to 1e-4. The
// models' inputs have a fixed batch of 2, so one row holds the whole
// tensor, row-major.
//
// covers: bigquery.jobs.query
func TestBigQueryMLImportedTFLiteOps3(t *testing.T) {
	h := New(t)
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
	models := []string{"ops3", "lstm"}
	for _, m := range models {
		w := bh.Object("tflo/" + m + ".tflite").NewWriter(ctx)
		if _, err := w.Write(read(m + ".tflite")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", m, err)
		}
	}
	var ref map[string]struct {
		Inputs  map[string]any `json:"inputs"`
		Outputs map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(read("litert_ops3.json"), &ref); err != nil {
		t.Fatal(err)
	}
	var flat func(v any) []float64
	flat = func(v any) []float64 {
		a, ok := v.([]any)
		if !ok {
			return []float64{v.(float64)}
		}
		var out []float64
		for _, e := range a {
			out = append(out, flat(e)...)
		}
		return out
	}
	arr := func(typ string, vs []float64) string {
		var s []string
		for _, v := range vs {
			s = append(s, fmt.Sprint(v))
		}
		return "ARRAY<" + typ + ">[" + strings.Join(s, ", ") + "]"
	}
	o3, ls := ref["ops3"], ref["lstm"]
	name := mlDataset(t, h, c, map[string]string{
		"rows": fmt.Sprintf("SELECT %s AS image, %s AS ids, %s AS x, %s AS seq",
			arr("FLOAT64", flat(o3.Inputs["image"])), arr("INT64", flat(o3.Inputs["ids"])), arr("FLOAT64", flat(o3.Inputs["x"])),
			arr("FLOAT64", flat(ls.Inputs["seq"]))),
	})
	queries := map[string]string{"ops3": "SELECT image, ids, x", "lstm": "SELECT seq"}
	for _, m := range models {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tflo/%s.tflite')", name, m, b, m)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m, err)
		}
		r := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.%s`, (%s FROM `%[1]s.rows`))", name, m, queries[m]))
		if len(r) != 1 {
			t.Fatalf("%s: %d rows", m, len(r))
		}
		var cols []string
		for col := range ref[m].Outputs {
			cols = append(cols, col)
		}
		sort.Strings(cols)
		for _, col := range cols {
			want := flat(ref[m].Outputs[col])
			var gs []bigquery.Value
			switch g := r[0][col].(type) {
			case []bigquery.Value:
				gs = g
			default:
				gs = []bigquery.Value{g}
			}
			if len(gs) != len(want) {
				t.Errorf("%s %s: %d values, LiteRT %d", m, col, len(gs), len(want))
				continue
			}
			for k, w := range want {
				var g float64
				switch v := gs[k].(type) {
				case float64:
					g = v
				case int64:
					g = float64(v)
				case bool:
					if v {
						g = 1
					}
				}
				if math.Abs(g-w) > 1e-4*math.Max(1, math.Abs(w)) {
					t.Errorf("%s %s[%d] = %v, LiteRT %v", m, col, k, gs[k], w)
				}
			}
		}
	}
}
