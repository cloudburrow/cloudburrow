package imagedetect

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"strings"

	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// The object detector: MediaPipe's EfficientDet-Lite0 (float32, version
// 1). Its TFLite metadata, read 2026-11-18, gives what is done with it:
//
//   - the input is one 320×320 RGB image normalised as (v - 127.5) / 127.5;
//     the object detector stretches the whole image to it (its graph sets
//     no keep_aspect_ratio);
//   - the outputs are 19,206 box regressions (YXHW, scale 1, exponential
//     on the size) against the 19,206 fixed anchors of its
//     DETECTOR_METADATA, and each box's 90 class probabilities (already
//     sigmoid, sigmoid_score false), in the order of labels.txt, which the
//     file carries as a zip archive appended to it;
//   - MediaPipe keeps each box's most probable class and suppresses boxes
//     overlapping a more probable one by more than IoU 0.3
//     (ObjectDetectorOptions' min_suppression_threshold default, across
//     classes as multiclass_nms is false by default).
//
// UNVERIFIED (docs re-read 2026-11-18, still unanswered:
// https://docs.cloud.google.com/vision/docs/object-localizer): Vision's
// own score threshold is not documented
// and the model's metadata sets none; ObjectScoreThreshold is the one
// MediaPipe's documentation examples use.
const (
	// ObjectSize is the object detector's input width and height.
	ObjectSize = 320
	// ObjectScoreThreshold is the least score an object is answered with.
	ObjectScoreThreshold = 0.5
	// objectSuppression is MediaPipe's default min_suppression_threshold.
	objectSuppression = 0.3
	// objectLabels is the label list inside the model file.
	objectLabels = "labels.txt"
	// unnamedLabel marks a COCO category ID with no category.
	unnamedLabel = "???"
)

// Object is one detected object.
type Object struct {
	Name  string
	Score float32
	Box   Box
}

// decoding is TensorsDecodingOptions (object_detector_metadata_schema.fbs).
type decoding struct {
	classes, boxes, coords int
	xScale, yScale         float32
	wScale, hScale         float32
	exp, sigmoid           bool
}

// anchor is a FixedAnchor.
type anchor struct{ x, y, w, h float32 }

// ObjectDetector is a loaded object detector, its anchors and labels.
type ObjectDetector struct {
	model   *tflitemodel.Model
	dec     decoding
	anchors []anchor
	labels  []string
	boxOut  int // the index of the box output; the other is the scores
}

// LoadObjectDetector reads a MediaPipe object detector .tflite file
// without in-model suppression: the model, its DETECTOR_METADATA and its
// label list.
func LoadObjectDetector(data []byte) (*ObjectDetector, error) {
	m, err := tflitemodel.Load(data)
	if err != nil {
		return nil, err
	}
	if len(m.Inputs) != 1 || len(m.Outputs) != 2 {
		return nil, fmt.Errorf("the object detector has %d inputs and %d outputs, not one and two", len(m.Inputs), len(m.Outputs))
	}
	if s := m.Inputs[0].Shape; len(s) != 4 || s[1] != ObjectSize || s[2] != ObjectSize || s[3] != 3 {
		return nil, fmt.Errorf("the object detector takes %v, not [1 %d %d 3]", s, ObjectSize, ObjectSize)
	}
	dec, anchors, err := detectorMetadata(data)
	if err != nil {
		return nil, err
	}
	labels, err := zipLabels(data, objectLabels)
	if err != nil {
		return nil, err
	}
	if len(labels) != dec.classes || len(anchors) != dec.boxes || dec.coords != 4 {
		return nil, fmt.Errorf("the object detector's metadata gives %d classes, %d boxes and %d coordinates for %d labels and %d anchors",
			dec.classes, dec.boxes, dec.coords, len(labels), len(anchors))
	}
	d := &ObjectDetector{model: m, dec: dec, anchors: anchors, labels: labels, boxOut: -1}
	for i, o := range m.Outputs {
		if len(o.Shape) == 3 && o.Shape[1] == dec.boxes && o.Shape[2] == 4 {
			d.boxOut = i
		}
	}
	if d.boxOut < 0 {
		return nil, errors.New("the object detector has no box output")
	}
	if s := m.Outputs[1-d.boxOut].Shape; len(s) != 3 || s[1] != dec.boxes || s[2] != dec.classes {
		return nil, fmt.Errorf("the object detector's scores are %v, not [1 %d %d]", s, dec.boxes, dec.classes)
	}
	return d, nil
}

// Labels are the detector's class names, "???" for an unnamed COCO ID.
func (d *ObjectDetector) Labels() []string { return d.labels }

// Detect answers the objects in img scoring at least
// ObjectScoreThreshold, most probable first, at most limit (limit <= 0:
// all).
func (d *ObjectDetector) Detect(img image.Image, limit int) ([]Object, error) {
	in, _, _ := imageInput(img, ObjectSize, false)
	out, err := d.model.Run([]tflitemodel.Tensor{{Type: tflitemodel.Float32, Shape: []int{1, ObjectSize, ObjectSize, 3}, F: in}})
	if err != nil {
		return nil, err
	}
	raw, scores := out[d.boxOut].F, out[1-d.boxOut].F
	nc := d.dec.classes
	var ds []detection
	for i, a := range d.anchors {
		best, cls := float32(math.Inf(-1)), -1
		for c := range nc {
			if d.labels[c] == unnamedLabel {
				continue
			}
			s := scores[i*nc+c]
			if d.dec.sigmoid {
				s = sigmoid(s)
			}
			if s > best {
				best, cls = s, c
			}
		}
		if cls < 0 || best < ObjectScoreThreshold {
			continue
		}
		ds = append(ds, detection{box: d.decode(raw[i*4:i*4+4], a), score: best, class: cls})
	}
	var res []Object
	for _, k := range nms(ds, objectSuppression, limit) {
		b := k.box
		res = append(res, Object{Name: d.labels[k.class], Score: k.score,
			Box: Box{clamp01(b.XMin), clamp01(b.YMin), clamp01(b.XMax), clamp01(b.YMax)}})
	}
	return res, nil
}

// decode is TensorsToDetectionsCalculator's DecodeBoxes for one YXHW box.
func (d *ObjectDetector) decode(r []float32, a anchor) Box {
	yc := r[0]/d.dec.yScale*a.h + a.y
	xc := r[1]/d.dec.xScale*a.w + a.x
	h, w := r[2]/d.dec.hScale, r[3]/d.dec.wScale
	if d.dec.exp {
		h, w = float32(math.Exp(float64(h)))*a.h, float32(math.Exp(float64(w)))*a.w
	} else {
		h, w = h*a.h, w*a.w
	}
	return Box{xc - w/2, yc - h/2, xc + w/2, yc + h/2}
}

// detectorMetadata reads the DETECTOR_METADATA custom metadata of the
// model's TFLITE_METADATA (metadata_schema.fbs: ModelMetadata
// subgraph_metadata 10, SubGraphMetadata custom_metadata 22,
// CustomMetadata name 4 and data 6; its data an ObjectDetectorOptions of
// object_detector_metadata_schema.fbs).
func detectorMetadata(data []byte) (dec decoding, anchors []anchor, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the object detector's metadata is not valid: %v", r)
		}
	}()
	md, ok, err := tflitemodel.Metadata(data, "TFLITE_METADATA")
	if err != nil {
		return dec, nil, err
	}
	if !ok {
		return dec, nil, errors.New("the object detector carries no TFLITE_METADATA")
	}
	root := fbRoot(md)
	for _, sg := range fbTables(root, 10) {
		for _, cm := range fbTables(sg, 22) {
			if fbString(cm, 4) != "DETECTOR_METADATA" {
				continue
			}
			o := cm.Offset(6)
			if o == 0 {
				break
			}
			od := fbRoot(cm.ByteVector(cm.Pos + flatbuffers.UOffsetT(o)))
			t := fbSub(od, 8)
			if t == nil {
				return dec, nil, errors.New("the object detector's DETECTOR_METADATA has no tensors_decoding_options")
			}
			dec = decoding{classes: int(t.GetInt32Slot(4, 0)), boxes: int(t.GetInt32Slot(6, 0)), coords: int(t.GetInt32Slot(8, 0)),
				xScale: t.GetFloat32Slot(16, 0), yScale: t.GetFloat32Slot(18, 0), wScale: t.GetFloat32Slot(20, 0), hScale: t.GetFloat32Slot(22, 0),
				exp: t.GetBoolSlot(24, false), sigmoid: t.GetBoolSlot(26, false)}
			if dec.xScale == 0 || dec.yScale == 0 || dec.wScale == 0 || dec.hScale == 0 {
				return dec, nil, errors.New("the object detector's decoding scales are zero")
			}
			fixed := fbSub(fbSub(od, 6), 4)
			for _, a := range fbTables(fixed, 4) {
				anchors = append(anchors, anchor{x: a.GetFloat32Slot(4, 0), y: a.GetFloat32Slot(6, 0), w: a.GetFloat32Slot(8, 0), h: a.GetFloat32Slot(10, 0)})
			}
			if len(anchors) == 0 {
				return dec, nil, errors.New("the object detector's DETECTOR_METADATA has no fixed anchors")
			}
			return dec, anchors, nil
		}
	}
	return dec, nil, errors.New("the object detector carries no DETECTOR_METADATA")
}

func fbRoot(b []byte) *flatbuffers.Table {
	return &flatbuffers.Table{Bytes: b, Pos: flatbuffers.GetUOffsetT(b)}
}

func fbSub(t *flatbuffers.Table, slot int) *flatbuffers.Table {
	if t == nil {
		return nil
	}
	o := t.Offset(flatbuffers.VOffsetT(slot))
	if o == 0 {
		return nil
	}
	return &flatbuffers.Table{Bytes: t.Bytes, Pos: t.Indirect(t.Pos + flatbuffers.UOffsetT(o))}
}

func fbTables(t *flatbuffers.Table, slot int) []*flatbuffers.Table {
	if t == nil {
		return nil
	}
	o := t.Offset(flatbuffers.VOffsetT(slot))
	if o == 0 {
		return nil
	}
	n := t.VectorLen(flatbuffers.UOffsetT(o))
	v := t.Vector(flatbuffers.UOffsetT(o))
	out := make([]*flatbuffers.Table, n)
	for i := range out {
		p := v + flatbuffers.UOffsetT(4*i)
		out[i] = &flatbuffers.Table{Bytes: t.Bytes, Pos: t.Indirect(p)}
	}
	return out
}

func fbString(t *flatbuffers.Table, slot int) string {
	o := t.Offset(flatbuffers.VOffsetT(slot))
	if o == 0 {
		return ""
	}
	return string(t.ByteVector(t.Pos + flatbuffers.UOffsetT(o)))
}

// zipLabels reads a label list, one a line, from the zip archive
// appended to a model file.
func zipLabels(data []byte, name string) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("the model carries no label list: %w", err)
	}
	f, err := zr.Open(name)
	if err != nil {
		return nil, fmt.Errorf("the model carries no %s: %w", name, err)
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
		return nil, errors.New("the model's label list is empty")
	}
	return labels, nil
}

var objects = slot[*ObjectDetector]{load: LoadObjectDetector}

// SetObjectSource sets where the object detector comes from, and forgets
// one loaded from the previous source.
func SetObjectSource(s Source) { objects.set(s) }

// DefaultObjects is the object detector of the configured source, loaded
// the first time it is asked for.
func DefaultObjects(ctx context.Context) (*ObjectDetector, error) { return objects.get(ctx) }
