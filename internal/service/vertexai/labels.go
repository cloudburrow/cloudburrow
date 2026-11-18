package vertexai

import (
	"context"
	"image"

	"github.com/cloudburrow/cloudburrow/internal/imagelabel"
)

// Vision's LABEL_DETECTION feature (#1799), answered locally by Google's
// MediaPipe EfficientNet-Lite0 image classifier (internal/imagelabel,
// fetched once from storage.googleapis.com and pinned by SHA-256; never a
// call to Vision). The answer has Vision's shape
// (https://docs.cloud.google.com/vision/docs/labels and the
// AnnotateImageResponse reference, read 2026-11-18): labelAnnotations, a
// list of EntityAnnotation with description, score and topicality, most
// probable first, at most the feature's maxResults of them, 10 when
// maxResults is omitted ("If you omit the maxResults field and its value,
// the API returns the default value of 10 results"); topicality equals
// score, as in that page's sample.
//
// UNVERIFIED (2026-11-18):
//   - the vocabulary: the labels are the 1,000 ImageNet classes the model
//     knows (its own label text as description), not Vision's Knowledge
//     Graph entities, so mid is omitted (no official mapping from ImageNet
//     classes to Knowledge Graph IDs exists) and a photograph Vision
//     labels "Street" or "Sky" is labelled with an ImageNet class here;
//   - Vision's preprocessing and score calibration, which are not
//     documented: the score is the model's softmax probability, and no
//     score threshold drops a label (Vision's sample answers five labels
//     where ten are the default); a negative maxResults is taken as
//     omitted. The answer is deterministic.

// defaultLabelResults is how many labels Vision answers when maxResults is
// omitted.
const defaultLabelResults = 10

// visionLabels answers LABEL_DETECTION for img: the labelAnnotations
// field, with at most max labels (defaultLabelResults when max <= 0).
func visionLabels(ctx context.Context, img image.Image, max int) ([]any, error) {
	c, err := imagelabel.Default(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := c.Classify(img)
	if err != nil {
		return nil, err
	}
	return labelAnnotations(labels, max), nil
}

// labelAnnotations is labels, most probable first, as Vision's
// EntityAnnotation list: at most max (defaultLabelResults when max <= 0).
func labelAnnotations(labels []imagelabel.Label, max int) []any {
	if max <= 0 {
		max = defaultLabelResults
	}
	out := make([]any, 0, min(max, len(labels)))
	for _, l := range labels[:min(max, len(labels))] {
		out = append(out, map[string]any{"description": l.Description, "score": l.Score, "topicality": l.Score})
	}
	return out
}
