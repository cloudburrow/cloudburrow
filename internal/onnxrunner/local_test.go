//go:build onnx

package onnxrunner

import (
	"context"
	"reflect"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	"github.com/cloudburrow/cloudburrow/internal/onnxmodel/onnxgen"
)

// TestRunnerOnONNXRuntime (#1711): onnxgen's linear model through the
// runner on ONNX Runtime (ONNXRUNTIME_LIB).
func TestRunnerOnONNXRuntime(t *testing.T) {
	_, c := newRunner(t, Local{}, 0)
	m := onnxgen.Linear()
	ctx := context.Background()
	in, out, err := c.Load(ctx, sum(m), m)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(ctx, sum(m), func() ([]byte, error) { return m, nil }, in, []onnxmodel.Tensor{
		{Shape: []int64{2, 1}, F: []float64{1, 0}},
		{Shape: []int64{2, 1}, F: []float64{1, 0.5}},
		{Shape: []int64{2, 2}, F: []float64{0, 0, 2, 1}},
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res[0].F, []float64{6, 3.5}) || !reflect.DeepEqual(res[1].I, []int64{1, 0}) ||
		!reflect.DeepEqual(res[2].F, []float64{6, -6, 3.5, -3.5}) {
		t.Errorf("outputs %+v", res)
	}
	if _, _, err := c.Load(ctx, sum(onnxgen.SequenceOutput()), []byte("not onnx")); !IsModelError(err) {
		t.Errorf("bytes of another sum: %v", err)
	}
}
