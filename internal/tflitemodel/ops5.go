package tflitemodel

import (
	"fmt"
	"math"
	"slices"

	flatbuffers "github.com/google/flatbuffers/go"
)

// More of LiteRT's builtin operators (#1775), each as its reference kernel
// computes it: WHERE, UNIQUE, DYNAMIC_UPDATE_SLICE, MATRIX_SET_DIAG,
// REVERSE_SEQUENCE, SPARSE_TO_DENSE, BROADCAST_ARGS, L2_POOL_2D (conv.go),
// BITWISE_XOR, RIGHT_SHIFT, FAKE_QUANT, SVDF, HASHTABLE_LOOKUP, DILATE, and
// the StableHLO operators LiteRT has kernels for that take no subgraph or
// window: STABLEHLO_ADD, _MULTIPLY, _MAXIMUM, _MINIMUM, _AND, _SHIFT_LEFT
// and _PAD, and STABLEHLO_COMPOSITE, which runs its decomposition subgraph.
const (
	opHashtableLookup    = 10
	opL2Pool2D           = 12
	opSVDF               = 27
	opSparseToDense      = 68
	opFakeQuant          = 80
	opUnique             = 103
	opWhere              = 109
	opReverseSequence    = 112
	opMatrixSetDiag      = 115
	opBroadcastArgs      = 145
	opDynamicUpdateSlice = 151
	opBitwiseXor         = 160
	opRightShift         = 161
	opStablehloAdd       = 163
	opStablehloMultiply  = 165
	opStablehloMaximum   = 166
	opStablehloAnd       = 176
	opStablehloMinimum   = 181
	opStablehloPad       = 195
	opDilate             = 203
	opStablehloComposite = 206
	opStablehloShiftLeft = 207
)

func init() {
	for code, name := range map[int32]string{opHashtableLookup: "HASHTABLE_LOOKUP", opL2Pool2D: "L2_POOL_2D", opSVDF: "SVDF",
		opSparseToDense: "SPARSE_TO_DENSE", opFakeQuant: "FAKE_QUANT", opUnique: "UNIQUE", opWhere: "WHERE",
		opReverseSequence: "REVERSE_SEQUENCE", opMatrixSetDiag: "MATRIX_SET_DIAG", opBroadcastArgs: "BROADCAST_ARGS",
		opDynamicUpdateSlice: "DYNAMIC_UPDATE_SLICE", opBitwiseXor: "BITWISE_XOR", opRightShift: "RIGHT_SHIFT",
		opStablehloAdd: "STABLEHLO_ADD", opStablehloMultiply: "STABLEHLO_MULTIPLY", opStablehloMaximum: "STABLEHLO_MAXIMUM",
		opStablehloAnd: "STABLEHLO_AND", opStablehloMinimum: "STABLEHLO_MINIMUM", opStablehloPad: "STABLEHLO_PAD", opDilate: "DILATE",
		opStablehloComposite: "STABLEHLO_COMPOSITE", opStablehloShiftLeft: "STABLEHLO_SHIFT_LEFT"} {
		opNames[code] = name
	}
}

// longs is a vector of int64 at a field.
func longs(t *flatbuffers.Table, slot int) []int {
	if t == nil {
		return nil
	}
	p := field(t, slot)
	if p == 0 {
		return nil
	}
	n := t.VectorLen(p)
	start := t.Vector(p)
	out := make([]int, n)
	for i := range n {
		out[i] = int(t.GetInt64(start + flatbuffers.UOffsetT(8*i)))
	}
	return out
}

// eval5 runs the operators of this file that have one output.
func (m *Model) eval5(o op, a []*Tensor) (*Tensor, error) {
	arg := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	need := func(n int) error {
		for i := range n {
			if arg(i) == nil {
				return fmt.Errorf("operator %s takes %d inputs", o.name, n)
			}
		}
		return nil
	}
	x := arg(0)
	switch o.code {
	case opWhere:
		return where(x)
	case opDynamicUpdateSlice:
		if err := need(3); err != nil {
			return nil, err
		}
		return dynamicUpdateSlice(o, x, arg(1), axes(arg(2)))
	case opMatrixSetDiag:
		if err := need(2); err != nil {
			return nil, err
		}
		return matrixSetDiag(o, x, arg(1))
	case opReverseSequence:
		if err := need(2); err != nil {
			return nil, err
		}
		return reverseSequence(o, x, axes(arg(1)))
	case opSparseToDense:
		if err := need(4); err != nil {
			return nil, err
		}
		return sparseToDense(o, x, axes(arg(1)), arg(2), arg(3))
	case opBroadcastArgs:
		if err := need(2); err != nil {
			return nil, err
		}
		s, err := broadcastShape(axes(x), axes(arg(1)))
		if err != nil {
			return nil, fmt.Errorf("operator BROADCAST_ARGS: %w", err)
		}
		r := &Tensor{Type: x.Type, Shape: []int{len(s)}, I: make([]int64, len(s))}
		for i, d := range s {
			r.I[i] = int64(d)
		}
		return r, nil
	case opL2Pool2D:
		return pool2D(o, x)
	case opBitwiseXor, opRightShift, opStablehloAnd, opStablehloShiftLeft:
		if err := need(2); err != nil {
			return nil, err
		}
		return bitwise(o, x, arg(1))
	case opStablehloAdd, opStablehloMultiply, opStablehloMaximum, opStablehloMinimum:
		if err := need(2); err != nil {
			return nil, err
		}
		if !slices.Equal(x.Shape, arg(1).Shape) {
			return nil, fmt.Errorf("operator %s takes two tensors of one shape, not %v and %v", o.name, x.Shape, arg(1).Shape)
		}
		code := map[int32]int32{opStablehloAdd: opAdd, opStablehloMultiply: opMul, opStablehloMaximum: opMaximum, opStablehloMinimum: opMinimum}[o.code]
		return elementwise(op{code: code, name: o.name}, x, arg(1), 0)
	case opStablehloPad:
		if err := need(2); err != nil {
			return nil, err
		}
		return stablehloPad(o, x, arg(1))
	case opDilate:
		if err := need(3); err != nil {
			return nil, err
		}
		return dilate(o, x, axes(arg(1)), arg(2))
	case opFakeQuant:
		return fakeQuant(o, x)
	case opSVDF:
		if err := need(3); err != nil {
			return nil, err
		}
		return svdf(o, x, arg(1), arg(2), arg(3), arg(4))
	case opBitcast: // ops6.go
		return bitcast(x, o.outType)
	case opEmbeddingLookupSparse:
		return embeddingLookupSparse(o, a)
	case opAudioSpectrogram: // audio.go
		return audioSpectrogram(o, x)
	case opMfcc:
		return mfcc(o, x, arg(1))
	case opStablehloGather: // hlo.go
		return stablehloGather(o, x, arg(1))
	case opStablehloScatter:
		return m.stablehloScatter(o, a)
	case opStablehloReduceWindow:
		return m.stablehloReduceWindow(o, a)
	}
	return nil, &UnsupportedError{"operator " + o.name + " is not supported"}
}

// where is WHERE: the coordinates of the true (non-zero) elements, row-major,
// as INT64 [count, rank].
func where(x *Tensor) (*Tensor, error) {
	rank := len(x.Shape)
	st := strides(x.Shape)
	r := &Tensor{Type: Int64}
	n := 0
	for i := range x.Len() {
		if x.f(i) == 0 {
			continue
		}
		n++
		for d := range rank {
			r.I = append(r.I, int64(i/st[d]%x.Shape[d]))
		}
	}
	r.Shape = []int{n, rank}
	if r.I == nil {
		r.I = []int64{}
	}
	return r, nil
}

// unique is UNIQUE: a 1-D tensor's distinct values in order of first
// appearance, and each element's index among them (UniqueOptions'
// idx_out_type, INT32 by default).
func unique(o op, x *Tensor, n int) ([]*Tensor, error) {
	if len(x.Shape) != 1 || n != 2 {
		return nil, fmt.Errorf("operator UNIQUE takes a 1-D tensor and gives 2 outputs")
	}
	idxType := Type(o.i8(4, int8(Int32)))
	idx := &Tensor{Type: idxType, Shape: x.Shape, I: make([]int64, x.Len())}
	seen := map[any]int{}
	var order []int
	for i := range x.Len() {
		var k any
		switch {
		case x.F != nil:
			k = x.F[i]
		case x.I != nil:
			k = x.I[i]
		case x.S != nil:
			k = x.S[i]
		case x.B != nil:
			k = x.B[i]
		}
		j, ok := seen[k]
		if !ok {
			j = len(order)
			seen[k] = j
			order = append(order, i)
		}
		idx.I[i] = int64(j)
	}
	y := alloc(x, []int{len(order)})
	for j, i := range order {
		at(y, j, x, i)
	}
	return []*Tensor{y, idx}, nil
}

// dynamicUpdateSlice is DYNAMIC_UPDATE_SLICE: operand with update written
// at start, each start clamped so the update fits.
func dynamicUpdateSlice(o op, x, u *Tensor, start []int) (*Tensor, error) {
	if len(u.Shape) != len(x.Shape) || len(start) != len(x.Shape) {
		return nil, fmt.Errorf("operator %s: operand %v, update %v and %d start indices", o.name, x.Shape, u.Shape, len(start))
	}
	for d := range x.Shape {
		if u.Shape[d] > x.Shape[d] {
			return nil, fmt.Errorf("operator %s: update %v is larger than operand %v", o.name, u.Shape, x.Shape)
		}
		start[d] = min(max(start[d], 0), x.Shape[d]-u.Shape[d])
	}
	r := alloc(x, x.Shape)
	for i := range x.Len() {
		at(r, i, x, i)
	}
	xs, us := strides(x.Shape), strides(u.Shape)
	c := convert(*u, x.Type)
	for i := range u.Len() {
		j := 0
		for d := range u.Shape {
			j += (i/us[d]%u.Shape[d] + start[d]) * xs[d]
		}
		at(r, j, &c, i)
	}
	return r, nil
}

// matrixSetDiag is MATRIX_SET_DIAG: the innermost matrices' main diagonals
// replaced.
func matrixSetDiag(o op, x, d *Tensor) (*Tensor, error) {
	r := len(x.Shape)
	if r < 2 {
		return nil, fmt.Errorf("operator %s takes a tensor of rank 2 or more", o.name)
	}
	rows, cols := x.Shape[r-2], x.Shape[r-1]
	k := min(rows, cols)
	batch := prod(x.Shape[:r-2])
	if d.Len() != batch*k {
		return nil, fmt.Errorf("operator %s: diagonal of shape %v for input %v", o.name, d.Shape, x.Shape)
	}
	out := alloc(x, x.Shape)
	c := convert(*d, x.Type)
	for b := range batch {
		for i := range rows {
			for j := range cols {
				p := (b*rows+i)*cols + j
				if i == j {
					at(out, p, &c, b*k+i)
				} else {
					at(out, p, x, p)
				}
			}
		}
	}
	return out, nil
}

// reverseSequence is REVERSE_SEQUENCE: along seq_dim, the first
// seq_lengths[b] elements of each slice b of batch_dim reversed.
func reverseSequence(o op, x *Tensor, lens []int) (*Tensor, error) {
	rank := len(x.Shape)
	sd, bd := int(o.i32(4, 0)), int(o.i32(6, 0))
	if sd < 0 {
		sd += rank
	}
	if bd < 0 {
		bd += rank
	}
	if sd < 0 || sd >= rank || bd < 0 || bd >= rank || sd == bd || len(lens) != x.Shape[bd] {
		return nil, fmt.Errorf("operator %s: seq_dim %d, batch_dim %d and %d lengths for shape %v", o.name, sd, bd, len(lens), x.Shape)
	}
	st := strides(x.Shape)
	r := alloc(x, x.Shape)
	for i := range x.Len() {
		s, b := i/st[sd]%x.Shape[sd], i/st[bd]%x.Shape[bd]
		n := lens[b]
		if n < 0 || n > x.Shape[sd] {
			return nil, fmt.Errorf("operator %s: sequence length %d for a dimension of %d", o.name, n, x.Shape[sd])
		}
		j := i
		if s < n {
			j = i + (n-1-2*s)*st[sd]
		}
		at(r, j, x, i)
	}
	return r, nil
}

// sparseToDense is SPARSE_TO_DENSE: default_value everywhere but at the
// indices (a scalar, [N] or [N, rank]), which take values (one, or one
// each); a later index wins.
func sparseToDense(o op, idx *Tensor, shape []int, vals, def *Tensor) (*Tensor, error) {
	if def.Len() != 1 {
		return nil, fmt.Errorf("operator %s: default_value must hold one value", o.name)
	}
	rank := len(shape)
	var n, w int
	switch len(idx.Shape) {
	case 0:
		n, w = 1, 1
	case 1:
		n, w = idx.Shape[0], 1
	case 2:
		n, w = idx.Shape[0], idx.Shape[1]
	default:
		return nil, fmt.Errorf("operator %s: indices of rank %d", o.name, len(idx.Shape))
	}
	if w != rank {
		return nil, fmt.Errorf("operator %s: indices of %d coordinates for an output of rank %d", o.name, w, rank)
	}
	if vals.Len() != 1 && vals.Len() != n {
		return nil, fmt.Errorf("operator %s: %d values for %d indices", o.name, vals.Len(), n)
	}
	r := alloc(def, shape)
	r.Type = def.Type
	for i := range r.Len() {
		at(r, i, def, 0)
	}
	cv := convert(*vals, def.Type)
	ix := axes(idx)
	st := strides(shape)
	for k := range n {
		j := 0
		for d := range rank {
			c := ix[k*w+d]
			if c < 0 || c >= shape[d] {
				return nil, fmt.Errorf("operator %s: index %d is outside dimension %d of %d", o.name, c, d, shape[d])
			}
			j += c * st[d]
		}
		v := 0
		if vals.Len() > 1 {
			v = k
		}
		at(r, j, &cv, v)
	}
	return r, nil
}

// bitwise is BITWISE_XOR, RIGHT_SHIFT (arithmetic for signed types, the
// shift clamped to [0, the type's width less one]), STABLEHLO_AND and
// STABLEHLO_SHIFT_LEFT, broadcast.
func bitwise(o op, x, y *Tensor) (*Tensor, error) {
	if x.I == nil || y.I == nil {
		if o.code == opStablehloAnd && x.B != nil && y.B != nil {
			return compare(op{code: opLogicalAnd, name: o.name}, x, y)
		}
		return nil, &UnsupportedError{fmt.Sprintf("operator %s on %s tensors is not supported", o.name, x.Type)}
	}
	shape, err := broadcastShape(x.Shape, y.Shape)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	width := map[Type]int{Int8: 8, Uint8: 8, Int16: 16, Uint16: 16, Int32: 32, Uint32: 32, Int64: 64, Uint64: 64}[x.Type]
	signed := x.Type == Int8 || x.Type == Int16 || x.Type == Int32 || x.Type == Int64
	n := prod(shape)
	r := &Tensor{Type: x.Type, Shape: shape, I: make([]int64, n)}
	for i := range n {
		p, q := x.I[broadcastIndex(i, shape, x.Shape)], y.I[broadcastIndex(i, shape, y.Shape)]
		var v int64
		switch o.code {
		case opBitwiseXor:
			v = p ^ q
		case opStablehloAnd:
			v = p & q
		case opRightShift:
			s := min(max(q, 0), int64(width-1))
			if signed {
				v = p >> s
			} else {
				v = int64(uint64(p) >> s)
			}
		case opStablehloShiftLeft:
			if q < 0 || q >= int64(width) {
				v = 0
			} else {
				v = p << q
			}
		}
		r.I[i] = wrap(v, x.Type)
	}
	return r, nil
}

// wrap is v in t's range, as the C++ type holds it.
func wrap(v int64, t Type) int64 {
	switch t {
	case Int8:
		return int64(int8(v))
	case Uint8:
		return int64(uint8(v))
	case Int16:
		return int64(int16(v))
	case Uint16:
		return int64(uint16(v))
	case Int32:
		return int64(int32(v))
	case Uint32:
		return int64(uint32(v))
	}
	return v
}

// stablehloPad is STABLEHLO_PAD: edge_padding_low and _high (negative
// crops) and interior_padding, filled with padding_value.
func stablehloPad(o op, x, pv *Tensor) (*Tensor, error) {
	rank := len(x.Shape)
	lo, hi, in := longs(o.opts, 4), longs(o.opts, 6), longs(o.opts, 8)
	if len(lo) != rank || len(hi) != rank || len(in) != rank || pv.Len() != 1 {
		return nil, fmt.Errorf("operator %s: padding for rank %d", o.name, rank)
	}
	shape := make([]int, rank)
	for d := range rank {
		if in[d] < 0 {
			return nil, fmt.Errorf("operator %s: negative interior padding", o.name)
		}
		shape[d] = lo[d] + hi[d] + x.Shape[d] + max(x.Shape[d]-1, 0)*in[d]
		if shape[d] < 0 {
			return nil, fmt.Errorf("operator %s: padding crops dimension %d below zero", o.name, d)
		}
	}
	r := alloc(x, shape)
	c := convert(*pv, x.Type)
	for i := range r.Len() {
		at(r, i, &c, 0)
	}
	xs, rs := strides(x.Shape), strides(shape)
	for i := range x.Len() {
		j, ok := 0, true
		for d := range rank {
			p := lo[d] + (i/xs[d]%x.Shape[d])*(in[d]+1)
			if p < 0 || p >= shape[d] {
				ok = false
				break
			}
			j += p * rs[d]
		}
		if ok {
			at(r, j, x, i)
		}
	}
	return r, nil
}

// dilate is DILATE: dilations[d]-1 copies of padding_value between
// neighbouring elements along each axis.
func dilate(o op, x *Tensor, dil []int, pv *Tensor) (*Tensor, error) {
	rank := len(x.Shape)
	if len(dil) != rank || pv.Len() != 1 {
		return nil, fmt.Errorf("operator %s: %d dilations for rank %d", o.name, len(dil), rank)
	}
	shape := make([]int, rank)
	for d := range rank {
		if dil[d] < 1 {
			return nil, fmt.Errorf("operator %s: dilation %d", o.name, dil[d])
		}
		shape[d] = 0
		if x.Shape[d] > 0 {
			shape[d] = (x.Shape[d]-1)*dil[d] + 1
		}
	}
	r := alloc(x, shape)
	c := convert(*pv, x.Type)
	for i := range r.Len() {
		at(r, i, &c, 0)
	}
	xs, rs := strides(x.Shape), strides(shape)
	for i := range x.Len() {
		j := 0
		for d := range rank {
			j += (i / xs[d] % x.Shape[d]) * dil[d] * rs[d]
		}
		at(r, j, x, i)
	}
	return r, nil
}

// fakeQuant is FAKE_QUANT (FakeQuantOptions min, max, num_bits):
// Nudge and FakeQuantizeArray. LiteRT refuses narrow_range at runtime, and
// so does this.
func fakeQuant(o op, x *Tensor) (*Tensor, error) {
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator FAKE_QUANT on " + x.Type.String() + " tensors is not supported"}
	}
	lo, hi, bitsN := o.f32(4, 0), o.f32(6, 0), int(o.i32(8, 0))
	if bitsN < 2 || bitsN > 16 {
		return nil, fmt.Errorf("operator FAKE_QUANT: num_bits %d", bitsN)
	}
	if o.bool(10) {
		return nil, fmt.Errorf("narrow_range FakeQuant is not currently supported at runtime. narrow_range is only meant to be applied to weights, not activations")
	}
	qmin, qmax := float32(0), float32(int(1)<<bitsN-1)
	scale := (hi - lo) / (qmax - qmin)
	zpf := qmin - lo/scale
	var zp float32
	switch {
	case zpf < qmin:
		zp = qmin
	case zpf > qmax:
		zp = qmax
	default:
		zp = float32(uint16(math.Round(float64(zpf))))
	}
	nmin, nmax := (qmin-zp)*scale, (qmax-zp)*scale
	inv := 1 / scale
	r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
	for i, v := range x.F {
		c := min(nmax, max(nmin, v)) - nmin
		r.F[i] = float32(math.Round(float64(c*inv)))*scale + nmin
	}
	return r, nil
}

// svdf is SVDF's float kernel on its initial (zero) state: input [batch,
// input_size], weights_feature [num_filters, input_size], weights_time
// [num_filters, memory_size], bias [num_units], state; SVDFOptions rank and
// fused_activation_function.
func svdf(o op, x, wf, wt, bias, state *Tensor) (*Tensor, error) {
	if !x.Type.IsFloat() || !wf.Type.IsFloat() || !wt.Type.IsFloat() || len(x.Shape) != 2 || len(wf.Shape) != 2 || len(wt.Shape) != 2 {
		return nil, &UnsupportedError{"SVDF with these operands is not supported"}
	}
	rank := int(o.i32(4, 0))
	batch, inSize := x.Shape[0], x.Shape[1]
	filters, mem := wf.Shape[0], wt.Shape[1]
	if rank <= 0 || filters%rank != 0 || wf.Shape[1] != inSize || wt.Shape[0] != filters {
		return nil, fmt.Errorf("operator SVDF: input %v, weights %v and %v, rank %d", x.Shape, wf.Shape, wt.Shape, rank)
	}
	units := filters / rank
	act := o.i8(6, 0)
	s := make([]float32, batch*filters*mem)
	if state != nil && state.F != nil && len(state.F) == len(s) {
		copy(s, state.F)
	}
	copy(s, s[1:]) // shift the state left by one
	for b := range batch {
		in := x.F[b*inSize : (b+1)*inSize]
		for f := range filters {
			var dot float32
			for k, v := range in {
				dot += wf.F[f*inSize+k] * v
			}
			s[(b*filters+f)*mem+mem-1] = dot
		}
	}
	r := &Tensor{Type: Float32, Shape: []int{batch, units}, F: make([]float32, batch*units)}
	for b := range batch {
		scratch := make([]float32, filters)
		for f := range filters {
			for j := range mem {
				scratch[f] += wt.F[f*mem+j] * s[(b*filters+f)*mem+j]
			}
		}
		for u := range units {
			if bias != nil {
				r.F[b*units+u] = bias.F[u]
			}
			var sum float32
			for k := range rank {
				sum += scratch[u*rank+k]
			}
			r.F[b*units+u] = activation(act, r.F[b*units+u]+sum)
		}
	}
	return r, nil
}

// hashtableLookup is HASHTABLE_LOOKUP: each lookup's row of values by
// its key (keys sorted), zeros and a hit of 0 for a missing one.
func hashtableLookup(o op, a []*Tensor, n int) ([]*Tensor, error) {
	if len(a) != 3 || n != 2 || a[0] == nil || a[1] == nil || a[2] == nil {
		return nil, fmt.Errorf("operator %s takes 3 inputs and gives 2 outputs", o.name)
	}
	look, keys, vals := a[0], a[1], a[2]
	if len(vals.Shape) < 1 || vals.Shape[0] != keys.Len() {
		return nil, fmt.Errorf("operator %s: %d keys for values of shape %v", o.name, keys.Len(), vals.Shape)
	}
	row := vals.Len() / max(vals.Shape[0], 1)
	shape := append([]int{look.Len()}, vals.Shape[1:]...)
	out := alloc(vals, shape)
	if out.S != nil {
		return nil, &UnsupportedError{"HASHTABLE_LOOKUP of STRING values is not supported"}
	}
	hits := &Tensor{Type: Uint8, Shape: []int{look.Len()}, I: make([]int64, look.Len())}
	ks := axes(keys)
	for i, l := range axes(look) {
		j, ok := slices.BinarySearch(ks, l)
		if !ok {
			continue
		}
		hits.I[i] = 1
		for k := range row {
			at(out, i*row+k, vals, j*row+k)
		}
	}
	return []*Tensor{out, hits}, nil
}
