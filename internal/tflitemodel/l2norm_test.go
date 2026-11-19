package tflitemodel

import (
	"math"
	"testing"
)

// TestL2Norm: L2_NORMALIZATION divides each last-axis vector by its L2
// norm, as LiteRT's reference kernel does, and a zero vector by the 1e-6
// floor (so it stays zero).
func TestL2Norm(t *testing.T) {
	var m Model
	x := &Tensor{Type: Float32, Shape: []int{2, 2}, F: []float32{3, 4, 0, 0}}
	r, err := m.eval(op{code: opL2Norm, name: opNames[opL2Norm]}, []*Tensor{x})
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{0.6, 0.8, 0, 0}
	for i, v := range r.F {
		if math.Abs(float64(v-want[i])) > 1e-6 {
			t.Errorf("out[%d] = %v, want %v", i, v, want[i])
		}
	}
	if _, err := m.eval(op{code: opL2Norm, name: "L2_NORMALIZATION"}, []*Tensor{{Type: Int32, Shape: []int{1}, I: []int64{1}}}); !IsUnsupported(err) {
		t.Errorf("int input: %v", err)
	}
}
