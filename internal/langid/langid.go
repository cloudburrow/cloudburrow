// Package langid identifies the language of a text with MediaPipe's
// Language Detector (Google Research's, Apache-2.0; 110 languages, the
// label set of CLD3), run in pure Go on internal/tflitemodel with ports of
// its two custom operators (ops.go). BigQuery's ML.TRANSLATE serves its
// DETECT_LANGUAGE mode, and TRANSLATE_TEXT's detected_language_code, with
// it (#1799); Google's Translation API's own detector is not published.
package langid

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"sync"

	langidmodel "github.com/cloudburrow/cloudburrow/third_party/mediapipe-langid"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// Prediction is one language and its probability.
type Prediction struct {
	Code        string
	Probability float64
}

var (
	once   sync.Once
	model  *tflitemodel.Model
	labels []string
	errMod error
	mu     sync.Mutex
)

func load() {
	model, errMod = tflitemodel.LoadWith(langidmodel.Model, map[string]tflitemodel.CustomOp{
		"NGramHash": ngramHash, "KmeansEmbeddingLookup": kmeansEmbeddingLookup})
	if errMod != nil {
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(langidmodel.Model), int64(len(langidmodel.Model)))
	if err != nil {
		errMod = err
		return
	}
	for _, f := range zr.File {
		if f.Name != "labels.txt" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			errMod = err
			return
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			errMod = err
			return
		}
		labels = strings.Fields(string(b))
	}
	if len(labels) == 0 {
		errMod = errors.New("the language detector has no labels")
	}
}

// Detect gives the text's languages, most probable first, as MediaPipe's
// LanguageDetector does: the model's scores, "unknown" left out. Empty
// text has no prediction.
func Detect(text string) ([]Prediction, error) {
	once.Do(load)
	if errMod != nil {
		return nil, errMod
	}
	if text == "" {
		return nil, nil
	}
	mu.Lock()
	out, err := model.Run([]tflitemodel.Tensor{{Type: tflitemodel.String, Shape: []int{1}, S: []string{text}}})
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(out) != 1 || len(out[0].F) != len(labels) {
		return nil, errors.New("the language detector gave an unexpected output")
	}
	var ps []Prediction
	for i, v := range out[0].F {
		if labels[i] == "unknown" || math.IsNaN(float64(v)) {
			continue
		}
		ps = append(ps, Prediction{Code: labels[i], Probability: float64(v)})
	}
	sort.SliceStable(ps, func(a, b int) bool { return ps[a].Probability > ps[b].Probability })
	return ps, nil
}
