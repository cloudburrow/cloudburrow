//go:build !onnx

package embedding

// Open reports that the runtime is not compiled into this binary.
func Open(modelDir, modelID string) (Embedder, error) { return nil, ErrNotCompiled }

// OpenCLIP reports that the runtime is not compiled into this binary.
func OpenCLIP(modelDir, modelID string) (MultimodalEmbedder, error) { return nil, ErrNotCompiled }
