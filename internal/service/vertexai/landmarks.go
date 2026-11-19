package vertexai

import (
	"context"
	"image"

	"github.com/cloudburrow/cloudburrow/internal/imagelandmark"
)

// Vision's LANDMARK_DETECTION feature (#1799), answered locally by
// Google's six on-device landmark classifiers, one a continent
// (internal/imagelandmark, fetched once from Kaggle Models and pinned by
// SHA-256; never a call to Vision). The answer has Vision's shape
// (https://docs.cloud.google.com/vision/docs/detecting-landmarks and the
// AnnotateImageResponse reference, read 2026-11-18): landmarkAnnotations,
// a list of EntityAnnotation with description and score, most probable
// first, at most the feature's maxResults of them, 10 when maxResults is
// omitted (as for the other features); an image with none omits the
// field, as proto3 JSON does.
//
// UNVERIFIED (docs re-read 2026-11-18, still unanswered:
// https://docs.cloud.google.com/vision/docs/detecting-landmarks):
//   - mid is omitted: the classifiers name Google Landmarks Dataset v2
//     clusters, with no Knowledge Graph ID;
//   - locations (the landmark's latitude and longitude) is omitted: the
//     classifiers carry no coordinates;
//   - boundingPoly is omitted: a classifier scores the whole image and
//     finds no box;
//   - the score is the classifiers' own, from their 0.7 threshold, not
//     Vision's calibration, and an image with no landmark is still
//     answered the landmarks it most resembles (a scanned page scores
//     0.95 for a museum); a negative maxResults is taken as omitted. The
//     answer is deterministic.

// visionLandmarks answers LANDMARK_DETECTION for img: the
// landmarkAnnotations field, with at most max landmarks
// (defaultLabelResults when max <= 0).
func visionLandmarks(ctx context.Context, img image.Image, max int) ([]any, error) {
	cs, err := imagelandmark.Default(ctx)
	if err != nil {
		return nil, err
	}
	ls, err := imagelandmark.Recognize(cs, img)
	if err != nil {
		return nil, err
	}
	return landmarkAnnotations(ls, max), nil
}

// landmarkAnnotations is ls, most probable first, as Vision's
// EntityAnnotation list: at most max (defaultLabelResults when max <= 0).
func landmarkAnnotations(ls []imagelandmark.Landmark, max int) []any {
	if max <= 0 {
		max = defaultLabelResults
	}
	out := make([]any, 0, min(max, len(ls)))
	for _, l := range ls[:min(max, len(ls))] {
		out = append(out, map[string]any{"description": l.Description, "score": l.Score})
	}
	return out
}
