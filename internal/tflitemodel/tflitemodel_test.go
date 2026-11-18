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

// TestControlFlowAndOps2 (#1775): loop.tflite and ops2.tflite, TensorFlow
// 2.20's TFLiteConverter of testdata/savedmodel/loop and ops2
// (gen_savedmodel.py), run their WHILE, IF, LESS, GREATER and SELECT_V2,
// and the operators ops2.go adds, to TensorFlow's own serving_default
// output for the SavedModels (tensorflow_predictions.json).
func TestControlFlowAndOps2(t *testing.T) {
	type rows []struct {
		Inputs  map[string][]float64 `json:"inputs"`
		Outputs map[string][]float64 `json:"outputs"`
	}
	var ref struct {
		Loop rows `json:"loop"`
		Ops2 rows `json:"ops2"`
	}
	b, err := os.ReadFile(filepath.Join("testdata", "tensorflow_predictions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]rows{"loop": ref.Loop, "ops2": ref.Ops2} {
		data, err := os.ReadFile(filepath.Join("testdata", model+".tflite"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := Load(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range want {
			var ins []Tensor
			for _, p := range m.Inputs {
				vs := row.Inputs[p.Name]
				shape := []int{1, len(vs)}
				if len(p.Shape) == 1 {
					shape = []int{len(vs)}
				}
				in := Tensor{Type: p.Type, Shape: shape}
				for _, v := range vs {
					if p.Type.IsInt() {
						in.I = append(in.I, int64(v))
					} else {
						in.F = append(in.F, float32(v))
					}
				}
				ins = append(ins, in)
			}
			outs, err := m.Run(ins)
			if err != nil {
				t.Fatalf("%s: %v", model, err)
			}
			for k, p := range m.Outputs {
				w := row.Outputs[p.Name]
				if outs[k].Len() != len(w) {
					t.Fatalf("%s %s: %d values, TensorFlow %d", model, p.Name, outs[k].Len(), len(w))
				}
				for i, wv := range w {
					g := float64(outs[k].f(i))
					if outs[k].I != nil {
						g = float64(outs[k].I[i])
					}
					if math.Abs(g-wv) > 1e-5*math.Max(1, math.Abs(wv)) {
						t.Errorf("%s %v %s[%d] = %v, TensorFlow %v", model, row.Inputs, p.Name, i, g, wv)
					}
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

// The ops2.go operators the converter folded away in ops2.tflite, checked
// against LiteRT's reference kernels' definitions (#1775).
func TestOps2Direct(t *testing.T) {
	m := &Model{}
	f := func(shape []int, v ...float32) *Tensor { return &Tensor{Type: Float32, Shape: shape, F: v} }
	i := func(shape []int, v ...int64) *Tensor { return &Tensor{Type: Int32, Shape: shape, I: v} }
	run := func(code int32, a ...*Tensor) *Tensor {
		t.Helper()
		r, err := m.eval(op{code: code, name: opNames[code]}, a)
		if err != nil {
			t.Fatalf("%s: %v", opNames[code], err)
		}
		return r
	}
	if r := run(opHardSwish, f([]int{4}, -4, -1, 1, 4)); !reflect.DeepEqual(r.F, []float32{0, -1.0 / 3, 2.0 / 3, 4}) {
		t.Errorf("HARD_SWISH %v", r.F)
	}
	if r := run(opPad, f([]int{1, 2}, 1, 2), i([]int{2, 2}, 0, 1, 1, 0)); !reflect.DeepEqual(r.Shape, []int{2, 3}) || !reflect.DeepEqual(r.F, []float32{0, 1, 2, 0, 0, 0}) {
		t.Errorf("PAD %v %v", r.Shape, r.F)
	}
	if r := run(opShape, f([]int{2, 3})); !reflect.DeepEqual(r.I, []int64{2, 3}) {
		t.Errorf("SHAPE %v", r.I)
	}
	if r := run(opFill, i([]int{2}, 2, 2), f([]int{}, 1.5)); !reflect.DeepEqual(r.F, []float32{1.5, 1.5, 1.5, 1.5}) {
		t.Errorf("FILL %v", r.F)
	}
	if r := run(opZerosLike, f([]int{3}, 1, 2, 3)); !reflect.DeepEqual(r.F, []float32{0, 0, 0}) {
		t.Errorf("ZEROS_LIKE %v", r.F)
	}
	outs, err := m.evalMulti(op{code: opSplitV, name: "SPLIT_V"}, []*Tensor{f([]int{1, 5}, 1, 2, 3, 4, 5), i([]int{2}, 2, -1), i([]int{}, 1)}, 2)
	if err != nil || !reflect.DeepEqual(outs[0].F, []float32{1, 2}) || !reflect.DeepEqual(outs[1].F, []float32{3, 4, 5}) {
		t.Errorf("SPLIT_V %v %v", outs, err)
	}
}
