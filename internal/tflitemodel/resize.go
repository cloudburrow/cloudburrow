package tflitemodel

import (
	"fmt"
	"math"
)

// LiteRT's image resizing operators (#1799): RESIZE_NEAREST_NEIGHBOR,
// which MediaPipe's EfficientDet-Lite0 object detector runs in its feature
// pyramid, and RESIZE_BILINEAR, each as LiteRT's reference kernels
// (tensorflow/lite/kernels/internal/reference/resize_nearest_neighbor.h
// and reference_ops.h's ResizeBilinear) compute it, on NHWC float tensors.
const (
	opResizeBilinear        = 23
	opResizeNearestNeighbor = 97
)

func init() {
	opNames[opResizeBilinear] = "RESIZE_BILINEAR"
	opNames[opResizeNearestNeighbor] = "RESIZE_NEAREST_NEIGHBOR"
}

// resizeOp runs either operator: x is [batch, height, width, channels]
// and size the output's [height, width]. The options' slots:
// ResizeNearestNeighborOptions align_corners 4, half_pixel_centers 6;
// ResizeBilinearOptions align_corners 8, half_pixel_centers 10.
func resizeOp(o op, x, size *Tensor) (*Tensor, error) {
	if x == nil || len(x.Shape) != 4 || !x.Type.IsFloat() {
		return nil, &UnsupportedError{fmt.Sprintf("operator %s takes a 4-D float tensor", o.name)}
	}
	hw := axes(size)
	if len(hw) != 2 || hw[0] <= 0 || hw[1] <= 0 {
		return nil, fmt.Errorf("operator %s takes an output size of two positive integers", o.name)
	}
	align, half := o.bool(4), o.bool(6)
	if o.code == opResizeBilinear {
		align, half = o.bool(8), o.bool(10)
	}
	n, ih, iw, c := x.Shape[0], x.Shape[1], x.Shape[2], x.Shape[3]
	oh, ow := hw[0], hw[1]
	scale := func(in, out int) float32 {
		if align && out > 1 {
			return float32(in-1) / float32(out-1)
		}
		return float32(in) / float32(out)
	}
	hs, ws := scale(ih, oh), scale(iw, ow)
	r := &Tensor{Type: x.Type, Shape: []int{n, oh, ow, c}, F: make([]float32, n*oh*ow*c)}
	in := func(b, y, xx, ch int) float32 { return x.F[((b*ih+y)*iw+xx)*c+ch] }
	if o.code == opResizeNearestNeighbor {
		nearest := func(v, insize int, s float32) int {
			off := float32(0)
			if half {
				off = 0.5
			}
			var out int
			if align {
				out = int(math.Round(float64((float32(v) + off) * s)))
			} else {
				out = int(math.Floor(float64((float32(v) + off) * s)))
			}
			out = min(out, insize-1)
			if half {
				out = max(out, 0)
			}
			return out
		}
		j := 0
		for b := range n {
			for y := range oh {
				sy := nearest(y, ih, hs)
				for xx := range ow {
					sx := nearest(xx, iw, ws)
					for ch := range c {
						r.F[j] = in(b, sy, sx, ch)
						j++
					}
				}
			}
		}
		return r, nil
	}
	interp := func(v, insize int, s float32) (float32, int, int) {
		var f float32
		if half {
			f = (float32(v)+0.5)*s - 0.5
		} else {
			f = float32(v) * s
		}
		lo := max(int(math.Floor(float64(f))), 0)
		hi := min(int(math.Ceil(float64(f))), insize-1)
		return f, lo, hi
	}
	j := 0
	for b := range n {
		for y := range oh {
			fy, y0, y1 := interp(y, ih, hs)
			for xx := range ow {
				fx, x0, x1 := interp(xx, iw, ws)
				for ch := range c {
					r.F[j] = in(b, y0, x0, ch)*(1-(fy-float32(y0)))*(1-(fx-float32(x0))) +
						in(b, y1, x0, ch)*(fy-float32(y0))*(1-(fx-float32(x0))) +
						in(b, y0, x1, ch)*(1-(fy-float32(y0)))*(fx-float32(x0)) +
						in(b, y1, x1, ch)*(fy-float32(y0))*(fx-float32(x0))
					j++
				}
			}
		}
	}
	return r, nil
}
