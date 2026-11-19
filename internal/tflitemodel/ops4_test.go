package tflitemodel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
)

// TestOps4MatchLiteRT (#1775) runs the models gen_ops4.py wrote and checks
// every output against LiteRT's reference kernels on the same inputs
// (litert_ops4.json): rnn4.tflite the fused LSTM and RNN kernels,
// random4.tflite the seeded random operators, ops4.tflite (TensorFlow
// 2.20's TFLiteConverter) NON_MAX_SUPPRESSION_V4/V5, CONV_3D,
// CONV_3D_TRANSPOSE, RFFT2D, REAL, IMAG and COMPLEX_ABS, table4.tflite the
// HASHTABLE operators under CALL_ONCE, var4.tflite the resource
// variables, ops5.tflite and ops6.tflite the operators of ops5.go and
// ops6.go with TFLite_Detection_PostProcess, audio4.tflite AudioSpectrogram
// and Mfcc, and hlo4.tflite STABLEHLO_GATHER, _SCATTER and _REDUCE_WINDOW.
func TestOps4MatchLiteRT(t *testing.T) {
	raw, err := os.ReadFile("testdata/litert_ops4.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Ops     []string         `json:"ops"`
		Inputs  map[string]any   `json:"inputs"`
		Outputs map[string]any   `json:"outputs"`
		Shapes  map[string][]int `json:"shapes"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 9 {
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
			uses := m.Uses()
			for _, o := range c.Ops {
				_, custom := builtinCustom[o]
				if !slices.Contains(uses, o) || !slices.Contains(Ops(), o) && !custom {
					t.Errorf("operator %s: in the model %v, run %v", o, slices.Contains(uses, o), slices.Contains(Ops(), o))
				}
			}
			var ins []Tensor
			for _, p := range m.Inputs {
				v, ok := c.Inputs[p.Name]
				if !ok {
					t.Fatalf("no input %s", p.Name)
				}
				if p.Type == String {
					shape, flat := strs(v)
					ins = append(ins, Tensor{Type: String, Shape: shape, S: flat})
					continue
				}
				shape, flat := flatten(v)
				in := Tensor{Type: Float32, Shape: shape}
				for _, x := range flat {
					in.F = append(in.F, float32(x))
				}
				ins = append(ins, convert(in, p.Type))
			}
			outs, err := m.Run(ins)
			if err != nil {
				t.Fatal(err)
			}
			if len(outs) != len(c.Outputs) {
				t.Fatalf("%d outputs, want %d", len(outs), len(c.Outputs))
			}
			for k, p := range m.Outputs {
				got := outs[k]
				if s, ok := c.Shapes[p.Name]; ok {
					_, want := strs(c.Outputs[p.Name])
					if !reflect.DeepEqual(got.Shape, s) || !reflect.DeepEqual(got.S, want) {
						t.Errorf("%s = %v %q, LiteRT gives %v %q", p.Name, got.Shape, got.S, s, want)
					}
					continue
				}
				shape, want := flatten(c.Outputs[p.Name])
				if !reflect.DeepEqual(got.Shape, shape) && !(len(shape) == 0 && len(got.Shape) == 0) {
					t.Fatalf("%s: shape %v, want %v", p.Name, got.Shape, shape)
				}
				for i, w := range want {
					g := float64(got.f(i))
					if math.Abs(g-w) > 2e-5*max(1, math.Abs(w)) {
						t.Errorf("%s[%d] = %v, LiteRT gives %v", p.Name, i, g, w)
					}
				}
			}
		})
	}
}

// strs is a JSON array of strings' shape and elements.
func strs(v any) ([]int, []string) {
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

// TestOps4Direct checks what the LiteRT runs above do not: each Run starts
// from the model's initial state, NMS's tie order and argument checks, the
// refused LSTM BASIC kernel, a hash table's first value for a repeated key
// and its one import, and unseeded random draws.
func TestOps4Direct(t *testing.T) {
	for _, name := range []string{"var4", "table4", "random4"} {
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
			in := Tensor{Type: p.Type, Shape: p.Shape}
			switch p.Type {
			case String:
				in.S = make([]string, prod(p.Shape))
			case Int64:
				in.I = make([]int64, prod(p.Shape))
			default:
				in.F = make([]float32, prod(p.Shape))
			}
			ins = append(ins, in)
		}
		a, err := m.Run(ins)
		if err != nil {
			t.Fatal(err)
		}
		b, err := m.Run(ins)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: a second Run gives %v, the first %v", name, b, a)
		}
	}

	boxes := []float32{0, 0, 1, 1, 0, 0, 1, 1, 5, 5, 6, 6}
	if idx, _ := nonMaxSuppression(boxes, []float32{0.5, 0.5, 0.5}, 3, 0.5, 0, 0); !reflect.DeepEqual(idx, []int{0, 2}) {
		t.Errorf("NMS of tied scores selects %v, want [0 2]", idx)
	}
	f := func(v ...float32) *Tensor { return &Tensor{Type: Float32, Shape: []int{len(v)}, F: v} }
	one := func(v float32) *Tensor { return &Tensor{Type: Float32, Shape: []int{}, F: []float32{v}} }
	bx := &Tensor{Type: Float32, Shape: []int{3, 4}, F: boxes}
	v5 := op{code: opNonMaxSuppressionV5, name: "NON_MAX_SUPPRESSION_V5"}
	if _, err := nms(v5, []*Tensor{bx, f(0.9, 0.8, 0.7), one(2), one(0.5), one(0), one(-1)}, 3); err == nil {
		t.Error("NMS took a negative sigma")
	}
	if _, err := nms(v5, []*Tensor{bx, f(0.9, 0.8, 0.7), one(-2), one(0.5), one(0), one(0)}, 3); err == nil {
		t.Error("NMS took a negative max_output_size")
	}
	outs, err := nms(v5, []*Tensor{bx, f(0.9, 0.8, 0.7), one(4), one(0.5), one(0), one(0)}, 3)
	if err != nil || !reflect.DeepEqual(outs[0].I, []int64{0, 2, 0, 0}) || outs[2].I[0] != 2 {
		t.Errorf("NMS V5 gives %v, %v", outs, err)
	}

	lstm := op{code: opLSTM, name: "LSTM", opts: optsTable(func(b *flatbuffers.Builder) {
		b.StartObject(4)
		b.PrependInt8Slot(3, 1, 0)
	})}
	in := make([]*Tensor, 24)
	in[0] = f(1, 2)
	if _, err := evalLSTM(lstm, in); !IsUnsupported(err) {
		t.Errorf("LSTM BASIC kernel: %v", err)
	}

	st := newRunState()
	m := &Model{}
	ht := op{code: opHashtable, name: "HASHTABLE", opts: optsTable(func(b *flatbuffers.Builder) {
		b.StartObject(3)
		b.PrependInt32Slot(0, 7, 0)
		b.PrependInt8Slot(1, int8(String), 0)
		b.PrependInt8Slot(2, int8(Int64), 0)
	})}
	h, err := m.evalResource(ht, nil, 0, st)
	if err != nil {
		t.Fatal(err)
	}
	keys := &Tensor{Type: String, Shape: []int{3}, S: []string{"a", "b", "a"}}
	imp := op{code: opHashtableImport, name: "HASHTABLE_IMPORT"}
	if _, err := m.evalResource(imp, []*Tensor{h[0], keys, {Type: Int64, Shape: []int{3}, I: []int64{1, 2, 3}}}, 0, st); err != nil {
		t.Fatal(err)
	}
	if _, err := m.evalResource(imp, []*Tensor{h[0], keys, {Type: Int64, Shape: []int{3}, I: []int64{9, 9, 9}}}, 0, st); err != nil {
		t.Fatal(err)
	}
	got, err := m.evalResource(op{code: opHashtableFind, name: "HASHTABLE_FIND"},
		[]*Tensor{h[0], {Type: String, Shape: []int{3}, S: []string{"a", "b", "z"}}, {Type: Int64, Shape: []int{}, I: []int64{-1}}}, 0, st)
	if err != nil || !reflect.DeepEqual(got[0].I, []int64{1, 2, -1}) {
		t.Errorf("HASHTABLE_FIND gives %v, %v; want [1 2 -1]", got, err)
	}

	r, err := random(op{code: opRandomUniform, name: "RANDOM_UNIFORM"}, []int{100})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range r.F {
		if v < 0 || v >= 1 {
			t.Fatalf("RANDOM_UNIFORM gives %v", v)
		}
	}
}

// optsTable is a builtin options table built by fill.
func optsTable(fill func(b *flatbuffers.Builder)) *flatbuffers.Table {
	b := flatbuffers.NewBuilder(64)
	fill(b)
	b.Finish(b.EndObject())
	buf := b.FinishedBytes()
	return &flatbuffers.Table{Bytes: buf, Pos: flatbuffers.GetUOffsetT(buf)}
}
