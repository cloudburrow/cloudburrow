//go:build !onnx

package embedding

// Open reports that the runtime is not compiled into this binary.
func Open(modelDir, modelID string) (Embedder, error) { return nil, ErrNotCompiled }
