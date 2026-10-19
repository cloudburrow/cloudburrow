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

// EmbeddingGemma 2 (#1454): onnx-community's int8 conversion of
// google/embeddinggemma-2, its text model and its vision encoder. The text
// model takes the token IDs, with one image token per soft token, and the
// soft tokens of every image, video and audio clip (empty for the absent
// ones), and returns the mean-pooled, normalised sentence_embedding, as
// Transformers.js's EmbeddingGemma2Model runs it. Audio and video are not
// served: their encoders and decoders are not loaded.

// visionEncoderFile marks a model directory as EmbeddingGemma 2's.
const visionEncoderFile = "vision_encoder_quantized.onnx"

// hiddenSize is the width of the soft tokens the text model consumes.
const hiddenSize = 512

func isEmbeddingGemma2(modelDir string) bool {
	_, err := os.Stat(filepath.Join(modelDir, "onnx", visionEncoderFile))
	return err == nil
}

func openEmbeddingGemma2(modelDir, modelID string) (Embedder, error) {
	tok, err := tokenizers.FromFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	text, err := ort.NewDynamicAdvancedSession(filepath.Join(modelDir, "onnx", "model_quantized.onnx"),
		[]string{"input_ids", "attention_mask", "image_features", "video_features", "audio_features"},
		[]string{"sentence_embedding"}, nil)
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("load text model: %w", err)
	}
	vision, err := ort.NewDynamicAdvancedSession(filepath.Join(modelDir, "onnx", visionEncoderFile),
		[]string{"pixel_values", "pixel_position_ids"}, []string{"image_features"}, nil)
	if err != nil {
		tok.Close()
		_ = text.Destroy()
		return nil, fmt.Errorf("load vision encoder: %w", err)
	}
	e := &gemma2Embedder{id: modelID, tok: tok, text: text, vision: vision}
	for name, s := range map[string]*uint32{BeginImageToken: &e.boi, ImageToken: &e.img, EndImageToken: &e.eoi} {
		ids, _ := tok.Encode(name, false)
		if len(ids) != 1 {
			_ = e.Close()
			return nil, fmt.Errorf("tokenizer: %q is %d tokens, want one", name, len(ids))
		}
		*s = ids[0]
	}
	return e, nil
}

type gemma2Embedder struct {
	id            string
	mu            sync.Mutex
	tok           *tokenizers.Tokenizer
	text, vision  *ort.DynamicAdvancedSession
	boi, img, eoi uint32
	closed        bool
}

func (e *gemma2Embedder) Model() string      { return e.id }
func (e *gemma2Embedder) EmbedsImages() bool { return true }

func (e *gemma2Embedder) Close() error {
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

func (e *gemma2Embedder) Embed(ctx context.Context, in []Input, dim int, autoTruncate bool) ([]Output, error) {
	if err := ValidateDim(dim); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("%s was unloaded", e.id)
	}
	out := make([]Output, 0, len(in))
	for i, x := range in {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o, err := e.embedOne(x, autoTruncate)
		if err != nil {
			return nil, fmt.Errorf("instance %d: %w", i, err)
		}
		o.Values = Truncate(o.Values, dim)
		out = append(out, o)
	}
	return out, nil
}

func (e *gemma2Embedder) embedOne(x Input, autoTruncate bool) (Output, error) {
	if len(x.Parts) == 0 {
		x.Parts = []Part{{Text: x.Content}}
	}
	text, _, err := MultimodalPrompt(x)
	if err != nil {
		return Output{}, err
	}
	var patches []ImagePatches
	for _, p := range x.Parts {
		if p.Image == nil {
			continue
		}
		ip, err := PatchImage(p.Image)
		if err != nil {
			return Output{}, err
		}
		patches = append(patches, ip)
	}
	ids, _ := e.tok.Encode(text, true)
	// Expand each placeholder to its image's soft tokens, in order.
	expanded := make([]uint32, 0, len(ids))
	k := 0
	for _, id := range ids {
		if id != e.img {
			expanded = append(expanded, id)
			continue
		}
		if k >= len(patches) {
			return Output{}, fmt.Errorf("the text has more image placeholders than images")
		}
		expanded = append(expanded, e.boi)
		for j := 0; j < patches[k].SoftTokens; j++ {
			expanded = append(expanded, e.img)
		}
		expanded = append(expanded, e.eoi)
		k++
	}
	if k != len(patches) {
		return Output{}, fmt.Errorf("%d images but %d placeholders", len(patches), k)
	}
	truncated := false
	if len(expanded) > MaxTokens2 {
		if len(patches) > 0 || !autoTruncate {
			return Output{}, fmt.Errorf("%d tokens exceeds %d", len(expanded), MaxTokens2)
		}
		expanded, truncated = expanded[:MaxTokens2], true
	}

	features := make([]float32, 0)
	for _, ip := range patches {
		f, err := e.encodeImage(ip)
		if err != nil {
			return Output{}, fmt.Errorf("vision encoder: %w", err)
		}
		features = append(features, f...)
	}
	v, err := e.run(expanded, features)
	if err != nil {
		return Output{}, err
	}
	return Output{Values: v, TokenCount: len(expanded), Truncated: truncated}, nil
}

// encodeImage runs the vision encoder on one image, padded to the processor's
// maximum number of patches with zero pixels and -1 positions, as the
// processor pads it.
func (e *gemma2Embedder) encodeImage(ip ImagePatches) ([]float32, error) {
	maxPatches := imageSoftTokens * imagePoolingKernel * imagePoolingKernel
	dim := imagePatchSize * imagePatchSize * 3
	pix := make([]float32, maxPatches*dim)
	copy(pix, ip.Pixels)
	pos := make([]int64, maxPatches*2)
	copy(pos, ip.Positions)
	for i := len(ip.Positions); i < len(pos); i++ {
		pos[i] = -1
	}
	pixT, err := ort.NewTensor(ort.NewShape(1, int64(maxPatches), int64(dim)), pix)
	if err != nil {
		return nil, err
	}
	defer pixT.Destroy()
	posT, err := ort.NewTensor(ort.NewShape(1, int64(maxPatches), 2), pos)
	if err != nil {
		return nil, err
	}
	defer posT.Destroy()
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(ip.SoftTokens), hiddenSize))
	if err != nil {
		return nil, err
	}
	defer outT.Destroy()
	if err := e.vision.Run([]ort.Value{pixT, posT}, []ort.Value{outT}); err != nil {
		return nil, err
	}
	return append([]float32(nil), outT.GetData()...), nil
}

func (e *gemma2Embedder) run(ids []uint32, imageFeatures []float32) ([]float32, error) {
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
	var feats [3]*ort.Tensor[float32]
	for i := range feats {
		data, rows := []float32{}, int64(0)
		if i == 0 {
			data, rows = imageFeatures, int64(len(imageFeatures)/hiddenSize)
		}
		var t *ort.Tensor[float32]
		if rows == 0 {
			t, err = ort.NewEmptyTensor[float32](ort.NewShape(0, hiddenSize))
		} else {
			t, err = ort.NewTensor(ort.NewShape(rows, hiddenSize), data)
		}
		if err != nil {
			return nil, err
		}
		defer t.Destroy()
		feats[i] = t
	}
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(1, Dimensions))
	if err != nil {
		return nil, err
	}
	defer outT.Destroy()
	if err := e.text.Run([]ort.Value{idT, maskT, feats[0], feats[1], feats[2]}, []ort.Value{outT}); err != nil {
		return nil, err
	}
	return append([]float32(nil), outT.GetData()...), nil
}
