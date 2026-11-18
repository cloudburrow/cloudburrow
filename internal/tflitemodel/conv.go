package tflitemodel

import (
	"fmt"
	"math"
)

// The convolution, pooling and gather operators (#1766), in float32 as
// LiteRT's reference kernels compute them (tensorflow/lite/kernels/
// internal/reference/conv.h, depthwiseconv_float.h, pooling.h): NHWC
// input, CONV_2D's filter [out, h, w, in], DEPTHWISE_CONV_2D's
// [1, h, w, in · depth_multiplier], SAME padding of (total) / 2 before,
// AVERAGE_POOL_2D averaging the window's points inside the input only.

// window is one spatial dimension's output size and padding before.
func window(in, k, stride, dil int, same bool) (out, pad int) {
	eff := (k-1)*dil + 1
	if same {
		out = (in + stride - 1) / stride
		pad = max((out-1)*stride+eff-in, 0) / 2
		return out, pad
	}
	return (in - eff + stride) / stride, 0
}

func conv2D(o op, x, w, b *Tensor) (*Tensor, error) {
	dw := o.code == opDepthwiseConv2D
	name := "CONV_2D"
	if dw {
		name = "DEPTHWISE_CONV_2D"
	}
	if w == nil || len(x.Shape) != 4 || len(w.Shape) != 4 || !x.Type.IsFloat() || !w.Type.IsFloat() || (b != nil && !b.Type.IsFloat()) {
		return nil, &UnsupportedError{name + " with these operands is not supported"}
	}
	same := o.i8(4, 0) == 0
	sw, sh := int(o.i32(6, 1)), int(o.i32(8, 1))
	var act int8
	var dilW, dilH, mult int
	if dw {
		mult, act, dilW, dilH = int(o.i32(10, 1)), o.i8(12, 0), int(o.i32(14, 1)), int(o.i32(16, 1))
	} else {
		act, dilW, dilH = o.i8(10, 0), int(o.i32(12, 1)), int(o.i32(14, 1))
	}
	n, ih, iw, ic := x.Shape[0], x.Shape[1], x.Shape[2], x.Shape[3]
	oc, kh, kw := w.Shape[0], w.Shape[1], w.Shape[2]
	if dw {
		oc = w.Shape[3]
		if mult < 1 {
			mult = oc / max(ic, 1)
		}
		if oc != ic*mult {
			return nil, fmt.Errorf("DEPTHWISE_CONV_2D: filter of %d channels for %d input channels and depth_multiplier %d", oc, ic, mult)
		}
	} else if w.Shape[3] != ic {
		if w.Shape[3] == 0 || ic%w.Shape[3] != 0 {
			return nil, fmt.Errorf("CONV_2D: filter of %d input channels for an input of %d", w.Shape[3], ic)
		}
		return nil, &UnsupportedError{"grouped CONV_2D is not supported"}
	}
	oh, ph := window(ih, kh, sh, dilH, same)
	ow, pw := window(iw, kw, sw, dilW, same)
	r := &Tensor{Type: Float32, Shape: []int{n, oh, ow, oc}, F: make([]float32, n*oh*ow*oc)}
	for bi := range n {
		for y := range oh {
			for xx := range ow {
				for c := range oc {
					var s float32
					for ky := range kh {
						iy := y*sh - ph + ky*dilH
						if iy < 0 || iy >= ih {
							continue
						}
						for kx := range kw {
							ix := xx*sw - pw + kx*dilW
							if ix < 0 || ix >= iw {
								continue
							}
							base := ((bi*ih+iy)*iw + ix) * ic
							if dw {
								s += x.F[base+c/mult] * w.F[(ky*kw+kx)*oc+c]
								continue
							}
							wb := ((c*kh+ky)*kw + kx) * ic
							for ci := range ic {
								s += x.F[base+ci] * w.F[wb+ci]
							}
						}
					}
					if b != nil {
						s += b.F[c]
					}
					r.F[((bi*oh+y)*ow+xx)*oc+c] = activation(act, s)
				}
			}
		}
	}
	return r, nil
}

func pool2D(o op, x *Tensor) (*Tensor, error) {
	if len(x.Shape) != 4 || !x.Type.IsFloat() {
		return nil, &UnsupportedError{"pooling of these tensors is not supported"}
	}
	same := o.i8(4, 0) == 0
	sw, sh, fw, fh, act := int(o.i32(6, 1)), int(o.i32(8, 1)), int(o.i32(10, 1)), int(o.i32(12, 1)), o.i8(14, 0)
	n, ih, iw, ch := x.Shape[0], x.Shape[1], x.Shape[2], x.Shape[3]
	oh, ph := window(ih, fh, sh, 1, same)
	ow, pw := window(iw, fw, sw, 1, same)
	avg := o.code == opAveragePool2D
	r := &Tensor{Type: Float32, Shape: []int{n, oh, ow, ch}, F: make([]float32, n*oh*ow*ch)}
	for bi := range n {
		for y := range oh {
			for xx := range ow {
				y0, x0 := max(y*sh-ph, 0), max(xx*sw-pw, 0)
				y1, x1 := min(y*sh-ph+fh, ih), min(xx*sw-pw+fw, iw)
				for c := range ch {
					var s float32
					if !avg {
						s = float32(math.Inf(-1))
					}
					cnt := 0
					for iy := y0; iy < y1; iy++ {
						for ix := x0; ix < x1; ix++ {
							v := x.F[((bi*ih+iy)*iw+ix)*ch+c]
							if avg {
								s += v
							} else {
								s = max(s, v)
							}
							cnt++
						}
					}
					if avg && cnt > 0 {
						s /= float32(cnt)
					}
					r.F[((bi*oh+y)*ow+xx)*ch+c] = activation(act, s)
				}
			}
		}
	}
	return r, nil
}

func gather(o op, x, idx *Tensor) (*Tensor, error) {
	if idx == nil || !idx.Type.IsInt() {
		return nil, &UnsupportedError{"GATHER with these indices is not supported"}
	}
	if o.i32(6, 0) != 0 {
		return nil, &UnsupportedError{"GATHER with batch_dims is not supported"}
	}
	axis := int(o.i32(4, 0))
	if axis < 0 {
		axis += len(x.Shape)
	}
	if axis < 0 || axis >= len(x.Shape) {
		return nil, fmt.Errorf("GATHER: axis %d of a rank-%d tensor", axis, len(x.Shape))
	}
	outer, inner := prod(x.Shape[:axis]), prod(x.Shape[axis+1:])
	dim := x.Shape[axis]
	shape := append(append(append([]int{}, x.Shape[:axis]...), idx.Shape...), x.Shape[axis+1:]...)
	r := alloc(x, shape)
	k := idx.Len()
	for a := range outer {
		for j := range k {
			v := int(idx.I[j])
			if v < 0 || v >= dim {
				return nil, fmt.Errorf("GATHER: index %d out of [0, %d)", v, dim)
			}
			for i := range inner {
				at(r, (a*k+j)*inner+i, x, (a*dim+v)*inner+i)
			}
		}
	}
	return r, nil
}
