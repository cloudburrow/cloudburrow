// Package imagelabel labels an image with Google's MediaPipe
// EfficientNet-Lite0 image classifier (#1799), run locally in pure Go by
// internal/tflitemodel, for Vision's LABEL_DETECTION.
//
// The model is MediaPipe's float32 efficientnet_lite0.tflite (Apache-2.0,
// https://ai.google.dev/edge/mediapipe/solutions/vision/image_classifier),
// fetched once from storage.googleapis.com into <state-dir>/models and
// checked against the SHA-256 pinned in internal/localai
// (localai.EfficientNetLite0). Its TFLite metadata, read 2026-11-18, gives
// what this package does with it:
//
//   - the input is one 224×224 RGB image, each channel normalised as
//     (v - 127) / 128 (NormalizationOptions mean 127, std 128: "min is
//     [-0.9921875] and max is [1.0]");
//   - the output is the softmax probability of each of 1,000 ImageNet
//     classes, in the order of labels_without_background.txt, which the
//     model file carries as a zip archive appended to it.
//
// UNVERIFIED (2026-11-18): Vision's own preprocessing is not documented;
// here the whole image is resized to 224×224 bilinearly (pixel centres
// aligned, aspect ratio not kept, as MediaPipe's classifier does with no
// region of interest) and its alpha dropped. The result is deterministic:
// the same pixels always give the same scores.
package imagelabel

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// Size is the model's input width and height.
const Size = 224

// labelsFile is the name of the label list inside the model file.
const labelsFile = "labels_without_background.txt"

// Label is one class and its probability.
type Label struct {
	Description string
	Score       float32
}

// Classifier is a loaded model and its labels.
type Classifier struct {
	model  *tflitemodel.Model
	labels []string
}

// Load reads a MediaPipe image classifier .tflite file: the model and the
// label list its metadata carries.
func Load(data []byte) (*Classifier, error) {
	m, err := tflitemodel.Load(data)
	if err != nil {
		return nil, err
	}
	if len(m.Inputs) != 1 || len(m.Outputs) != 1 {
		return nil, fmt.Errorf("the image classifier has %d inputs and %d outputs, not one of each", len(m.Inputs), len(m.Outputs))
	}
	if s := m.Inputs[0].Shape; len(s) != 4 || s[1] != Size || s[2] != Size || s[3] != 3 {
		return nil, fmt.Errorf("the image classifier takes %v, not [1 %d %d 3]", s, Size, Size)
	}
	labels, err := readLabels(data)
	if err != nil {
		return nil, err
	}
	if s := m.Outputs[0].Shape; len(s) != 2 || s[1] != len(labels) {
		return nil, fmt.Errorf("the image classifier answers %v, for %d labels", s, len(labels))
	}
	return &Classifier{model: m, labels: labels}, nil
}

// readLabels reads the label list from the zip archive appended to the
// model file, one label a line.
func readLabels(data []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("the image classifier carries no label list: %w", err)
	}
	f, err := zr.Open(labelsFile)
	if err != nil {
		return nil, fmt.Errorf("the image classifier carries no %s: %w", labelsFile, err)
	}
	defer f.Close()
	var labels []string
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			labels = append(labels, l)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(labels) == 0 {
		return nil, errors.New("the image classifier's label list is empty")
	}
	return labels, nil
}

// Labels is how many classes the model knows.
func (c *Classifier) Labels() int { return len(c.labels) }

// Classify answers every class's probability for img, most probable
// first; equal scores keep the model's class order.
func (c *Classifier) Classify(img image.Image) ([]Label, error) {
	in := tflitemodel.Tensor{Type: tflitemodel.Float32, Shape: []int{1, Size, Size, 3}, F: Input(img)}
	out, err := c.model.Run([]tflitemodel.Tensor{in})
	if err != nil {
		return nil, err
	}
	probs := out[0].F
	if len(probs) != len(c.labels) {
		return nil, fmt.Errorf("the image classifier answered %d scores for %d labels", len(probs), len(c.labels))
	}
	res := make([]Label, len(probs))
	for i, p := range probs {
		res[i] = Label{Description: c.labels[i], Score: p}
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Score > res[j].Score })
	return res, nil
}

// Input is img resized to Size×Size bilinearly and normalised as the
// model's metadata says, row-major RGB.
func Input(img image.Image) []float32 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	px := func(x, y int) (r, g, bl float64) {
		cr, cg, cb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
		return float64(cr >> 8), float64(cg >> 8), float64(cb >> 8)
	}
	out := make([]float32, 0, Size*Size*3)
	for oy := 0; oy < Size; oy++ {
		sy := (float64(oy)+0.5)*float64(h)/Size - 0.5
		y0, fy := splitCoord(sy, h)
		y1 := min(y0+1, h-1)
		for ox := 0; ox < Size; ox++ {
			sx := (float64(ox)+0.5)*float64(w)/Size - 0.5
			x0, fx := splitCoord(sx, w)
			x1 := min(x0+1, w-1)
			r00, g00, b00 := px(x0, y0)
			r10, g10, b10 := px(x1, y0)
			r01, g01, b01 := px(x0, y1)
			r11, g11, b11 := px(x1, y1)
			lerp := func(a, b, c, d float64) float32 {
				v := (a*(1-fx)+b*fx)*(1-fy) + (c*(1-fx)+d*fx)*fy
				return float32((v - 127) / 128)
			}
			out = append(out, lerp(r00, r10, r01, r11), lerp(g00, g10, g01, g11), lerp(b00, b10, b01, b11))
		}
	}
	return out
}

// splitCoord is a source coordinate's whole pixel, clamped to [0, n-1],
// and its fraction toward the next.
func splitCoord(s float64, n int) (int, float64) {
	if s <= 0 {
		return 0, 0
	}
	i := int(s)
	if i >= n-1 {
		return n - 1, 0
	}
	return i, s - float64(i)
}

// Source returns the path of the model file, fetching it first when it is
// missing: cmd/cloudburrow sets it to localai's pinned fetch.
type Source func(ctx context.Context) (string, error)

var (
	mu      sync.Mutex
	source  Source
	loaded  *Classifier
	loadErr error
)

// SetSource sets where the model file comes from, and forgets a model
// loaded from the previous one.
func SetSource(s Source) {
	mu.Lock()
	defer mu.Unlock()
	source, loaded, loadErr = s, nil, nil
}

// ErrNoModel means no source is set: this process has no label model.
var ErrNoModel = errors.New("no image label model is configured")

// ErrFetch means the model file could not be fetched.
var ErrFetch = errors.New("the image label model could not be fetched")

// Default is the classifier of the configured source, loaded the first
// time it is asked for. A failed fetch is retried the next time; a model
// that does not load is remembered.
func Default(ctx context.Context) (*Classifier, error) {
	mu.Lock()
	defer mu.Unlock()
	if loaded != nil || loadErr != nil {
		return loaded, loadErr
	}
	if source == nil {
		return nil, ErrNoModel
	}
	path, err := source(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetch, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	loaded, loadErr = Load(data)
	return loaded, loadErr
}
