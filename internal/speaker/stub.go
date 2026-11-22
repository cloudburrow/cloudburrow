//go:build !onnx

package speaker

import "context"

// Available reports whether this binary runs the speaker model.
const Available = false

func embedWindows(ctx context.Context, feats [][][]float32) ([][]float32, error) {
	return nil, ErrNotCompiled
}
