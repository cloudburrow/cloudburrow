// Package onnxmodel runs an imported ONNX model (BigQuery ML's
// MODEL_TYPE='ONNX', #1711) on the ONNX Runtime that the opt-in `-tags onnx`
// build loads (ONNXRUNTIME_LIB), as internal/embedding does for Vertex AI
// embeddings. The default build has stub.go, whose functions return
// ErrNotCompiled; the BigQuery front then refuses an ONNX model 501.
package onnxmodel

import "errors"

// ErrNotCompiled means this binary was built without the onnx tag.
var ErrNotCompiled = errors.New("the ONNX Runtime is not compiled in: an imported ONNX model needs CloudBurrow's " +
	"`-tags onnx` build (make build-onnx) run with ONNXRUNTIME_LIB set (see docs/embeddings.md)")

// Port is one of a model's inputs or outputs, as its graph declares it.
type Port struct {
	Name string `json:"name"`
	// Kind is its ONNX value type: ONNX_TYPE_TENSOR, ONNX_TYPE_SEQUENCE,
	// ONNX_TYPE_MAP, ONNX_TYPE_OPAQUE, ONNX_TYPE_SPARSETENSOR,
	// ONNX_TYPE_OPTIONAL or ONNX_TYPE_UNKNOWN.
	Kind string `json:"kind"`
	// Elem is a tensor's element type, ONNX's name without its
	// ONNX_TENSOR_ELEMENT_DATA_TYPE_ prefix: FLOAT, DOUBLE, INT64, BOOL,
	// STRING, FLOAT16, ...
	Elem string `json:"elem,omitempty"`
	// Dims are a tensor's dimensions, -1 where the graph leaves one free.
	Dims []int64 `json:"dims,omitempty"`
}

// Tensor is the data of one input or output, flat in row-major order.
// Exactly one of the slices is used: F for floating-point elements, I for
// integers, B for booleans and S for strings.
type Tensor struct {
	Shape []int64
	F     []float64
	I     []int64
	B     []bool
	S     []string
}

// Kind of a Port that is a tensor.
const KindTensor = "ONNX_TYPE_TENSOR"

// IsFloat reports whether elem is a floating-point element type.
func IsFloat(elem string) bool {
	switch elem {
	case "FLOAT", "DOUBLE", "FLOAT16", "BFLOAT16":
		return true
	}
	return false
}

// IsInt reports whether elem is an integer element type.
func IsInt(elem string) bool {
	switch elem {
	case "INT8", "UINT8", "INT16", "UINT16", "INT32", "UINT32", "INT64", "UINT64":
		return true
	}
	return false
}
