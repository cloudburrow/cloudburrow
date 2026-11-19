package bigqueryfront

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// A port's first dimension is the batch when it is free or 1; a fixed one
// of more than one is part of each row's ARRAY, so no value is dropped
// (#1775).
func TestTFLiteColumnOfShapes(t *testing.T) {
	for _, c := range []struct {
		shape []int
		array bool
	}{
		{[]int{-1}, false}, {[]int{1}, false}, {[]int{-1, 1}, false}, {[]int{}, false},
		{[]int{-1, 3}, true}, {[]int{2}, true}, {[]int{4, 1}, true}, {[]int{1, -1}, true},
	} {
		col, code, msg := tfliteColumnOf(tflitemodel.Port{Name: "p", Type: tflitemodel.Float32, Shape: c.shape}, "output")
		if code != 0 {
			t.Fatalf("%v: %d %s", c.shape, code, msg)
		}
		if col.Array != c.array {
			t.Errorf("shape %v: Array %v, want %v", c.shape, col.Array, c.array)
		}
	}
}
