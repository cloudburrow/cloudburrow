//go:build onnx

package onnxmodel

import (
	"math"
	"reflect"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel/onnxgen"
)

// TestLinearModel runs onnxgen's linear model on ONNX Runtime
// (ONNXRUNTIME_LIB): its ports as the graph declares them, and its outputs
// for a batch of two rows.
func TestLinearModel(t *testing.T) {
	m := onnxgen.Linear()
	in, out, err := Inspect(m)
	if err != nil {
		t.Fatal(err)
	}
	wantIn := []Port{{"a", KindTensor, "FLOAT", []int64{-1, 1}}, {"b", KindTensor, "FLOAT", []int64{-1, 1}}, {"v", KindTensor, "FLOAT", []int64{-1, 2}}}
	wantOut := []Port{{"y", KindTensor, "FLOAT", []int64{-1, 1}}, {"label", KindTensor, "INT64", []int64{-1}}, {"scores", KindTensor, "FLOAT", []int64{-1, 2}}}
	if !reflect.DeepEqual(in, wantIn) || !reflect.DeepEqual(out, wantOut) {
		t.Fatalf("ports %+v %+v", in, out)
	}
	res, err := Run(m, in, []Tensor{
		{Shape: []int64{2, 1}, F: []float64{1, 0}},
		{Shape: []int64{2, 1}, F: []float64{1, 0.5}},
		{Shape: []int64{2, 2}, F: []float64{0, 0, 2, 1}},
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	// y = 2a + 3b + v0 - v1 + 1: 6 and 3.5.
	if !reflect.DeepEqual(res[0].F, []float64{6, 3.5}) || !reflect.DeepEqual(res[1].I, []int64{1, 0}) ||
		!reflect.DeepEqual(res[2].F, []float64{6, -6, 3.5, -3.5}) || !reflect.DeepEqual(res[2].Shape, []int64{2, 2}) {
		t.Errorf("outputs %+v", res)
	}
	seqIn, seqOut, err := Inspect(onnxgen.SequenceOutput())
	if err != nil {
		t.Fatal(err)
	}
	if len(seqIn) != 1 || len(seqOut) != 1 || seqOut[0].Kind != "ONNX_TYPE_SEQUENCE" {
		t.Errorf("sequence model ports %+v %+v", seqIn, seqOut)
	}
}

func TestHalf(t *testing.T) {
	for _, f := range []float32{0, 1, -2.5, 65504, 0.000061035156, 5.96e-8} {
		if got := fromHalf(toHalf(f)); math.Abs(float64(got-f)) > 1e-3*math.Abs(float64(f))+6e-8 {
			t.Errorf("%g → %g", f, got)
		}
	}
	if !math.IsInf(float64(fromHalf(toHalf(float32(math.Inf(1))))), 1) || !math.IsNaN(float64(fromHalf(toHalf(float32(math.NaN()))))) {
		t.Error("inf/nan")
	}
}
