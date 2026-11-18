package imagedetect

import (
	"context"
	"fmt"
	"image"
	"math"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// The face detector: MediaPipe's BlazeFace short-range (float16, version
// 1). Its graph (mediapipe/tasks/cc/vision/face_detector/
// face_detector_graph.cc, read 2026-11-18) runs it this way, and so does
// this package:
//
//   - the input is one 128×128 RGB image normalised as
//     (v - 127.5) / 127.5 (its metadata), the image letterboxed into it
//     (keep_aspect_ratio, BORDER_ZERO);
//   - SsdAnchorsCalculator's anchors for the short-range model: 4 layers,
//     strides 8, 16, 16, 16, aspect ratio 1 plus an interpolated scale,
//     offsets 0.5, fixed anchor size: 896 anchors;
//   - TensorsToDetectionsCalculator: 16 values a box (box at 0, six
//     keypoints of x and y from 4), reverse_output_order (XYWH), scales
//     128, the score the sigmoid of the logit clipped to ±100, kept at
//     min_detection_confidence 0.5 (FaceDetectorGraphOptions' default);
//   - NonMaxSuppressionCalculator WEIGHTED over IoU, min_suppression_threshold
//     0.5 (the options' default).
//
// The keypoints are, in order, the right eye, left eye, nose tip, mouth
// centre, right ear tragion and left ear tragion of the person (MediaPipe's
// face detector page; face_detector_graph.cc calls keypoint 0 "Left eye
// from the observer's point of view").
const (
	// FaceSize is the face detector's input width and height.
	FaceSize = 128
	// FaceScoreThreshold is min_detection_confidence's default.
	FaceScoreThreshold = 0.5
	// faceSuppression is min_suppression_threshold's default.
	faceSuppression = 0.5
	faceCoords      = 16
	faceKeypoints   = 6
	faceScale       = 128
	scoreClip       = 100
)

// Face is one detected face: its box and its six keypoints, normalised to
// the image, and its score.
type Face struct {
	Box       Box
	Keypoints [faceKeypoints]Point
	Score     float32
}

// FaceDetector is a loaded face detector and its anchors.
type FaceDetector struct {
	model   *tflitemodel.Model
	anchors []anchor
	boxOut  int
}

// LoadFaceDetector reads MediaPipe's BlazeFace short-range .tflite file.
func LoadFaceDetector(data []byte) (*FaceDetector, error) {
	m, err := tflitemodel.Load(data)
	if err != nil {
		return nil, err
	}
	if len(m.Inputs) != 1 || len(m.Outputs) != 2 {
		return nil, fmt.Errorf("the face detector has %d inputs and %d outputs, not one and two", len(m.Inputs), len(m.Outputs))
	}
	if s := m.Inputs[0].Shape; len(s) != 4 || s[1] != FaceSize || s[2] != FaceSize || s[3] != 3 {
		return nil, fmt.Errorf("the face detector takes %v, not [1 %d %d 3]: only the short-range model is run", s, FaceSize, FaceSize)
	}
	anchors := faceAnchors()
	d := &FaceDetector{model: m, anchors: anchors, boxOut: -1}
	for i, o := range m.Outputs {
		if len(o.Shape) == 3 && o.Shape[1] == len(anchors) && o.Shape[2] == faceCoords {
			d.boxOut = i
		}
	}
	if d.boxOut < 0 {
		return nil, fmt.Errorf("the face detector has no [1 %d %d] box output", len(anchors), faceCoords)
	}
	if s := m.Outputs[1-d.boxOut].Shape; len(s) != 3 || s[1] != len(anchors) || s[2] != 1 {
		return nil, fmt.Errorf("the face detector's scores are %v, not [1 %d 1]", s, len(anchors))
	}
	return d, nil
}

// faceAnchors is SsdAnchorsCalculator's GenerateAnchors with the
// short-range options; with fixed_anchor_size every anchor is 1×1, so
// only the centres matter.
func faceAnchors() []anchor {
	strides := []int{8, 16, 16, 16}
	const offset = 0.5
	var out []anchor
	for layer := 0; layer < len(strides); {
		last := layer
		n := 0
		for last < len(strides) && strides[last] == strides[layer] {
			n += 2 // aspect ratio 1.0, and the interpolated scale
			last++
		}
		fm := int(math.Ceil(float64(FaceSize) / float64(strides[layer])))
		for y := range fm {
			for x := range fm {
				for range n {
					out = append(out, anchor{x: (float32(x) + offset) / float32(fm), y: (float32(y) + offset) / float32(fm), w: 1, h: 1})
				}
			}
		}
		layer = last
	}
	return out
}

// Detect answers the faces in img scoring at least FaceScoreThreshold,
// most probable first, at most limit (limit <= 0: all).
func (d *FaceDetector) Detect(img image.Image, limit int) ([]Face, error) {
	in, padX, padY := imageInput(img, FaceSize, true)
	out, err := d.model.Run([]tflitemodel.Tensor{{Type: tflitemodel.Float32, Shape: []int{1, FaceSize, FaceSize, 3}, F: in}})
	if err != nil {
		return nil, err
	}
	raw, scores := out[d.boxOut].F, out[1-d.boxOut].F
	var ds []detection
	for i, a := range d.anchors {
		s := sigmoid(min(max(scores[i], -scoreClip), scoreClip))
		if s < FaceScoreThreshold {
			continue
		}
		r := raw[i*faceCoords : (i+1)*faceCoords]
		xc := r[0]/faceScale*a.w + a.x
		yc := r[1]/faceScale*a.h + a.y
		w, h := r[2]/faceScale*a.w, r[3]/faceScale*a.h
		det := detection{box: Box{xc - w/2, yc - h/2, xc + w/2, yc + h/2}, score: s}
		for k := range faceKeypoints {
			det.keypoints = append(det.keypoints, Point{r[4+2*k]/faceScale*a.w + a.x, r[4+2*k+1]/faceScale*a.h + a.y})
		}
		ds = append(ds, det)
	}
	var res []Face
	for _, k := range weightedNMS(ds, faceSuppression) {
		f := Face{Score: k.score, Box: Box{
			XMin: unletter(k.box.XMin, padX), YMin: unletter(k.box.YMin, padY),
			XMax: unletter(k.box.XMax, padX), YMax: unletter(k.box.YMax, padY)}}
		for i, p := range k.keypoints {
			f.Keypoints[i] = Point{unletter(p.X, padX), unletter(p.Y, padY)}
		}
		res = append(res, f)
		if limit > 0 && len(res) >= limit {
			break
		}
	}
	return res, nil
}

var faces = slot[*FaceDetector]{load: LoadFaceDetector}

// SetFaceSource sets where the face detector comes from, and forgets one
// loaded from the previous source.
func SetFaceSource(s Source) { faces.set(s) }

// DefaultFaces is the face detector of the configured source, loaded the
// first time it is asked for.
func DefaultFaces(ctx context.Context) (*FaceDetector, error) { return faces.get(ctx) }
