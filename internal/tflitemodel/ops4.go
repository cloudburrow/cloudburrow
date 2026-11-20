package tflitemodel

import (
	"cmp"
	"fmt"
	"math"
	"math/bits"
	"math/cmplx"
	"math/rand/v2"
	"slices"

	flatbuffers "github.com/google/flatbuffers/go"
)

// More of LiteRT's builtin operators (#1775), each as its reference kernel
// computes it: NON_MAX_SUPPRESSION_V4 and V5
// (reference/non_max_suppression.h), CONV_3D and CONV_3D_TRANSPOSE
// (reference/conv3d.h, conv3d_transpose.h), RANDOM_UNIFORM and
// RANDOM_STANDARD_NORMAL (random_ops.cc: TensorFlow's Philox4x32-10 from
// the op's seeds), RFFT2D, REAL, IMAG and COMPLEX_ABS; and, in
// resource.go, the HASHTABLE and variable operators and CALL_ONCE. The
// fused recurrent kernels are rnn.go's.
const (
	opNonMaxSuppressionV4 = 120
	opNonMaxSuppressionV5 = 121
	opCallOnce            = 129
	opRfft2D              = 131
	opConv3D              = 132
	opImag                = 133
	opReal                = 134
	opComplexAbs          = 135
	opHashtable           = 136
	opHashtableFind       = 137
	opHashtableImport     = 138
	opHashtableSize       = 139
	opConv3DTranspose     = 141
	opVarHandle           = 142
	opReadVariable        = 143
	opAssignVariable      = 144
	opRandomNormal        = 146
	opRandomUniform       = 148
)

func init() {
	for code, name := range map[int32]string{opNonMaxSuppressionV4: "NON_MAX_SUPPRESSION_V4", opNonMaxSuppressionV5: "NON_MAX_SUPPRESSION_V5",
		opCallOnce: "CALL_ONCE", opRfft2D: "RFFT2D", opConv3D: "CONV_3D", opImag: "IMAG", opReal: "REAL", opComplexAbs: "COMPLEX_ABS",
		opHashtable: "HASHTABLE", opHashtableFind: "HASHTABLE_FIND", opHashtableImport: "HASHTABLE_IMPORT", opHashtableSize: "HASHTABLE_SIZE",
		opConv3DTranspose: "CONV_3D_TRANSPOSE", opVarHandle: "VAR_HANDLE", opReadVariable: "READ_VARIABLE", opAssignVariable: "ASSIGN_VARIABLE",
		opRandomNormal: "RANDOM_STANDARD_NORMAL", opRandomUniform: "RANDOM_UNIFORM"} {
		opNames[code] = name
	}
}

// listOutput reports whether code is an operator evalList runs: one of
// several (or no) outputs, or one that keeps state across operators.
func listOutput(code int32) bool {
	switch code {
	case opNonMaxSuppressionV4, opNonMaxSuppressionV5, opBidirectionalSequenceLSTM, opBidirectionalSequenceRNN,
		opCallOnce, opHashtable, opHashtableFind, opHashtableImport, opHashtableSize, opVarHandle, opReadVariable, opAssignVariable,
		opUnique, opHashtableLookup, opStablehloComposite, opDetectionPostProcess, opStablehloCase:
		return true
	}
	return false
}

// evalList runs the operators listOutput names, giving n outputs.
func (m *Model) evalList(o op, a []*Tensor, n, depth int, st *runState) ([]*Tensor, error) {
	switch o.code {
	case opNonMaxSuppressionV4, opNonMaxSuppressionV5:
		return nms(o, a, n)
	case opBidirectionalSequenceLSTM:
		return evalBiLSTM(o, a, n)
	case opBidirectionalSequenceRNN:
		return evalBiRNN(o, a, n)
	case opUnique:
		if len(a) == 0 || a[0] == nil {
			return nil, fmt.Errorf("operator UNIQUE takes an input")
		}
		return unique(o, a[0], n) // ops5.go
	case opHashtableLookup:
		return hashtableLookup(o, a, n)
	case opDetectionPostProcess:
		return detectionPostProcess(o, a, n) // detect.go
	case opStablehloComposite:
		k := int(o.i32(6, -1))
		if k < 0 || k >= len(m.graphs) {
			return nil, fmt.Errorf("operator STABLEHLO_COMPOSITE names subgraph %d, which the model does not have", k)
		}
		return m.call(k, a, depth, st)
	case opStablehloCase:
		return m.stablehloCase(o, a, depth, st)
	}
	return m.evalResource(o, a, depth, st) // resource.go
}

// eval4 runs the operators of this file and rnn.go that have one output.
func (m *Model) eval4(o op, a []*Tensor) (*Tensor, error) {
	arg := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	x := arg(0)
	switch o.code {
	case opLSTM, opUnidirectionalSequenceLSTM:
		return evalLSTM(o, a)
	case opRNN, opUnidirectionalSequenceRNN:
		return evalRNN(o, a)
	case opConv3D:
		return conv3D(o, x, arg(1), arg(2))
	case opConv3DTranspose:
		return conv3DTranspose(o, axes(x), arg(1), arg(2), arg(3))
	case opRandomUniform, opRandomNormal:
		return random(o, axes(x))
	case opRfft2D:
		return rfft2D(o, x, axes(arg(1)))
	case opReal, opImag, opComplexAbs:
		if x.C == nil {
			return nil, fmt.Errorf("operator %s takes a COMPLEX64 tensor, not %s", o.name, x.Type)
		}
		r := &Tensor{Type: Float32, Shape: x.Shape, F: make([]float32, len(x.C))}
		for i, c := range x.C {
			switch o.code {
			case opReal:
				r.F[i] = real(c)
			case opImag:
				r.F[i] = imag(c)
			default:
				r.F[i] = float32(cmplx.Abs(complex128(c)))
			}
		}
		return r, nil
	}
	return m.eval5(o, a) // ops5.go
}

// scalar is a one-value tensor's value.
func scalar(o op, t *Tensor, what string) (float32, error) {
	if t == nil || t.Len() != 1 {
		return 0, fmt.Errorf("operator %s: %s must hold one value", o.name, what)
	}
	return t.f(0), nil
}

// nms is NON_MAX_SUPPRESSION_V4 (boxes, scores, max_output_size,
// iou_threshold, score_threshold: selected_indices, valid_outputs) and V5
// (and soft_nms_sigma: selected_indices, selected_scores, valid_outputs),
// their outputs padded with zeros to max_output_size.
func nms(o op, a []*Tensor, n int) ([]*Tensor, error) {
	want, outs := 5, 2
	if o.code == opNonMaxSuppressionV5 {
		want, outs = 6, 3
	}
	if len(a) != want || n != outs {
		return nil, fmt.Errorf("operator %s takes %d inputs and gives %d outputs", o.name, want, outs)
	}
	boxes, scores := a[0], a[1]
	if boxes == nil || len(boxes.Shape) != 2 || boxes.Shape[1] != 4 || !boxes.Type.IsFloat() {
		return nil, fmt.Errorf("operator %s: boxes must be [num_boxes, 4] floats", o.name)
	}
	num := boxes.Shape[0]
	if scores == nil || len(scores.Shape) != 1 || scores.Shape[0] != num || !scores.Type.IsFloat() {
		return nil, fmt.Errorf("operator %s: scores must be [%d] floats", o.name, num)
	}
	mo, err := scalar(o, a[2], "max_output_size")
	if err != nil {
		return nil, err
	}
	maxOut := int(mo)
	if maxOut < 0 {
		return nil, fmt.Errorf("operator %s: invalid max_output_size %d", o.name, maxOut)
	}
	iou, err := scalar(o, a[3], "iou_threshold")
	if err != nil {
		return nil, err
	}
	thr, err := scalar(o, a[4], "score_threshold")
	if err != nil {
		return nil, err
	}
	var sigma float32
	if o.code == opNonMaxSuppressionV5 {
		if sigma, err = scalar(o, a[5], "soft_nms_sigma"); err != nil {
			return nil, err
		}
		if sigma < 0 {
			return nil, fmt.Errorf("operator %s: invalid sigma value for soft NMS: %v", o.name, sigma)
		}
	}
	idx, sc := nonMaxSuppression(boxes.F, scores.F, maxOut, iou, thr, sigma)
	ind := &Tensor{Type: Int32, Shape: []int{maxOut}, I: make([]int64, maxOut)}
	for i, v := range idx {
		ind.I[i] = int64(v)
	}
	valid := &Tensor{Type: Int32, Shape: []int{}, I: []int64{int64(len(idx))}}
	if o.code == opNonMaxSuppressionV4 {
		return []*Tensor{ind, valid}, nil
	}
	s := &Tensor{Type: Float32, Shape: []int{maxOut}, F: make([]float32, maxOut)}
	copy(s.F, sc)
	return []*Tensor{ind, s, valid}, nil
}

// iouAt is ComputeIntersectionOverUnion of boxes i and j.
func iouAt(boxes []float32, i, j int) float32 {
	bi, bj := boxes[4*i:4*i+4], boxes[4*j:4*j+4]
	iy0, iy1 := min(bi[0], bi[2]), max(bi[0], bi[2])
	ix0, ix1 := min(bi[1], bi[3]), max(bi[1], bi[3])
	jy0, jy1 := min(bj[0], bj[2]), max(bj[0], bj[2])
	jx0, jx1 := min(bj[1], bj[3]), max(bj[1], bj[3])
	ai := (iy1 - iy0) * (ix1 - ix0)
	aj := (jy1 - jy0) * (jx1 - jx0)
	if ai <= 0 || aj <= 0 {
		return 0
	}
	inter := max(min(iy1, jy1)-max(iy0, jy0), 0) * max(min(ix1, jx1)-max(ix0, jx0), 0)
	return inter / (ai + aj - inter)
}

// nonMaxSuppression is reference_ops::NonMaxSuppression: greedy selection
// by score, hard suppression at iou_threshold and, when sigma > 0, the
// Gaussian soft suppression. Ties go to the lower index.
func nonMaxSuppression(boxes, scores []float32, maxOut int, iouThr, scoreThr, sigma float32) ([]int, []float32) {
	type cand struct {
		index int
		score float32
		begin int
	}
	var q []cand
	for i, s := range scores {
		if s > scoreThr {
			q = append(q, cand{i, s, 0})
		}
	}
	better := func(a, b cand) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.index, b.index)
	}
	numOut := min(len(q), maxOut)
	var sel []int
	var selScores []float32
	var scale float32
	if sigma > 0 {
		scale = -0.5 / sigma
	}
	for len(sel) < numOut && len(q) > 0 {
		best := 0
		for i := range q {
			if better(q[i], q[best]) < 0 {
				best = i
			}
		}
		next := q[best]
		q = slices.Delete(q, best, best+1)
		orig := next.score
		hard := false
		for j := len(sel) - 1; j >= next.begin; j-- {
			iou := iouAt(boxes, next.index, sel[j])
			if iou >= iouThr {
				hard = true
				break
			}
			if sigma > 0 {
				next.score *= float32(math.Exp(float64(scale * iou * iou)))
			}
			if next.score <= scoreThr {
				break
			}
		}
		next.begin = len(sel)
		if !hard {
			if next.score == orig {
				sel = append(sel, next.index)
				selScores = append(selScores, next.score)
			}
			if next.score > scoreThr {
				q = append(q, next)
			}
		}
	}
	return sel, selScores
}

// conv3dOpts are Conv3DOptions: padding, stride_d, stride_w, stride_h,
// fused_activation_function, dilation_d/w/h_factor.
func conv3dOpts(o op) (same bool, stride, dil [3]int, act int8) {
	same = o.i8(4, 0) == 0
	stride = [3]int{int(o.i32(6, 1)), int(o.i32(10, 1)), int(o.i32(8, 1))}
	dil = [3]int{int(o.i32(14, 1)), int(o.i32(18, 1)), int(o.i32(16, 1))}
	for i := range 3 {
		stride[i], dil[i] = max(stride[i], 1), max(dil[i], 1)
	}
	return same, stride, dil, o.i8(12, 0)
}

// convOut is ComputeOutSize, and pad ComputePaddingWithOffset's padding
// before.
func convOut(same bool, in, filter, stride, dil int) int {
	eff := (filter-1)*dil + 1
	if same {
		return (in + stride - 1) / stride
	}
	return (in + stride - eff) / stride
}

func convPad(stride, dil, in, filter, out int) int {
	eff := (filter-1)*dil + 1
	return max((out-1)*stride+eff-in, 0) / 2
}

func conv3D(o op, x, w, b *Tensor) (*Tensor, error) {
	if w == nil || len(x.Shape) != 5 || len(w.Shape) != 5 || !x.Type.IsFloat() || !w.Type.IsFloat() {
		return nil, &UnsupportedError{"CONV_3D with these operands is not supported"}
	}
	same, stride, dil, act := conv3dOpts(o)
	nb, in, ic := x.Shape[0], x.Shape[1:4], x.Shape[4]
	f, oc := w.Shape[0:3], w.Shape[4]
	if w.Shape[3] != ic {
		return nil, fmt.Errorf("CONV_3D: %d input channels for a filter of %d", ic, w.Shape[3])
	}
	var out, pad [3]int
	for i := range 3 {
		out[i] = convOut(same, in[i], f[i], stride[i], dil[i])
		if out[i] <= 0 {
			return nil, fmt.Errorf("CONV_3D: input %v is smaller than the filter %v", x.Shape, w.Shape)
		}
		pad[i] = convPad(stride[i], dil[i], in[i], f[i], out[i])
	}
	r := &Tensor{Type: Float32, Shape: []int{nb, out[0], out[1], out[2], oc}}
	r.F = make([]float32, prod(r.Shape))
	k := 0
	for n := range nb {
		for od := range out[0] {
			for oy := range out[1] {
				for ox := range out[2] {
					for c := range oc {
						var s float32
						for fd := range f[0] {
							id := od*stride[0] - pad[0] + fd*dil[0]
							if id < 0 || id >= in[0] {
								continue
							}
							for fy := range f[1] {
								iy := oy*stride[1] - pad[1] + fy*dil[1]
								if iy < 0 || iy >= in[1] {
									continue
								}
								for fx := range f[2] {
									ix := ox*stride[2] - pad[2] + fx*dil[2]
									if ix < 0 || ix >= in[2] {
										continue
									}
									xo := (((n*in[0]+id)*in[1]+iy)*in[2] + ix) * ic
									wo := ((fd*f[1]+fy)*f[2] + fx) * ic * oc
									for i := range ic {
										s += x.F[xo+i] * w.F[wo+i*oc+c]
									}
								}
							}
						}
						if b != nil {
							s += b.F[c]
						}
						r.F[k] = activation(act, s)
						k++
					}
				}
			}
		}
	}
	return r, nil
}

// conv3DTranspose is CONV_3D_TRANSPOSE: output_shape, filter [d, h, w,
// out, in], input, and an optional bias.
func conv3DTranspose(o op, shape []int, w, x, b *Tensor) (*Tensor, error) {
	if w == nil || x == nil || len(shape) != 5 || len(x.Shape) != 5 || len(w.Shape) != 5 || !x.Type.IsFloat() || !w.Type.IsFloat() {
		return nil, &UnsupportedError{"CONV_3D_TRANSPOSE with these operands is not supported"}
	}
	same, stride, dil, act := conv3dOpts(o)
	nb, in, ic := x.Shape[0], x.Shape[1:4], x.Shape[4]
	f, oc := w.Shape[0:3], w.Shape[3]
	if w.Shape[4] != ic || shape[4] != oc || shape[0] != nb {
		return nil, fmt.Errorf("CONV_3D_TRANSPOSE: output shape %v, filter %v and input %v do not match", shape, w.Shape, x.Shape)
	}
	out := shape[1:4]
	var pad [3]int
	for i := range 3 {
		if convOut(same, out[i], f[i], stride[i], dil[i]) != in[i] {
			return nil, fmt.Errorf("CONV_3D_TRANSPOSE: output shape %v does not give input %v", shape, x.Shape)
		}
		pad[i] = convPad(stride[i], dil[i], out[i], f[i], in[i])
	}
	r := &Tensor{Type: Float32, Shape: append([]int{}, shape...)}
	r.F = make([]float32, prod(shape))
	for n := range nb {
		for id := range in[0] {
			for iy := range in[1] {
				for ix := range in[2] {
					for i := range ic {
						v := x.F[(((n*in[0]+id)*in[1]+iy)*in[2]+ix)*ic+i]
						for fd := range f[0] {
							od := id*stride[0] - pad[0] + fd*dil[0]
							if od < 0 || od >= out[0] {
								continue
							}
							for fy := range f[1] {
								oy := iy*stride[1] - pad[1] + fy*dil[1]
								if oy < 0 || oy >= out[1] {
									continue
								}
								for fx := range f[2] {
									ox := ix*stride[2] - pad[2] + fx*dil[2]
									if ox < 0 || ox >= out[2] {
										continue
									}
									ro := (((n*out[0]+od)*out[1]+oy)*out[2] + ox) * oc
									wo := ((fd*f[1]+fy)*f[2] + fx) * oc * ic
									for c := range oc {
										r.F[ro+c] += v * w.F[wo+c*ic+i]
									}
								}
							}
						}
					}
				}
			}
		}
	}
	for k, v := range r.F {
		if b != nil {
			v += b.F[k%oc]
		}
		r.F[k] = activation(act, v)
	}
	return r, nil
}

// philox is TensorFlow's PhiloxRandom (Philox4x32-10): key from seed,
// counter words 2 and 3 from seed2.
type philox struct {
	key     [2]uint32
	counter [4]uint32
}

func newPhilox(seed, seed2 uint64) *philox {
	return &philox{key: [2]uint32{uint32(seed), uint32(seed >> 32)}, counter: [4]uint32{0, 0, uint32(seed2), uint32(seed2 >> 32)}}
}

func (p *philox) next() [4]uint32 {
	c, k := p.counter, p.key
	for round := range 10 {
		hi0, lo0 := bits.Mul32(0xD2511F53, c[0])
		hi1, lo1 := bits.Mul32(0xCD9E8D57, c[2])
		c = [4]uint32{hi1 ^ c[1] ^ k[0], lo1, hi0 ^ c[3] ^ k[1], lo0}
		if round < 9 {
			k[0] += 0x9E3779B9
			k[1] += 0xBB67AE85
		}
	}
	for i := range p.counter {
		p.counter[i]++
		if p.counter[i] != 0 {
			break
		}
	}
	return c
}

// seeded is a random operator's generator from RandomOptions' seed and
// seed2. LiteRT's TfLiteRandomParams holds them as int (the low 32 bits,
// sign-extended); both zero draws fresh seeds, as LiteRT does.
func seeded(o op) *philox {
	var seed, seed2 int64
	if o.opts != nil {
		seed = o.opts.GetInt64Slot(flatbuffers.VOffsetT(4), 0)
		seed2 = o.opts.GetInt64Slot(flatbuffers.VOffsetT(6), 0)
	}
	seed, seed2 = int64(int32(seed)), int64(int32(seed2))
	if seed == 0 && seed2 == 0 {
		seed, seed2 = rand.Int64(), rand.Int64()
	}
	return newPhilox(uint64(seed), uint64(seed2))
}

// uint32ToFloat is random::Uint32ToFloat: a float in [0, 1).
func uint32ToFloat(x uint32) float32 {
	return math.Float32frombits(127<<23|x&0x7fffff) - 1
}

// random is RANDOM_UNIFORM ([0, 1)) and RANDOM_STANDARD_NORMAL (Box-Muller)
// from RandomOptions' seed and seed2; both zero draws fresh seeds, as
// LiteRT does. Each Run starts the generator afresh, as a new interpreter.
func random(o op, shape []int) (*Tensor, error) {
	for _, d := range shape {
		if d < 0 {
			return nil, fmt.Errorf("operator %s: negative dimension in shape %v", o.name, shape)
		}
	}
	p := seeded(o)
	n := prod(shape)
	r := &Tensor{Type: Float32, Shape: append([]int{}, shape...), F: make([]float32, n)}
	for i := 0; i < n; i += 4 {
		s := p.next()
		var v [4]float32
		if o.code == opRandomUniform {
			for j := range 4 {
				v[j] = uint32ToFloat(s[j])
			}
		} else {
			for j := 0; j < 4; j += 2 {
				u1 := max(uint32ToFloat(s[j]), 1e-7)
				v1 := float32(2*math.Pi) * uint32ToFloat(s[j+1])
				u2 := float32(math.Sqrt(float64(-2 * float32(math.Log(float64(u1))))))
				v[j] = float32(math.Sin(float64(v1))) * u2
				v[j+1] = float32(math.Cos(float64(v1))) * u2
			}
		}
		copy(r.F[i:], v[:min(4, n-i)])
	}
	return r, nil
}

// rfft2D is RFFT2D: the 2-D DFT of the input's last two axes, cropped or
// zero-padded to fft_length, keeping fft_length[1]/2+1 columns.
func rfft2D(o op, x *Tensor, fl []int) (*Tensor, error) {
	if !x.Type.IsFloat() || len(x.Shape) < 2 || len(fl) != 2 || fl[0] <= 0 || fl[1] <= 0 {
		return nil, fmt.Errorf("operator RFFT2D takes a float tensor of rank 2 or more and two positive fft lengths")
	}
	r := len(x.Shape)
	h, w := x.Shape[r-2], x.Shape[r-1]
	fh, fw := fl[0], fl[1]
	cols := fw/2 + 1
	outer := prod(x.Shape[:r-2])
	shape := append(append([]int{}, x.Shape[:r-2]...), fh, cols)
	t := &Tensor{Type: Complex, Shape: shape, C: make([]complex64, outer*fh*cols)}
	for b := range outer {
		in := x.F[b*h*w : (b+1)*h*w]
		for k1 := range fh {
			for k2 := range cols {
				var s complex128
				for n1 := range min(h, fh) {
					for n2 := range min(w, fw) {
						ang := -2 * math.Pi * (float64(k1*n1)/float64(fh) + float64(k2*n2)/float64(fw))
						s += complex(float64(in[n1*w+n2]), 0) * cmplx.Rect(1, ang)
					}
				}
				t.C[(b*fh+k1)*cols+k2] = complex64(s)
			}
		}
	}
	return t, nil
}

// opStablehloCase is schema.fbs's STABLEHLO_CASE (#1775), which
// TFLiteConverter writes for a stablehlo.case of a SavedModel's
// XlaCallModule (a jax2tf model).
const opStablehloCase = 209

func init() { opNames[opStablehloCase] = "STABLEHLO_CASE" }

// stablehloCase runs STABLEHLO_CASE (StablehloCaseOptions:
// branch_subgraph_indices) as the StableHLO specification defines case:
// input 0 is the index, a scalar; the branch it names (the last one when
// it is negative or past the last, "if index < 0 or index >= N, index =
// N-1") is called with the operator's other inputs, and its outputs are
// the operator's.
func (m *Model) stablehloCase(o op, a []*Tensor, depth int, st *runState) ([]*Tensor, error) {
	var branches []int
	if o.opts != nil {
		branches = ints(o.opts, 4)
	}
	if len(branches) == 0 {
		return nil, fmt.Errorf("operator STABLEHLO_CASE has no branches")
	}
	if len(a) == 0 || a[0] == nil || a[0].Len() != 1 {
		return nil, fmt.Errorf("operator STABLEHLO_CASE takes a scalar index")
	}
	i := int64(a[0].f(0))
	if a[0].I != nil {
		i = a[0].I[0]
	}
	if i < 0 || i >= int64(len(branches)) {
		i = int64(len(branches) - 1)
	}
	k := branches[i]
	if k < 0 || k >= len(m.graphs) {
		return nil, fmt.Errorf("operator STABLEHLO_CASE names subgraph %d, which the model does not have", k)
	}
	return m.call(k, a[1:], depth, st)
}
