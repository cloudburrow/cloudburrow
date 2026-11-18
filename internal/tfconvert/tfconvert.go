// Package tfconvert converts BigQuery ML's imported TensorFlow SavedModels
// (MODEL_TYPE='TENSORFLOW', #1775) to TensorFlow Lite, on the host.
//
// The BigQuery front is a CGO-less static binary in the cluster and cannot
// run TensorFlow, so the CLI serves this converter on --port-tfconvert,
// published to the cluster through cloudburrow-host like the ONNX runner.
// The front sends the SavedModel's files as a tar; the converter writes
// them to a temporary directory and runs the converter image
// (deploy/tfconvert: Google's tf.lite.TFLiteConverter.from_saved_model on
// PyPI's tensorflow) with `docker run --network none`, and answers the
// TensorFlow Lite flatbuffer, which internal/tflitemodel runs. The CLI
// only shells out to docker: it stays static.
package tfconvert

import (
	"encoding/json"
	"fmt"
)

// Refusal is why a SavedModel did not convert, as the converter script
// classifies it.
type Refusal struct {
	// Kind is "format" (not a SavedModel), "signature" (no
	// serving_default), "flex" (needs TensorFlow ops TensorFlow Lite's
	// builtins lack), "convert" (any other converter failure), "limit"
	// (too large) or "unavailable" (no docker, or the image would not
	// build).
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return r.Message }

// Unsupported reports whether the refusal is CloudBurrow's, not the
// model's: BigQuery, which runs TensorFlow itself, would import it.
func (r *Refusal) Unsupported() bool {
	return r.Kind == "flex" || r.Kind == "unavailable"
}

// File is one file of a SavedModel, its path relative to the SavedModel's
// directory (saved_model.pb, variables/variables.index, ...).
type File struct {
	Name string
	Data []byte
}

// MaxModelBytes is BigQuery's "Models are limited to 450 MB in size"
// (bigqueryml-syntax-create-tensorflow).
const MaxModelBytes = 450 << 20

func decodeRefusal(b []byte) *Refusal {
	var r Refusal
	if err := json.Unmarshal(b, &r); err != nil || r.Kind == "" {
		return &Refusal{Kind: "convert", Message: fmt.Sprintf("the converter answered %q", string(b))}
	}
	return &r
}
