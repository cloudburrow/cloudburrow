package embedding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF decoder DecodeImage uses
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

// The local multimodal embedding model (#1454): the community ONNX
// conversion of OpenAI's CLIP ViT-B/32 (Xenova/clip-vit-base-patch32), pinned
// in internal/localai. Its text and image encoders project into one 512-wide
// space, so a caption and the picture it describes land close together.
//
// Like EmbeddingGemma, the runtime is compiled only with the onnx build tag
// (clip_onnx.go); the default build's OpenCLIP returns ErrNotCompiled.
// Preprocessing is here, untagged, so it is tested in every build.

// CLIPDimensions is the model's native, projected output size.
const CLIPDimensions = 512

// CLIPMaxTokens is the text encoder's context, BOS and EOS included. Longer
// text is cut to it, keeping the EOS the encoder pools at, as Vertex's
// multimodal model shortens text past its own limit.
const CLIPMaxTokens = 77

// CLIPEOS is the end-of-text token id; the text encoder pools at it.
const CLIPEOS = 49407

// CLIPImageSize is the side of the square the vision encoder takes.
const CLIPImageSize = 224

// ValidMultimodalDimensions are the sizes served: multimodalembedding's
// documented 128, 256 and 512. Its fourth, 1408, is wider than this model's
// native 512 and is refused rather than padded.
var ValidMultimodalDimensions = []int{128, 256, 512}

// MaxImageBytes bounds an image to decode.
const MaxImageBytes = 20 << 20

// ErrUnsupportedImage means the bytes are not an image the decoder knows.
var ErrUnsupportedImage = errors.New("unsupported image")

// MultimodalEmbedder embeds text and images into one vector space.
type MultimodalEmbedder interface {
	// Model is the identity reported to clients.
	Model() string
	// EmbedText embeds one text; dim is 0 for the native size.
	EmbedText(ctx context.Context, text string, dim int) (Output, error)
	// EmbedImage embeds one decoded image.
	EmbedImage(ctx context.Context, img image.Image, dim int) (Output, error)
	Close() error
}

// ValidateMultimodalDim checks a requested size; 0 is the native size.
func ValidateMultimodalDim(dim int) error {
	if dim == 0 {
		return nil
	}
	for _, d := range ValidMultimodalDimensions {
		if d == dim {
			return nil
		}
	}
	return fmt.Errorf("dimension %d is not supported; use one of %v", dim, ValidMultimodalDimensions)
}

// DecodeImage decodes PNG, JPEG or GIF bytes. mimeType, when set, must agree
// with what the bytes are.
func DecodeImage(data []byte, mimeType string) (image.Image, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: the image is empty", ErrUnsupportedImage)
	}
	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("%w: the image is %d bytes, over the %d-byte limit", ErrUnsupportedImage, len(data), MaxImageBytes)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v; PNG, JPEG and GIF are decoded", ErrUnsupportedImage, err)
	}
	if mt := strings.ToLower(strings.TrimSpace(mimeType)); mt != "" {
		want := "image/" + format
		if mt == "image/jpg" {
			mt = "image/jpeg"
		}
		if mt != want {
			return nil, fmt.Errorf("%w: mimeType %q, but the bytes are %s", ErrUnsupportedImage, mimeType, want)
		}
	}
	return img, nil
}

// CLIP's normalisation, from the pinned preprocessor_config.json.
var (
	clipMean = [3]float64{0.48145466, 0.4578275, 0.40821073}
	clipStd  = [3]float64{0.26862954, 0.26130258, 0.27577711}
)

// PreprocessCLIP is CLIPImageProcessor: convert to RGB, resize the shortest
// edge to 224 with bicubic resampling (antialiased when shrinking, as PIL
// does), centre-crop 224x224, rescale to [0,1] and normalise. The result is
// pixel_values in CHW order, 3*224*224 values.
func PreprocessCLIP(img image.Image) []float32 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := CLIPImageSize, CLIPImageSize
	if w < h {
		nh = int(math.Round(float64(h) * CLIPImageSize / float64(w)))
	} else {
		nw = int(math.Round(float64(w) * CLIPImageSize / float64(h)))
	}
	// RGB planes in [0,255].
	src := make([][3]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			src[y*w+x] = [3]float64{float64(r >> 8), float64(g >> 8), float64(bl >> 8)}
		}
	}
	tmp := resample(src, w, h, nw, true)   // horizontal: nw x h
	dst := resample(tmp, nw, h, nh, false) // vertical: nw x nh
	left, top := (nw-CLIPImageSize)/2, (nh-CLIPImageSize)/2
	out := make([]float32, 3*CLIPImageSize*CLIPImageSize)
	plane := CLIPImageSize * CLIPImageSize
	for y := 0; y < CLIPImageSize; y++ {
		for x := 0; x < CLIPImageSize; x++ {
			p := dst[(y+top)*nw+x+left]
			for c := 0; c < 3; c++ {
				v := math.Round(math.Max(0, math.Min(255, p[c]))) / 255
				out[c*plane+y*CLIPImageSize+x] = float32((v - clipMean[c]) / clipStd[c])
			}
		}
	}
	return out
}

// bicubic is PIL's cubic kernel, a = -0.5.
func bicubic(x float64) float64 {
	const a = -0.5
	x = math.Abs(x)
	switch {
	case x < 1:
		return ((a+2)*x-(a+3))*x*x + 1
	case x < 2:
		return (((x-5)*x+8)*x - 4) * a
	}
	return 0
}

// resample resizes one axis of a w x h image to n, PIL's way: the kernel's
// support widens by the scale when shrinking, so the result is antialiased.
func resample(src [][3]float64, w, h, n int, horizontal bool) [][3]float64 {
	in := w
	if !horizontal {
		in = h
	}
	scale := float64(in) / float64(n)
	fs := math.Max(scale, 1)
	support := 2 * fs
	ow, oh := n, h
	if !horizontal {
		ow, oh = w, n
	}
	out := make([][3]float64, ow*oh)
	for i := 0; i < n; i++ {
		center := (float64(i) + 0.5) * scale
		lo := int(math.Max(0, math.Floor(center-support+0.5)))
		hi := int(math.Min(float64(in), math.Floor(center+support+0.5)))
		ws := make([]float64, hi-lo)
		var sum float64
		for j := lo; j < hi; j++ {
			ws[j-lo] = bicubic((float64(j) - center + 0.5) / fs)
			sum += ws[j-lo]
		}
		if sum != 0 {
			for k := range ws {
				ws[k] /= sum
			}
		}
		if horizontal {
			for y := 0; y < h; y++ {
				var acc [3]float64
				for j := lo; j < hi; j++ {
					p := src[y*w+j]
					for c := 0; c < 3; c++ {
						acc[c] += p[c] * ws[j-lo]
					}
				}
				out[y*ow+i] = acc
			}
		} else {
			for x := 0; x < w; x++ {
				var acc [3]float64
				for j := lo; j < hi; j++ {
					p := src[j*w+x]
					for c := 0; c < 3; c++ {
						acc[c] += p[c] * ws[j-lo]
					}
				}
				out[i*ow+x] = acc
			}
		}
	}
	return out
}

// CLIPTokens fits a tokenised text to the encoder's context: at most
// CLIPMaxTokens, ending with EOS, and reports whether it was cut.
func CLIPTokens(ids []uint32) ([]uint32, bool) {
	if len(ids) <= CLIPMaxTokens {
		return ids, false
	}
	out := append([]uint32(nil), ids[:CLIPMaxTokens]...)
	out[CLIPMaxTokens-1] = CLIPEOS
	return out, true
}

// Normalize scales v to unit length; CLIP's projections are not normalised.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := math.Sqrt(sum)
	if n == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}
