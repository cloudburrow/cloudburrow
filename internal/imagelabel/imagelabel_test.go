package imagelabel

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"reflect"
	"testing"

	_ "golang.org/x/image/webp"
)

// EnvModel names MediaPipe's efficientnet_lite0.tflite (float32, version
// 1, the file localai.EfficientNetLite0 pins) for the tests that run it.
const EnvModel = "CLOUDBURROW_TEST_LABEL_MODEL"

// TestInput: a flat image resizes to flat values, normalised as the
// model's metadata says, (v - 127) / 128.
func TestInput(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 50, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 50; x++ {
			img.Set(x, y, color.RGBA{255, 127, 0, 255})
		}
	}
	in := Input(img)
	if len(in) != Size*Size*3 {
		t.Fatalf("%d values", len(in))
	}
	for i := 0; i < len(in); i += 3 {
		if in[i] != 1 || in[i+1] != 0 || in[i+2] != -127.0/128 {
			t.Fatalf("pixel %d: %v", i/3, in[i:i+3])
		}
	}
}

// TestReadLabels reads the label list from a zip archive appended to the
// model, as MediaPipe's metadata writer packs it.
func TestReadLabels(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("TFL3 model bytes")
	zw := zip.NewWriter(&buf)
	zw.SetOffset(int64(buf.Len()))
	w, _ := zw.Create(labelsFile)
	_, _ = w.Write([]byte("tench\ngoldfish\n\ngreat white shark\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readLabels(buf.Bytes())
	if err != nil || !reflect.DeepEqual(got, []string{"tench", "goldfish", "great white shark"}) {
		t.Fatalf("labels %q, %v", got, err)
	}
	if _, err := readLabels([]byte("TFL3 no archive")); err == nil {
		t.Error("a model with no label list loaded")
	}
}

// TestDefaultWithoutSource: no source is ErrNoModel; a failed fetch is
// ErrFetch, and retried.
func TestDefaultWithoutSource(t *testing.T) {
	t.Cleanup(func() { SetSource(nil) })
	SetSource(nil)
	if _, err := Default(context.Background()); !errors.Is(err, ErrNoModel) {
		t.Fatalf("no source: %v", err)
	}
	calls := 0
	SetSource(func(context.Context) (string, error) { calls++; return "", errors.New("offline") })
	for range 2 {
		if _, err := Default(context.Background()); !errors.Is(err, ErrFetch) {
			t.Fatalf("failed fetch: %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("a failed fetch was tried %d times, not retried", calls)
	}
}

func loadModel(t *testing.T) *Classifier {
	t.Helper()
	path := os.Getenv(EnvModel)
	if path == "" {
		t.Skip("set " + EnvModel + " to MediaPipe's efficientnet_lite0.tflite to run the label model")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func decode(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// checkerboard is a 224×224 board of 8×8 black and white squares.
func checkerboard() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			v := uint8(0)
			if (x/28+y/28)%2 == 0 {
				v = 255
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

// TestClassifyFixedImages runs the model on fixed images whose top label
// was measured on 2026-11-18 (MediaPipe's own burger.jpg, not kept in the
// repo, was labelled cheeseburger at 0.861 by the same code): every score
// in [0, 1], summing to one, most probable first, and the same answer
// twice.
func TestClassifyFixedImages(t *testing.T) {
	c := loadModel(t)
	if c.Labels() != 1000 {
		t.Fatalf("%d labels", c.Labels())
	}
	for _, tc := range []struct {
		name string
		img  image.Image
		top  string
	}{
		{"scanned invoice", decode(t, "../ocr/testdata/scan.png"), "web site"},
		{"lossy WebP scene", decode(t, "../../test/compat/testdata/scene-lossy.webp"), "volcano"},
		{"checkerboard", checkerboard(), "crossword puzzle"},
	} {
		got, err := c.Classify(tc.img)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 1000 || got[0].Description != tc.top {
			t.Errorf("%s: top labels %v, want %q first", tc.name, got[:3], tc.top)
		}
		var sum float64
		for i, l := range got {
			if l.Score < 0 || l.Score > 1 || (i > 0 && l.Score > got[i-1].Score) {
				t.Fatalf("%s: label %d %+v", tc.name, i, l)
			}
			sum += float64(l.Score)
		}
		if sum < 0.99 || sum > 1.01 {
			t.Errorf("%s: scores sum to %v", tc.name, sum)
		}
		again, _ := c.Classify(tc.img)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("%s: two runs differ", tc.name)
		}
	}
}
