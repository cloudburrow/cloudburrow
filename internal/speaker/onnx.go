//go:build onnx

package speaker

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	ort "github.com/yalue/onnxruntime_go"
)

// Available reports whether this binary runs the speaker model.
const Available = true

// Dimensions is CAM++'s embedding size (its metadata's output_dim).
const Dimensions = 192

var (
	sessMu   sync.Mutex
	sess     *ort.DynamicAdvancedSession
	sessPath string
)

// session is the loaded model, loaded the first time from the model
// source; one window batch runs at a time.
func session(ctx context.Context) (*ort.DynamicAdvancedSession, error) {
	path, err := modelPath(ctx)
	if err != nil {
		return nil, fmt.Errorf("speaker model: %w", err)
	}
	if sess != nil && sessPath == path {
		return sess, nil
	}
	if err := onnxmodel.InitRuntime(); err != nil {
		return nil, err
	}
	s, err := ort.NewDynamicAdvancedSession(path, []string{"x"}, []string{"embedding"}, nil)
	if err != nil {
		return nil, fmt.Errorf("load the speaker model: %w", err)
	}
	if sess != nil {
		_ = sess.Destroy()
	}
	sess, sessPath = s, path
	return sess, nil
}

// embedWindows runs CAM++ on windows of features of the same length.
func embedWindows(ctx context.Context, feats [][][]float32) ([][]float32, error) {
	if len(feats) == 0 {
		return nil, nil
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	s, err := session(ctx)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(feats))
	// Windows of one length run as one batch; the last, shorter window of
	// short audio runs on its own.
	for i := 0; i < len(feats); {
		j := i + 1
		for j < len(feats) && len(feats[j]) == len(feats[i]) {
			j++
		}
		v, err := run(s, feats[i:j])
		if err != nil {
			return nil, err
		}
		out = append(out, v...)
		i = j
	}
	return out, nil
}

func run(s *ort.DynamicAdvancedSession, feats [][][]float32) ([][]float32, error) {
	n, t := int64(len(feats)), int64(len(feats[0]))
	data := make([]float32, 0, n*t*NumMelBins)
	for _, f := range feats {
		for _, row := range f {
			data = append(data, row...)
		}
	}
	in, err := ort.NewTensor(ort.NewShape(n, t, NumMelBins), data)
	if err != nil {
		return nil, err
	}
	defer in.Destroy()
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(n, Dimensions))
	if err != nil {
		return nil, err
	}
	defer outT.Destroy()
	if err := s.Run([]ort.Value{in}, []ort.Value{outT}); err != nil {
		return nil, fmt.Errorf("run the speaker model: %w", err)
	}
	flat := outT.GetData()
	res := make([][]float32, n)
	for k := range res {
		res[k] = append([]float32(nil), flat[k*Dimensions:(k+1)*Dimensions]...)
	}
	return res, nil
}
