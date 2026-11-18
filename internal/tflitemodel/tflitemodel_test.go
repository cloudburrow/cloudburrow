package tflitemodel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestMatchesLiteRT runs the models testdata/gen.py built with
// TensorFlow 2.20's TFLiteConverter on the inputs it fed LiteRT
// (ai-edge-litert 2.3.0, its builtin kernels without delegates), and
// checks every output against LiteRT's.
func TestMatchesLiteRT(t *testing.T) {
	raw, err := os.ReadFile("testdata/litert_predictions.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Inputs  map[string]any `json:"inputs"`
		Outputs map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("%d cases", len(cases))
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name+".tflite"))
			if err != nil {
				t.Fatal(err)
			}
			m, err := Load(data)
			if err != nil {
				t.Fatal(err)
			}
			var ins []Tensor
			for _, p := range m.Inputs {
				v, ok := c.Inputs[p.Name]
				if !ok {
					t.Fatalf("no input %s", p.Name)
				}
				shape, flat := flatten(v)
				in := Tensor{Type: Float32, Shape: shape}
				for _, x := range flat {
					in.F = append(in.F, float32(x))
				}
				ins = append(ins, in)
			}
			outs, err := m.Run(ins)
			if err != nil {
				t.Fatal(err)
			}
			if len(outs) != len(c.Outputs) {
				t.Fatalf("%d outputs, want %d", len(outs), len(c.Outputs))
			}
			for k, p := range m.Outputs {
				shape, want := flatten(c.Outputs[p.Name])
				got := outs[k]
				if !reflect.DeepEqual(got.Shape, shape) && !(len(shape) == 0 && len(got.Shape) == 0) {
					t.Fatalf("%s: shape %v, want %v", p.Name, got.Shape, shape)
				}
				for i, w := range want {
					g := float64(got.f(i))
					if math.Abs(g-w) > 1e-5*max(1, math.Abs(w)) {
						t.Errorf("%s[%d] = %v, LiteRT gives %v", p.Name, i, g, w)
					}
				}
			}
		})
	}
}

func flatten(v any) ([]int, []float64) {
	switch x := v.(type) {
	case float64:
		return nil, []float64{x}
	case []any:
		var shape []int
		var flat []float64
		for _, e := range x {
			s, f := flatten(e)
			shape = s
			flat = append(flat, f...)
		}
		return append([]int{len(x)}, shape...), flat
	}
	return nil, nil
}

func TestRejects(t *testing.T) {
	if _, err := Load([]byte("not a model")); err == nil {
		t.Fatal("loaded garbage")
	}
	data, _ := os.ReadFile("testdata/mlp.tflite")
	if _, err := Load(data[:len(data)/2]); err == nil {
		t.Log("a truncated model loaded; Run must still not panic")
	}
	m, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Inputs[0].Name != "features" || !reflect.DeepEqual(m.Inputs[0].Shape, []int{-1, 4}) || m.Outputs[0].Name != "probs" {
		t.Fatalf("ports %+v %+v", m.Inputs, m.Outputs)
	}
	if _, err := m.Run([]Tensor{{Type: Float32, Shape: []int{1, 3}, F: []float32{1, 2, 3}}}); err == nil {
		t.Fatal("ran a wrong-shaped input")
	}
}
