package tflitemodel

import (
	"cmp"
	"fmt"
	"math"
	"strings"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Run runs the model on inputs, one per m.Inputs in order, each with its
// free dimensions given, and returns one tensor per m.Outputs.
func (m *Model) Run(inputs []Tensor) (out []Tensor, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("the TensorFlow Lite model failed to run: %v", r)
		}
	}()
	if len(inputs) != len(m.Inputs) {
		return nil, fmt.Errorf("the model takes %d inputs, not %d", len(m.Inputs), len(inputs))
	}
	vals := make([]*Tensor, len(m.tensors))
	for i, d := range m.tensors {
		vals[i] = d.data
	}
	for k, p := range m.Inputs {
		in := inputs[k]
		if len(in.Shape) != len(p.Shape) {
			return nil, fmt.Errorf("input %s has rank %d, not %d", p.Name, len(in.Shape), len(p.Shape))
		}
		for j, d := range p.Shape {
			if d >= 0 && in.Shape[j] != d {
				return nil, fmt.Errorf("input %s has shape %v, not %v", p.Name, in.Shape, p.Shape)
			}
		}
		t := convert(in, p.Type)
		if q := m.tensors[p.index].q; q != nil {
			t = *q.dequantize(&t)
		}
		vals[p.index] = &t
	}
	if err := m.runGraph(&graph{tensors: m.tensors, ops: m.ops}, vals, 0); err != nil {
		return nil, err
	}
	for _, p := range m.Outputs {
		if vals[p.index] == nil {
			return nil, fmt.Errorf("no operator writes output %s", p.Name)
		}
		v := *vals[p.index]
		if q := m.tensors[p.index].q; q != nil {
			v = q.quantize(&v, p.Type)
		}
		out = append(out, v)
	}
	return out, nil
}

// convert is t as typ: floats as float32 (what LiteRT computes in), and
// integers truncated toward zero as CAST does.
func convert(t Tensor, typ Type) Tensor {
	r := Tensor{Type: typ, Shape: t.Shape}
	n := t.Len()
	if typ == String || t.Type == String {
		if typ == t.Type {
			return t
		}
		panic(fmt.Sprintf("a %s tensor cannot be converted to %s", t.Type, typ))
	}
	switch {
	case typ.IsFloat():
		r.F = make([]float32, n)
		for i := range n {
			r.F[i] = t.f(i)
		}
	case typ == Bool:
		r.B = make([]bool, n)
		for i := range n {
			r.B[i] = t.f(i) != 0
		}
	default:
		r.I = make([]int64, n)
		for i := range n {
			switch {
			case t.Type.IsFloat():
				r.I[i] = int64(t.F[i])
			case t.Type == Bool:
				if t.B[i] {
					r.I[i] = 1
				}
			default:
				r.I[i] = t.I[i]
			}
		}
		if typ == Int32 {
			for i := range r.I {
				r.I[i] = int64(int32(r.I[i]))
			}
		}
	}
	return r
}

// maxDepth bounds nested WHILE and IF calls, and maxIterations one
// WHILE's iterations, so a model cannot run forever (#1775).
const (
	maxDepth      = 64
	maxIterations = 1 << 20
)

// runGraph runs g's operators on vals, its tensors' values, in order.
func (m *Model) runGraph(g *graph, vals []*Tensor, depth int) error {
	for _, o := range g.ops {
		args := make([]*Tensor, len(o.in))
		for j, i := range o.in {
			if i >= 0 {
				if vals[i] == nil {
					return fmt.Errorf("operator %s reads tensor %s before any operator writes it", o.name, g.tensors[i].name)
				}
				args[j] = vals[i]
			}
		}
		if o.code == opWhile || o.code == opIf || multiOutput(o.code) {
			var outs []*Tensor
			var err error
			if multiOutput(o.code) {
				outs, err = m.evalMulti(o, args, len(o.out)) // ops2.go
			} else {
				outs, err = m.control(o, args, depth)
			}
			if err != nil {
				return err
			}
			if len(outs) != len(o.out) {
				return fmt.Errorf("operator %s gives %d values for %d outputs", o.name, len(outs), len(o.out))
			}
			for k, i := range o.out {
				if i >= 0 {
					vals[i] = outs[k]
				}
			}
			continue
		}
		if len(o.out) != 1 {
			return &UnsupportedError{fmt.Sprintf("operator %s has %d outputs", o.name, len(o.out))}
		}
		var r *Tensor
		var err error
		if o.custom != nil {
			r, err = o.custom(o.customOpts, args)
		} else {
			r, err = m.eval(o, args)
		}
		if err != nil {
			return err
		}
		want := g.tensors[o.out[0]].typ
		if r.Type != want {
			c := convert(*r, want)
			r = &c
		}
		if q := g.tensors[o.out[0]].q; q != nil {
			r = q.round(r)
		}
		vals[o.out[0]] = r
	}
	return nil
}

// call runs subgraph k on args, one per its inputs, and returns its
// outputs.
func (m *Model) call(k int, args []*Tensor, depth int) ([]*Tensor, error) {
	if depth >= maxDepth {
		return nil, fmt.Errorf("control flow nests deeper than %d calls", maxDepth)
	}
	g := m.graphs[k]
	if len(args) != len(g.in) {
		return nil, fmt.Errorf("subgraph %d takes %d inputs, not %d", k, len(g.in), len(args))
	}
	vals := make([]*Tensor, len(g.tensors))
	for i, d := range g.tensors {
		vals[i] = d.data
	}
	for j, i := range g.in {
		a := args[j]
		if want := g.tensors[i].typ; a != nil && a.Type != want && !(a.Type.IsFloat() && want.IsFloat()) {
			c := convert(*a, want)
			a = &c
		}
		vals[i] = a
	}
	if err := m.runGraph(g, vals, depth+1); err != nil {
		return nil, err
	}
	out := make([]*Tensor, len(g.out))
	for j, i := range g.out {
		if vals[i] == nil {
			return nil, fmt.Errorf("subgraph %d does not write its output %s", k, g.tensors[i].name)
		}
		out[j] = vals[i]
	}
	return out, nil
}

// truth is a condition tensor's one value.
func truth(t *Tensor) (bool, error) {
	if t == nil || t.Len() != 1 {
		return false, fmt.Errorf("a condition must hold one value")
	}
	return t.f(0) != 0, nil
}

// control runs WHILE (WhileOptions: cond_subgraph_index,
// body_subgraph_index) and IF (IfOptions: then_subgraph_index,
// else_subgraph_index), as LiteRT's kernels do.
func (m *Model) control(o op, args []*Tensor, depth int) ([]*Tensor, error) {
	a, b := int(o.i32(4, -1)), int(o.i32(6, -1))
	if o.code == opIf {
		if len(args) == 0 {
			return nil, fmt.Errorf("operator IF has no condition")
		}
		c, err := truth(args[0])
		if err != nil {
			return nil, fmt.Errorf("operator IF: %w", err)
		}
		if c {
			return m.call(a, args[1:], depth)
		}
		return m.call(b, args[1:], depth)
	}
	return m.while(a, b, args, depth)
}

// while runs WHILE: body subgraph b on its state for as long as cond
// subgraph a says.
func (m *Model) while(a, b int, state []*Tensor, depth int) ([]*Tensor, error) {
	for n := 0; ; n++ {
		if n >= maxIterations {
			return nil, fmt.Errorf("operator WHILE ran %d iterations without its condition turning false", maxIterations)
		}
		cond, err := m.call(a, state, depth)
		if err != nil {
			return nil, err
		}
		if len(cond) != 1 {
			return nil, fmt.Errorf("operator WHILE's condition gives %d values, not 1", len(cond))
		}
		c, err := truth(cond[0])
		if err != nil {
			return nil, fmt.Errorf("operator WHILE: %w", err)
		}
		if !c {
			return state, nil
		}
		if state, err = m.call(b, state, depth); err != nil {
			return nil, err
		}
	}
}

// compare is a comparison operator's BOOL result, broadcast.
func compare(o op, x, y *Tensor) (*Tensor, error) {
	if y == nil {
		return nil, fmt.Errorf("operator %s takes two inputs", o.name)
	}
	shape, err := broadcastShape(x.Shape, y.Shape)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	n := prod(shape)
	r := &Tensor{Type: Bool, Shape: shape, B: make([]bool, n)}
	for i := range n {
		xi, yi := broadcastIndex(i, shape, x.Shape), broadcastIndex(i, shape, y.Shape)
		var c int
		switch {
		case x.Type == String && y.Type == String:
			c = strings.Compare(x.S[xi], y.S[yi])
		case x.I != nil && y.I != nil:
			c = cmp.Compare(x.I[xi], y.I[yi])
		default:
			c = cmp.Compare(x.f(xi), y.f(yi))
		}
		switch o.code {
		case opLess:
			r.B[i] = c < 0
		case opLessEqual:
			r.B[i] = c <= 0
		case opGreater:
			r.B[i] = c > 0
		case opGreaterEqual:
			r.B[i] = c >= 0
		case opEqual:
			r.B[i] = c == 0
		case opNotEqual:
			r.B[i] = c != 0
		case opLogicalAnd:
			r.B[i] = x.f(xi) != 0 && y.f(yi) != 0
		case opLogicalOr:
			r.B[i] = x.f(xi) != 0 || y.f(yi) != 0
		}
	}
	return r, nil
}

// selectOp is SELECT and SELECT_V2: x where cond, else y, broadcast (SELECT
// also takes a condition of rank 1 that picks whole rows).
func selectOp(o op, cond, x, y *Tensor) (*Tensor, error) {
	if x == nil || y == nil {
		return nil, fmt.Errorf("operator %s takes three inputs", o.name)
	}
	shape, err := broadcastShape(x.Shape, y.Shape)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	rowwise := o.code == opSelect && len(cond.Shape) == 1 && len(shape) > 1 && cond.Shape[0] == shape[0]
	if !rowwise {
		if shape, err = broadcastShape(cond.Shape, shape); err != nil {
			return nil, fmt.Errorf("operator %s: %w", o.name, err)
		}
	}
	r := alloc(x, shape)
	n := prod(shape)
	row := 1
	if rowwise {
		row = n / shape[0]
	}
	for i := range n {
		var c bool
		if rowwise {
			c = cond.f(i/row) != 0
		} else {
			c = cond.f(broadcastIndex(i, shape, cond.Shape)) != 0
		}
		if c {
			at(r, i, x, broadcastIndex(i, shape, x.Shape))
		} else {
			at(r, i, y, broadcastIndex(i, shape, y.Shape))
		}
	}
	return r, nil
}

// f is element i as float32.
func (t Tensor) f(i int) float32 {
	switch {
	case t.F != nil:
		return t.F[i]
	case t.I != nil:
		return float32(t.I[i])
	case t.B != nil && t.B[i]:
		return 1
	}
	return 0
}

func activation(code int8, v float32) float32 {
	switch code {
	case 1:
		return max(v, 0)
	case 2:
		return min(max(v, -1), 1)
	case 3:
		return min(max(v, 0), 6)
	case 4:
		return float32(math.Tanh(float64(v)))
	case 5:
		if math.Signbit(float64(v)) {
			return 1
		}
		return 0
	}
	return v
}

func (o op) i8(slot int, d int8) int8 {
	if o.opts == nil {
		return d
	}
	return o.opts.GetInt8Slot(flatbuffers.VOffsetT(slot), d)
}

func (o op) i32(slot int, d int32) int32 {
	if o.opts == nil {
		return d
	}
	return o.opts.GetInt32Slot(flatbuffers.VOffsetT(slot), d)
}

func (o op) bool(slot int) bool {
	if o.opts == nil {
		return false
	}
	return o.opts.GetBoolSlot(flatbuffers.VOffsetT(slot), false)
}

func (o op) f32(slot int, d float32) float32 {
	if o.opts == nil {
		return d
	}
	return o.opts.GetFloat32Slot(flatbuffers.VOffsetT(slot), d)
}

// axes reads a constant int tensor as a list.
func axes(t *Tensor) []int {
	if t == nil {
		return nil
	}
	out := make([]int, t.Len())
	for i := range out {
		out[i] = int(t.f(i))
		if t.I != nil {
			out[i] = int(t.I[i])
		}
	}
	return out
}

func (m *Model) eval(o op, a []*Tensor) (*Tensor, error) {
	arg := func(i int) *Tensor {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	x := arg(0)
	if x == nil {
		return nil, fmt.Errorf("operator %s has no input", o.name)
	}
	if x.Type == String {
		switch o.code {
		case opReshape, opSqueeze, opExpandDims, opTranspose, opConcatenation, opPack, opStridedSlice, opGather,
			opEqual, opNotEqual, opLess, opLessEqual, opGreater, opGreaterEqual:
		default:
			return nil, &UnsupportedError{fmt.Sprintf("operator %s on STRING tensors is not supported", o.name)}
		}
	}
	unary := func(fn func(float32) float32) (*Tensor, error) {
		if !x.Type.IsFloat() {
			return nil, &UnsupportedError{fmt.Sprintf("operator %s on %s tensors is not supported", o.name, x.Type)}
		}
		r := &Tensor{Type: x.Type, Shape: x.Shape, F: make([]float32, len(x.F))}
		for i, v := range x.F {
			r.F[i] = fn(v)
		}
		return r, nil
	}
	f64 := func(fn func(float64) float64) func(float32) float32 {
		return func(v float32) float32 { return float32(fn(float64(v))) }
	}
	switch o.code {
	case opLess, opLessEqual, opGreater, opGreaterEqual, opEqual, opNotEqual, opLogicalAnd, opLogicalOr:
		return compare(o, x, arg(1))
	case opLogicalNot:
		r := &Tensor{Type: Bool, Shape: x.Shape, B: make([]bool, x.Len())}
		for i := range r.B {
			r.B[i] = x.f(i) == 0
		}
		return r, nil
	case opSelect, opSelectV2:
		return selectOp(o, x, arg(1), arg(2))
	case opAdd, opSub, opMul, opDiv, opMaximum, opMinimum, opPow, opSquaredDiff:
		act := int8(0)
		if o.code == opAdd || o.code == opSub || o.code == opMul || o.code == opDiv {
			act = o.i8(4, 0)
		}
		return elementwise(o, x, arg(1), act)
	case opAbs:
		return unary(func(v float32) float32 { return float32(math.Abs(float64(v))) })
	case opNeg:
		return unary(func(v float32) float32 { return -v })
	case opExp:
		return unary(f64(math.Exp))
	case opLog:
		return unary(f64(math.Log))
	case opSqrt:
		return unary(f64(math.Sqrt))
	case opRsqrt:
		return unary(func(v float32) float32 { return 1 / float32(math.Sqrt(float64(v))) })
	case opSquare:
		return unary(func(v float32) float32 { return v * v })
	case opTanh:
		return unary(f64(math.Tanh))
	case opLogistic:
		return unary(func(v float32) float32 { return float32(1 / (1 + math.Exp(-float64(v)))) })
	case opRelu:
		return unary(func(v float32) float32 { return activation(1, v) })
	case opReluN1To1:
		return unary(func(v float32) float32 { return activation(2, v) })
	case opRelu6:
		return unary(func(v float32) float32 { return activation(3, v) })
	case opLeakyRelu:
		alpha := o.f32(4, 0)
		return unary(func(v float32) float32 {
			if v < 0 {
				return alpha * v
			}
			return v
		})
	case opGelu:
		approx := o.bool(4)
		return unary(func(v float32) float32 {
			x := float64(v)
			if approx {
				return float32(0.5 * x * (1 + math.Tanh(math.Sqrt(2/math.Pi)*(x+0.044715*x*x*x))))
			}
			return float32(0.5 * x * (1 + math.Erf(x/math.Sqrt2)))
		})
	case opQuantize:
		return x, nil // Run rounds it to the output tensor's grid
	case opDequantize, opCast:
		var to Type
		if o.code == opDequantize {
			to = Float32
		} else {
			to = Type(o.i8(6, int8(x.Type)))
		}
		if o.code == opCast && o.opts == nil {
			return x, nil // Run converts to the output tensor's type
		}
		r := convert(*x, to)
		return &r, nil
	case opSoftmax:
		beta := o.f32(4, 1)
		r, err := unary(func(v float32) float32 { return v })
		if err != nil {
			return nil, err
		}
		inner := x.Shape[len(x.Shape)-1]
		for s := 0; s < len(r.F); s += inner {
			row := r.F[s : s+inner]
			mx := row[0]
			for _, v := range row {
				mx = max(mx, v)
			}
			var sum float32
			for i, v := range row {
				row[i] = float32(math.Exp(float64((v - mx) * beta)))
				sum += row[i]
			}
			for i := range row {
				row[i] /= sum
			}
		}
		return r, nil
	case opFullyConnected:
		return fullyConnected(o, x, arg(1), arg(2))
	case opConv2D, opDepthwiseConv2D:
		return conv2D(o, x, arg(1), arg(2))
	case opAveragePool2D, opMaxPool2D:
		return pool2D(o, x)
	case opGather:
		return gather(o, x, arg(1))
	case opBatchMatMul:
		return batchMatMul(o, x, arg(1))
	case opReshape:
		var shape []int
		if s := arg(1); s != nil {
			shape = axes(s)
		} else if o.opts != nil {
			shape = ints(o.opts, 4)
		}
		return reshape(x, shape)
	case opSqueeze:
		dims := map[int]bool{}
		if o.opts != nil {
			for _, d := range ints(o.opts, 4) {
				if d < 0 {
					d += len(x.Shape)
				}
				dims[d] = true
			}
		}
		var shape []int
		for i, d := range x.Shape {
			if d == 1 && (len(dims) == 0 || dims[i]) {
				continue
			}
			shape = append(shape, d)
		}
		return reshape(x, shape)
	case opExpandDims:
		ax := axes(arg(1))
		if len(ax) != 1 {
			return nil, fmt.Errorf("EXPAND_DIMS takes one axis")
		}
		d := ax[0]
		if d < 0 {
			d += len(x.Shape) + 1
		}
		shape := append(append(append([]int{}, x.Shape[:d]...), 1), x.Shape[d:]...)
		return reshape(x, shape)
	case opConcatenation:
		return concat(a, int(o.i32(4, 0)), o.i8(6, 0))
	case opPack:
		axis := int(o.i32(6, 0))
		if axis < 0 {
			axis += len(x.Shape) + 1
		}
		var parts []*Tensor
		for _, t := range a {
			shape := append(append(append([]int{}, t.Shape[:axis]...), 1), t.Shape[axis:]...)
			r, err := reshape(t, shape)
			if err != nil {
				return nil, err
			}
			parts = append(parts, r)
		}
		return concat(parts, axis, 0)
	case opTranspose:
		return transpose(x, axes(arg(1)))
	case opMean, opSum:
		return reduce(o, x, axes(arg(1)), o.bool(4))
	case opStridedSlice:
		return stridedSlice(o, x, axes(arg(1)), axes(arg(2)), axes(arg(3)))
	}
	return m.eval2(o, a) // ops2.go, #1775
}

// broadcastShape is numpy's broadcast of two shapes.
func broadcastShape(a, b []int) ([]int, error) {
	n := max(len(a), len(b))
	out := make([]int, n)
	for i := range n {
		da, db := 1, 1
		if j := len(a) - n + i; j >= 0 {
			da = a[j]
		}
		if j := len(b) - n + i; j >= 0 {
			db = b[j]
		}
		switch {
		case da == db, db == 1:
			out[i] = da
		case da == 1:
			out[i] = db
		default:
			return nil, fmt.Errorf("shapes %v and %v do not broadcast", a, b)
		}
	}
	return out, nil
}

// broadcastIndex maps a flat index of shape out to one of shape s.
func broadcastIndex(idx int, out, s []int) int {
	r, stride := 0, 1
	for i := len(out) - 1; i >= 0; i-- {
		c := idx % out[i]
		idx /= out[i]
		j := len(s) - len(out) + i
		if j >= 0 {
			if s[j] != 1 {
				r += c * stride
			}
			stride *= s[j]
		}
	}
	return r
}

func elementwise(o op, x, y *Tensor, act int8) (*Tensor, error) {
	if y == nil {
		return nil, fmt.Errorf("operator %s takes two inputs", o.name)
	}
	shape, err := broadcastShape(x.Shape, y.Shape)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	n := prod(shape)
	if !x.Type.IsFloat() || !y.Type.IsFloat() {
		if x.I == nil || y.I == nil {
			return nil, &UnsupportedError{fmt.Sprintf("operator %s on %s and %s tensors is not supported", o.name, x.Type, y.Type)}
		}
		r := &Tensor{Type: x.Type, Shape: shape, I: make([]int64, n)}
		for i := range n {
			p, q := x.I[broadcastIndex(i, shape, x.Shape)], y.I[broadcastIndex(i, shape, y.Shape)]
			switch o.code {
			case opAdd:
				r.I[i] = p + q
			case opSub:
				r.I[i] = p - q
			case opMul:
				r.I[i] = p * q
			case opMaximum:
				r.I[i] = max(p, q)
			case opMinimum:
				r.I[i] = min(p, q)
			case opSquaredDiff:
				r.I[i] = (p - q) * (p - q)
			default:
				return nil, &UnsupportedError{"operator " + o.name + " on integer tensors is not supported"}
			}
		}
		return r, nil
	}
	r := &Tensor{Type: x.Type, Shape: shape, F: make([]float32, n)}
	for i := range n {
		p, q := x.F[broadcastIndex(i, shape, x.Shape)], y.F[broadcastIndex(i, shape, y.Shape)]
		var v float32
		switch o.code {
		case opAdd:
			v = p + q
		case opSub:
			v = p - q
		case opMul:
			v = p * q
		case opDiv:
			v = p / q
		case opMaximum:
			v = max(p, q)
		case opMinimum:
			v = min(p, q)
		case opPow:
			v = float32(math.Pow(float64(p), float64(q)))
		case opSquaredDiff:
			v = (p - q) * (p - q)
		}
		r.F[i] = activation(act, v)
	}
	return r, nil
}

func fullyConnected(o op, x, w, b *Tensor) (*Tensor, error) {
	if w == nil || len(w.Shape) != 2 || !x.Type.IsFloat() || !w.Type.IsFloat() {
		return nil, &UnsupportedError{"FULLY_CONNECTED with these weights is not supported"}
	}
	if o.i8(6, 0) != 0 {
		return nil, &UnsupportedError{"FULLY_CONNECTED with shuffled weights is not supported"}
	}
	units, k := w.Shape[0], w.Shape[1]
	if x.Len()%k != 0 {
		return nil, fmt.Errorf("FULLY_CONNECTED: input of shape %v does not divide into rows of %d", x.Shape, k)
	}
	rows := x.Len() / k
	shape := []int{rows, units}
	if o.bool(8) {
		shape = append(append([]int{}, x.Shape[:len(x.Shape)-1]...), units)
	}
	act := o.i8(4, 0)
	r := &Tensor{Type: Float32, Shape: shape, F: make([]float32, rows*units)}
	for i := range rows {
		in := x.F[i*k : (i+1)*k]
		for u := range units {
			var s float32
			wr := w.F[u*k : (u+1)*k]
			for j, v := range in {
				s += v * wr[j]
			}
			if b != nil {
				s += b.F[u]
			}
			r.F[i*units+u] = activation(act, s)
		}
	}
	return r, nil
}

func batchMatMul(o op, x, y *Tensor) (*Tensor, error) {
	if y == nil || len(x.Shape) < 2 || len(y.Shape) < 2 || !x.Type.IsFloat() || !y.Type.IsFloat() {
		return nil, &UnsupportedError{"BATCH_MATMUL with these operands is not supported"}
	}
	if o.bool(4) {
		t, err := transposeLast(x)
		if err != nil {
			return nil, err
		}
		x = t
	}
	if o.bool(6) {
		t, err := transposeLast(y)
		if err != nil {
			return nil, err
		}
		y = t
	}
	rx, ry := len(x.Shape), len(y.Shape)
	m, k, n := x.Shape[rx-2], x.Shape[rx-1], y.Shape[ry-1]
	if y.Shape[ry-2] != k {
		return nil, fmt.Errorf("BATCH_MATMUL: shapes %v and %v do not multiply", x.Shape, y.Shape)
	}
	batch, err := broadcastShape(x.Shape[:rx-2], y.Shape[:ry-2])
	if err != nil {
		return nil, err
	}
	nb := prod(batch)
	shape := append(append([]int{}, batch...), m, n)
	r := &Tensor{Type: Float32, Shape: shape, F: make([]float32, nb*m*n)}
	for bi := range nb {
		xo := broadcastIndex(bi, batch, x.Shape[:rx-2]) * m * k
		yo := broadcastIndex(bi, batch, y.Shape[:ry-2]) * k * n
		for i := range m {
			for j := range n {
				var s float32
				for l := range k {
					s += x.F[xo+i*k+l] * y.F[yo+l*n+j]
				}
				r.F[bi*m*n+i*n+j] = s
			}
		}
	}
	return r, nil
}

func transposeLast(t *Tensor) (*Tensor, error) {
	perm := make([]int, len(t.Shape))
	for i := range perm {
		perm[i] = i
	}
	perm[len(perm)-1], perm[len(perm)-2] = perm[len(perm)-2], perm[len(perm)-1]
	return transpose(t, perm)
}

func reshape(x *Tensor, shape []int) (*Tensor, error) {
	shape = append([]int{}, shape...)
	free, known := -1, 1
	for i, d := range shape {
		if d < 0 {
			if free >= 0 {
				return nil, fmt.Errorf("RESHAPE: shape %v has more than one -1", shape)
			}
			free = i
			continue
		}
		known *= d
	}
	if free >= 0 {
		if known == 0 || x.Len()%known != 0 {
			return nil, fmt.Errorf("RESHAPE: cannot reshape %v to %v", x.Shape, shape)
		}
		shape[free] = x.Len() / known
	}
	if prod(shape) != x.Len() {
		return nil, fmt.Errorf("RESHAPE: cannot reshape %v to %v", x.Shape, shape)
	}
	r := *x
	r.Shape = shape
	return &r, nil
}

// at copies element i of src to element j of dst.
func at(dst *Tensor, j int, src *Tensor, i int) {
	switch {
	case src.F != nil:
		dst.F[j] = src.F[i]
	case src.I != nil:
		dst.I[j] = src.I[i]
	case src.B != nil:
		dst.B[j] = src.B[i]
	case src.S != nil:
		dst.S[j] = src.S[i]
	}
}

func alloc(like *Tensor, shape []int) *Tensor {
	n := prod(shape)
	r := &Tensor{Type: like.Type, Shape: shape}
	switch {
	case like.F != nil:
		r.F = make([]float32, n)
	case like.I != nil:
		r.I = make([]int64, n)
	case like.B != nil:
		r.B = make([]bool, n)
	case like.S != nil:
		r.S = make([]string, n)
	}
	return r
}

func concat(parts []*Tensor, axis int, act int8) (*Tensor, error) {
	first := parts[0]
	if axis < 0 {
		axis += len(first.Shape)
	}
	shape := append([]int{}, first.Shape...)
	shape[axis] = 0
	for _, p := range parts {
		if len(p.Shape) != len(shape) {
			return nil, fmt.Errorf("CONCATENATION: ranks differ")
		}
		shape[axis] += p.Shape[axis]
	}
	outer := prod(shape[:axis])
	inner := prod(shape[axis+1:])
	r := alloc(first, shape)
	j := 0
	for o := range outer {
		for _, p := range parts {
			c := convert(*p, first.Type)
			w := p.Shape[axis] * inner
			for i := range w {
				at(r, j, &c, o*w+i)
				j++
			}
		}
	}
	if act != 0 && r.F != nil {
		for i, v := range r.F {
			r.F[i] = activation(act, v)
		}
	}
	return r, nil
}

func strides(shape []int) []int {
	s := make([]int, len(shape))
	n := 1
	for i := len(shape) - 1; i >= 0; i-- {
		s[i] = n
		n *= shape[i]
	}
	return s
}

func transpose(x *Tensor, perm []int) (*Tensor, error) {
	if len(perm) != len(x.Shape) {
		return nil, fmt.Errorf("TRANSPOSE: permutation %v of a rank-%d tensor", perm, len(x.Shape))
	}
	shape := make([]int, len(perm))
	for i, p := range perm {
		shape[i] = x.Shape[p]
	}
	in := strides(x.Shape)
	r := alloc(x, shape)
	for j := range r.Len() {
		idx, src := j, 0
		for i := len(shape) - 1; i >= 0; i-- {
			src += (idx % shape[i]) * in[perm[i]]
			idx /= shape[i]
		}
		at(r, j, x, src)
	}
	return r, nil
}

func reduce(o op, x *Tensor, ax []int, keep bool) (*Tensor, error) {
	if !x.Type.IsFloat() {
		return nil, &UnsupportedError{"operator " + o.name + " on " + x.Type.String() + " tensors is not supported"}
	}
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
	r := &Tensor{Type: x.Type, Shape: shape, F: make([]float32, prod(kept))}
	ks := strides(kept)
	count := x.Len() / max(1, prod(kept))
	for i, v := range x.F {
		idx, dst := i, 0
		for d := len(x.Shape) - 1; d >= 0; d-- {
			c := idx % x.Shape[d]
			idx /= x.Shape[d]
			if !red[d] {
				dst += c * ks[d]
			}
		}
		r.F[dst] += v
	}
	if o.code == opMean {
		for i := range r.F {
			r.F[i] /= float32(count)
		}
	}
	return r, nil
}

func stridedSlice(o op, x *Tensor, begin, end, step []int) (*Tensor, error) {
	if o.i32(8, 0) != 0 || o.i32(10, 0) != 0 {
		return nil, &UnsupportedError{"STRIDED_SLICE with an ellipsis or new axis mask is not supported"}
	}
	bm, em, sm := o.i32(4, 0), o.i32(6, 0), o.i32(12, 0)
	offset := o.bool(14)
	rank := len(x.Shape)
	starts, counts, steps := make([]int, rank), make([]int, rank), make([]int, rank)
	var shape []int
	for d := range rank {
		n := x.Shape[d]
		b, e, s := 0, n, 1
		if d < len(step) {
			s = step[d]
		}
		if s == 0 {
			return nil, fmt.Errorf("STRIDED_SLICE: stride 0")
		}
		if d < len(begin) {
			b = begin[d]
		}
		if d < len(end) {
			e = end[d]
			if offset {
				e += b
			}
		}
		clamp := func(v int, lo, hi int) int { return min(max(v, lo), hi) }
		if b < 0 {
			b += n
		}
		if e < 0 && !(offset && d < len(end)) {
			e += n
		}
		if s > 0 {
			b, e = clamp(b, 0, n), clamp(e, 0, n)
			if bm&(1<<d) != 0 || d >= len(begin) {
				b = 0
			}
			if em&(1<<d) != 0 || d >= len(end) {
				e = n
			}
		} else {
			b, e = clamp(b, -1, n-1), clamp(e, -1, n-1)
			if bm&(1<<d) != 0 || d >= len(begin) {
				b = n - 1
			}
			if em&(1<<d) != 0 || d >= len(end) {
				e = -1
			}
		}
		if sm&(1<<d) != 0 {
			if d < len(begin) {
				b = begin[d]
				if b < 0 {
					b += n
				}
			}
			e, s = b+1, 1
		}
		c := 0
		if s > 0 && e > b {
			c = (e - b + s - 1) / s
		} else if s < 0 && b > e {
			c = (b - e - s - 1) / -s
		}
		starts[d], counts[d], steps[d] = b, c, s
		if sm&(1<<d) == 0 {
			shape = append(shape, c)
		}
	}
	in := strides(x.Shape)
	r := alloc(x, shape)
	total := prod(counts)
	for j := range total {
		idx, src := j, 0
		for d := rank - 1; d >= 0; d-- {
			c := idx % counts[d]
			idx /= counts[d]
			src += (starts[d] + c*steps[d]) * in[d]
		}
		at(r, j, x, src)
	}
	if r.Shape == nil {
		r.Shape = []int{}
	}
	return r, nil
}
