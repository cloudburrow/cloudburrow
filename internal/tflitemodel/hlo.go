package tflitemodel

import (
	"fmt"
	"slices"

	flatbuffers "github.com/google/flatbuffers/go"
)

// The StableHLO operators of LiteRT's kernels that take dimension numbers
// or subgraphs (#1775), as the StableHLO specification defines them and
// kernels/stablehlo_*.cc compute them: STABLEHLO_GATHER,
// STABLEHLO_SCATTER and STABLEHLO_REDUCE_WINDOW (their update or body
// subgraph one of LiteRT's supported reductions, or, for SCATTER, the
// replacement).
const (
	opStablehloScatter      = 190
	opStablehloReduceWindow = 198
	opStablehloGather       = 201
)

func init() {
	for code, name := range map[int32]string{opStablehloScatter: "STABLEHLO_SCATTER", opStablehloReduceWindow: "STABLEHLO_REDUCE_WINDOW",
		opStablehloGather: "STABLEHLO_GATHER"} {
		opNames[code] = name
	}
}

func (o op) i64(slot int, d int64) int64 {
	if o.opts == nil {
		return d
	}
	return o.opts.GetInt64Slot(flatbuffers.VOffsetT(slot), d)
}

// unravel is flat index i of shape as coordinates.
func unravel(i int, shape []int) []int {
	c := make([]int, len(shape))
	for d := len(shape) - 1; d >= 0; d-- {
		c[d] = i % shape[d]
		i /= shape[d]
	}
	return c
}

func ravel(c, shape []int) int {
	i := 0
	for d := range shape {
		i = i*shape[d] + c[d]
	}
	return i
}

// startIndex is the start index at batch coordinates bi of start_indices,
// whose index vector lies along ivd (or is each one value).
func startIndex(idx []int, ishape []int, ivd int, bi []int) []int {
	if ivd >= len(ishape) {
		return []int{idx[ravel(bi, ishape)]}
	}
	c := make([]int, 0, len(ishape))
	c = append(c, bi[:ivd]...)
	c = append(c, 0)
	c = append(c, bi[ivd:]...)
	out := make([]int, ishape[ivd])
	for k := range out {
		c[ivd] = k
		out[k] = idx[ravel(c, ishape)]
	}
	return out
}

func stablehloGather(o op, x, ind *Tensor) (*Tensor, error) {
	offDims, collapsed, startMap := longs(o.opts, 4), longs(o.opts, 6), longs(o.opts, 8)
	ivd, sizes := int(o.i64(10, 0)), longs(o.opts, 12)
	rank := len(x.Shape)
	if len(sizes) != rank || len(startMap) == 0 {
		return nil, fmt.Errorf("operator %s: slice_sizes %v for an operand of rank %d", o.name, sizes, rank)
	}
	ishape := ind.Shape
	var bshape []int
	for d, n := range ishape {
		if d != ivd {
			bshape = append(bshape, n)
		}
	}
	var oshape []int
	for d, n := range sizes {
		if !slices.Contains(collapsed, d) {
			oshape = append(oshape, n)
		}
	}
	if len(oshape) != len(offDims) {
		return nil, fmt.Errorf("operator %s: %d offset dims for %d uncollapsed slice dims", o.name, len(offDims), len(oshape))
	}
	rrank := len(bshape) + len(oshape)
	shape := make([]int, rrank)
	var batchDims []int
	bi, oi := 0, 0
	for d := range rrank {
		if slices.Contains(offDims, d) {
			shape[d] = oshape[oi]
			oi++
		} else {
			shape[d] = bshape[bi]
			batchDims = append(batchDims, d)
			bi++
		}
	}
	if bi != len(bshape) {
		return nil, fmt.Errorf("operator %s: offset_dims %v do not fit a result of rank %d", o.name, offDims, rrank)
	}
	idx := axes(ind)
	r := alloc(x, shape)
	for i := range r.Len() {
		ri := unravel(i, shape)
		b := make([]int, len(batchDims))
		for k, d := range batchDims {
			b[k] = ri[d]
		}
		start := startIndex(idx, ishape, ivd, b)
		full := make([]int, rank)
		for k, d := range startMap {
			if k < len(start) && d < rank {
				full[d] = min(max(start[k], 0), x.Shape[d]-sizes[d])
			}
		}
		oi := 0
		for d := range rank {
			if slices.Contains(collapsed, d) {
				continue
			}
			full[d] += ri[offDims[oi]]
			oi++
		}
		at(r, i, x, ravel(full, x.Shape))
	}
	return r, nil
}

// reducer is the binary function a body or update subgraph computes, when
// it is one LiteRT runs: a single ADD, MULTIPLY, MAXIMUM, MINIMUM or AND
// of its two inputs, or (replace) its second input.
func (m *Model) reducer(o op, k int) (func(a, b float64) float64, error) {
	if k < 0 || k >= len(m.graphs) {
		return nil, fmt.Errorf("operator %s names subgraph %d, which the model does not have", o.name, k)
	}
	g := m.graphs[k]
	if len(g.in) != 2 || len(g.out) != 1 {
		return nil, &UnsupportedError{"operator " + o.name + " with a computation of other than two inputs and one output is not supported"}
	}
	if len(g.ops) == 0 && g.out[0] == g.in[1] {
		return func(_, b float64) float64 { return b }, nil
	}
	if len(g.ops) == 1 {
		c := g.ops[0]
		if len(c.in) == 2 && len(c.out) == 1 && c.out[0] == g.out[0] && (c.in[0] == g.in[0] && c.in[1] == g.in[1] || c.in[0] == g.in[1] && c.in[1] == g.in[0]) {
			switch c.code {
			case opStablehloAdd, opAdd:
				return func(a, b float64) float64 { return a + b }, nil
			case opStablehloMultiply, opMul:
				return func(a, b float64) float64 { return a * b }, nil
			case opStablehloMaximum, opMaximum:
				return func(a, b float64) float64 { return max(a, b) }, nil
			case opStablehloMinimum, opMinimum:
				return func(a, b float64) float64 { return min(a, b) }, nil
			case opStablehloAnd, opLogicalAnd:
				return func(a, b float64) float64 {
					if a != 0 && b != 0 {
						return 1
					}
					return 0
				}, nil
			}
		}
	}
	return nil, &UnsupportedError{"operator " + o.name + " with this computation subgraph is not supported"}
}

// set writes v, in t's type, at element i.
func set(t *Tensor, i int, v float64) {
	switch {
	case t.F != nil:
		t.F[i] = float32(v)
	case t.I != nil:
		t.I[i] = wrap(int64(v), t.Type)
	case t.B != nil:
		t.B[i] = v != 0
	}
}

func get(t *Tensor, i int) float64 {
	if t.I != nil {
		return float64(t.I[i])
	}
	return float64(t.f(i))
}

func (m *Model) stablehloReduceWindow(o op, a []*Tensor) (*Tensor, error) {
	if len(a) != 2 || a[0] == nil || a[1] == nil || a[1].Len() != 1 {
		return nil, &UnsupportedError{"operator " + o.name + " of other than one input and its init value is not supported"}
	}
	x, init := a[0], a[1]
	if x.S != nil {
		return nil, &UnsupportedError{"operator " + o.name + " on STRING tensors is not supported"}
	}
	rank := len(x.Shape)
	fill := func(v []int, d int) []int {
		if v == nil {
			v = make([]int, rank)
			for i := range v {
				v[i] = d
			}
		}
		return v
	}
	win := longs(o.opts, 4)
	strides, bdil, wdil := fill(longs(o.opts, 6), 1), fill(longs(o.opts, 8), 1), fill(longs(o.opts, 10), 1)
	pad := longs(o.opts, 12)
	if pad == nil {
		pad = make([]int, 2*rank)
	}
	if len(win) != rank || len(strides) != rank || len(bdil) != rank || len(wdil) != rank || len(pad) != 2*rank {
		return nil, fmt.Errorf("operator %s: window options for rank %d", o.name, rank)
	}
	fn, err := m.reducer(o, int(o.i32(14, -1)))
	if err != nil {
		return nil, err
	}
	shape := make([]int, rank)
	padded := make([]int, rank)
	for d := range rank {
		if strides[d] < 1 || bdil[d] < 1 || wdil[d] < 1 || win[d] < 1 {
			return nil, fmt.Errorf("operator %s: non-positive window option", o.name)
		}
		padded[d] = pad[2*d] + pad[2*d+1]
		if x.Shape[d] > 0 {
			padded[d] += (x.Shape[d]-1)*bdil[d] + 1
		}
		ew := (win[d]-1)*wdil[d] + 1
		if padded[d] < ew {
			shape[d] = 0
		} else {
			shape[d] = (padded[d]-ew)/strides[d] + 1
		}
	}
	r := alloc(x, shape)
	iv := get(init, 0)
	wn := prod(win)
	for i := range r.Len() {
		ri := unravel(i, shape)
		acc := iv
		for w := range wn {
			wi := unravel(w, win)
			src, ok := 0, true
			for d := range rank {
				p := ri[d]*strides[d] + wi[d]*wdil[d] - pad[2*d]
				if p < 0 || p%bdil[d] != 0 || p/bdil[d] >= x.Shape[d] {
					ok = false
					break
				}
				src = src*x.Shape[d] + p/bdil[d]
			}
			v := iv
			if ok {
				v = get(x, src)
			}
			acc = fn(acc, v)
		}
		set(r, i, acc)
	}
	return r, nil
}

func (m *Model) stablehloScatter(o op, a []*Tensor) (*Tensor, error) {
	if len(a) != 3 || a[0] == nil || a[1] == nil || a[2] == nil {
		return nil, &UnsupportedError{"operator " + o.name + " of other than one operand is not supported"}
	}
	x, ind, upd := a[0], a[1], a[2]
	if x.S != nil {
		return nil, &UnsupportedError{"operator " + o.name + " on STRING tensors is not supported"}
	}
	winDims, inserted, toOperand := longs(o.opts, 6), longs(o.opts, 8), longs(o.opts, 10)
	ivd := int(o.i64(12, 0))
	fn, err := m.reducer(o, int(o.i32(16, -1)))
	if err != nil {
		return nil, err
	}
	rank := len(x.Shape)
	if len(winDims)+len(inserted) != rank {
		return nil, fmt.Errorf("operator %s: %d window and %d inserted dims for rank %d", o.name, len(winDims), len(inserted), rank)
	}
	r := alloc(x, x.Shape)
	for i := range x.Len() {
		at(r, i, x, i)
	}
	idx := axes(ind)
	var scatterDims []int
	for d := range upd.Shape {
		if !slices.Contains(winDims, d) {
			scatterDims = append(scatterDims, d)
		}
	}
	for i := range upd.Len() {
		ui := unravel(i, upd.Shape)
		b := make([]int, len(scatterDims))
		for k, d := range scatterDims {
			b[k] = ui[d]
		}
		start := startIndex(idx, ind.Shape, ivd, b)
		full := make([]int, rank)
		for k, d := range toOperand {
			if k < len(start) && d < rank {
				full[d] = start[k]
			}
		}
		wi := 0
		ok := true
		for d := range rank {
			if !slices.Contains(inserted, d) {
				full[d] += ui[winDims[wi]]
				wi++
			}
			if full[d] < 0 || full[d] >= x.Shape[d] {
				ok = false
			}
		}
		if !ok {
			continue // StableHLO skips an update outside the operand
		}
		j := ravel(full, x.Shape)
		set(r, j, fn(get(r, j), get(upd, i)))
	}
	return r, nil
}
