// Package tflitemodel reads a TensorFlow Lite (LiteRT) model, the .tflite
// FlatBuffer of LiteRT's schema.fbs (Google's, Apache-2.0; read through
// Google's FlatBuffers Go runtime), and runs its float builtin operators
// in pure Go, so the CGO-less front can run imported TENSORFLOW_LITE
// models in BigQuery ML (#1755).
//
// It runs one subgraph: the model's serving_default (or only) SignatureDef,
// or subgraph 0's inputs and outputs when the model has no signature, with
// float32 arithmetic as LiteRT's reference kernels do, checked against
// LiteRT's own interpreter (ai-edge-litert 2.3.0) on models
// TensorFlow 2.20's TFLiteConverter wrote (tflitemodel_test.go).
// Quantized tensors (quant.go) and STRING tensors (strings.go) run too
// (#1766). An operator it does not run or a second subgraph is an
// *UnsupportedError, so the caller can refuse it as not implemented
// rather than as the model's fault.
package tflitemodel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Type is a tensor's element type: schema.fbs's TensorType.
type Type int8

// The TensorType values of schema.fbs.
const (
	Float32 Type = 0
	Float16 Type = 1
	Int32   Type = 2
	Uint8   Type = 3
	Int64   Type = 4
	String  Type = 5
	Bool    Type = 6
	Int16   Type = 7
	Complex Type = 8
	Int8    Type = 9
	Float64 Type = 10
	Cplx128 Type = 11
	Uint64  Type = 12
	Resrc   Type = 13
	Variant Type = 14
	Uint32  Type = 15
	Uint16  Type = 16
)

var typeNames = map[Type]string{Float32: "FLOAT32", Float16: "FLOAT16", Int32: "INT32", Uint8: "UINT8", Int64: "INT64", String: "STRING",
	Bool: "BOOL", Int16: "INT16", Complex: "COMPLEX64", Int8: "INT8", Float64: "FLOAT64", Cplx128: "COMPLEX128", Uint64: "UINT64",
	Resrc: "RESOURCE", Variant: "VARIANT", Uint32: "UINT32", Uint16: "UINT16"}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("TensorType(%d)", int8(t))
}

// IsFloat reports whether t is FLOAT16, FLOAT32 or FLOAT64.
func (t Type) IsFloat() bool { return t == Float16 || t == Float32 || t == Float64 }

// IsInt reports whether t is one of the integer types.
func (t Type) IsInt() bool {
	switch t {
	case Int8, Int16, Int32, Int64, Uint8, Uint16, Uint32, Uint64:
		return true
	}
	return false
}

// Tensor is a value: its shape, and its elements row-major in F (the
// float types), I (the integer types), B (BOOL) or S (STRING, #1766).
type Tensor struct {
	Type  Type
	Shape []int
	F     []float32
	I     []int64
	B     []bool
	S     []string
	// C holds a COMPLEX64 tensor's elements (RFFT2D, ops4.go, #1775).
	C []complex64
}

// Len is the number of elements the shape holds.
func (t Tensor) Len() int { return prod(t.Shape) }

// Port is a model input or output: its signature name, element type and
// shape, where -1 is a dimension the model leaves free (the batch).
type Port struct {
	Name  string
	Type  Type
	Shape []int
	index int
}

// UnsupportedError is a model this package does not run.
type UnsupportedError struct{ What string }

func (e *UnsupportedError) Error() string { return e.What }

// IsUnsupported reports whether err is an *UnsupportedError.
func IsUnsupported(err error) bool {
	var u *UnsupportedError
	return errors.As(err, &u)
}

type tensorDef struct {
	name  string
	typ   Type
	shape []int
	data  *Tensor // a constant
	// q is a quantized tensor's parameters (quant.go, #1766): its values
	// are carried as float32, typ is FLOAT32, and stored is its own type.
	q      *quant
	stored Type
	// variable is schema.fbs's is_variable: a recurrent kernel's state,
	// zero at the start of each Run unless the model gives it a value
	// (#1775).
	variable bool
}

type op struct {
	code    int32
	name    string
	in, out []int
	opts    *flatbuffers.Table
	// custom is a custom operator's kernel, and customOpts its
	// custom_options (a FlexBuffer), when the caller supplied one (LoadWith).
	custom     CustomOp
	customOpts []byte
	// outType is its first output tensor's type, as stored (BITCAST's
	// target).
	outType Type
}

// CustomOp runs one custom operator: its custom_options, as the model
// stores them, and its inputs; it gives its one output.
type CustomOp func(opts []byte, in []*Tensor) (*Tensor, error)

// opCustom is schema.fbs's CUSTOM.
const opCustom = 32

// Model is a loaded .tflite model.
type Model struct {
	Inputs, Outputs []Port
	// tensors and ops are the signature's subgraph's.
	tensors []tensorDef
	ops     []op
	// graphs are every subgraph, which WHILE and IF call (#1775).
	graphs []*graph
}

// graph is one subgraph: its tensors, operators, and the tensors its
// inputs and outputs are.
type graph struct {
	tensors []tensorDef
	ops     []op
	in, out []int
}

// builtin operator codes of schema.fbs's BuiltinOperator that Run runs.
const (
	opAdd            = 0
	opConcatenation  = 2
	opDequantize     = 6
	opFullyConnected = 9
	opLogistic       = 14
	opMul            = 18
	opRelu           = 19
	opReluN1To1      = 20
	opRelu6          = 21
	opReshape        = 22
	opSoftmax        = 25
	opTanh           = 28
	opTranspose      = 39
	opMean           = 40
	opSub            = 41
	opDiv            = 42
	opSqueeze        = 43
	opStridedSlice   = 45
	opExp            = 47
	opCast           = 53
	opMaximum        = 55
	opMinimum        = 57
	opNeg            = 59
	opExpandDims     = 70
	opLog            = 73
	opSum            = 74
	opSqrt           = 75
	opRsqrt          = 76
	opPow            = 78
	opPack           = 83
	opSquare         = 92
	opLeakyRelu      = 98
	opSquaredDiff    = 99
	opAbs            = 101
	opBatchMatMul    = 126
	opGelu           = 150
	// The convolution, pooling and gather operators (#1766).
	opAveragePool2D   = 1
	opConv2D          = 3
	opDepthwiseConv2D = 4
	opMaxPool2D       = 17
	opGather          = 36
	opQuantize        = 114
	// Control flow, comparisons, logic and selection (#1775).
	opLess         = 58
	opGreater      = 61
	opGreaterEqual = 62
	opLessEqual    = 63
	opSelect       = 64
	opEqual        = 71
	opNotEqual     = 72
	opLogicalOr    = 84
	opLogicalAnd   = 86
	opLogicalNot   = 87
	opIf           = 118
	opWhile        = 119
	opSelectV2     = 123
)

var opNames = map[int32]string{opAdd: "ADD", opConcatenation: "CONCATENATION", opDequantize: "DEQUANTIZE", opFullyConnected: "FULLY_CONNECTED",
	opLogistic: "LOGISTIC", opMul: "MUL", opRelu: "RELU", opReluN1To1: "RELU_N1_TO_1", opRelu6: "RELU6", opReshape: "RESHAPE", opSoftmax: "SOFTMAX",
	opTanh: "TANH", opTranspose: "TRANSPOSE", opMean: "MEAN", opSub: "SUB", opDiv: "DIV", opSqueeze: "SQUEEZE", opStridedSlice: "STRIDED_SLICE",
	opExp: "EXP", opCast: "CAST", opMaximum: "MAXIMUM", opMinimum: "MINIMUM", opNeg: "NEG", opExpandDims: "EXPAND_DIMS", opLog: "LOG",
	opSum: "SUM", opSqrt: "SQRT", opRsqrt: "RSQRT", opPow: "POW", opPack: "PACK", opSquare: "SQUARE", opLeakyRelu: "LEAKY_RELU",
	opSquaredDiff: "SQUARED_DIFFERENCE", opAbs: "ABS", opBatchMatMul: "BATCH_MATMUL", opGelu: "GELU",
	opAveragePool2D: "AVERAGE_POOL_2D", opConv2D: "CONV_2D", opDepthwiseConv2D: "DEPTHWISE_CONV_2D", opMaxPool2D: "MAX_POOL_2D", opGather: "GATHER", opQuantize: "QUANTIZE",
	opLess: "LESS", opGreater: "GREATER", opGreaterEqual: "GREATER_EQUAL", opLessEqual: "LESS_EQUAL", opSelect: "SELECT", opEqual: "EQUAL",
	opNotEqual: "NOT_EQUAL", opLogicalOr: "LOGICAL_OR", opLogicalAnd: "LOGICAL_AND", opLogicalNot: "LOGICAL_NOT", opIf: "IF", opWhile: "WHILE",
	opSelectV2: "SELECT_V2"}

// Ops are the builtin operators Run runs, by schema.fbs name.
func Ops() []string {
	var out []string
	for _, n := range opNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// table is the table at the uoffset at pos.
func table(buf []byte, pos flatbuffers.UOffsetT) *flatbuffers.Table {
	return &flatbuffers.Table{Bytes: buf, Pos: pos + flatbuffers.GetUOffsetT(buf[pos:])}
}

// field is a table's field's offset from the table, as the
// flatbuffers.Table accessors take it, or 0.
func field(t *flatbuffers.Table, slot int) flatbuffers.UOffsetT {
	return flatbuffers.UOffsetT(t.Offset(flatbuffers.VOffsetT(slot)))
}

// sub is the table a table's field refers to, or nil.
func sub(t *flatbuffers.Table, slot int) *flatbuffers.Table {
	p := field(t, slot)
	if p == 0 {
		return nil
	}
	return table(t.Bytes, t.Pos+p)
}

// tables is a vector of tables at a field.
func tables(t *flatbuffers.Table, slot int) []*flatbuffers.Table {
	p := field(t, slot)
	if p == 0 {
		return nil
	}
	n := t.VectorLen(p)
	start := t.Vector(p)
	out := make([]*flatbuffers.Table, n)
	for i := range n {
		out[i] = table(t.Bytes, start+flatbuffers.UOffsetT(4*i))
	}
	return out
}

// ints is a vector of int32 at a field.
func ints(t *flatbuffers.Table, slot int) []int {
	p := field(t, slot)
	if p == 0 {
		return nil
	}
	n := t.VectorLen(p)
	start := t.Vector(p)
	out := make([]int, n)
	for i := range n {
		out[i] = int(t.GetInt32(start + flatbuffers.UOffsetT(4*i)))
	}
	return out
}

func str(t *flatbuffers.Table, slot int) string {
	p := field(t, slot)
	if p == 0 {
		return ""
	}
	return t.String(t.Pos + p) // String takes an absolute position
}

func i32(t *flatbuffers.Table, slot int, d int32) int32 {
	return t.GetInt32Slot(flatbuffers.VOffsetT(slot), d)
}

// Load reads a .tflite model.
func Load(data []byte) (m *Model, err error) {
	return LoadWith(data, nil)
}

// LoadWith is Load for a model that uses custom operators, each run by the
// kernel custom names (by its custom_code). A custom operator it does not
// name is an *UnsupportedError, as Load's are.
func LoadWith(data []byte, custom map[string]CustomOp) (m *Model, err error) {
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("the file is not a valid TensorFlow Lite model: %v", r)
		}
	}()
	return loadNoRecover(data, custom)
}

func loadNoRecover(data []byte, custom map[string]CustomOp) (m *Model, err error) {
	if len(data) < 8 || string(data[4:8]) != "TFL3" {
		return nil, errors.New("the file is not a TensorFlow Lite model (it lacks the TFL3 FlatBuffer identifier)")
	}
	root := table(data, 0)
	codes := tables(root, 6)
	subs := tables(root, 8)
	bufs := tables(root, 12)
	if len(subs) == 0 {
		return nil, errors.New("the model has no subgraph")
	}
	sigs := tables(root, 18)
	sg, subIdx := subs[0], 0
	var sig *flatbuffers.Table
	for _, s := range sigs {
		if sig == nil || str(s, 8) == "serving_default" {
			sig = s
		}
	}
	if sig != nil {
		subIdx = int(sig.GetUint32Slot(12, 0))
		if subIdx >= len(subs) {
			return nil, errors.New("the model's signature names a subgraph it does not have")
		}
		sg = subs[subIdx]
	}
	m = &Model{}
	for _, s := range subs {
		g, err := loadGraph(s, codes, bufs, custom)
		if err != nil {
			return nil, err
		}
		m.graphs = append(m.graphs, g)
	}
	_ = sg
	main := m.graphs[subIdx]
	m.tensors, m.ops = main.tensors, main.ops
	for _, g := range m.graphs {
		for _, o := range g.ops {
			if o.code == opWhile || o.code == opIf {
				for _, slot := range []int{4, 6} {
					if k := int(o.i32(slot, -1)); k < 0 || k >= len(m.graphs) {
						return nil, fmt.Errorf("operator %s names subgraph %d, which the model does not have", o.name, k)
					}
				}
			}
		}
	}
	port := func(name string, i int) (Port, error) {
		if i < 0 || i >= len(m.tensors) {
			return Port{}, errors.New("the model names a tensor it does not have")
		}
		d := m.tensors[i]
		if name == "" {
			name = d.name
		}
		typ := d.typ
		if d.q != nil {
			typ = d.stored // a quantized port takes and gives its integers
		}
		return Port{Name: name, Type: typ, Shape: d.shape, index: i}, nil
	}
	sg = subs[subIdx]
	if sig != nil {
		for k, slot := range []int{4, 6} {
			for _, tm := range tables(sig, slot) {
				p, err := port(str(tm, 4), int(tm.GetUint32Slot(6, 0)))
				if err != nil {
					return nil, err
				}
				if k == 0 {
					m.Inputs = append(m.Inputs, p)
				} else {
					m.Outputs = append(m.Outputs, p)
				}
			}
		}
	} else {
		for k, slot := range []int{6, 8} {
			for _, i := range ints(sg, slot) {
				p, err := port("", i)
				if err != nil {
					return nil, err
				}
				if k == 0 {
					m.Inputs = append(m.Inputs, p)
				} else {
					m.Outputs = append(m.Outputs, p)
				}
			}
		}
	}
	return m, nil
}

// loadGraph reads one subgraph.
func loadGraph(sg *flatbuffers.Table, codes, bufs []*flatbuffers.Table, custom map[string]CustomOp) (*graph, error) {
	m := &graph{in: ints(sg, 6), out: ints(sg, 8)}
	for _, t := range tables(sg, 4) {
		d := tensorDef{name: str(t, 10), typ: Type(t.GetInt8Slot(6, 0)), shape: ints(t, 4)}
		if sh := ints(t, 18); sh != nil {
			d.shape = sh
		}
		d.variable = t.GetBoolSlot(14, false)
		if qt := sub(t, 12); qt != nil && !d.typ.IsFloat() {
			q, err := readQuant(qt, d)
			if err != nil {
				return nil, err
			}
			d.q = q
		}
		if field(t, 16) != 0 {
			return nil, &UnsupportedError{"tensor " + d.name + " is sparse: sparse tensors are not supported"}
		}
		if b := int(t.GetUint32Slot(8, 0)); b > 0 && b < len(bufs) {
			bp := field(bufs[b], 4)
			if bp != 0 {
				raw := bufs[b].ByteVector(bufs[b].Pos + bp)
				c, err := constant(d.typ, ints(t, 4), raw)
				if err != nil {
					return nil, fmt.Errorf("tensor %s: %w", d.name, err)
				}
				if d.q != nil {
					c = d.q.dequantize(c)
				}
				d.data = c
			} else if bufs[b].GetUint64Slot(6, 0) > 1 {
				return nil, &UnsupportedError{"tensor " + d.name + " keeps its data outside the FlatBuffer: CloudBurrow reads models under 2 GB"}
			}
		}
		if d.q != nil {
			d.stored, d.typ = d.typ, Float32
		}
		m.tensors = append(m.tensors, d)
	}
	for _, o := range tables(sg, 10) {
		idx := int(o.GetUint32Slot(4, 0))
		if idx >= len(codes) {
			return nil, errors.New("an operator names an operator code the model does not have")
		}
		c := codes[idx]
		code := i32(c, 10, 0)
		if dep := int32(c.GetInt8Slot(4, 0)); dep > code {
			code = dep
		}
		if code == opCustom {
			if k := custom[str(c, 6)]; k != nil {
				var opts []byte
				if f := field(o, 14); f != 0 {
					opts = o.ByteVector(o.Pos + f)
				}
				m.ops = append(m.ops, op{code: code, name: str(c, 6), in: ints(o, 6), out: ints(o, 8), custom: k, customOpts: opts})
				continue
			}
			if bc, ok := builtinCustom[str(c, 6)]; ok { // detect.go
				var opts []byte
				if f := field(o, 14); f != 0 {
					opts = o.ByteVector(o.Pos + f)
				}
				m.ops = append(m.ops, op{code: bc, name: str(c, 6), in: ints(o, 6), out: ints(o, 8), customOpts: opts})
				continue
			}
			return nil, &UnsupportedError{"the model uses the custom operator " + str(c, 6) + ": only TensorFlow core operations are supported"}
		}
		name, ok := opNames[code]
		if !ok {
			return nil, &UnsupportedError{fmt.Sprintf("the model uses TensorFlow Lite builtin operator %d, which CloudBurrow's interpreter does not run (it runs %v)", code, Ops())}
		}
		op := op{code: code, name: name, in: ints(o, 6), out: ints(o, 8)}
		if outs := op.out; len(outs) > 0 && outs[0] >= 0 && outs[0] < len(m.tensors) {
			op.outType = m.tensors[outs[0]].typ
			if m.tensors[outs[0]].q != nil {
				op.outType = m.tensors[outs[0]].stored
			}
		}
		op.opts = sub(o, 12)
		if op.opts == nil {
			op.opts = sub(o, 28) // builtin_options_2: the StableHLO operators' (#1775)
		}
		m.ops = append(m.ops, op)
	}
	return m, nil
}

// constant decodes a buffer's little-endian elements.
func constant(typ Type, shape []int, raw []byte) (*Tensor, error) {
	t := &Tensor{Type: typ, Shape: shape}
	if typ == String {
		return stringConstant(t, raw)
	}
	n := prod(shape)
	size := map[Type]int{Float32: 4, Float16: 2, Float64: 8, Int8: 1, Uint8: 1, Int16: 2, Uint16: 2, Int32: 4, Uint32: 4, Int64: 8, Uint64: 8, Bool: 1}[typ]
	if size == 0 {
		return nil, &UnsupportedError{"constant tensors of type " + typ.String() + " are not supported"}
	}
	if len(raw) < n*size {
		return nil, fmt.Errorf("its buffer holds %d bytes, not the %d its shape needs", len(raw), n*size)
	}
	le := binary.LittleEndian
	for i := range n {
		b := raw[i*size:]
		switch typ {
		case Float32:
			t.F = append(t.F, math.Float32frombits(le.Uint32(b)))
		case Float16:
			t.F = append(t.F, half(le.Uint16(b)))
		case Float64:
			t.F = append(t.F, float32(math.Float64frombits(le.Uint64(b))))
		case Int8:
			t.I = append(t.I, int64(int8(b[0])))
		case Uint8:
			t.I = append(t.I, int64(b[0]))
		case Int16:
			t.I = append(t.I, int64(int16(le.Uint16(b))))
		case Uint16:
			t.I = append(t.I, int64(le.Uint16(b)))
		case Int32:
			t.I = append(t.I, int64(int32(le.Uint32(b))))
		case Uint32:
			t.I = append(t.I, int64(le.Uint32(b)))
		case Int64, Uint64:
			t.I = append(t.I, int64(le.Uint64(b)))
		case Bool:
			t.B = append(t.B, b[0] != 0)
		}
	}
	return t, nil
}

// half is an IEEE 754 binary16 value.
func half(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := int(h>>10) & 0x1f
	frac := uint32(h & 0x3ff)
	switch {
	case exp == 0:
		v := float32(frac) / 1024 / 16384
		if sign != 0 {
			v = -v
		}
		return v
	case exp == 31:
		return math.Float32frombits(sign | 0x7f800000 | frac<<13)
	}
	return math.Float32frombits(sign | uint32(exp-15+127)<<23 | frac<<13)
}

func prod(s []int) int {
	n := 1
	for _, d := range s {
		n *= d
	}
	return n
}

// Uses are the builtin operators the model's subgraphs use, by schema.fbs
// name, sorted.
func (m *Model) Uses() []string {
	seen := map[string]bool{}
	for _, g := range m.graphs {
		for _, o := range g.ops {
			seen[o.name] = true
		}
	}
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
