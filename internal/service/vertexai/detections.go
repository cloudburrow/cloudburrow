package vertexai

import (
	"context"
	"image"
	"math"
	"unicode"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/imagedetect"
)

// Vision's OBJECT_LOCALIZATION and FACE_DETECTION features (#1799),
// answered locally by Google's MediaPipe detectors (internal/imagedetect:
// EfficientDet-Lite0 and BlazeFace short-range, each fetched once from
// storage.googleapis.com and pinned by SHA-256; never a call to Vision).
// The answers have Vision's shape (the AnnotateImageResponse reference,
// https://docs.cloud.google.com/vision/docs/object-localizer and
// https://docs.cloud.google.com/vision/docs/detecting-faces, read
// 2026-11-18):
//
//   - localizedObjectAnnotations: LocalizedObjectAnnotation entries of
//     name, score and boundingPoly.normalizedVertices (four corners
//     clockwise from the top left, in [0, 1]), most probable first;
//   - faceAnnotations: FaceAnnotation entries of fdBoundingPoly (the
//     detector's box, in pixels), boundingPoly (that box scaled 1.5 times
//     about its centre and clipped to the image: MediaPipe's own expanded
//     face rectangle, "likely contain[ing] the whole face"), the landmarks
//     BlazeFace gives (LEFT_EYE, RIGHT_EYE, NOSE_TIP, MOUTH_CENTER,
//     LEFT_EAR_TRAGION and RIGHT_EAR_TRAGION, left and right from the
//     viewer's side as Vision defines them), detectionConfidence, and
//     rollAngle, the angle of the line from the left eye to the right eye.
//     panAngle, tiltAngle and landmarkingConfidence cannot be derived
//     from six 2-D keypoints and are omitted (zero); every likelihood
//     (joy, sorrow, anger, surprise, under-exposed, blurred, headwear) is
//     UNKNOWN, Vision's "Unknown likelihood.".
//
// At most the feature's maxResults are answered, 10 when it is omitted.
//
// UNVERIFIED (docs re-read 2026-11-18, still unanswered:
// https://docs.cloud.google.com/vision/docs/object-localizer and
// https://docs.cloud.google.com/vision/docs/detecting-faces):
//   - the object vocabulary: EfficientDet-Lite0 knows COCO's 80 object
//     categories only, named as its label file names them with the first
//     letter capitalised ("Person", "Dog", "Traffic light"), not Vision's
//     Knowledge Graph objects, so mid is omitted;
//   - score calibration: an object's score is the model's class
//     probability, answered from 0.5 (the threshold MediaPipe's examples
//     use; Vision's is not documented), a face's the BlazeFace score from
//     0.5 (MediaPipe's default min_detection_confidence);
//   - the face landmarks: Vision answers some 34 landmarks in 3-D; here
//     there are six, with no z, and Vision's boundingPoly framing, its
//     roll sign convention and its maxResults default for these features
//     are not documented.
//
// The answers are deterministic.

// defaultDetectResults is how many objects or faces are answered when
// maxResults is omitted.
const defaultDetectResults = 10

// visionObjects answers OBJECT_LOCALIZATION for img: the
// localizedObjectAnnotations field.
func visionObjects(ctx context.Context, img image.Image, limit int) ([]any, error) {
	d, err := imagedetect.DefaultObjects(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultDetectResults
	}
	objs, err := d.Detect(img, limit)
	if err != nil {
		return nil, err
	}
	return objectAnnotations(objs), nil
}

// objectAnnotations is objs as Vision's LocalizedObjectAnnotation list.
func objectAnnotations(objs []imagedetect.Object) []any {
	out := make([]any, 0, len(objs))
	for _, o := range objs {
		b := o.Box
		out = append(out, map[string]any{
			"name":  objectName(o.Name),
			"score": o.Score,
			"boundingPoly": map[string]any{"normalizedVertices": []any{
				normVertex(b.XMin, b.YMin), normVertex(b.XMax, b.YMin), normVertex(b.XMax, b.YMax), normVertex(b.XMin, b.YMax),
			}},
		})
	}
	return out
}

// objectName is a COCO label with its first letter capitalised, as
// Vision's object names are written ("Person", "Bicycle wheel").
func objectName(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// normVertex is a NormalizedVertex with zero coordinates omitted, as
// proto3 JSON writes it.
func normVertex(x, y float32) map[string]any {
	v := map[string]any{}
	if x != 0 {
		v["x"] = x
	}
	if y != 0 {
		v["y"] = y
	}
	return v
}

// visionFaces answers FACE_DETECTION for img: the faceAnnotations field.
func visionFaces(ctx context.Context, img image.Image, limit int) ([]any, error) {
	d, err := imagedetect.DefaultFaces(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultDetectResults
	}
	fs, err := d.Detect(img, limit)
	if err != nil {
		return nil, err
	}
	return faceAnnotations(fs, img.Bounds()), nil
}

// faceLandmarks are Vision's landmark types for BlazeFace's keypoints, in
// its order: the person's right eye first, which is the viewer's left.
var faceLandmarks = [...]string{"LEFT_EYE", "RIGHT_EYE", "NOSE_TIP", "MOUTH_CENTER", "LEFT_EAR_TRAGION", "RIGHT_EAR_TRAGION"}

// faceLikelihoods are the FaceAnnotation likelihoods BlazeFace cannot
// tell.
var faceLikelihoods = []string{"joyLikelihood", "sorrowLikelihood", "angerLikelihood", "surpriseLikelihood",
	"underExposedLikelihood", "blurredLikelihood", "headwearLikelihood"}

// faceExpand is MediaPipe's face detector's RectTransformationCalculator
// scale, its expanded face rectangle.
const faceExpand = 1.5

// faceAnnotations is fs as Vision's FaceAnnotation list, in the pixels
// of an image of bounds b.
func faceAnnotations(fs []imagedetect.Face, b image.Rectangle) []any {
	w, h := float32(b.Dx()), float32(b.Dy())
	out := make([]any, 0, len(fs))
	for _, f := range fs {
		x0, y0, x1, y1 := f.Box.XMin*w, f.Box.YMin*h, f.Box.XMax*w, f.Box.YMax*h
		cx, cy, hw, hh := (x0+x1)/2, (y0+y1)/2, (x1-x0)/2*faceExpand, (y1-y0)/2*faceExpand
		marks := make([]any, 0, len(faceLandmarks))
		for i, t := range faceLandmarks {
			p := f.Keypoints[i]
			pos := map[string]any{}
			if x := p.X * w; x != 0 {
				pos["x"] = x
			}
			if y := p.Y * h; y != 0 {
				pos["y"] = y
			}
			marks = append(marks, map[string]any{"type": t, "position": pos})
		}
		le, re := f.Keypoints[0], f.Keypoints[1]
		roll := float32(math.Atan2(float64((re.Y-le.Y)*h), float64((re.X-le.X)*w)) * 180 / math.Pi)
		a := map[string]any{
			"boundingPoly":        pixelPoly(cx-hw, cy-hh, cx+hw, cy+hh, w, h),
			"fdBoundingPoly":      pixelPoly(x0, y0, x1, y1, w, h),
			"landmarks":           marks,
			"detectionConfidence": f.Score,
		}
		if roll != 0 {
			a["rollAngle"] = roll
		}
		for _, k := range faceLikelihoods {
			a[k] = "UNKNOWN"
		}
		out = append(out, a)
	}
	return out
}

// pixelPoly is a BoundingPoly of a box's corners, clockwise from the top
// left, rounded to whole pixels and clipped to a w×h image.
func pixelPoly(x0, y0, x1, y1, w, h float32) map[string]any {
	px := func(v, n float32) int { return int(math.Round(float64(min(max(v, 0), n)))) }
	l, t, r, btm := px(x0, w), px(y0, h), px(x1, w), px(y1, h)
	return map[string]any{"vertices": []any{vertex(l, t), vertex(r, t), vertex(r, btm), vertex(l, btm)}}
}
