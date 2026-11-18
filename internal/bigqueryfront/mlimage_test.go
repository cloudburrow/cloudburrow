package bigqueryfront

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tfImage is a TensorFlow result of tensorflow_image.json (gen_image.py).
type tfImage struct {
	Shape []int     `json:"shape"`
	Data  []float64 `json:"data"`
}

type tfImageFixture struct {
	Decode      map[string]tfImage `json:"decode"`
	JPEGExact   tfImage            `json:"decode_jpeg_accurate"`
	Resize      map[string]tfImage `json:"resize"`
	Color       map[string]tfImage `json:"color"`
	ConvertType tfImage            `json:"convert_type"`
}

func readTFImageFixture(t *testing.T) (tfImageFixture, func(string) []byte) {
	t.Helper()
	dir := filepath.Join("..", "tflitemodel", "testdata")
	var fx tfImageFixture
	b, err := os.ReadFile(filepath.Join(dir, "tensorflow_image.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	return fx, func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

func sameImage(t *testing.T, what string, got *mlImage, want tfImage, tol float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: nil image", what)
	}
	if shape := []int{got.h, got.w, got.c}; len(want.Shape) != 3 || shape[0] != want.Shape[0] || shape[1] != want.Shape[1] || shape[2] != want.Shape[2] {
		t.Fatalf("%s: shape %v, TensorFlow's %v", what, shape, want.Shape)
	}
	worst := 0.0
	for i := range want.Data {
		worst = max(worst, math.Abs(got.v[i]-want.Data[i]))
	}
	if worst > tol {
		t.Errorf("%s: differs from TensorFlow's by up to %g (tolerance %g)", what, worst, tol)
	}
}

// TestImageFunctionsMatchTensorFlow checks the image functions against
// TensorFlow 2.20's own output for the same images (#1798).
func TestImageFunctionsMatchTensorFlow(t *testing.T) {
	fx, read := readTFImageFixture(t)
	dec := map[string]*mlImage{}
	for name, want := range fx.Decode {
		m, why := decodeImage(read(name), name)
		if why != "" {
			t.Fatalf("%s: %s", name, why)
		}
		tol := 1e-6
		if strings.HasSuffix(name, ".jpg") {
			// TensorFlow's default fast IDCT; the accurate one within two levels (below).
			tol = 6.0 / 255
			sameImage(t, "decode "+name+" (accurate IDCT)", m, fx.JPEGExact, 2.0/255+1e-6)
		}
		sameImage(t, "decode "+name, m, want, tol)
		dec[name] = m
	}
	big, small := dec["image_big.png"], dec["image_small.png"]
	resize := func(m *mlImage, h, w int, p bool) *mlImage {
		h, w = resizeTo(m, h, w, p)
		return resizeImage(m, h, w)
	}
	sameImage(t, "resize big 6x5", resize(big, 6, 5, false), fx.Resize["big_6x5"], 1e-5)
	sameImage(t, "resize big 4x4 preserve", resize(big, 4, 4, true), fx.Resize["big_4x4_preserve"], 1e-5)
	sameImage(t, "resize small 9x7", resize(small, 9, 7, false), fx.Resize["small_9x7"], 1e-5)
	for space, want := range fx.Color {
		got, why := convertColorSpace(small, space)
		if why != "" {
			t.Fatal(why)
		}
		sameImage(t, "color "+space, got, want, 1e-5)
	}
	got, why := convertImageType(small)
	if why != "" {
		t.Fatal(why)
	}
	sameImage(t, "convert type", got, fx.ConvertType, 0)

	// The documented example: 20 x 200 into 10 x 120, keeping the ratio, is 10 x 100.
	if h, w := resizeTo(&mlImage{h: 20, w: 200, c: 3}, 10, 120, true); h != 10 || w != 100 {
		t.Errorf("preserve 20x200 into 10x120 = %dx%d, want 10x100", h, w)
	}
}

func TestDecodeImageRefusals(t *testing.T) {
	if _, why := decodeImage([]byte("hello"), "gs://b/a.txt"); !strings.Contains(why, "JPEG, PNG and BMP") {
		t.Errorf("text: %q", why)
	}
	if _, why := decodeImage(make([]byte, maxDecodeImageBytes+1), "gs://b/big"); !strings.Contains(why, "10,000,000 bytes") {
		t.Errorf("oversize: %q", why)
	}
	if _, why := decodeImage([]byte("\x89PNG\r\n\x1a\nbroken"), "gs://b/x.png"); !strings.Contains(why, "could not be decoded") {
		t.Errorf("broken png: %q", why)
	}
	if _, why := imageFromJSON(json.RawMessage(`{"":[1,2,3],"":[0.5]}`), false, "ML.RESIZE_IMAGE"); !strings.Contains(why, "6 values") {
		t.Errorf("short data: %q", why)
	}
	if _, why := imageFromJSON(json.RawMessage(`{"":[1,1,1],"":[2.5]}`), false, "ML.RESIZE_IMAGE"); !strings.Contains(why, "[0, 1)") {
		t.Errorf("range: %q", why)
	}
	m, why := imageFromJSON(json.RawMessage(`{"":[1,2,1],"":[0.25,1]}`), false, "ML.RESIZE_IMAGE")
	if why != "" || m.h != 1 || m.w != 2 || m.v[1] != 1 {
		t.Errorf("good: %+v %q", m, why)
	}
	if _, why := convertImageType(&mlImage{h: 1, w: 1, c: 1, ints: true, v: []float64{3}}); why == "" {
		t.Error("convert type of an INT64 image")
	}
	if _, why := convertColorSpace(&mlImage{h: 1, w: 1, c: 1, v: []float64{0.5}}, "HSV"); !strings.Contains(why, "RGB") {
		t.Errorf("grey to HSV: %q", why)
	}
	if _, why := convertColorSpace(&mlImage{h: 1, w: 1, c: 3, v: []float64{0.5, 0.5, 0.5}}, "LAB"); !strings.Contains(why, "HSV, GRAYSCALE, YIQ and YUV") {
		t.Errorf("LAB: %q", why)
	}
}
