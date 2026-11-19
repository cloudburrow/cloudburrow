package tflitemodel

import (
	"fmt"
	"math"
)

// More of LiteRT's builtin operators (#1775): rounding and trigonometric
// functions, ELU and HARD_SWISH, LOG_SOFTMAX, the max, min and product
// reductions, ARG_MAX and ARG_MIN, SLICE, TILE, PAD and PADV2, FLOOR_DIV
// and FLOOR_MOD, ADD_N, SHAPE, FILL and ZEROS_LIKE, and the operators of
// several outputs SPLIT, SPLIT_V and UNPACK, each as LiteRT's reference
// kernel computes it.
const (
	opFloor      = 8
	opPad        = 34
	opSplit      = 49
	opLogSoftmax = 50
	opArgMax     = 56
	opPadV2      = 60
	opSlice      = 65
	opSin        = 66
	opTile       = 69
	opShape      = 77
	opArgMin     = 79
	opReduceProd = 81
	opReduceMax  = 82
	opUnpack     = 88
	opReduceMin  = 89
	opFloorDiv   = 90
	opZerosLike  = 93
	opFill       = 94
	opFloorMod   = 95
	opSplitV     = 102
	opCeil       = 104
	opAddN       = 106
	opCos        = 108
	opElu        = 111
	opRound      = 116
	opHardSwish  = 117
)

func init() {
	for code, name := range map[int32]string{opFloor: "FLOOR", opPad: "PAD", opSplit: "SPLIT", opLogSoftmax: "LOG_SOFTMAX", opArgMax: "ARG_MAX",
		opPadV2: "PADV2", opSlice: "SLICE", opSin: "SIN", opTile: "TILE", opShape: "SHAPE", opArgMin: "ARG_MIN", opReduceProd: "REDUCE_PROD",
		opReduceMax: "REDUCE_MAX", opUnpack: "UNPACK", opReduceMin: "REDUCE_MIN", opFloorDiv: "FLOOR_DIV", opZerosLike: "ZEROS_LIKE",
		opFill: "FILL", opFloorMod: "FLOOR_MOD", opSplitV: "SPLIT_V", opCeil: "CEIL", opAddN: "ADD_N", opCos: "COS", opElu: "ELU",
		opRound: "ROUND", opHardSwish: "HARD_SWISH"} {
		opNames[code] = name
	}
}

// multiOutput reports whether code is an operator of several outputs.
func multiOutput(code int32) bool { return code == opSplit || code == opSplitV || code == opUnpack }

// eval2 runs the operators of this file that have one output.
func (m *Model) eval2(o op, a []*Tensor) (*Tensor, error) {
	arg := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	x := arg(0)
	unary := func(fn func(float64) float64) (*Tensor, error) {
		if !x.Type.IsFloat() {
			return nil, &UnsupportedError{fmt.Sprintf("operator %s on %s tensors is not supported", o.name, x.Type)}
		}
		r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
		for i, v := range x.F {
			r.F[i] = float32(fn(float64(v)))
		}
		return r, nil
	}
	switch o.code {
	case opFloor:
		return unary(math.Floor)
	case opCeil:
		return unary(math.Ceil)
	case opRound:
		return unary(math.RoundToEven) // LiteRT's ROUND rounds half to even
	case opSin:
		return unary(math.Sin)
	case opCos:
		return unary(math.Cos)
	case opElu:
		return unary(func(v float64) float64 {
			if v < 0 {
				return math.Expm1(v)
			}
			return v
		})
	case opHardSwish:
		return unary(func(v float64) float64 { return v * math.Min(6, math.Max(0, v+3)) / 6 })
	case opLogSoftmax:
		return logSoftmax(o, x)
	case opReduceMax, opReduceMin, opReduceProd:
		return reduceOp(o, x, axes(arg(1)), o.bool(4))
	case opArgMax, opArgMin:
		ax := axes(arg(1))
		if len(ax) != 1 {
			return nil, fmt.Errorf("operator %s takes one axis", o.name)
		}
		return argExtreme(o, x, ax[0])
	case opSlice:
		return slice(o, x, axes(arg(1)), axes(arg(2)))
	case opTile:
		return tile(x, axes(arg(1)))
	case opPad, opPadV2:
		var c *Tensor
		if o.code == opPadV2 {
			c = arg(2)
		}
		return pad(o, x, axes(arg(1)), c)
	case opFloorDiv, opFloorMod:
		return floorOp(o, x, arg(1))
	case opAddN:
		r := alloc(x, x.Shape)
		for _, t := range a {
			if t == nil || t.Len() != x.Len() {
				return nil, fmt.Errorf("operator ADD_N takes inputs of one shape")
			}
			for i := range x.Len() {
				if r.F != nil {
					r.F[i] += t.F[i]
				} else if r.I != nil {
					r.I[i] += t.I[i]
				}
			}
		}
		return r, nil
	case opShape:
		r := &Tensor{Type: Int64, Shape: []int{len(x.Shape)}, I: make([]int64, len(x.Shape))}
		for i, d := range x.Shape {
			r.I[i] = int64(d)
		}
		return r, nil
	case opZerosLike:
		return alloc(x, x.Shape), nil
	case opFill:
		v := arg(1)
		if v == nil || v.Len() != 1 {
			return nil, fmt.Errorf("operator FILL takes a value of one element")
		}
		r := alloc(v, axes(x))
		for i := range r.Len() {
			at(r, i, v, 0)
		}
		return r, nil
	case opResizeNearestNeighbor, opResizeBilinear:
		return resizeOp(o, x, arg(1)) // resize.go, #1799
	}
	return nil, &UnsupportedError{"operator " + o.name + " is not supported"}
}

// evalMulti runs SPLIT, SPLIT_V and UNPACK.
func (m *Model) evalMulti(o op, a []*Tensor, n int) ([]*Tensor, error) {
	get := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	var x *Tensor
	var axis int
	var sizes []int
	switch o.code {
	case opSplit: // inputs: axis, value
		x = get(1)
		ax := axes(get(0))
		if x == nil || len(ax) != 1 {
			return nil, fmt.Errorf("operator SPLIT takes an axis and a value")
		}
		axis = ax[0]
	case opSplitV: // inputs: value, size_splits, axis
		x = get(0)
		ax := axes(get(2))
		if x == nil || len(ax) != 1 {
			return nil, fmt.Errorf("operator SPLIT_V takes a value, its sizes and an axis")
		}
		axis, sizes = ax[0], axes(get(1))
	case opUnpack:
		x = get(0)
		axis = int(o.i32(6, 0))
	}
	if axis < 0 {
		axis += len(x.Shape)
	}
	if axis < 0 || axis >= len(x.Shape) {
		return nil, fmt.Errorf("operator %s: axis out of range", o.name)
	}
	d := x.Shape[axis]
	if sizes == nil {
		if d%n != 0 {
			return nil, fmt.Errorf("operator %s: dimension %d does not split %d ways", o.name, d, n)
		}
		for range n {
			sizes = append(sizes, d/n)
		}
	}
	free, known := -1, 0
	for i, s := range sizes {
		if s < 0 {
			free = i
		} else {
			known += s
		}
	}
	if free >= 0 {
		sizes[free] = d - known
	}
	if len(sizes) != n {
		return nil, fmt.Errorf("operator %s gives %d values for %d outputs", o.name, len(sizes), n)
	}
	outer, inner := 1, 1
	for i := range axis {
		outer *= x.Shape[i]
	}
	for i := axis + 1; i < len(x.Shape); i++ {
		inner *= x.Shape[i]
	}
	var outs []*Tensor
	start := 0
	for _, s := range sizes {
		shape := append(append(append([]int{}, x.Shape[:axis]...), s), x.Shape[axis+1:]...)
		if o.code == opUnpack {
			shape = append(append([]int{}, x.Shape[:axis]...), x.Shape[axis+1:]...)
		}
		r := alloc(x, shape)
		j := 0
		for p := range outer {
			for k := start; k < start+s; k++ {
				for q := range inner {
					at(r, j, x, (p*d+k)*inner+q)
					j++
				}
			}
		}
		outs = append(outs, r)
		start += s
	}
	return outs, nil
}

func logSoftmax(o op, x *Tensor) (*Tensor, error) {
	if !x.Type.IsFloat() || len(x.Shape) == 0 {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	n := x.Shape[len(x.Shape)-1]
	r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
	for s := 0; s+n <= len(x.F); s += n {
		mx := math.Inf(-1)
		for _, v := range x.F[s : s+n] {
			mx = math.Max(mx, float64(v))
		}
		sum := 0.0
		for _, v := range x.F[s : s+n] {
			sum += math.Exp(float64(v) - mx)
		}
		for i, v := range x.F[s : s+n] {
			r.F[s+i] = float32(float64(v) - mx - math.Log(sum))
		}
	}
	return r, nil
}

// reduceOp is REDUCE_MAX, REDUCE_MIN and REDUCE_PROD over ax.
func reduceOp(o op, x *Tensor, ax []int, keep bool) (*Tensor, error) {
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
	nk := prod(kept)
	acc := make([]float64, nk)
	seen := make([]bool, nk)
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
		v := float64(x.f(i))
		if x.I != nil {
			v = float64(x.I[i])
		}
		switch {
		case !seen[dst]:
			acc[dst], seen[dst] = v, true
		case o.code == opReduceMax:
			acc[dst] = math.Max(acc[dst], v)
		case o.code == opReduceMin:
			acc[dst] = math.Min(acc[dst], v)
		default:
			acc[dst] *= v
		}
	}
	r := &Tensor{Type: x.Type, Shape: shape}
	if x.Type.IsFloat() {
		r.F = make([]float32, nk)
		for i, v := range acc {
			r.F[i] = float32(v)
		}
	} else if x.I != nil {
		r.I = make([]int64, nk)
		for i, v := range acc {
			r.I[i] = int64(v)
		}
	} else {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
	return r, nil
}

func argExtreme(o op, x *Tensor, axis int) (*Tensor, error) {
	if axis < 0 {
		axis += len(x.Shape)
	}
	if axis < 0 || axis >= len(x.Shape) {
		return nil, fmt.Errorf("operator %s: axis out of range", o.name)
	}
	d := x.Shape[axis]
	outer, inner := 1, 1
	for i := range axis {
		outer *= x.Shape[i]
	}
	for i := axis + 1; i < len(x.Shape); i++ {
		inner *= x.Shape[i]
	}
	shape := append(append([]int{}, x.Shape[:axis]...), x.Shape[axis+1:]...)
	r := &Tensor{Type: Int64, Shape: shape, I: make([]int64, outer*inner)}
	for p := range outer {
		for q := range inner {
			best := 0
			for k := 1; k < d; k++ {
				v, b := x.f((p*d+k)*inner+q), x.f((p*d+best)*inner+q)
				if o.code == opArgMax && v > b || o.code == opArgMin && v < b {
					best = k
				}
			}
			r.I[p*inner+q] = int64(best)
		}
	}
	return r, nil
}

func slice(o op, x *Tensor, begin, size []int) (*Tensor, error) {
	if len(begin) != len(x.Shape) || len(size) != len(x.Shape) {
		return nil, fmt.Errorf("operator %s takes a begin and size per dimension", o.name)
	}
	shape := make([]int, len(x.Shape))
	for i := range shape {
		if size[i] < 0 {
			size[i] = x.Shape[i] - begin[i]
		}
		if begin[i] < 0 || begin[i]+size[i] > x.Shape[i] {
			return nil, fmt.Errorf("operator %s: slice out of range", o.name)
		}
		shape[i] = size[i]
	}
	r := alloc(x, shape)
	xs, rs := strides(x.Shape), strides(shape)
	for j := range r.Len() {
		src, rem := 0, j
		for d := range shape {
			c := rem / rs[d]
			rem %= rs[d]
			src += (c + begin[d]) * xs[d]
		}
		at(r, j, x, src)
	}
	return r, nil
}

func tile(x *Tensor, mult []int) (*Tensor, error) {
	if len(mult) != len(x.Shape) {
		return nil, fmt.Errorf("operator TILE takes a multiple per dimension")
	}
	shape := make([]int, len(x.Shape))
	for i, d := range x.Shape {
		shape[i] = d * mult[i]
	}
	r := alloc(x, shape)
	xs, rs := strides(x.Shape), strides(shape)
	for j := range r.Len() {
		src, rem := 0, j
		for d := range shape {
			c := rem / rs[d]
			rem %= rs[d]
			src += (c % x.Shape[d]) * xs[d]
		}
		at(r, j, x, src)
	}
	return r, nil
}

func pad(o op, x *Tensor, pads []int, c *Tensor) (*Tensor, error) {
	if len(pads) != 2*len(x.Shape) {
		return nil, fmt.Errorf("operator %s takes a before and after padding per dimension", o.name)
	}
	shape := make([]int, len(x.Shape))
	for i, d := range x.Shape {
		shape[i] = pads[2*i] + d + pads[2*i+1]
	}
	r := alloc(x, shape)
	if c != nil && c.Len() == 1 {
		for j := range r.Len() {
			at(r, j, c, 0)
		}
	}
	xs, rs := strides(x.Shape), strides(shape)
	for i := range x.Len() {
		dst, rem := 0, i
		for d := range x.Shape {
			k := rem / xs[d]
			rem %= xs[d]
			dst += (k + pads[2*d]) * rs[d]
		}
		at(r, dst, x, i)
	}
	return r, nil
}

func floorOp(o op, x, y *Tensor) (*Tensor, error) {
	if y == nil {
		return nil, fmt.Errorf("operator %s takes two inputs", o.name)
	}
	shape, err := broadcastShape(x.Shape, y.Shape)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	n := prod(shape)
	r := alloc(x, shape)
	for i := range n {
		xi, yi := broadcastIndex(i, shape, x.Shape), broadcastIndex(i, shape, y.Shape)
		if r.I != nil && y.I != nil {
			p, q := x.I[xi], y.I[yi]
			if q == 0 {
				return nil, fmt.Errorf("operator %s divides by zero", o.name)
			}
			d := p / q
			if (p%q != 0) && ((p < 0) != (q < 0)) {
				d--
			}
			if o.code == opFloorDiv {
				r.I[i] = d
			} else {
				r.I[i] = p - d*q
			}
			continue
		}
		p, q := float64(x.f(xi)), float64(y.f(yi))
		if r.F == nil {
			return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
		}
		if o.code == opFloorDiv {
			r.F[i] = float32(math.Floor(p / q))
		} else {
			m := math.Mod(p, q)
			if m != 0 && (m < 0) != (q < 0) {
				m += q
			}
			r.F[i] = float32(m)
		}
	}
	return r, nil
}
