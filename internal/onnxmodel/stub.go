//go:build !onnx

package onnxmodel

// Available reports whether this binary runs ONNX models.
const Available = false

// Inspect reports that the runtime is not compiled into this binary.
func Inspect(model []byte) (inputs, outputs []Port, err error) { return nil, nil, ErrNotCompiled }

// Run reports that the runtime is not compiled into this binary.
func Run(model []byte, inputs []Port, in []Tensor, outputs []Port) ([]Tensor, error) {
	return nil, ErrNotCompiled
}
