// Package imagelandmark recognises the landmark in a photograph with
// Google's on-device landmark classifiers (#1799), run locally in pure Go
// by internal/tflitemodel, for Vision's LANDMARK_DETECTION.
//
// The models are Google's landmarks_classifier_<continent>_V1 (Apache-2.0,
// https://www.kaggle.com/models/google/landmarks, read 2026-11-18), one a
// continent (Africa, Asia, Europe, North America, Oceania and Antarctica,
// South America), MobileNetV3 models trained on the Google Landmarks
// Dataset v2, fetched once into <state-dir>/models and checked against the
// SHA-256 pinned in internal/localai (localai.LandmarkClassifiers). Their
// TFLite metadata, read 2026-11-18, gives what this package does with
// them:
//
//   - the input is one 321×321 RGB image, uint8, "each value in
//     [0.0,255.0]";
//   - the output is a "probability" a class (19,292 to 99,423 of them),
//     whose English names are probability-labels-en.txt, one a line in
//     output order, in the zip archive appended to the model file (a
//     landmark has several classes, one a cluster of its photographs);
//   - the output's ScoreThresholdingOptions global_score_threshold is 0.7.
//
// Recognize runs all six, keeps each landmark name's best score, drops the
// names below the models' 0.7 threshold, and answers the rest, most
// probable first.
//
// UNVERIFIED (docs re-read 2026-11-18, still unanswered:
// https://docs.cloud.google.com/vision/docs/detecting-landmarks): Vision's
// own preprocessing and calibration are not documented; here the whole
// image is resized bilinearly to 321×321 (aspect ratio not kept, as a
// TFLite Task image classifier does with no region of interest) and its
// alpha dropped. The classifiers answer a landmark for any photograph:
// they were trained on landmark photographs only and score a scanned
// document as high as a landmark (measured 2026-11-18: 0.95 for a museum
// in South America), so an image with no landmark is not answered "none"
// as Vision would. The result is deterministic.
package imagelandmark

import (
	"archive/zip"
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

	"github.com/cloudburrow/cloudburrow/internal/imagelabel"
	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// Size is the models' input width and height.
const Size = 321

// Threshold is the models' own score threshold (their metadata's
// global_score_threshold): a landmark scored below it is not answered.
const Threshold = 0.7

// labelsFile is the name of the English label list inside a model file.
const labelsFile = "probability-labels-en.txt"

// Landmark is one landmark's name and its best score.
type Landmark struct {
	Description string
	Score       float32
}

// Classifier is one loaded model and its labels.
type Classifier struct {
	model  *tflitemodel.Model
	labels []string
}

// Load reads one landmark classifier .tflite file: the model and the
// English label list it carries.
func Load(data []byte) (*Classifier, error) {
	m, err := tflitemodel.Load(data)
	if err != nil {
		return nil, err
	}
	if len(m.Inputs) != 1 || len(m.Outputs) != 1 {
		return nil, fmt.Errorf("the landmark classifier has %d inputs and %d outputs, not one of each", len(m.Inputs), len(m.Outputs))
	}
	if s := m.Inputs[0].Shape; len(s) != 4 || s[1] != Size || s[2] != Size || s[3] != 3 || m.Inputs[0].Type != tflitemodel.Uint8 {
		return nil, fmt.Errorf("the landmark classifier takes %v %v, not UINT8 [1 %d %d 3]", m.Inputs[0].Type, s, Size, Size)
	}
	labels, err := readLabels(data)
	if err != nil {
		return nil, err
	}
	if s := m.Outputs[0].Shape; len(s) != 2 || s[1] != len(labels) {
		return nil, fmt.Errorf("the landmark classifier answers %v, for %d labels", s, len(labels))
	}
	return &Classifier{model: m, labels: labels}, nil
}

// readLabels reads the English label list from the zip archive appended
// to the model file: one label a line, in output order.
func readLabels(data []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("the landmark classifier carries no label list: %w", err)
	}
	f, err := zr.Open(labelsFile)
	if err != nil {
		return nil, fmt.Errorf("the landmark classifier carries no %s: %w", labelsFile, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 32<<20))
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if text == "" {
		return nil, errors.New("the landmark classifier's label list is empty")
	}
	return strings.Split(text, "\n"), nil
}

// Labels is how many classes the model knows.
func (c *Classifier) Labels() int { return len(c.labels) }

// Scores answers each landmark name's best score for img.
func (c *Classifier) Scores(img image.Image) (map[string]float32, error) {
	rgb := imagelabel.ResizeRGB(img, Size, Size)
	in := tflitemodel.Tensor{Type: tflitemodel.Uint8, Shape: []int{1, Size, Size, 3}, I: make([]int64, len(rgb))}
	for i, v := range rgb {
		in.I[i] = int64(min(255, max(0, v+0.5)))
	}
	out, err := c.model.Run([]tflitemodel.Tensor{in})
	if err != nil {
		return nil, err
	}
	probs := out[0].F
	if len(probs) != len(c.labels) {
		return nil, fmt.Errorf("the landmark classifier answered %d scores for %d labels", len(probs), len(c.labels))
	}
	best := map[string]float32{}
	for i, p := range probs {
		if l := c.labels[i]; l != "" {
			if s, ok := best[l]; !ok || p > s {
				best[l] = p
			}
		}
	}
	return best, nil
}

// Recognize answers the landmarks the classifiers find in img: each
// name's best score over every model, those from Threshold, most probable
// first (equal scores by name).
func Recognize(cs []*Classifier, img image.Image) ([]Landmark, error) {
	scores := make([]map[string]float32, len(cs))
	errs := make([]error, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scores[i], errs[i] = c.Scores(img)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return merge(scores), nil
}

// merge is each name's best score over every map, from Threshold, most
// probable first.
func merge(scores []map[string]float32) []Landmark {
	best := map[string]float32{}
	for _, m := range scores {
		for l, s := range m {
			if s < Threshold {
				continue
			}
			if b, ok := best[l]; !ok || s > b {
				best[l] = s
			}
		}
	}
	out := make([]Landmark, 0, len(best))
	for l, s := range best {
		out = append(out, Landmark{Description: l, Score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Description < out[j].Description
	})
	return out
}

// Source returns the paths of the model files, one a continent, fetching
// them first when they are missing: cmd/cloudburrow sets it to localai's
// pinned fetch.
type Source func(ctx context.Context) ([]string, error)

var (
	mu      sync.Mutex
	source  Source
	loaded  []*Classifier
	loadErr error
)

// SetSource sets where the model files come from, and forgets models
// loaded from the previous one.
func SetSource(s Source) {
	mu.Lock()
	defer mu.Unlock()
	source, loaded, loadErr = s, nil, nil
}

// ErrNoModel means no source is set: this process has no landmark models.
var ErrNoModel = errors.New("no landmark model is configured")

// ErrFetch means a model file could not be fetched.
var ErrFetch = errors.New("the landmark models could not be fetched")

// Default is the classifiers of the configured source, loaded the first
// time they are asked for. A failed fetch is retried the next time; a
// model that does not load is remembered.
func Default(ctx context.Context) ([]*Classifier, error) {
	mu.Lock()
	defer mu.Unlock()
	if loaded != nil || loadErr != nil {
		return loaded, loadErr
	}
	if source == nil {
		return nil, ErrNoModel
	}
	paths, err := source(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetch, err)
	}
	cs := make([]*Classifier, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		c, err := Load(data)
		if err != nil {
			loadErr = err
			return nil, err
		}
		cs = append(cs, c)
	}
	loaded = cs
	return loaded, nil
}
