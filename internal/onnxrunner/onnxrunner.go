// Package onnxrunner runs BigQuery ML's imported ONNX models (#1711) for
// the BigQuery front, which runs in the cluster from a CGO-less binary that
// cannot load ONNX Runtime.
//
// The Server runs in the CLI of the opt-in `-tags onnx` build (make
// build-onnx), on the instance's --port-onnx, and is published to the
// cluster through cloudburrow-host as Resource Manager is (#1392). The
// front is given its address with --onnx and calls it through a Client:
// it sends a model's bytes once, keyed by their SHA-256, and then runs it
// by that key. The Server keeps the models it was sent in a cache bounded
// by bytes, evicting the least recently used; a Run of a model it no longer
// holds is answered 404 and the Client sends the bytes again.
//
// It is CloudBurrow's own API, not a Google one: JSON over HTTP, served on
// loopback (and to the cluster through cloudburrow-host) only.
package onnxrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

// MaxModelBytes is BigQuery's "Maximum model size is 450 MB".
const MaxModelBytes = 450 << 20

// DefaultCacheBytes bounds the models a Server keeps: two of the largest
// BigQuery imports.
const DefaultCacheBytes = 2 * MaxModelBytes

// Engine inspects and runs models; Local is ONNX Runtime's.
type Engine interface {
	// Init loads the runtime, or says why it cannot be.
	Init() error
	Inspect(model []byte) (inputs, outputs []onnxmodel.Port, err error)
	Run(model []byte, inputs []onnxmodel.Port, in []onnxmodel.Tensor, outputs []onnxmodel.Port) ([]onnxmodel.Tensor, error)
}

// Local is the Engine of internal/onnxmodel: ONNX Runtime in the `-tags
// onnx` build, ErrNotCompiled in the default one.
type Local struct{}

func (Local) Init() error { return onnxmodel.InitRuntime() }

func (Local) Inspect(model []byte) ([]onnxmodel.Port, []onnxmodel.Port, error) {
	return onnxmodel.Inspect(model)
}

func (Local) Run(model []byte, inputs []onnxmodel.Port, in []onnxmodel.Tensor, outputs []onnxmodel.Port) ([]onnxmodel.Tensor, error) {
	return onnxmodel.Run(model, inputs, in, outputs)
}

// Error is the runner's answer to a request it refused: Status is the HTTP
// status it answered, and Message what it said.
//
// 400 is the model's own fault (it does not load, or fails to run on the
// input); 404 a model the runner does not hold; 501 a runner with no ONNX
// Runtime to run it on.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsModelError reports whether err is the model's fault (400), rather than
// the runner's or the way to it.
func IsModelError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 400
}

// IsUnavailable reports whether the runner has no ONNX Runtime (501).
func IsUnavailable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 501
}

// ports is the answer to a load: the model's inputs and outputs.
type ports struct {
	Inputs  []onnxmodel.Port `json:"inputs"`
	Outputs []onnxmodel.Port `json:"outputs"`
}

// runRequest is a run of a loaded model.
type runRequest struct {
	Inputs  []onnxmodel.Port `json:"inputs"`
	Outputs []onnxmodel.Port `json:"outputs"`
	Tensors []wireTensor     `json:"tensors"`
}

type runResponse struct {
	Tensors []wireTensor `json:"tensors"`
}

type errorBody struct {
	Error string `json:"error"`
}

// wireTensor is an onnxmodel.Tensor as JSON, whose floats may be NaN or
// infinite.
type wireTensor struct {
	Shape []int64     `json:"shape"`
	F     []wireFloat `json:"f,omitempty"`
	I     []int64     `json:"i,omitempty"`
	B     []bool      `json:"b,omitempty"`
	S     []string    `json:"s,omitempty"`
}

func toWire(ts []onnxmodel.Tensor) []wireTensor {
	out := make([]wireTensor, len(ts))
	for i, t := range ts {
		w := wireTensor{Shape: t.Shape, I: t.I, B: t.B, S: t.S}
		if t.F != nil {
			w.F = make([]wireFloat, len(t.F))
			for j, f := range t.F {
				w.F[j] = wireFloat(f)
			}
		}
		out[i] = w
	}
	return out
}

func fromWire(ws []wireTensor) []onnxmodel.Tensor {
	out := make([]onnxmodel.Tensor, len(ws))
	for i, w := range ws {
		t := onnxmodel.Tensor{Shape: w.Shape, I: w.I, B: w.B, S: w.S}
		if w.F != nil {
			t.F = make([]float64, len(w.F))
			for j, f := range w.F {
				t.F[j] = float64(f)
			}
		}
		out[i] = t
	}
	return out
}

// wireFloat is a float64 whose NaN and infinities are the strings "NaN",
// "Infinity" and "-Infinity", which encoding/json cannot write as numbers.
type wireFloat float64

func (f wireFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	switch {
	case math.IsNaN(v):
		return []byte(`"NaN"`), nil
	case math.IsInf(v, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(v, -1):
		return []byte(`"-Infinity"`), nil
	}
	return json.Marshal(v)
}

func (f *wireFloat) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		switch s {
		case "NaN":
			*f = wireFloat(math.NaN())
		case "Infinity":
			*f = wireFloat(math.Inf(1))
		case "-Infinity":
			*f = wireFloat(math.Inf(-1))
		default:
			return fmt.Errorf("onnxrunner: not a float: %q", s)
		}
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = wireFloat(v)
	return nil
}
