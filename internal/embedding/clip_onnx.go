//go:build onnx

package embedding

import (
	"context"
	"fmt"
	"image"
	"path/filepath"
	"sync"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// OpenCLIP loads the pinned CLIP conversion from modelDir: its text and
// vision encoders, each a separate ONNX graph, and the text tokenizer.
func OpenCLIP(modelDir, modelID string) (MultimodalEmbedder, error) {
	if err := initORT(); err != nil {
		return nil, err
	}
	tok, err := tokenizers.FromFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	textPath := filepath.Join(modelDir, "onnx", "text_model_quantized.onnx")
	// The text graph's inputs are read rather than assumed: an attention
	// mask is fed only when the graph takes one.
	ins, _, err := ort.GetInputOutputInfo(textPath)
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("read text model inputs: %w", err)
	}
	names := []string{}
	mask := false
	for _, in := range ins {
		switch in.Name {
		case "input_ids":
			names = append(names, in.Name)
		case "attention_mask":
			names, mask = append(names, in.Name), true
		}
	}
	text, err := ort.NewDynamicAdvancedSession(textPath, names, []string{"text_embeds"}, nil)
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("load text model: %w", err)
	}
	vision, err := ort.NewDynamicAdvancedSession(filepath.Join(modelDir, "onnx", "vision_model_quantized.onnx"),
		[]string{"pixel_values"}, []string{"image_embeds"}, nil)
	if err != nil {
		tok.Close()
		_ = text.Destroy()
		return nil, fmt.Errorf("load vision model: %w", err)
	}
	return &clipEmbedder{id: modelID, tok: tok, text: text, vision: vision, mask: mask}, nil
}

type clipEmbedder struct {
	id     string
	mu     sync.Mutex
	tok    *tokenizers.Tokenizer
	text   *ort.DynamicAdvancedSession
	vision *ort.DynamicAdvancedSession
	mask   bool
	closed bool
}

func (e *clipEmbedder) Model() string { return e.id }

func (e *clipEmbedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.tok.Close()
	err := e.text.Destroy()
	if verr := e.vision.Destroy(); err == nil {
		err = verr
	}
	return err
}

func (e *clipEmbedder) EmbedText(ctx context.Context, text string, dim int) (Output, error) {
	if err := ValidateMultimodalDim(dim); err != nil {
		return Output{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Output{}, fmt.Errorf("%s was unloaded", e.id)
	}
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	raw, _ := e.tok.Encode(text, true)
	ids, truncated := CLIPTokens(raw)
	n := int64(len(ids))
	idData := make([]int64, n)
	maskData := make([]int64, n)
	for i, id := range ids {
		idData[i], maskData[i] = int64(id), 1
	}
	idT, err := ort.NewTensor(ort.NewShape(1, n), idData)
	if err != nil {
		return Output{}, err
	}
	defer idT.Destroy()
	inputs := []ort.Value{idT}
	if e.mask {
		maskT, err := ort.NewTensor(ort.NewShape(1, n), maskData)
		if err != nil {
			return Output{}, err
		}
		defer maskT.Destroy()
		inputs = append(inputs, maskT)
	}
	v, err := runEmbeds(e.text, inputs)
	if err != nil {
		return Output{}, fmt.Errorf("text encoder: %w", err)
	}
	return Output{Values: Truncate(Normalize(v), dim), TokenCount: len(ids), Truncated: truncated}, nil
}

func (e *clipEmbedder) EmbedImage(ctx context.Context, img image.Image, dim int) (Output, error) {
	if err := ValidateMultimodalDim(dim); err != nil {
		return Output{}, err
	}
	pixels := PreprocessCLIP(img)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Output{}, fmt.Errorf("%s was unloaded", e.id)
	}
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	pt, err := ort.NewTensor(ort.NewShape(1, 3, CLIPImageSize, CLIPImageSize), pixels)
	if err != nil {
		return Output{}, err
	}
	defer pt.Destroy()
	v, err := runEmbeds(e.vision, []ort.Value{pt})
	if err != nil {
		return Output{}, fmt.Errorf("vision encoder: %w", err)
	}
	return Output{Values: Truncate(Normalize(v), dim)}, nil
}

func runEmbeds(s *ort.DynamicAdvancedSession, in []ort.Value) ([]float32, error) {
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, CLIPDimensions))
	if err != nil {
		return nil, err
	}
	defer out.Destroy()
	if err := s.Run(in, []ort.Value{out}); err != nil {
		return nil, err
	}
	return append([]float32(nil), out.GetData()...), nil
}
