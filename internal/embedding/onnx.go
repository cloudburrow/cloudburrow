//go:build onnx

package embedding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

var envOnce sync.Once
var envErr error

// Open loads the pinned model from modelDir, the revision directory that
// localai.Cache.PinnedDir names. ONNXRUNTIME_LIB points at libonnxruntime.
func Open(modelDir, modelID string) (Embedder, error) {
	envOnce.Do(func() {
		if lib := os.Getenv("ONNXRUNTIME_LIB"); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		envErr = ort.InitializeEnvironment()
	})
	if envErr != nil {
		return nil, fmt.Errorf("initialise ONNX Runtime (set ONNXRUNTIME_LIB to libonnxruntime): %w", envErr)
	}
	tok, err := tokenizers.FromFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	// model_quantized.onnx references model_quantized.onnx_data beside it;
	// ONNX Runtime resolves it relative to the model file.
	sess, err := ort.NewDynamicAdvancedSession(filepath.Join(modelDir, "onnx", "model_quantized.onnx"),
		[]string{"input_ids", "attention_mask"}, []string{"sentence_embedding"}, nil)
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("load model: %w", err)
	}
	return &onnxEmbedder{id: modelID, tok: tok, sess: sess}, nil
}

type onnxEmbedder struct {
	id   string
	mu   sync.Mutex
	tok  *tokenizers.Tokenizer
	sess *ort.DynamicAdvancedSession
}

func (e *onnxEmbedder) Model() string { return e.id }

func (e *onnxEmbedder) Close() error {
	e.tok.Close()
	return e.sess.Destroy()
}

// Embed runs one input at a time, so no padding is involved and the attention
// mask is all ones.
func (e *onnxEmbedder) Embed(ctx context.Context, in []Input, dim int, autoTruncate bool) ([]Output, error) {
	if err := ValidateDim(dim); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Output, 0, len(in))
	for i, x := range in {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, err := Prompt(x)
		if err != nil {
			return nil, fmt.Errorf("instance %d: %w", i, err)
		}
		ids, _ := e.tok.Encode(text, true)
		truncated := false
		if len(ids) > MaxTokens {
			if !autoTruncate {
				return nil, fmt.Errorf("instance %d: %d tokens exceeds %d and autoTruncate is false", i, len(ids), MaxTokens)
			}
			ids, truncated = ids[:MaxTokens], true
		}
		v, err := e.run(ids)
		if err != nil {
			return nil, fmt.Errorf("instance %d: %w", i, err)
		}
		out = append(out, Output{Values: Truncate(v, dim), TokenCount: len(ids), Truncated: truncated})
	}
	return out, nil
}

func (e *onnxEmbedder) run(ids []uint32) ([]float32, error) {
	n := int64(len(ids))
	idData := make([]int64, n)
	mask := make([]int64, n)
	for i, id := range ids {
		idData[i], mask[i] = int64(id), 1
	}
	idT, err := ort.NewTensor(ort.NewShape(1, n), idData)
	if err != nil {
		return nil, err
	}
	defer idT.Destroy()
	maskT, err := ort.NewTensor(ort.NewShape(1, n), mask)
	if err != nil {
		return nil, err
	}
	defer maskT.Destroy()
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(1, Dimensions))
	if err != nil {
		return nil, err
	}
	defer outT.Destroy()
	if err := e.sess.Run([]ort.Value{idT, maskT}, []ort.Value{outT}); err != nil {
		return nil, err
	}
	return append([]float32(nil), outT.GetData()...), nil
}
