package tflitemodel

import (
	"fmt"
	"math"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Quantized models (#1766): a tensor with QuantizationParameters (a
// scale and zero point, per tensor or per channel of its
// quantized_dimension) holds real value scale · (q − zero_point), as
// schema.fbs documents. CloudBurrow carries every quantized tensor as
// float32: a constant (weights, an INT32 bias) is dequantized when the
// model is read, a quantized input when it is fed, and each quantized
// operator output is rounded to its grid, q = clamp(round(x / scale) +
// zero_point) of its type's range, so a value is one LiteRT's integer
// kernels can produce; a quantized output is given as its integers.
// QUANTIZE is that rounding, DEQUANTIZE the identity on the float.
//
// UNVERIFIED (re-read 2026-11-17, LiteRT does not document it): LiteRT's integer kernels compute in integers
// with fixed-point multipliers (tensorflow/lite/kernels/internal/
// reference/integer_ops), so an output can differ from CloudBurrow's by
// a step of its scale; tflitemodel_test.go checks a dynamic range and a
// full integer model against LiteRT within one step.

// quant is a tensor's quantization.
type quant struct {
	scale []float32
	zp    []int64
	dim   int // the channel dimension of per-channel parameters
	typ   Type
}

// readQuant reads a QuantizationParameters table of tensor d, nil when it
// has no scale.
func readQuant(qt *flatbuffers.Table, d tensorDef) (*quant, error) {
	q := &quant{typ: d.typ}
	if p := field(qt, 8); p != 0 {
		n := qt.VectorLen(p)
		v := qt.Vector(p)
		for i := range n {
			q.scale = append(q.scale, qt.GetFloat32(flatbuffers.UOffsetT(v+flatbuffers.UOffsetT(i*4))))
		}
	}
	if len(q.scale) == 0 {
		return nil, nil
	}
	if p := field(qt, 10); p != 0 {
		n := qt.VectorLen(p)
		v := qt.Vector(p)
		for i := range n {
			q.zp = append(q.zp, qt.GetInt64(flatbuffers.UOffsetT(v+flatbuffers.UOffsetT(i*8))))
		}
	}
	if field(qt, 14) != 0 { // details: a custom quantization
		return nil, &UnsupportedError{"tensor " + d.name + " has a custom quantization: CloudBurrow runs affine (scale and zero point) quantization"}
	}
	q.dim = int(qt.GetInt32Slot(16, 0))
	if len(q.zp) == 0 {
		q.zp = make([]int64, len(q.scale))
	}
	if len(q.zp) != len(q.scale) {
		return nil, fmt.Errorf("tensor %s has %d scales and %d zero points", d.name, len(q.scale), len(q.zp))
	}
	switch d.typ {
	case Int8, Uint8, Int16, Int32, Int64:
	default:
		return nil, &UnsupportedError{"quantized " + d.typ.String() + " tensors are not supported"}
	}
	return q, nil
}

// channel is the parameter index of element i of a tensor of shape.
func (q *quant) channel(shape []int, i int) int {
	if len(q.scale) == 1 || q.dim >= len(shape) {
		return 0
	}
	inner := prod(shape[q.dim+1:])
	return (i / inner) % shape[q.dim]
}

// dequantize is t's real values, as FLOAT32.
func (q *quant) dequantize(t *Tensor) *Tensor {
	r := &Tensor{Type: Float32, Shape: t.Shape, F: make([]float32, t.Len())}
	for i := range r.F {
		c := q.channel(t.Shape, i)
		r.F[i] = q.scale[c] * (t.f(i) - float32(q.zp[c]))
	}
	return r
}

// bounds are q's type's range.
func (q *quant) bounds(typ Type) (float64, float64) {
	switch typ {
	case Int8:
		return -128, 127
	case Uint8:
		return 0, 255
	case Int16:
		return -32768, 32767
	case Int32:
		return math.MinInt32, math.MaxInt32
	}
	return math.MinInt64, math.MaxInt64
}

func (q *quant) level(v float32, c int) float64 {
	lo, hi := q.bounds(q.typ)
	x := math.Round(float64(v)/float64(q.scale[c])) + float64(q.zp[c])
	return math.Min(math.Max(x, lo), hi)
}

// round is t's values on q's grid, as FLOAT32.
func (q *quant) round(t *Tensor) *Tensor {
	r := &Tensor{Type: Float32, Shape: t.Shape, F: make([]float32, t.Len())}
	for i := range r.F {
		c := q.channel(t.Shape, i)
		r.F[i] = float32((q.level(t.f(i), c) - float64(q.zp[c])) * float64(q.scale[c]))
	}
	return r
}

// quantize is t's values as q's integers, of type typ.
func (q *quant) quantize(t *Tensor, typ Type) Tensor {
	r := Tensor{Type: typ, Shape: t.Shape, I: make([]int64, t.Len())}
	for i := range r.I {
		r.I[i] = int64(q.level(t.f(i), q.channel(t.Shape, i)))
	}
	return r
}
