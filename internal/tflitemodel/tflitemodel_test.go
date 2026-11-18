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
	type litertCase struct {
		Inputs  map[string]any `json:"inputs"`
		Outputs map[string]any `json:"outputs"`
		// Tol is a quantized model's absolute tolerance (gen_quant.py).
		Tol float64 `json:"tol"`
	}
	cases := map[string]litertCase{}
	// litert_conv.json is gen_conv.py's: the convolution, pooling and
	// gather operators; litert_quant.json gen_quant.py's quantized models
	// (#1766).
	for _, f := range []string{"testdata/litert_predictions.json", "testdata/litert_conv.json", "testdata/litert_quant.json"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var cs map[string]litertCase
		if err := json.Unmarshal(raw, &cs); err != nil {
			t.Fatal(err)
		}
		for k, v := range cs {
			cases[k] = v
		}
	}
	if len(cases) != 6 {
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
					tol := 1e-5 * max(1, math.Abs(w))
					if c.Tol > 0 {
						tol = c.Tol
					}
					if math.Abs(g-w) > tol {
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

// TestStringsMatchLiteRT (#1766): strings.tflite (gen_conv.py) reshapes a
// STRING input and gathers from a STRING constant, as LiteRT does.
func TestStringsMatchLiteRT(t *testing.T) {
	raw, err := os.ReadFile("testdata/litert_strings.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs map[string]struct {
		Inputs  map[string]any `json:"inputs"`
		Outputs map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	c := cs["strings"]
	data, err := os.ReadFile("testdata/strings.tflite")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	strs := func(v any) ([]int, []string) {
		var shape []int
		var flat []string
		var walk func(any, int)
		walk = func(x any, depth int) {
			switch e := x.(type) {
			case string:
				flat = append(flat, e)
			case []any:
				if len(shape) == depth {
					shape = append(shape, len(e))
				}
				for _, y := range e {
					walk(y, depth+1)
				}
			}
		}
		walk(v, 0)
		return shape, flat
	}
	var ins []Tensor
	for _, p := range m.Inputs {
		if p.Type == String {
			shape, flat := strs(c.Inputs[p.Name])
			ins = append(ins, Tensor{Type: String, Shape: shape, S: flat})
			continue
		}
		shape, flat := flatten(c.Inputs[p.Name])
		in := Tensor{Type: Int32, Shape: shape}
		for _, x := range flat {
			in.I = append(in.I, int64(x))
		}
		ins = append(ins, in)
	}
	outs, err := m.Run(ins)
	if err != nil {
		t.Fatal(err)
	}
	for k, p := range m.Outputs {
		shape, want := strs(c.Outputs[p.Name])
		if !reflect.DeepEqual(outs[k].Shape, shape) || !reflect.DeepEqual(outs[k].S, want) {
			t.Errorf("%s = %v %q; LiteRT gives %v %q", p.Name, outs[k].Shape, outs[k].S, shape, want)
		}
	}
	// Arithmetic on a STRING tensor is unsupported, not a crash.
	sx := &Tensor{Type: String, Shape: []int{1}, S: []string{"a"}}
	if _, err := m.eval(op{code: opAdd, name: "ADD"}, []*Tensor{sx, sx}); !IsUnsupported(err) {
		t.Errorf("ADD of strings: %v", err)
	}
}

// TestControlFlow (#1775): loop.tflite, TensorFlow 2.20's TFLiteConverter
// of testdata/savedmodel/loop (gen_savedmodel.py), runs its WHILE, IF,
// LESS, GREATER and SELECT_V2 to TensorFlow's own serving_default output
// for the SavedModel (tensorflow_predictions.json's "loop").
func TestControlFlow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "loop.tflite"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Loop []struct {
			Inputs  map[string][]float32 `json:"inputs"`
			Outputs map[string][]float64 `json:"outputs"`
		} `json:"loop"`
	}
	b, err := os.ReadFile(filepath.Join("testdata", "tensorflow_predictions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	for _, row := range ref.Loop {
		var ins []Tensor
		for _, p := range m.Inputs {
			vs := row.Inputs[p.Name]
			shape := []int{1, len(vs)}
			if len(p.Shape) == 1 {
				shape = []int{len(vs)}
			}
			ins = append(ins, Tensor{Type: Float32, Shape: shape, F: vs})
		}
		outs, err := m.Run(ins)
		if err != nil {
			t.Fatal(err)
		}
		for k, p := range m.Outputs {
			for i, w := range row.Outputs[p.Name] {
				if g := float64(outs[k].f(i)); math.Abs(g-w) > 1e-5*math.Max(1, math.Abs(w)) {
					t.Errorf("%v %s[%d] = %v, TensorFlow %v", row.Inputs, p.Name, i, g, w)
				}
			}
		}
	}
}

// A WHILE whose condition never turns false is stopped, not run forever.
func TestWhileBounded(t *testing.T) {
	m := &Model{graphs: []*graph{
		{tensors: []tensorDef{{name: "c", typ: Bool, data: &Tensor{Type: Bool, Shape: []int{}, B: []bool{true}}}, {name: "x", typ: Float32}},
			in: []int{1}, out: []int{0}},
		{tensors: []tensorDef{{name: "x", typ: Float32}}, in: []int{0}, out: []int{0}},
	}}
	_, err := m.while(0, 1, []*Tensor{{Type: Float32, Shape: []int{1}, F: []float32{1}}}, 0)
	if err == nil {
		t.Fatal("an endless WHILE returned")
	}
}
