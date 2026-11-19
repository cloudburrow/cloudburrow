package tflitemodel

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sort"

	flatbuffers "github.com/google/flatbuffers/go"
)

// More of LiteRT's builtin operators (#1775): TRANSPOSE_CONV, PRELU,
// MIRROR_PAD, the space and depth rearrangements, L2_NORMALIZATION and
// LOCAL_RESPONSE_NORMALIZATION, GATHER_ND, SCATTER_ND, EMBEDDING_LOOKUP,
// ONE_HOT, RANGE, REVERSE_V2, BROADCAST_TO, CUMSUM, REDUCE_ANY and
// REDUCE_ALL, TOPK_V2, SIGN, ATAN2, BUCKETIZE, MATRIX_DIAG, RANK,
// RELU_0_TO_1 and the segment reductions, each as LiteRT's reference kernel
// (tensorflow/lite/kernels/internal/reference) computes it, checked against
// LiteRT's interpreter on TensorFlow 2.20's conversions (gen_ops3.py).
const (
	opDepthToSpace        = 5
	opEmbeddingLookup     = 7
	opL2Normalization     = 11
	opLocalResponseNorm   = 13
	opSpaceToDepth        = 26
	opBatchToSpaceND      = 37
	opSpaceToBatchND      = 38
	opTopKV2              = 48
	opPrelu               = 54
	opTransposeConv       = 67
	opOneHot              = 85
	opReduceAny           = 91
	opRange               = 96
	opMirrorPad           = 100
	opReverseV2           = 105
	opGatherND            = 107
	opRank                = 110
	opMatrixDiag          = 113
	opScatterND           = 122
	opSegmentSum          = 125
	opCumsum              = 128
	opBroadcastTo         = 130
	opReduceAll           = 140
	opBucketize           = 147
	opRelu0To1            = 152
	opUnsortedSegmentProd = 153
	opUnsortedSegmentMax  = 154
	opUnsortedSegmentSum  = 155
	opAtan2               = 156
	opUnsortedSegmentMin  = 157
	opSign                = 158
)

func init() {
	for code, name := range map[int32]string{opDepthToSpace: "DEPTH_TO_SPACE", opEmbeddingLookup: "EMBEDDING_LOOKUP",
		opL2Normalization: "L2_NORMALIZATION", opLocalResponseNorm: "LOCAL_RESPONSE_NORMALIZATION", opSpaceToDepth: "SPACE_TO_DEPTH",
		opBatchToSpaceND: "BATCH_TO_SPACE_ND", opSpaceToBatchND: "SPACE_TO_BATCH_ND", opTopKV2: "TOPK_V2", opPrelu: "PRELU",
		opTransposeConv: "TRANSPOSE_CONV", opOneHot: "ONE_HOT", opReduceAny: "REDUCE_ANY", opRange: "RANGE", opMirrorPad: "MIRROR_PAD",
		opReverseV2: "REVERSE_V2", opGatherND: "GATHER_ND", opRank: "RANK", opMatrixDiag: "MATRIX_DIAG", opScatterND: "SCATTER_ND",
		opSegmentSum: "SEGMENT_SUM", opCumsum: "CUMSUM", opBroadcastTo: "BROADCAST_TO", opReduceAll: "REDUCE_ALL",
		opBucketize: "BUCKETIZE", opRelu0To1: "RELU_0_TO_1", opUnsortedSegmentProd: "UNSORTED_SEGMENT_PROD",
		opUnsortedSegmentMax: "UNSORTED_SEGMENT_MAX", opUnsortedSegmentSum: "UNSORTED_SEGMENT_SUM", opAtan2: "ATAN2",
		opUnsortedSegmentMin: "UNSORTED_SEGMENT_MIN", opSign: "SIGN"} {
		opNames[code] = name
	}
}

// eval3 runs the operators of this file that have one output.
func (m *Model) eval3(o op, a []*Tensor) (*Tensor, error) {
	arg := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	x := arg(0)
	need := func(n int) error {
		for i := range n {
			if arg(i) == nil {
				return fmt.Errorf("operator %s takes %d inputs", o.name, n)
			}
		}
		return nil
	}
	if err := need(o.arity()); err != nil {
		return nil, err
	}
	switch o.code {
	case opRelu0To1:
		return floatMap(o, x, func(v float64) float64 { return math.Min(math.Max(v, 0), 1) })
	case opSign:
		if x.I != nil {
			r := alloc(x, x.Shape)
			for i, v := range x.I {
				r.I[i] = int64(cmp.Compare(v, 0))
			}
			return r, nil
		}
		return floatMap(o, x, func(v float64) float64 {
			switch {
			case v > 0:
				return 1
			case v < 0:
				return -1
			}
			return v // 0, -0 and NaN unchanged
		})
	case opRank:
		return &Tensor{Type: Int32, Shape: []int{}, I: []int64{int64(len(x.Shape))}}, nil
	case opAtan2, opPrelu:
		y := arg(1)
		if !x.Type.IsFloat() || !y.Type.IsFloat() {
			return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
		}
		shape, err := broadcastShape(x.Shape, y.Shape)
		if err != nil {
			return nil, fmt.Errorf("operator %s: %w", o.name, err)
		}
		r := &Tensor{Type: x.Type, Shape: shape, F: make([]float32, prod(shape))}
		for i := range r.F {
			p, q := x.F[broadcastIndex(i, shape, x.Shape)], y.F[broadcastIndex(i, shape, y.Shape)]
			if o.code == opAtan2 {
				r.F[i] = float32(math.Atan2(float64(p), float64(q)))
			} else if p >= 0 {
				r.F[i] = p
			} else {
				r.F[i] = p * q
			}
		}
		return r, nil
	case opTransposeConv:
		return transposeConv(o, axes(arg(0)), arg(1), arg(2), arg(3))
	case opMirrorPad:
		return mirrorPad(o, x, axes(arg(1)))
	case opDepthToSpace, opSpaceToDepth:
		return depthSpace(o, x, int(o.i32(4, 0)))
	case opSpaceToBatchND:
		return spaceToBatch(o, x, axes(arg(1)), axes(arg(2)))
	case opBatchToSpaceND:
		return batchToSpace(o, x, axes(arg(1)), axes(arg(2)))
	case opL2Normalization:
		return l2Normalize(o, x)
	case opLocalResponseNorm:
		return lrn(o, x)
	case opGatherND:
		return gatherND(o, x, arg(1))
	case opEmbeddingLookup:
		return gather(op{code: opGather, name: o.name}, arg(1), x)
	case opScatterND:
		return scatterND(o, x, arg(1), axes(arg(2)))
	case opOneHot:
		return oneHot(o, x, arg(1), arg(2), arg(3))
	case opRange:
		return rangeOp(o, x, arg(1), arg(2))
	case opReverseV2:
		return reverse(x, axes(arg(1)))
	case opBroadcastTo:
		shape := axes(arg(1))
		if _, err := broadcastShape(x.Shape, shape); err != nil || len(shape) < len(x.Shape) {
			return nil, fmt.Errorf("operator BROADCAST_TO: %v does not broadcast to %v", x.Shape, shape)
		}
		r := alloc(x, shape)
		for i := range r.Len() {
			at(r, i, x, broadcastIndex(i, shape, x.Shape))
		}
		return r, nil
	case opCumsum:
		ax := axes(arg(1))
		if len(ax) != 1 {
			return nil, fmt.Errorf("operator CUMSUM takes one axis")
		}
		return cumsum(o, x, ax[0])
	case opReduceAny, opReduceAll:
		return reduceBool(o, x, axes(arg(1)), o.bool(4))
	case opBucketize:
		return bucketize(o, x)
	case opMatrixDiag:
		if len(x.Shape) == 0 {
			return nil, fmt.Errorf("operator MATRIX_DIAG takes a tensor of rank 1 or more")
		}
		n := x.Shape[len(x.Shape)-1]
		r := alloc(x, append(slices.Clone(x.Shape), n))
		for b := range x.Len() / max(n, 1) {
			for i := range n {
				at(r, (b*n+i)*n+i, x, b*n+i)
			}
		}
		return r, nil
	case opSegmentSum, opUnsortedSegmentSum, opUnsortedSegmentProd, opUnsortedSegmentMax, opUnsortedSegmentMin:
		return segment(o, x, arg(1), arg(2))
	}
	return m.eval4(o, a) // ops4.go
}

// arity is the number of inputs an operator of this file needs.
func (o op) arity() int {
	switch o.code {
	case opRelu0To1, opSign, opRank, opDepthToSpace, opSpaceToDepth, opL2Normalization, opLocalResponseNorm, opMatrixDiag, opBucketize:
		return 1
	case opTransposeConv, opSpaceToBatchND, opBatchToSpaceND, opScatterND, opRange, opUnsortedSegmentSum, opUnsortedSegmentProd,
		opUnsortedSegmentMax, opUnsortedSegmentMin:
		return 3
	case opOneHot:
		return 4
	case opLSTM, opUnidirectionalSequenceLSTM, opRNN, opUnidirectionalSequenceRNN, opRandomUniform, opRandomNormal, opReal, opImag, opComplexAbs,
		opWhere, opDynamicUpdateSlice, opMatrixSetDiag, opReverseSequence, opSparseToDense, opBroadcastArgs, opL2Pool2D, opBitwiseXor,
		opRightShift, opStablehloAdd, opStablehloMultiply, opStablehloMaximum, opStablehloMinimum, opStablehloAnd, opStablehloShiftLeft,
		opStablehloPad, opDilate, opFakeQuant, opSVDF, opBitcast, opEmbeddingLookupSparse,
		opAudioSpectrogram:
		return 1 // ops4.go and rnn.go check the rest
	case opConv3DTranspose, opStablehloScatter:
		return 3
	}
	return 2
}

func floatMap(o op, x *Tensor, fn func(float64) float64) (*Tensor, error) {
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
	for i, v := range x.F {
		r.F[i] = float32(fn(float64(v)))
	}
	return r, nil
}

// transposeConv is TRANSPOSE_CONV (inputs: output_shape, weights
// [out, h, w, in], input, optional bias; TransposeConvOptions: padding,
// stride_w, stride_h, fused_activation_function), as reference
// transpose_conv.h scatters each input point through the filter, with the
// padding LiteRT computes from the output's size.
func transposeConv(o op, outShape []int, w, x, b *Tensor) (*Tensor, error) {
	if len(outShape) != 4 || len(x.Shape) != 4 || len(w.Shape) != 4 || !x.Type.IsFloat() || !w.Type.IsFloat() || (b != nil && !b.Type.IsFloat()) {
		return nil, &UnsupportedError{"TRANSPOSE_CONV with these operands is not supported"}
	}
	same := o.i8(4, 0) == 0
	sw, sh, act := int(o.i32(6, 1)), int(o.i32(8, 1)), o.i8(10, 0)
	n, ih, iw, ic := x.Shape[0], x.Shape[1], x.Shape[2], x.Shape[3]
	oc, kh, kw := w.Shape[0], w.Shape[1], w.Shape[2]
	if w.Shape[3] != ic || outShape[0] != n || outShape[3] != oc {
		return nil, fmt.Errorf("TRANSPOSE_CONV: filter %v, input %v and output shape %v do not agree", w.Shape, x.Shape, outShape)
	}
	oh, ow := outShape[1], outShape[2]
	_, ph := window(oh, kh, sh, 1, same)
	_, pw := window(ow, kw, sw, 1, same)
	r := &Tensor{Type: Float32, Shape: slices.Clone(outShape), F: make([]float32, prod(outShape))}
	for bi := range n {
		for y := range ih {
			for xx := range iw {
				in := x.F[((bi*ih+y)*iw+xx)*ic:][:ic]
				for ky := range kh {
					oy := y*sh - ph + ky
					if oy < 0 || oy >= oh {
						continue
					}
					for kx := range kw {
						ox := xx*sw - pw + kx
						if ox < 0 || ox >= ow {
							continue
						}
						dst := r.F[((bi*oh+oy)*ow+ox)*oc:][:oc]
						for c := range oc {
							wr := w.F[((c*kh+ky)*kw+kx)*ic:][:ic]
							var s float32
							for ci, v := range in {
								s += v * wr[ci]
							}
							dst[c] += s
						}
					}
				}
			}
		}
	}
	for i := range r.F {
		if b != nil {
			r.F[i] += b.F[i%oc]
		}
		r.F[i] = activation(act, r.F[i])
	}
	return r, nil
}

// mirrorPad is MIRROR_PAD (MirrorPadOptions.mode: REFLECT 0, SYMMETRIC 1).
func mirrorPad(o op, x *Tensor, pads []int) (*Tensor, error) {
	if len(pads) != 2*len(x.Shape) {
		return nil, fmt.Errorf("operator MIRROR_PAD takes a before and after padding per dimension")
	}
	off := 1
	if o.i8(4, 0) == 1 {
		off = 0
	}
	shape := make([]int, len(x.Shape))
	for i, d := range x.Shape {
		if pads[2*i] < 0 || pads[2*i+1] < 0 || pads[2*i] > d-off || pads[2*i+1] > d-off {
			return nil, fmt.Errorf("operator MIRROR_PAD: padding %d, %d of a dimension of %d", pads[2*i], pads[2*i+1], d)
		}
		shape[i] = pads[2*i] + d + pads[2*i+1]
	}
	r := alloc(x, shape)
	xs, rs := strides(x.Shape), strides(shape)
	for j := range r.Len() {
		src, rem := 0, j
		for d := range shape {
			c := rem/rs[d] - pads[2*d]
			rem %= rs[d]
			n := x.Shape[d]
			switch {
			case c < 0:
				c = -c - 1 + off
			case c >= n:
				c = 2*n - c - 1 - off
			}
			src += c * xs[d]
		}
		at(r, j, x, src)
	}
	return r, nil
}

// depthSpace is DEPTH_TO_SPACE and SPACE_TO_DEPTH of an NHWC tensor
// (block_size), in TensorFlow's DCR order.
func depthSpace(o op, x *Tensor, bs int) (*Tensor, error) {
	if len(x.Shape) != 4 || bs < 1 {
		return nil, fmt.Errorf("operator %s takes a 4-D tensor and a block size", o.name)
	}
	n, h, w, c := x.Shape[0], x.Shape[1], x.Shape[2], x.Shape[3]
	if o.code == opDepthToSpace {
		if c%(bs*bs) != 0 {
			return nil, fmt.Errorf("operator DEPTH_TO_SPACE: depth %d is not a multiple of %d", c, bs*bs)
		}
		oc := c / (bs * bs)
		r := alloc(x, []int{n, h * bs, w * bs, oc})
		for j := range r.Len() {
			cc, rem := j%oc, j/oc
			ox, rem := rem%(w*bs), rem/(w*bs)
			oy, b := rem%(h*bs), rem/(h*bs)
			src := ((b*h+oy/bs)*w+ox/bs)*c + ((oy%bs)*bs+ox%bs)*oc + cc
			at(r, j, x, src)
		}
		return r, nil
	}
	if h%bs != 0 || w%bs != 0 {
		return nil, fmt.Errorf("operator SPACE_TO_DEPTH: %dx%d is not a multiple of %d", h, w, bs)
	}
	r := alloc(x, []int{n, h / bs, w / bs, c * bs * bs})
	for j := range x.Len() {
		cc, rem := j%c, j/c
		ix, rem := rem%w, rem/w
		iy, b := rem%h, rem/h
		dst := ((b*(h/bs)+iy/bs)*(w/bs)+ix/bs)*c*bs*bs + ((iy%bs)*bs+ix%bs)*c + cc
		at(r, dst, x, j)
	}
	return r, nil
}

// spaceToBatch is SPACE_TO_BATCH_ND: the spatial dimensions padded, then
// each block's offset moved to the batch (batch index = offset · N + b).
func spaceToBatch(o op, x *Tensor, block, pads []int) (*Tensor, error) {
	m := len(block)
	if len(pads) != 2*m || len(x.Shape) < m+1 {
		return nil, fmt.Errorf("operator SPACE_TO_BATCH_ND takes a block and paddings per spatial dimension")
	}
	n := x.Shape[0]
	shape := slices.Clone(x.Shape)
	nb := 1
	for i, bs := range block {
		p := pads[2*i] + x.Shape[i+1] + pads[2*i+1]
		if bs < 1 || p%bs != 0 {
			return nil, fmt.Errorf("operator SPACE_TO_BATCH_ND: padded size %d is not a multiple of %d", p, bs)
		}
		shape[i+1] = p / bs
		nb *= bs
	}
	shape[0] = n * nb
	r := alloc(x, shape)
	rs, xs := strides(shape), strides(x.Shape)
	for j := range r.Len() {
		ob := j / rs[0]
		off, b := ob/n, ob%n
		src, inside := b*xs[0], true
		bstr := nb
		for i := range m {
			bstr /= block[i]
			bo := (off / bstr) % block[i]
			c := (j/rs[i+1])%shape[i+1]*block[i] + bo - pads[2*i]
			if c < 0 || c >= x.Shape[i+1] {
				inside = false
				break
			}
			src += c * xs[i+1]
		}
		if inside {
			src += j % rs[m]
			at(r, j, x, src)
		}
	}
	return r, nil
}

// batchToSpace is BATCH_TO_SPACE_ND, SPACE_TO_BATCH_ND's inverse, then
// cropped.
func batchToSpace(o op, x *Tensor, block, crops []int) (*Tensor, error) {
	m := len(block)
	nb := 1
	for _, bs := range block {
		nb *= max(bs, 1)
	}
	if len(crops) != 2*m || len(x.Shape) < m+1 || x.Shape[0]%nb != 0 {
		return nil, fmt.Errorf("operator BATCH_TO_SPACE_ND takes a block and crops per spatial dimension of a batch that divides by the block")
	}
	n := x.Shape[0] / nb
	shape := slices.Clone(x.Shape)
	shape[0] = n
	for i, bs := range block {
		shape[i+1] = x.Shape[i+1]*bs - crops[2*i] - crops[2*i+1]
		if shape[i+1] < 0 {
			return nil, fmt.Errorf("operator BATCH_TO_SPACE_ND crops more than the tensor holds")
		}
	}
	r := alloc(x, shape)
	rs, xs := strides(shape), strides(x.Shape)
	for j := range r.Len() {
		b := j / rs[0]
		off, src := 0, 0
		for i := range m {
			c := (j/rs[i+1])%shape[i+1] + crops[2*i]
			off = off*block[i] + c%block[i]
			src += c / block[i] * xs[i+1]
		}
		src += (off*n+b)*xs[0] + j%rs[m]
		at(r, j, x, src)
	}
	return r, nil
}

// l2Normalize is L2_NORMALIZATION over the last dimension, with
// reference l2normalization.h's epsilon of 1e-6 on the norm.
func l2Normalize(o op, x *Tensor) (*Tensor, error) {
	if !x.Type.IsFloat() || len(x.Shape) == 0 {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	d := x.Shape[len(x.Shape)-1]
	act := o.i8(4, 0)
	r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
	for s := 0; s+d <= len(x.F); s += d {
		var sq float32
		for _, v := range x.F[s : s+d] {
			sq += v * v
		}
		norm := max(float32(math.Sqrt(float64(sq))), 1e-6)
		for i, v := range x.F[s : s+d] {
			r.F[s+i] = activation(act, v/norm)
		}
	}
	return r, nil
}

// lrn is LOCAL_RESPONSE_NORMALIZATION over the last dimension
// (LocalResponseNormalizationOptions: radius, bias, alpha, beta).
func lrn(o op, x *Tensor) (*Tensor, error) {
	if !x.Type.IsFloat() || len(x.Shape) == 0 {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	rad, bias, alpha, beta := int(o.i32(4, 0)), o.f32(6, 0), o.f32(8, 0), o.f32(10, 0)
	d := x.Shape[len(x.Shape)-1]
	r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
	for s := 0; s+d <= len(x.F); s += d {
		row := x.F[s : s+d]
		for i, v := range row {
			var acc float32
			for j := max(0, i-rad); j <= min(d-1, i+rad); j++ {
				acc += row[j] * row[j]
			}
			r.F[s+i] = v * float32(math.Pow(float64(bias+alpha*acc), float64(-beta)))
		}
	}
	return r, nil
}

func gatherND(o op, x, idx *Tensor) (*Tensor, error) {
	if !idx.Type.IsInt() || len(idx.Shape) == 0 {
		return nil, &UnsupportedError{"GATHER_ND with these indices is not supported"}
	}
	k := idx.Shape[len(idx.Shape)-1]
	if k > len(x.Shape) {
		return nil, fmt.Errorf("GATHER_ND: index depth %d of a rank-%d tensor", k, len(x.Shape))
	}
	inner := prod(x.Shape[k:])
	xs := strides(x.Shape)
	shape := append(slices.Clone(idx.Shape[:len(idx.Shape)-1]), x.Shape[k:]...)
	r := alloc(x, shape)
	for g := range idx.Len() / max(k, 1) {
		base := 0
		for d := range k {
			v := int(idx.I[g*k+d])
			if v < 0 || v >= x.Shape[d] {
				return nil, fmt.Errorf("GATHER_ND: index %d out of [0, %d)", v, x.Shape[d])
			}
			base += v * xs[d]
		}
		for i := range inner {
			at(r, g*inner+i, x, base+i)
		}
	}
	return r, nil
}

// scatterND is SCATTER_ND (indices, updates, shape): zeros of shape with
// each update added at its index, duplicates summed.
func scatterND(o op, idx, upd *Tensor, shape []int) (*Tensor, error) {
	if !idx.Type.IsInt() || len(idx.Shape) == 0 || (upd.F == nil && upd.I == nil) {
		return nil, &UnsupportedError{"SCATTER_ND with these operands is not supported"}
	}
	k := idx.Shape[len(idx.Shape)-1]
	if k > len(shape) {
		return nil, fmt.Errorf("SCATTER_ND: index depth %d of a rank-%d shape", k, len(shape))
	}
	inner := prod(shape[k:])
	ss := strides(shape)
	r := alloc(upd, shape)
	n := idx.Len() / max(k, 1)
	if upd.Len() != n*inner {
		return nil, fmt.Errorf("SCATTER_ND: %d updates for %d indices of %d elements", upd.Len(), n, inner)
	}
	for g := range n {
		base := 0
		for d := range k {
			v := int(idx.I[g*k+d])
			if v < 0 || v >= shape[d] {
				return nil, fmt.Errorf("SCATTER_ND: index %d out of [0, %d)", v, shape[d])
			}
			base += v * ss[d]
		}
		for i := range inner {
			if r.F != nil {
				r.F[base+i] += upd.F[g*inner+i]
			} else {
				r.I[base+i] += upd.I[g*inner+i]
			}
		}
	}
	return r, nil
}

// oneHot is ONE_HOT (indices, depth, on_value, off_value; OneHotOptions
// axis, -1 for a new last dimension).
func oneHot(o op, idx, depth, on, off *Tensor) (*Tensor, error) {
	if !idx.Type.IsInt() || on.Len() != 1 || off.Len() != 1 || depth.Len() != 1 {
		return nil, &UnsupportedError{"ONE_HOT with these operands is not supported"}
	}
	d := axes(depth)[0]
	axis := int(o.i32(4, -1))
	if axis < 0 {
		axis += len(idx.Shape) + 1
	}
	if d < 0 || axis < 0 || axis > len(idx.Shape) {
		return nil, fmt.Errorf("ONE_HOT: depth %d, axis %d", d, axis)
	}
	shape := append(append(slices.Clone(idx.Shape[:axis]), d), idx.Shape[axis:]...)
	outer, inner := prod(idx.Shape[:axis]), prod(idx.Shape[axis:])
	r := alloc(on, shape)
	for p := range outer {
		for k := range d {
			for q := range inner {
				j := (p*d+k)*inner + q
				if idx.I[p*inner+q] == int64(k) {
					at(r, j, on, 0)
				} else {
					at(r, j, off, 0)
				}
			}
		}
	}
	return r, nil
}

func rangeOp(o op, start, limit, delta *Tensor) (*Tensor, error) {
	if start.Len() != 1 || limit.Len() != 1 || delta.Len() != 1 {
		return nil, fmt.Errorf("operator RANGE takes scalars")
	}
	if start.I != nil && limit.I != nil && delta.I != nil {
		s, l, d := start.I[0], limit.I[0], delta.I[0]
		if d == 0 || (d > 0) != (l >= s) && l != s {
			return nil, fmt.Errorf("operator RANGE: start %d, limit %d, delta %d", s, l, d)
		}
		n := max(0, (abs64(l-s)+abs64(d)-1)/abs64(d))
		r := &Tensor{Type: start.Type, Shape: []int{int(n)}, I: make([]int64, n)}
		for i := range r.I {
			r.I[i] = s + int64(i)*d
		}
		return r, nil
	}
	s, l, d := float64(start.f(0)), float64(limit.f(0)), float64(delta.f(0))
	if d == 0 || (d > 0) != (l >= s) && l != s {
		return nil, fmt.Errorf("operator RANGE: start %v, limit %v, delta %v", s, l, d)
	}
	n := int(math.Ceil(math.Abs((l - s) / d)))
	r := &Tensor{Type: Float32, Shape: []int{n}, F: make([]float32, n)}
	for i := range r.F {
		r.F[i] = float32(s + float64(i)*d)
	}
	return r, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func reverse(x *Tensor, ax []int) (*Tensor, error) {
	rev := make([]bool, len(x.Shape))
	for _, a := range ax {
		if a < 0 {
			a += len(x.Shape)
		}
		if a < 0 || a >= len(x.Shape) {
			return nil, fmt.Errorf("REVERSE_V2: axis out of range")
		}
		rev[a] = true
	}
	r := alloc(x, x.Shape)
	xs := strides(x.Shape)
	for j := range r.Len() {
		src := 0
		for d, n := range x.Shape {
			c := j / xs[d] % n
			if rev[d] {
				c = n - 1 - c
			}
			src += c * xs[d]
		}
		at(r, j, x, src)
	}
	return r, nil
}

// cumsum is CUMSUM (CumsumOptions: exclusive, reverse).
func cumsum(o op, x *Tensor, axis int) (*Tensor, error) {
	if axis < 0 {
		axis += len(x.Shape)
	}
	if axis < 0 || axis >= len(x.Shape) || (x.F == nil && x.I == nil) {
		return nil, fmt.Errorf("CUMSUM: axis %d of a rank-%d %s tensor", axis, len(x.Shape), x.Type)
	}
	excl, rev := o.bool(4), o.bool(6)
	d := x.Shape[axis]
	outer, inner := prod(x.Shape[:axis]), prod(x.Shape[axis+1:])
	r := alloc(x, x.Shape)
	for p := range outer {
		for q := range inner {
			var fs float32
			var is int64
			for s := range d {
				k := s
				if rev {
					k = d - 1 - s
				}
				i := (p*d+k)*inner + q
				if r.F != nil {
					if !excl {
						fs += x.F[i]
					}
					r.F[i] = fs
					if excl {
						fs += x.F[i]
					}
				} else {
					if !excl {
						is += x.I[i]
					}
					r.I[i] = is
					if excl {
						is += x.I[i]
					}
				}
			}
		}
	}
	return r, nil
}

func reduceBool(o op, x *Tensor, ax []int, keep bool) (*Tensor, error) {
	red := map[int]bool{}
	for _, a := range ax {
		if a < 0 {
			a += len(x.Shape)
		}
		red[a] = true
	}
	var shape, kept []int
	for i, d := range x.Shape {
		if red[i] {
			kept = append(kept, 1)
			if keep {
				shape = append(shape, 1)
			}
			continue
		}
		kept = append(kept, d)
		shape = append(shape, d)
	}
	all := o.code == opReduceAll
	r := &Tensor{Type: Bool, Shape: shape, B: make([]bool, prod(kept))}
	for i := range r.B {
		r.B[i] = all
	}
	ks := strides(kept)
	for i := range x.Len() {
		idx, dst := i, 0
		for d := len(x.Shape) - 1; d >= 0; d-- {
			c := idx % x.Shape[d]
			idx /= x.Shape[d]
			if !red[d] {
				dst += c * ks[d]
			}
		}
		if v := x.f(i) != 0; all {
			r.B[dst] = r.B[dst] && v
		} else {
			r.B[dst] = r.B[dst] || v
		}
	}
	if r.Shape == nil {
		r.Shape = []int{}
	}
	return r, nil
}

// bucketize is BUCKETIZE (BucketizeOptions.boundaries): the number of
// boundaries at or below each value.
func bucketize(o op, x *Tensor) (*Tensor, error) {
	var bounds []float32
	if o.opts != nil {
		if p := field(o.opts, 4); p != 0 {
			n, start := o.opts.VectorLen(p), o.opts.Vector(p)
			for i := range n {
				bounds = append(bounds, o.opts.GetFloat32(start+flatbuffers.UOffsetT(4*i)))
			}
		}
	}
	if !sort.SliceIsSorted(bounds, func(i, j int) bool { return bounds[i] < bounds[j] }) {
		return nil, fmt.Errorf("BUCKETIZE: boundaries are not sorted")
	}
	r := &Tensor{Type: Int32, Shape: x.Shape, I: make([]int64, x.Len())}
	for i := range r.I {
		v := x.f(i)
		r.I[i] = int64(sort.Search(len(bounds), func(k int) bool { return bounds[k] > v }))
	}
	return r, nil
}

// segment is SEGMENT_SUM (sorted ids, as many segments as the largest id
// plus one) and the UNSORTED_SEGMENT_* reductions (num_segments given):
// the ids index data's leading dimensions; an empty segment holds the
// reduction's identity (0, 1, the lowest or the highest value).
func segment(o op, x, ids, num *Tensor) (*Tensor, error) {
	if !ids.Type.IsInt() || len(ids.Shape) > len(x.Shape) || (x.F == nil && x.I == nil) {
		return nil, &UnsupportedError{"operator " + o.name + " with these operands is not supported"}
	}
	for i, d := range ids.Shape {
		if x.Shape[i] != d {
			return nil, fmt.Errorf("operator %s: segment ids %v do not prefix data %v", o.name, ids.Shape, x.Shape)
		}
	}
	ns := 0
	if o.code == opSegmentSum {
		for i, v := range ids.I {
			if i > 0 && v < ids.I[i-1] {
				return nil, fmt.Errorf("SEGMENT_SUM: segment ids are not sorted")
			}
			ns = max(ns, int(v)+1)
		}
	} else {
		if num == nil || num.Len() != 1 {
			return nil, fmt.Errorf("operator %s takes num_segments", o.name)
		}
		ns = axes(num)[0]
	}
	inner := prod(x.Shape[len(ids.Shape):])
	shape := append([]int{ns}, x.Shape[len(ids.Shape):]...)
	acc := make([]float64, ns*inner)
	init := 0.0
	switch o.code {
	case opUnsortedSegmentProd:
		init = 1
	case opUnsortedSegmentMax:
		init = math.Inf(-1)
	case opUnsortedSegmentMin:
		init = math.Inf(1)
	}
	for i := range acc {
		acc[i] = init
	}
	for s, id := range ids.I {
		if id < 0 || int(id) >= ns {
			continue // LiteRT skips ids outside [0, num_segments)
		}
		for i := range inner {
			v := float64(x.f(s*inner + i))
			if x.I != nil {
				v = float64(x.I[s*inner+i])
			}
			a := &acc[int(id)*inner+i]
			switch o.code {
			case opUnsortedSegmentProd:
				*a *= v
			case opUnsortedSegmentMax:
				*a = math.Max(*a, v)
			case opUnsortedSegmentMin:
				*a = math.Min(*a, v)
			default:
				*a += v
			}
		}
	}
	r := alloc(x, shape)
	for i, v := range acc {
		if r.F != nil {
			switch {
			case math.IsInf(v, -1):
				v = -math.MaxFloat32
			case math.IsInf(v, 1):
				v = math.MaxFloat32
			}
			r.F[i] = float32(v)
			continue
		}
		switch {
		case math.IsInf(v, -1):
			r.I[i] = math.MinInt32
		case math.IsInf(v, 1):
			r.I[i] = math.MaxInt32
		default:
			r.I[i] = int64(v)
		}
	}
	return r, nil
}

// topK is TOPK_V2 (input, k): the k largest of each last-dimension row,
// in descending order, the lower index first among equals, and their
// indices.
func topK(o op, x, kt *Tensor) ([]*Tensor, error) {
	if x == nil || kt == nil || kt.Len() != 1 || len(x.Shape) == 0 {
		return nil, fmt.Errorf("operator TOPK_V2 takes a tensor and k")
	}
	d := x.Shape[len(x.Shape)-1]
	k := axes(kt)[0]
	if k < 0 || k > d {
		return nil, fmt.Errorf("TOPK_V2: k %d of a dimension of %d", k, d)
	}
	shape := append(slices.Clone(x.Shape[:len(x.Shape)-1]), k)
	vals := alloc(x, shape)
	idx := &Tensor{Type: Int32, Shape: shape, I: make([]int64, prod(shape))}
	order := make([]int, d)
	rows := x.Len() / max(d, 1)
	for row := range rows {
		for i := range order {
			order[i] = i
		}
		val := func(i int) float64 {
			if x.I != nil {
				return float64(x.I[row*d+i])
			}
			return float64(x.f(row*d + i))
		}
		sort.SliceStable(order, func(i, j int) bool { return val(order[i]) > val(order[j]) })
		for j := range k {
			at(vals, row*k+j, x, row*d+order[j])
			idx.I[row*k+j] = int64(order[j])
		}
	}
	return []*Tensor{vals, idx}, nil
}
