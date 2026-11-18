package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strconv"
	"strings"

	"golang.org/x/image/bmp"
)

// BigQuery ML's image functions (#1798), as these reference pages
// document them (read 2026-11-18, under
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/):
//
//   - ML.DECODE_IMAGE(data) (bigqueryml-syntax-decode-image): data is a
//     BYTES value of a JPEG, PNG or BMP image, or an object table's data
//     pseudocolumn, of "10 MB or less (10,000,000 bytes)"; the result is
//     a STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>>, the first array the image's
//     height, width and channels, the second its pixels flattened (H × W ×
//     C values in [0, 1)); a struct over 60 MB "is downscaled to that
//     size while keeping its aspect ratio".
//   - ML.RESIZE_IMAGE(image, target_height, target_width,
//     preserve_aspect_ratio) (bigqueryml-syntax-resize-image): bilinear;
//     with preserve_aspect_ratio the largest image within the targets of
//     the same ratio; the result has the input's form (FLOAT64 or INT64).
//   - ML.CONVERT_COLOR_SPACE(image, target_color_space)
//     (bigqueryml-syntax-convert-color-space): an RGB image to HSV,
//     GRAYSCALE, YIQ or YUV, a STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>>.
//   - ML.CONVERT_IMAGE_TYPE(image) (bigqueryml-syntax-convert-image-type):
//     a FLOAT64 image's values to INT64 in [0, 255).
//   - Every image argument: STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>> (values
//     in [0, 1)) or STRUCT<ARRAY<INT64>, ARRAY<INT64>> (in [0, 255)),
//     three dimensions, H × W × C values, 60 MB or smaller.
//
// The pages name no formula; the examples feed TensorFlow models, and
// BigQuery's image preprocessing is TensorFlow's, so these are
// TensorFlow's, checked against TensorFlow 2.20's own output
// (internal/tflitemodel/testdata/gen_image.py, tensorflow_image.json):
// decode is tf.io.decode_image then tf.image.convert_image_dtype (a pixel
// over its type's maximum, so 255 is 1.0), resize tf.image.resize
// (bilinear, half-pixel centres; the preserved ratio's size rounded), the
// colour spaces tf.image.rgb_to_hsv / rgb_to_grayscale / rgb_to_yiq /
// rgb_to_yuv, and the INT64 form tf.image.convert_image_dtype to uint8
// (value × 255.5, truncated).
//
// JPEG: Go's image/jpeg runs libjpeg's accurate integer IDCT, and the
// chroma is upsampled and converted as libjpeg does (fancyRGB), so its
// pixels are tf.io.decode_jpeg(dct_method='INTEGER_ACCURATE')'s to within
// two levels of 255; tf.io.decode_image's default fast IDCT differs by up
// to six.
//
// UNVERIFIED (the pages say nothing of them): which IDCT BigQuery runs; the channels decode gives
// (the file's own, as tf.io.decode_image: 1 for grey, 3 for colour, 4
// with alpha); the INT64 form of a resized INT64 image (rounded); an
// INT64 image's colour conversion (its values over 255 first); and every
// error's wording.

// maxDecodeImageBytes is ML.DECODE_IMAGE's "10 MB or less (10,000,000
// bytes)"; maxImageStructBytes the 60 MB of an image STRUCT, at 8 bytes a
// value.
const (
	maxDecodeImageBytes = 10_000_000
	maxImageStructBytes = 60_000_000
)

// mlImage is an image STRUCT: h × w × c values, row-major, channels last.
type mlImage struct {
	h, w, c int
	v       []float64
	ints    bool // the INT64 form, values in [0, 255]
}

func (m *mlImage) bytes() int { return 8 * (3 + len(m.v)) }

// decodeImage is ML.DECODE_IMAGE of b; what names b in an error.
func decodeImage(b []byte, what string) (*mlImage, string) {
	if len(b) > maxDecodeImageBytes {
		return nil, fmt.Sprintf("ML.DECODE_IMAGE: %s is %d bytes; the image must be 10 MB or less (10,000,000 bytes)", what, len(b))
	}
	var img image.Image
	var err error
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		img, err = png.Decode(bytes.NewReader(b))
	case bytes.HasPrefix(b, []byte{0xff, 0xd8}):
		img, err = jpeg.Decode(bytes.NewReader(b))
	case bytes.HasPrefix(b, []byte("BM")):
		img, err = bmp.Decode(bytes.NewReader(b))
	default:
		return nil, "ML.DECODE_IMAGE: " + what + " is not an image in a supported format: the supported formats are JPEG, PNG and BMP"
	}
	if err != nil {
		return nil, "ML.DECODE_IMAGE: " + what + " could not be decoded as an image: " + err.Error()
	}
	bd := img.Bounds()
	m := &mlImage{h: bd.Dy(), w: bd.Dx(), c: imageChannels(img)}
	if m.h == 0 || m.w == 0 {
		return nil, "ML.DECODE_IMAGE: " + what + " is an empty image"
	}
	m.v = make([]float64, 0, m.h*m.w*m.c)
	if yc, ok := img.(*image.YCbCr); ok && (yc.SubsampleRatio == image.YCbCrSubsampleRatio420 || yc.SubsampleRatio == image.YCbCrSubsampleRatio422) {
		for _, p := range fancyRGB(yc) {
			m.v = append(m.v, float64(p)/255)
		}
		img = nil
	}
	for y := bd.Min.Y; img != nil && y < bd.Max.Y; y++ {
		for x := bd.Min.X; x < bd.Max.X; x++ {
			switch m.c {
			case 1:
				g := color.Gray16Model.Convert(img.At(x, y)).(color.Gray16)
				m.v = append(m.v, float64(g.Y)/65535)
			default:
				n := color.NRGBA64Model.Convert(img.At(x, y)).(color.NRGBA64)
				m.v = append(m.v, float64(n.R)/65535, float64(n.G)/65535, float64(n.B)/65535)
				if m.c == 4 {
					m.v = append(m.v, float64(n.A)/65535)
				}
			}
		}
	}
	if m.bytes() > maxImageStructBytes {
		s := math.Sqrt(float64(maxImageStructBytes-24) / float64(8*len(m.v)))
		m = resizeImage(m, max(1, int(float64(m.h)*s)), max(1, int(float64(m.w)*s)))
	}
	return m, ""
}

// fancyRGB is a subsampled JPEG's RGB pixels as libjpeg (which
// TensorFlow's decode_jpeg runs) gives them: chroma upsampled by its
// "fancy" triangle filter (jdsample.c, h2v1 and h2v2) and converted with
// its fixed-point tables (jdcolor.c), where Go's image/jpeg replicates
// chroma samples. Each pixel's R, G, B.
func fancyRGB(yc *image.YCbCr) []uint8 {
	bd := yc.Bounds()
	w, h := bd.Dx(), bd.Dy()
	v2 := yc.SubsampleRatio == image.YCbCrSubsampleRatio420
	cw := (w + 1) / 2
	ch := h
	if v2 {
		ch = (h + 1) / 2
	}
	up := func(plane []uint8) []int {
		at := func(y, x int) int { return int(plane[min(max(y, 0), ch-1)*yc.CStride+x]) }
		out := make([]int, w*h)
		for y := range h {
			cy := y
			var near int
			if v2 {
				cy = y / 2
				near = cy - 1
				if y%2 == 1 {
					near = cy + 1
				}
			}
			col := func(x int) int {
				if v2 {
					return at(cy, x)*3 + at(near, x)
				}
				return at(cy, x)
			}
			// h2v2 sums are 4x a sample (shift 4); h2v1 1x (shift 2).
			const mul = 4
			shift, bias0, bias1 := 4, 8, 7
			if !v2 {
				shift, bias0, bias1 = 2, 1, 2
			}
			for cx := range cw {
				this := col(cx)
				last, next := this, this
				if cx > 0 {
					last = col(cx - 1)
				}
				if cx+1 < cw {
					next = col(cx + 1)
				}
				var a, b int
				switch {
				case cw == 1:
					a, b = (this*mul+bias0)>>shift, (this*mul+bias1)>>shift
				case cx == 0:
					a, b = (this*mul+bias0)>>shift, (this*3+next+bias1)>>shift
				case cx == cw-1:
					a, b = (this*3+last+bias0)>>shift, (this*mul+bias1)>>shift
				default:
					a, b = (this*3+last+bias0)>>shift, (this*3+next+bias1)>>shift
				}
				if 2*cx < w {
					out[y*w+2*cx] = a
				}
				if 2*cx+1 < w {
					out[y*w+2*cx+1] = b
				}
			}
		}
		return out
	}
	cb, cr := up(yc.Cb), up(yc.Cr)
	const one = 1 << 16
	fix := func(x float64) int { return int(x*one + 0.5) }
	clamp := func(v int) uint8 { return uint8(min(max(v, 0), 255)) }
	out := make([]uint8, 0, 3*w*h)
	for y := range h {
		for x := range w {
			yy := int(yc.Y[y*yc.YStride+x])
			b, r := cb[y*w+x]-128, cr[y*w+x]-128
			out = append(out,
				clamp(yy+(fix(1.40200)*r+one/2)>>16),
				clamp(yy+((-fix(0.34414)*b+-fix(0.71414)*r+one/2)>>16)),
				clamp(yy+(fix(1.77200)*b+one/2)>>16))
		}
	}
	return out
}

// imageChannels is the file's channels, as tf.io.decode_image gives them.
func imageChannels(img image.Image) int {
	switch p := img.(type) {
	case *image.Gray, *image.Gray16:
		return 1
	case *image.NRGBA, *image.NRGBA64:
		return 4
	case *image.Paletted:
		for _, c := range p.Palette {
			if _, _, _, a := c.RGBA(); a != 0xffff {
				return 4
			}
		}
	}
	return 3
}

// resizeImage is tf.image.resize's bilinear (half-pixel centres) to h × w.
func resizeImage(m *mlImage, h, w int) *mlImage {
	out := &mlImage{h: h, w: w, c: m.c, ints: m.ints, v: make([]float64, h*w*m.c)}
	weights := func(in, outN int) (lo, hi []int, frac []float64) {
		scale := float64(in) / float64(outN)
		for i := range outN {
			p := (float64(i)+0.5)*scale - 0.5
			f := math.Floor(p)
			l, u := max(int(f), 0), min(int(math.Ceil(p)), in-1)
			lo, hi, frac = append(lo, l), append(hi, u), append(frac, p-f)
		}
		return
	}
	ylo, yhi, yf := weights(m.h, h)
	xlo, xhi, xf := weights(m.w, w)
	at := func(y, x, c int) float64 { return m.v[(y*m.w+x)*m.c+c] }
	for y := range h {
		for x := range w {
			for c := range m.c {
				tl, tr := at(ylo[y], xlo[x], c), at(ylo[y], xhi[x], c)
				bl, br := at(yhi[y], xlo[x], c), at(yhi[y], xhi[x], c)
				top := tl + (tr-tl)*xf[x]
				bot := bl + (br-bl)*xf[x]
				v := top + (bot-top)*yf[y]
				if m.ints {
					v = math.Round(v)
				}
				out.v[(y*w+x)*m.c+c] = v
			}
		}
	}
	return out
}

// resizeTo is ML.RESIZE_IMAGE's size: the target, or with preserve the
// largest of the same ratio within it (tf.image.resize's rounding).
func resizeTo(m *mlImage, th, tw int, preserve bool) (int, int) {
	if !preserve {
		return th, tw
	}
	s := min(float64(th)/float64(m.h), float64(tw)/float64(m.w))
	return max(1, int(math.Round(float32Round(s*float64(m.h))))), max(1, int(math.Round(float32Round(s*float64(m.w)))))
}

// float32Round is v as TensorFlow's float32 arithmetic holds it.
func float32Round(v float64) float64 { return float64(float32(v)) }

// colorKernels are TensorFlow's RGB to YIQ and YUV matrices (pixel ×
// kernel).
var colorKernels = map[string][3][3]float64{
	"YIQ": {{0.299, 0.59590059, 0.2115}, {0.587, -0.27455667, -0.52273617}, {0.114, -0.32134392, 0.31119955}},
	"YUV": {{0.299, -0.14714119, 0.61497538}, {0.587, -0.28886916, -0.51496512}, {0.114, 0.43601035, -0.10001026}},
}

// convertColorSpace is ML.CONVERT_COLOR_SPACE.
func convertColorSpace(m *mlImage, space string) (*mlImage, string) {
	space = strings.ToUpper(space)
	if m.c != 3 {
		return nil, fmt.Sprintf("ML.CONVERT_COLOR_SPACE: the image must be an RGB image, of 3 channels, not %d", m.c)
	}
	px := func(i int) (float64, float64, float64) {
		r, g, b := m.v[i], m.v[i+1], m.v[i+2]
		if m.ints {
			r, g, b = r/255, g/255, b/255
		}
		return r, g, b
	}
	out := &mlImage{h: m.h, w: m.w, c: 3}
	switch space {
	case "GRAYSCALE":
		out.c = 1
		for i := 0; i < len(m.v); i += 3 {
			r, g, b := px(i)
			out.v = append(out.v, 0.2989*r+0.5870*g+0.1140*b)
		}
	case "YIQ", "YUV":
		k := colorKernels[space]
		for i := 0; i < len(m.v); i += 3 {
			r, g, b := px(i)
			for j := range 3 {
				out.v = append(out.v, r*k[0][j]+g*k[1][j]+b*k[2][j])
			}
		}
	case "HSV":
		for i := 0; i < len(m.v); i += 3 {
			r, g, b := px(i)
			v := max(r, g, b)
			rng := v - min(r, g, b)
			s := 0.0
			if v > 0 {
				s = rng / v
			}
			h := 0.0
			if rng > 0 {
				n := 1 / (6 * rng)
				switch v {
				case r:
					h = n * (g - b)
				case g:
					h = n*(b-r) + 2.0/6
				default:
					h = n*(r-g) + 4.0/6
				}
				if h < 0 {
					h++
				}
			}
			out.v = append(out.v, h, s, v)
		}
	default:
		return nil, "ML.CONVERT_COLOR_SPACE: unsupported target color space " + strconv.Quote(space) + ": the supported color spaces are HSV, GRAYSCALE, YIQ and YUV"
	}
	return out, ""
}

// convertImageType is ML.CONVERT_IMAGE_TYPE.
func convertImageType(m *mlImage) (*mlImage, string) {
	if m.ints {
		return nil, "ML.CONVERT_IMAGE_TYPE: the image must be a STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>>, of values in [0, 1)"
	}
	out := &mlImage{h: m.h, w: m.w, c: m.c, ints: true, v: make([]float64, len(m.v))}
	for i, x := range m.v {
		out.v[i] = math.Trunc(min(max(float64(float32(x)*255.5), 0), 255))
	}
	return out, ""
}

// imageFromJSON reads an image STRUCT as TO_JSON_STRING wrote it: an
// object of two arrays, in order (its fields may have no names). ints is
// the struct's second array's type; fn names the function in an error.
func imageFromJSON(raw json.RawMessage, ints bool, fn string) (*mlImage, string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	bad := fn + ": the image must be a STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>> or STRUCT<ARRAY<INT64>, ARRAY<INT64>>: its height, width and channels, and its H × W × C values"
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if tk, err := dec.Token(); err != nil || tk != json.Delim('{') {
		return nil, bad
	}
	var arrays [][]json.Number
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return nil, bad
		}
		var a []json.Number
		if err := dec.Decode(&a); err != nil {
			return nil, bad
		}
		arrays = append(arrays, a)
	}
	if len(arrays) != 2 || len(arrays[0]) != 3 {
		return nil, bad
	}
	var dims [3]int
	for i, n := range arrays[0] {
		d, err := n.Int64()
		if err != nil || d <= 0 {
			return nil, bad
		}
		dims[i] = int(d)
	}
	m := &mlImage{h: dims[0], w: dims[1], c: dims[2], ints: ints}
	if m.h*m.w*m.c != len(arrays[1]) {
		return nil, fmt.Sprintf("%s: the image's dimensions are %d × %d × %d, which is %d values, but it has %d", fn, m.h, m.w, m.c, m.h*m.w*m.c, len(arrays[1]))
	}
	if m.bytes() > maxImageStructBytes {
		return nil, fn + ": the image STRUCT must be 60 MB or smaller"
	}
	lim := 1.0
	if ints {
		lim = 255
	}
	m.v = make([]float64, len(arrays[1]))
	for i, n := range arrays[1] {
		v, err := n.Float64()
		if err != nil || v < 0 || v > lim {
			return nil, fmt.Sprintf("%s: the image's values must be in [0, 1) for FLOAT64 and [0, 255) for INT64; it has %s", fn, n)
		}
		m.v[i] = v
	}
	return m, ""
}

// literal is m as a SQL STRUCT<ARRAY<INT64>, ARRAY<FLOAT64 | INT64>>.
func (m *mlImage) literal() string {
	var b strings.Builder
	b.WriteString("STRUCT(ARRAY<INT64>[" + strconv.Itoa(m.h) + ", " + strconv.Itoa(m.w) + ", " + strconv.Itoa(m.c) + "], ARRAY<")
	if m.ints {
		b.WriteString("INT64>[")
	} else {
		b.WriteString("FLOAT64>[")
	}
	for i, v := range m.v {
		if i > 0 {
			b.WriteString(", ")
		}
		if m.ints {
			b.WriteString(strconv.FormatInt(int64(v), 10))
		} else {
			b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	b.WriteString("])")
	return b.String()
}
