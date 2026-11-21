package bigqueryfront

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// SparseTensor inputs and VarLenFeature tf.train.Example features
// (#1775): testdata/tensorflow's models are the TensorFlow converter's
// output (deploy/tfconvert/convert.py, TensorFlow 2.20.0) for
// test/compat/testdata/tensorflow/gen_import.py's sparsein and sparse
// SavedModels, and sparse_predictions.json TensorFlow's own
// serving_default output for each row, its SparseTensors fed as
// "BigQuery ML automatically converts them into Sparse format" (a row's n
// values at [0, 0] .. [0, n-1]). Run on internal/tflitemodel through the
// front's columns, every output matches TensorFlow's, an empty ARRAY (no
// values) among them.
func TestTensorFlowSparseInputs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "tensorflow", "sparse_predictions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ref map[string][]struct {
		Inputs  map[string]json.RawMessage `json:"inputs"`
		Outputs map[string][]float64       `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sparsein", "varlen"} {
		data, err := os.ReadFile(filepath.Join("testdata", "tensorflow", name+".tflite"))
		if err != nil {
			t.Fatal(err)
		}
		imp, code, msg := tflitePorts("gs://b/"+name, data, "TensorFlow")
		if code != 0 {
			t.Fatalf("%s: %d %s", name, code, msg)
		}
		sparse := map[string]bool{}
		for _, c := range imp.Inputs {
			if c.Sparse {
				sparse[c.Name] = true
			}
		}
		if !sparse["v"] || !sparse["ids"] || len(sparse) != 2 {
			t.Fatalf("%s: sparse inputs %v", name, sparse)
		}
		m, err := tflitemodel.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		for i, row := range ref[name] {
			var ins []tflitemodel.Tensor
			for _, c := range imp.Inputs {
				tn, why := tfliteTensor(c, row.Inputs[c.Name])
				if why != "" {
					t.Fatalf("%s row %d %s: %s", name, i, c.Name, why)
				}
				ins = append(ins, tn)
			}
			outs, err := m.Run(ins)
			if err != nil {
				t.Fatalf("%s row %d: %v", name, i, err)
			}
			for k, o := range imp.Outputs {
				want := row.Outputs[o.Name]
				got := outs[k]
				if got.Len() != len(want) {
					t.Fatalf("%s row %d %s: %d values, TensorFlow %d", name, i, o.Name, got.Len(), len(want))
				}
				for j, w := range want {
					if g := float64(got.F[j]); math.Abs(g-w) > 1e-5*math.Max(1, math.Abs(w)) {
						t.Errorf("%s row %d %s[%d] = %v, TensorFlow %v", name, i, o.Name, j, g, w)
					}
				}
			}
		}
	}
}

// A SparseTensor input takes a NULL (no values), a value or an ARRAY, and
// at most its fixed dense dimension's values.
func TestTensorFlowSparseColumn(t *testing.T) {
	c := tflitePortCol{Name: "v", Type: "FLOAT64", Array: true, Shape: []int{-1, -1}, Sparse: true, SparseDims: []int{4}}
	for in, n := range map[string]int{`null`: 0, `2.5`: 1, `[]`: 0, `[1, 2, 3]`: 3} {
		tn, why := tfliteTensor(c, json.RawMessage(in))
		if why != "" || tn.Len() != n || len(tn.Shape) != 2 || tn.Shape[0] != 1 || tn.Shape[1] != n {
			t.Errorf("%s: %v %q, want %d values", in, tn.Shape, why, n)
		}
	}
	if _, why := tfliteTensor(c, json.RawMessage(`[1, 2, 3, 4, 5]`)); !strings.Contains(why, "at most 4") {
		t.Errorf("five values for [batch, 4]: %q", why)
	}
}
