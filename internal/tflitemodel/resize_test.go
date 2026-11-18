package tflitemodel

import (
	"os"
	"reflect"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
)

// resizeOpts builds a Resize*Options table whose two bool fields sit at
// slots a and b (4 and 6 for nearest neighbour, 8 and 10 for bilinear).
func resizeOpts(a, b int, align, half bool) *flatbuffers.Table {
	fb := flatbuffers.NewBuilder(0)
	n := max(a, b)/2 - 1
	fb.StartObject(n)
	fb.PrependBoolSlot(a/2-2, align, false)
	fb.PrependBoolSlot(b/2-2, half, false)
	fb.Finish(fb.EndObject())
	buf := fb.FinishedBytes()
	return &flatbuffers.Table{Bytes: buf, Pos: flatbuffers.GetUOffsetT(buf)}
}

// TestResize checks RESIZE_NEAREST_NEIGHBOR and RESIZE_BILINEAR against
// LiteRT's reference kernels' formulas, worked by hand, on a 1×2×2×1
// tensor [[1, 2], [3, 4]].
func TestResize(t *testing.T) {
	m := &Model{}
	x := &Tensor{Type: Float32, Shape: []int{1, 2, 2, 1}, F: []float32{1, 2, 3, 4}}
	size := func(h, w int64) *Tensor { return &Tensor{Type: Int32, Shape: []int{2}, I: []int64{h, w}} }
	for _, c := range []struct {
		name        string
		code        int32
		align, half bool
		h, w        int64
		want        []float32
	}{
		// scale 0.5: floor(v*0.5) = 0, 0, 1, 1
		{"nearest", opResizeNearestNeighbor, false, false, 4, 4, []float32{1, 1, 2, 2, 1, 1, 2, 2, 3, 3, 4, 4, 3, 3, 4, 4}},
		// half pixel: floor((v+0.5)*0.5) = 0, 0, 1, 1 too
		{"nearest half pixel", opResizeNearestNeighbor, false, true, 4, 4, []float32{1, 1, 2, 2, 1, 1, 2, 2, 3, 3, 4, 4, 3, 3, 4, 4}},
		// align corners, scale 1/3: round(v/3) = 0, 0, 1, 1
		{"nearest align corners", opResizeNearestNeighbor, true, false, 1, 4, []float32{1, 1, 2, 2}},
		// scale 0.5: x = 0, 0.5, 1, 1.5 → 1, 1.5, 2, 2 (upper clamped)
		{"bilinear", opResizeBilinear, false, false, 1, 4, []float32{1, 1.5, 2, 2}},
		// half pixel: y = 0.5 (the rows' mean, 2 and 3), x = -0.25, 0.25,
		// 0.75, 1.25, both bounds clamped at the edges
		{"bilinear half pixel", opResizeBilinear, false, true, 1, 4, []float32{2, 2.25, 2.75, 3}},
		// align corners, scale 1/3: x = 0, 1/3, 2/3, 1
		{"bilinear align corners", opResizeBilinear, true, false, 1, 4, []float32{1, 4.0 / 3, 5.0 / 3, 2}},
	} {
		a, b := 4, 6
		if c.code == opResizeBilinear {
			a, b = 8, 10
		}
		o := op{code: c.code, name: opNames[c.code], opts: resizeOpts(a, b, c.align, c.half)}
		r, err := m.eval(o, []*Tensor{x, size(c.h, c.w)})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(r.Shape, []int{1, int(c.h), int(c.w), 1}) {
			t.Errorf("%s: shape %v", c.name, r.Shape)
		}
		for i := range c.want {
			if d := r.F[i] - c.want[i]; d > 1e-6 || d < -1e-6 {
				t.Errorf("%s: %v, want %v", c.name, r.F, c.want)
				break
			}
		}
	}
}

// TestMetadata: the test models carry their converter's
// min_runtime_version and no TFLITE_METADATA.
func TestMetadata(t *testing.T) {
	data, err := os.ReadFile("testdata/mlp.tflite")
	if err != nil {
		t.Fatal(err)
	}
	if b, ok, err := Metadata(data, "min_runtime_version"); err != nil || !ok || len(b) == 0 {
		t.Errorf("min_runtime_version: %q %v %v", b, ok, err)
	}
	if _, ok, err := Metadata(data, "TFLITE_METADATA"); err != nil || ok {
		t.Errorf("TFLITE_METADATA: %v %v", ok, err)
	}
	if _, _, err := Metadata([]byte("not a model"), "x"); err == nil {
		t.Error("not a model: no error")
	}
}
