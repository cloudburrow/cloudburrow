// Package imagedetect finds objects and faces in an image with Google's
// MediaPipe detectors (#1799), run locally in pure Go by
// internal/tflitemodel, for Vision's OBJECT_LOCALIZATION and
// FACE_DETECTION:
//
//   - objects with MediaPipe's EfficientDet-Lite0 object detector
//     (float32, Apache-2.0,
//     https://ai.google.dev/edge/mediapipe/solutions/vision/object_detector),
//     whose 90 output classes are COCO's category IDs, 80 of them named
//     (the other ten are "???" in its label file and are never answered);
//   - faces with MediaPipe's BlazeFace short-range face detector
//     (float16, Apache-2.0,
//     https://ai.google.dev/edge/mediapipe/solutions/vision/face_detector),
//     a box and six keypoints per face.
//
// Each model is fetched once from storage.googleapis.com into
// <state-dir>/models and checked against the SHA-256 pinned in
// internal/localai. The models give raw box regressions and scores; the
// decoding that MediaPipe's Tasks run after them is done here as
// MediaPipe's own calculators do it (read 2026-11-18 from
// github.com/google-ai-edge/mediapipe): SsdAnchorsCalculator's anchors
// (BlazeFace's options from face_detector_graph.cc; EfficientDet's fixed
// anchors from its DETECTOR_METADATA), TensorsToDetectionsCalculator's
// box decoding and NonMaxSuppressionCalculator's suppression.
package imagedetect

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"sort"
	"sync"
)

// Box is a detection's box, normalised to the image: 0 is its left or
// top edge and 1 its right or bottom edge.
type Box struct{ XMin, YMin, XMax, YMax float32 }

// Point is a normalised keypoint.
type Point struct{ X, Y float32 }

// detection is one decoded box, before suppression.
type detection struct {
	box       Box
	score     float32
	class     int
	keypoints []Point
}

// iou is the boxes' intersection over union, NonMaxSuppressionCalculator's
// INTERSECTION_OVER_UNION: 0 when they do not intersect.
func iou(a, b Box) float32 {
	ix := min(a.XMax, b.XMax) - max(a.XMin, b.XMin)
	iy := min(a.YMax, b.YMax) - max(a.YMin, b.YMin)
	if ix <= 0 || iy <= 0 {
		return 0
	}
	inter := ix * iy
	union := (a.XMax-a.XMin)*(a.YMax-a.YMin) + (b.XMax-b.XMin)*(b.YMax-b.YMin) - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// byScore sorts ds most probable first; equal scores keep their order.
func byScore(ds []detection) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].score > ds[j].score })
}

// nms is NonMaxSuppressionCalculator's DEFAULT algorithm: most probable
// first, a detection is kept unless it overlaps a kept one by more than
// threshold, until max are kept (max <= 0: no limit).
func nms(ds []detection, threshold float32, max int) []detection {
	byScore(ds)
	var out []detection
	for _, d := range ds {
		suppressed := false
		for _, k := range out {
			if iou(d.box, k.box) > threshold {
				suppressed = true
				break
			}
		}
		if !suppressed {
			out = append(out, d)
			if max > 0 && len(out) >= max {
				break
			}
		}
	}
	return out
}

// weightedNMS is NonMaxSuppressionCalculator's WEIGHTED algorithm: the
// most probable remaining detection and every remaining one that overlaps
// it by more than threshold are merged into one, their box and keypoints
// averaged by score, with the most probable one's score.
func weightedNMS(ds []detection, threshold float32) []detection {
	byScore(ds)
	var out []detection
	remain := ds
	for len(remain) > 0 {
		first := remain[0]
		var rest, cand []detection
		for _, d := range remain {
			if iou(d.box, first.box) > threshold {
				cand = append(cand, d)
			} else {
				rest = append(rest, d)
			}
		}
		merged := first
		if len(cand) > 0 {
			var total, x0, y0, x1, y1 float32
			kp := make([]Point, len(first.keypoints))
			for _, c := range cand {
				total += c.score
				x0 += c.box.XMin * c.score
				y0 += c.box.YMin * c.score
				x1 += c.box.XMax * c.score
				y1 += c.box.YMax * c.score
				for i := range kp {
					if i < len(c.keypoints) {
						kp[i].X += c.keypoints[i].X * c.score
						kp[i].Y += c.keypoints[i].Y * c.score
					}
				}
			}
			merged.box = Box{x0 / total, y0 / total, x1 / total, y1 / total}
			for i := range kp {
				kp[i].X /= total
				kp[i].Y /= total
			}
			merged.keypoints = kp
		}
		out = append(out, merged)
		if len(rest) == len(remain) {
			break
		}
		remain = rest
	}
	return out
}

// sigmoid is the logistic function.
func sigmoid(v float32) float32 { return float32(1 / (1 + math.Exp(-float64(v)))) }

// imageInput is img as a size×size RGB tensor, each channel normalised as
// (v - 127.5) / 127.5 (both models' NormalizationOptions), sampled
// bilinearly with pixel centres aligned. With letterbox the image keeps
// its aspect ratio, centred in a square whose padding is pixel value 0
// (ImageToTensorCalculator's keep_aspect_ratio with BORDER_ZERO, as
// MediaPipe's face detector runs it); without, it is stretched to the
// square (the object detector). It answers the padding, as fractions of
// the square, that unletter maps back.
func imageInput(img image.Image, size int, letterbox bool) (in []float32, padX, padY float32) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	sw, sh := float64(w), float64(h)
	var offX, offY float64
	if letterbox {
		s := math.Max(sw, sh)
		offX, offY = (s-sw)/2, (s-sh)/2
		sw, sh = s, s
		padX, padY = float32(offX/s), float32(offY/s)
	}
	px := func(x, y int) (r, g, bl float64) {
		if x < 0 || y < 0 || x >= w || y >= h {
			return 0, 0, 0
		}
		cr, cg, cb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
		return float64(cr >> 8), float64(cg >> 8), float64(cb >> 8)
	}
	in = make([]float32, 0, size*size*3)
	for oy := range size {
		fy := (float64(oy)+0.5)*sh/float64(size) - 0.5 - offY
		y0 := int(math.Floor(fy))
		ty := fy - float64(y0)
		if !letterbox {
			y0, ty = clampCoord(fy, h)
		}
		for ox := range size {
			fx := (float64(ox)+0.5)*sw/float64(size) - 0.5 - offX
			x0 := int(math.Floor(fx))
			tx := fx - float64(x0)
			if !letterbox {
				x0, tx = clampCoord(fx, w)
			}
			x1, y1 := x0+1, y0+1
			if !letterbox {
				x1, y1 = min(x1, w-1), min(y1, h-1)
			}
			r00, g00, b00 := px(x0, y0)
			r10, g10, b10 := px(x1, y0)
			r01, g01, b01 := px(x0, y1)
			r11, g11, b11 := px(x1, y1)
			lerp := func(a, b, c, d float64) float32 {
				v := (a*(1-tx)+b*tx)*(1-ty) + (c*(1-tx)+d*tx)*ty
				return float32((v - 127.5) / 127.5)
			}
			in = append(in, lerp(r00, r10, r01, r11), lerp(g00, g10, g01, g11), lerp(b00, b10, b01, b11))
		}
	}
	return in, padX, padY
}

// clampCoord is a source coordinate's whole pixel, clamped to [0, n-1],
// and its fraction toward the next.
func clampCoord(s float64, n int) (int, float64) {
	if s <= 0 {
		return 0, 0
	}
	i := int(s)
	if i >= n-1 {
		return n - 1, 0
	}
	return i, s - float64(i)
}

// unletter maps a coordinate normalised to the letterboxed square back to
// the image (DetectionProjectionCalculator's projection of the letterbox).
func unletter(v, pad float32) float32 { return (v - pad) / (1 - 2*pad) }

// clamp01 is v clamped to [0, 1].
func clamp01(v float32) float32 { return min(max(v, 0), 1) }

// Source returns the path of a model file, fetching it first when it is
// missing: cmd/cloudburrow sets it to localai's pinned fetch.
type Source func(ctx context.Context) (string, error)

// ErrNoModel means no source is set: this process has no such model.
var ErrNoModel = errors.New("no detection model is configured")

// ErrFetch means a model file could not be fetched.
var ErrFetch = errors.New("the detection model could not be fetched")

// slot is a model loaded from its source the first time it is asked for.
// A failed fetch is retried the next time; a model that does not load is
// remembered.
type slot[T any] struct {
	mu     sync.Mutex
	source Source
	load   func([]byte) (T, error)
	loaded T
	done   bool
	err    error
}

func (s *slot[T]) set(src Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	s.source, s.loaded, s.done, s.err = src, zero, false, nil
}

func (s *slot[T]) get(ctx context.Context) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	if s.done {
		return s.loaded, s.err
	}
	if s.source == nil {
		return zero, ErrNoModel
	}
	path, err := s.source(ctx)
	if err != nil {
		return zero, fmt.Errorf("%w: %w", ErrFetch, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, err
	}
	s.loaded, s.err = s.load(data)
	s.done = true
	return s.loaded, s.err
}
