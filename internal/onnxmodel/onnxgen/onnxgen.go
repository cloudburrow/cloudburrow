// Package onnxgen writes the tiny ONNX models CloudBurrow's tests import
// into BigQuery ML (#1711), by hand, as ONNX ModelProto protobuf
// (https://github.com/onnx/onnx/blob/main/onnx/onnx.proto, field numbers
// cited at each message), so their provenance is this file and nothing
// else: no framework exported them and no weights were trained.
//
// The checked-in testdata/*.onnx are this package's output;
// TestCheckedInModels regenerates them and compares (go test
// ./internal/onnxmodel/onnxgen -update rewrites them).
package onnxgen

import (
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// ONNX TensorProto.DataType values.
const (
	typeFloat = 1
	typeInt64 = 7
)

// Opset and IR version: opset 13 and IR version 7 are within what ONNX
// Runtime 1.12.0, the version BigQuery documents, reads.
const (
	irVersion = 7
	opset     = 13
)

type msg []byte

func (m msg) bytes(field protowire.Number, b []byte) msg {
	m = protowire.AppendTag(m, field, protowire.BytesType)
	return protowire.AppendBytes(m, b)
}

func (m msg) str(field protowire.Number, s string) msg { return m.bytes(field, []byte(s)) }

func (m msg) varint(field protowire.Number, v uint64) msg {
	m = protowire.AppendTag(m, field, protowire.VarintType)
	return protowire.AppendVarint(m, v)
}

// dim is a TensorShapeProto.Dimension: dim_value = 1, dim_param = 2.
func dim(v int64, param string) []byte {
	if param != "" {
		return msg{}.str(2, param)
	}
	return msg{}.varint(1, uint64(v))
}

// tensorType is a TypeProto (tensor_type = 1) of a TypeProto.Tensor
// (elem_type = 1, shape = 2) whose TensorShapeProto has dim = 1; a
// dimension of -1 is the free batch dimension "N".
func tensorType(elem int, dims ...int64) []byte {
	var shape msg
	for _, d := range dims {
		if d < 0 {
			shape = shape.bytes(1, dim(0, "N"))
		} else {
			shape = shape.bytes(1, dim(d, ""))
		}
	}
	t := msg{}.varint(1, uint64(elem)).bytes(2, shape)
	return msg{}.bytes(1, t)
}

// sequenceType is a TypeProto (sequence_type = 4) of a TypeProto.Sequence
// (elem_type = 1).
func sequenceType(elem []byte) []byte { return msg{}.bytes(4, msg{}.bytes(1, elem)) }

// valueInfo is a ValueInfoProto: name = 1, type = 2.
func valueInfo(name string, typ []byte) []byte { return msg{}.str(1, name).bytes(2, typ) }

// floatTensor is a TensorProto: dims = 1, data_type = 2, float_data = 4
// (packed), name = 8.
func floatTensor(name string, dims []int64, data []float32) []byte {
	var m msg
	for _, d := range dims {
		m = m.varint(1, uint64(d))
	}
	m = m.varint(2, typeFloat)
	var packed []byte
	for _, f := range data {
		packed = protowire.AppendFixed32(packed, math.Float32bits(f))
	}
	return m.bytes(4, packed).str(8, name)
}

// intAttr is an AttributeProto: name = 1, i = 3, type = 20 (INT = 2).
func intAttr(name string, v int64) []byte {
	return msg{}.str(1, name).varint(3, uint64(v)).varint(20, 2)
}

// intsAttr is an AttributeProto: name = 1, ints = 8, type = 20 (INTS = 7).
func intsAttr(name string, vs ...int64) []byte {
	var packed []byte
	for _, v := range vs {
		packed = protowire.AppendVarint(packed, uint64(v))
	}
	return msg{}.str(1, name).bytes(8, packed).varint(20, 7)
}

// node is a NodeProto: input = 1, output = 2, name = 3, op_type = 4,
// attribute = 5.
func node(op, name string, in, out []string, attrs ...[]byte) []byte {
	var m msg
	for _, s := range in {
		m = m.str(1, s)
	}
	for _, s := range out {
		m = m.str(2, s)
	}
	m = m.str(3, name).str(4, op)
	for _, a := range attrs {
		m = m.bytes(5, a)
	}
	return m
}

// model is a ModelProto (ir_version = 1, producer_name = 2, graph = 7,
// opset_import = 8) of a GraphProto (node = 1, name = 2, initializer = 5,
// input = 11, output = 12); the OperatorSetIdProto has domain = 1,
// version = 2, the empty domain being ONNX's.
func model(name string, nodes, inits, inputs, outputs [][]byte) []byte {
	var g msg
	for _, n := range nodes {
		g = g.bytes(1, n)
	}
	g = g.str(2, name)
	for _, t := range inits {
		g = g.bytes(5, t)
	}
	for _, v := range inputs {
		g = g.bytes(11, v)
	}
	for _, v := range outputs {
		g = g.bytes(12, v)
	}
	return msg{}.varint(1, irVersion).str(2, "cloudburrow-onnxgen").bytes(7, g).bytes(8, msg{}.str(1, "").varint(2, opset))
}

// Linear is a linear model of two FLOAT inputs, a and b, each [N, 1] (one
// value of a row, a FLOAT64 column in BigQuery), and a FLOAT input v of
// [N, 2] (an ARRAY<FLOAT64> column of two values):
//
//	y      = 2a + 3b + v[0] - v[1] + 1   FLOAT [N, 1]  (a FLOAT64 column)
//	label  = y > 5                       INT64 [N]     (an INT64 column)
//	scores = [y, -y]                     FLOAT [N, 2]  (an ARRAY<FLOAT64>)
//
// computed as Concat(a, b, v) (axis 1) MatMul W [[2], [3], [1], [-1]], Add
// bias [1].
func Linear() []byte {
	nodes := [][]byte{
		node("Concat", "concat", []string{"a", "b", "v"}, []string{"x"}, intAttr("axis", 1)),
		node("MatMul", "matmul", []string{"x", "W"}, []string{"xw"}),
		node("Add", "add", []string{"xw", "bias"}, []string{"y"}),
		node("Greater", "greater", []string{"y", "five"}, []string{"over"}),
		node("Squeeze", "squeeze", []string{"over", "axes"}, []string{"over1"}),
		node("Cast", "cast", []string{"over1"}, []string{"label"}, intAttr("to", typeInt64)),
		node("Neg", "neg", []string{"y"}, []string{"negy"}),
		node("Concat", "concat2", []string{"y", "negy"}, []string{"scores"}, intAttr("axis", 1)),
	}
	axes := msg{}.varint(1, 1).varint(2, typeInt64).bytes(7, protowire.AppendVarint(nil, 1)).str(8, "axes")
	inits := [][]byte{
		floatTensor("W", []int64{4, 1}, []float32{2, 3, 1, -1}),
		floatTensor("bias", []int64{1}, []float32{1}),
		floatTensor("five", []int64{1}, []float32{5}),
		axes,
	}
	inputs := [][]byte{
		valueInfo("a", tensorType(typeFloat, -1, 1)),
		valueInfo("b", tensorType(typeFloat, -1, 1)),
		valueInfo("v", tensorType(typeFloat, -1, 2)),
	}
	outputs := [][]byte{
		valueInfo("y", tensorType(typeFloat, -1, 1)),
		valueInfo("label", tensorType(typeInt64, -1)),
		valueInfo("scores", tensorType(typeFloat, -1, 2)),
	}
	return model("linear", nodes, inits, inputs, outputs)
}

// SequenceOutput is Linear's y, returned as a sequence of one tensor
// (SequenceConstruct), the output type BigQuery documents refusing: "ONNX
// model output 'y' has unsupported ONNX type: ONNX_TYPE_SEQUENCE."
func SequenceOutput() []byte {
	nodes := [][]byte{
		node("Add", "add", []string{"a", "bias"}, []string{"y0"}),
		node("SequenceConstruct", "seq", []string{"y0"}, []string{"y"}),
	}
	inits := [][]byte{floatTensor("bias", []int64{1}, []float32{1})}
	inputs := [][]byte{valueInfo("a", tensorType(typeFloat, -1, 1))}
	outputs := [][]byte{valueInfo("y", sequenceType(tensorType(typeFloat, -1, 1)))}
	return model("sequence", nodes, inits, inputs, outputs)
}

// ImageMean takes an image as BigQuery's object table inference documents
// a model's input, "a shape of [batch_size, width, height, 3]" (#2104): a
// FLOAT input named input of [N, 2, 2, 3], channels last, and returns
//
//	rgb        = the mean of each channel   FLOAT [N, 3]  (an ARRAY<FLOAT64>)
//	brightness = the mean of every value    FLOAT [N]     (a FLOAT64 column)
//
// computed as ReduceMean over axes 1 and 2, and over axes 1, 2 and 3, with
// keepdims 0.
func ImageMean() []byte {
	nodes := [][]byte{
		node("ReduceMean", "rgb", []string{"input"}, []string{"rgb"}, intsAttr("axes", 1, 2), intAttr("keepdims", 0)),
		node("ReduceMean", "brightness", []string{"input"}, []string{"brightness"}, intsAttr("axes", 1, 2, 3), intAttr("keepdims", 0)),
	}
	inputs := [][]byte{valueInfo("input", tensorType(typeFloat, -1, 2, 2, 3))}
	outputs := [][]byte{
		valueInfo("rgb", tensorType(typeFloat, -1, 3)),
		valueInfo("brightness", tensorType(typeFloat, -1)),
	}
	return model("image_mean", nodes, nil, inputs, outputs)
}
