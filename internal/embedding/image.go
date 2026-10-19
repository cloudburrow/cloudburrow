package embedding

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
)

// Images for EmbeddingGemma 2's vision encoder (#1454), prepared as its
// processor (Gemma4ImageProcessor in Hugging Face Transformers and
// Transformers.js) prepares them: converted to RGB, resized with bicubic
// resampling, keeping the aspect ratio, to the largest size whose 16-pixel
// patches fit the soft-token budget in whole 3x3 pooling blocks, rescaled by
// 1/255 (no mean or std normalisation), and cut into patches of 16x16x3
// values in (row, column, channel) order, each with its (column, row)
// position.

// ImageMIMETypes are the image types decoded here: Go's standard library
// decoders. Google's gemini-embedding-2 also takes WebP, BMP, HEIC, HEIF and
// AVIF, which need decoders this build does not have.
var ImageMIMETypes = []string{"image/png", "image/jpeg"}

// MaxImageSide is the largest width or height accepted, Google's documented
// 16384 x 16384 for gemini-embedding-2.
const MaxImageSide = 16384

// imagePatchSize, imagePoolingKernel and imageSoftTokens are the model's
// processor_config.json values: patch_size, pooling_kernel_size and
// max_soft_tokens.
const (
	imagePatchSize     = 16
	imagePoolingKernel = 3
	imageSoftTokens    = 280
)

// DecodeImage decodes an image of one of ImageMIMETypes.
func DecodeImage(mimeType string, data []byte) (image.Image, error) {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the image data is empty")
	}
	// Check the size before decoding the pixels.
	var cfg image.Config
	var err error
	switch mt {
	case "image/png":
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case "image/jpeg", "image/jpg":
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	default:
		return nil, fmt.Errorf("mime_type %q is not supported; send one of %s", mimeType, strings.Join(ImageMIMETypes, ", "))
	}
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid %s image: %v", mt, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxImageSide || cfg.Height > MaxImageSide {
		return nil, fmt.Errorf("the image is %dx%d; each side must be 1 to %d pixels", cfg.Width, cfg.Height, MaxImageSide)
	}
	var img image.Image
	if mt == "image/png" {
		img, err = png.Decode(bytes.NewReader(data))
	} else {
		img, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid %s image: %v", mt, err)
	}
	return img, nil
}

// imageTargetSize is the processor's get_aspect_ratio_preserving_size.
func imageTargetSize(h, w, maxPatches int) (int, int, error) {
	targetPx := float64(maxPatches * imagePatchSize * imagePatchSize)
	factor := math.Sqrt(targetPx / float64(h*w))
	side := imagePoolingKernel * imagePatchSize
	th := int(math.Floor(factor*float64(h)/float64(side))) * side
	tw := int(math.Floor(factor*float64(w)/float64(side))) * side
	if th == 0 && tw == 0 {
		return 0, 0, fmt.Errorf("the image (%dx%d) is too small to resize", w, h)
	}
	maxSide := (maxPatches / (imagePoolingKernel * imagePoolingKernel)) * side
	if th == 0 {
		th = side
		tw = min((w/h)*side, maxSide)
	} else if tw == 0 {
		tw = side
		th = min((h/w)*side, maxSide)
	}
	return th, tw, nil
}

// ImagePatches are one image's vision-encoder inputs.
type ImagePatches struct {
	// Pixels is NumPatches x 768 values (16 x 16 x 3, rescaled to [0, 1]).
	Pixels []float32
	// Positions is NumPatches x 2: each patch's column, then its row.
	Positions []int64
	// NumPatches is the number of patches; SoftTokens the number of soft
	// tokens the encoder makes of them (one per 3x3 block).
	NumPatches, SoftTokens int
}

// PatchImage prepares img for the vision encoder.
func PatchImage(img image.Image) (ImagePatches, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return ImagePatches{}, fmt.Errorf("the image is empty")
	}
	maxPatches := imageSoftTokens * imagePoolingKernel * imagePoolingKernel
	th, tw, err := imageTargetSize(h, w, maxPatches)
	if err != nil {
		return ImagePatches{}, err
	}
	rgb := toRGB(img)
	if th != h || tw != w {
		rgb = resizeBicubic(rgb, w, h, tw, th)
	}
	ph, pw := th/imagePatchSize, tw/imagePatchSize
	n := ph * pw
	dim := imagePatchSize * imagePatchSize * 3
	out := ImagePatches{
		Pixels:     make([]float32, 0, n*dim),
		Positions:  make([]int64, 0, n*2),
		NumPatches: n,
		SoftTokens: n / (imagePoolingKernel * imagePoolingKernel),
	}
	const rescale = float32(1.0 / 255.0)
	for py := 0; py < ph; py++ {
		for px := 0; px < pw; px++ {
			for dy := 0; dy < imagePatchSize; dy++ {
				row := ((py*imagePatchSize+dy)*tw + px*imagePatchSize) * 3
				for i := 0; i < imagePatchSize*3; i++ {
					out.Pixels = append(out.Pixels, float32(rgb[row+i])*rescale)
				}
			}
			out.Positions = append(out.Positions, int64(px), int64(py))
		}
	}
	return out, nil
}

// toRGB is img's pixels as 8-bit RGB, row by row, dropping any alpha as
// PIL's convert("RGB") does.
func toRGB(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]uint8, 0, w*h*3)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			out = append(out, c.R, c.G, c.B)
		}
	}
	return out
}

// bicubic is PIL's bicubic filter (a = -0.5), support 2.
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

// resampleWeights are PIL's precomputed coefficients for one axis: for each
// output index, the first input index and the normalised weights, the
// filter widened by the scale when downsampling (antialiasing).
func resampleWeights(in, out int) ([]int, [][]float64) {
	scale := float64(in) / float64(out)
	filterScale := max(scale, 1)
	support := 2 * filterScale
	starts := make([]int, out)
	weights := make([][]float64, out)
	for o := 0; o < out; o++ {
		center := (float64(o) + 0.5) * scale
		lo := max(int(center-support+0.5), 0)
		hi := min(int(center+support+0.5), in)
		ws := make([]float64, hi-lo)
		var sum float64
		for i := range ws {
			ws[i] = bicubic((float64(i+lo) - center + 0.5) / filterScale)
			sum += ws[i]
		}
		if sum != 0 {
			for i := range ws {
				ws[i] /= sum
			}
		}
		starts[o], weights[o] = lo, ws
	}
	return starts, weights
}

func clip8(v float64) uint8 {
	v = math.Round(v)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// resizeBicubic resizes 8-bit RGB as PIL's Image.resize(BICUBIC) does: a
// horizontal pass, then a vertical one, each rounded to 8 bits.
func resizeBicubic(src []uint8, w, h, tw, th int) []uint8 {
	xs, xw := resampleWeights(w, tw)
	tmp := make([]uint8, tw*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < tw; x++ {
			for c := 0; c < 3; c++ {
				var v float64
				for i, wt := range xw[x] {
					v += wt * float64(src[(y*w+xs[x]+i)*3+c])
				}
				tmp[(y*tw+x)*3+c] = clip8(v)
			}
		}
	}
	ys, yw := resampleWeights(h, th)
	out := make([]uint8, tw*th*3)
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			for c := 0; c < 3; c++ {
				var v float64
				for i, wt := range yw[y] {
					v += wt * float64(tmp[((ys[y]+i)*tw+x)*3+c])
				}
				out[(y*tw+x)*3+c] = clip8(v)
			}
		}
	}
	return out
}
