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

// TestBigQueryMLImportedTFLiteOps4 (#1775): imported TENSORFLOW_LITE
// models of the builtin operators internal/tflitemodel's rnn.go, ops4.go
// and resource.go add, through the official bigquery and storage Go
// clients, against LiteRT's reference kernels' outputs
// (internal/tflitemodel/testdata/gen_ops4.py, TensorFlow 2.20):
//   - rnn4.tflite: LSTM, UNIDIRECTIONAL_SEQUENCE_LSTM (CIFG, peephole,
//     projection, clipping, layer norm), BIDIRECTIONAL_SEQUENCE_LSTM (with
//     and without the auxiliary input), RNN, UNIDIRECTIONAL_SEQUENCE_RNN and
//     BIDIRECTIONAL_SEQUENCE_RNN;
//   - ops4.tflite: NON_MAX_SUPPRESSION_V4 and V5, CONV_3D,
//     CONV_3D_TRANSPOSE, RFFT2D, REAL, IMAG and COMPLEX_ABS;
//   - table4.tflite: a vocabulary's HASHTABLE, HASHTABLE_FIND,
//     HASHTABLE_SIZE, initialized by CALL_ONCE and HASHTABLE_IMPORT;
//   - var4.tflite: VAR_HANDLE, READ_VARIABLE, ASSIGN_VARIABLE;
//   - random4.tflite: seeded RANDOM_UNIFORM and RANDOM_STANDARD_NORMAL;
//   - ops5.tflite: WHERE, UNIQUE, DYNAMIC_UPDATE_SLICE, MATRIX_SET_DIAG,
//     REVERSE_SEQUENCE, SPARSE_TO_DENSE, BROADCAST_ARGS, L2_POOL_2D,
//     BITWISE_XOR, RIGHT_SHIFT, FAKE_QUANT, SVDF, HASHTABLE_LOOKUP, DILATE
//     and STABLEHLO_ADD, _MULTIPLY, _MAXIMUM, _MINIMUM, _AND, _SHIFT_LEFT,
//     _PAD and _COMPOSITE;
//   - ops6.tflite: BITCAST, EMBEDDING_LOOKUP_SPARSE and the
//     TFLite_Detection_PostProcess custom operator (fast and regular NMS);
//   - audio4.tflite: the AudioSpectrogram and Mfcc custom operators;
//   - hlo4.tflite: STABLEHLO_GATHER, STABLEHLO_SCATTER and
//     STABLEHLO_REDUCE_WINDOW.
//
// Numbers match to 1e-4 and strings exactly. Every input has a fixed
// shape, so one row holds each whole tensor, row-major.
//
// covers: bigquery.jobs.query
func TestBigQueryMLImportedTFLiteOps4(t *testing.T) {
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
	models := []string{"rnn4", "ops4", "ops5", "ops6", "audio4", "hlo4", "table4", "var4", "random4"}
	for _, m := range models {
		w := bh.Object("tflo4/" + m + ".tflite").NewWriter(ctx)
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
	if err := json.Unmarshal(read("litert_ops4.json"), &ref); err != nil {
		t.Fatal(err)
	}
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
	// The ports that are not FLOAT32: table4's tokens (STRING), and the
	// integer ones (INT64).
	types := map[string]string{"tokens": "STRING", "codes": "INT64", "u": "INT64", "look": "INT64", "q": "INT64", "ind": "INT64"}
	var cols []string
	inputs := map[string][]string{}
	for _, m := range models {
		for name, v := range ref[m].Inputs {
			typ := types[name]
			if typ == "" {
				typ = "FLOAT64"
			}
			var s []string
			for _, e := range flat(v) {
				switch x := e.(type) {
				case string:
					s = append(s, "'"+x+"'")
				default:
					s = append(s, fmt.Sprint(x))
				}
			}
			col := m + "_" + name
			cols = append(cols, fmt.Sprintf("ARRAY<%s>[%s] AS %s", typ, strings.Join(s, ", "), col))
			inputs[m] = append(inputs[m], col+" AS "+name)
		}
	}
	sort.Strings(cols)
	name := mlDataset(t, h, c, map[string]string{"rows": "SELECT " + strings.Join(cols, ", ")})
	for _, m := range models {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tflo4/%s.tflite')", name, m, b, m)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m, err)
		}
		r := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.%s`, (SELECT %s FROM `%[1]s.rows`))", name, m, strings.Join(inputs[m], ", ")))
		if len(r) != 1 {
			t.Fatalf("%s: %d rows", m, len(r))
		}
		var outs []string
		for col := range ref[m].Outputs {
			// WHERE's and UNIQUE's outputs have a size only the run knows;
			// the model declares a placeholder shape, which the port mapping
			// follows, so the unit test checks them, not this one.
			if m == "ops5" && (col == "where" || col == "unique_y") {
				continue
			}
			outs = append(outs, col)
		}
		sort.Strings(outs)
		for _, col := range outs {
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
				if ws, ok := w.(string); ok {
					if gs[k] != ws {
						t.Errorf("%s %s[%d] = %v, LiteRT %q", m, col, k, gs[k], ws)
					}
					continue
				}
				var g float64
				switch v := gs[k].(type) {
				case float64:
					g = v
				case int64:
					g = float64(v)
				}
				if wf := w.(float64); math.Abs(g-wf) > 1e-4*math.Max(1, math.Abs(wf)) {
					t.Errorf("%s %s[%d] = %v, LiteRT %v", m, col, k, gs[k], wf)
				}
			}
		}
	}
}
