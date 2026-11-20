package tflitemodel

import (
	"encoding/json"
	"math"
	"os"
	"slices"
	"testing"
)

// STABLEHLO_CASE (#1775): hlocase.tflite is CloudBurrow's TensorFlow
// converter's output (deploy/tfconvert, TensorFlow 2.20.0's
// TFLiteConverter, builtins only) for
// test/compat/testdata/tensorflow/gen_import.py's hlocase SavedModel, an
// XlaCallModule whose StableHLO module is a stablehlo.case of three
// branches, as jax2tf writes a lax.switch. tensorflow_hlocase.json is
// TensorFlow's own output for each row (LiteRT 2.20's STABLEHLO_CASE
// kernel does not prepare, so TensorFlow is the reference): an index
// below zero or past the last branch runs the last one.
func TestStablehloCaseMatchesTensorFlow(t *testing.T) {
	data, err := os.ReadFile("testdata/hlocase.tflite")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Uses(), "STABLEHLO_CASE") || !slices.Contains(Ops(), "STABLEHLO_CASE") {
		t.Fatalf("uses %v", m.Uses())
	}
	raw, err := os.ReadFile("testdata/tensorflow_hlocase.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Inputs struct {
			I int64     `json:"i"`
			X []float32 `json:"x"`
		} `json:"inputs"`
		Outputs struct {
			Y []float64 `json:"y"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var ins []Tensor
		for _, p := range m.Inputs {
			switch p.Name {
			case "i":
				ins = append(ins, Tensor{Type: Int32, I: []int64{r.Inputs.I}})
			case "x":
				ins = append(ins, Tensor{Type: Float32, Shape: []int{3}, F: r.Inputs.X})
			default:
				t.Fatalf("input %s", p.Name)
			}
		}
		outs, err := m.Run(ins)
		if err != nil {
			t.Fatalf("i=%d: %v", r.Inputs.I, err)
		}
		for j, w := range r.Outputs.Y {
			if g := float64(outs[0].F[j]); math.Abs(g-w) > 1e-6 {
				t.Errorf("i=%d y[%d] = %v, TensorFlow %v", r.Inputs.I, j, g, w)
			}
		}
	}
}
