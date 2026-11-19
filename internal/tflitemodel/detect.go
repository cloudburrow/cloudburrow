package tflitemodel

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

// TFLite_Detection_PostProcess (#1775): the custom operator TensorFlow's
// Object Detection API writes at the end of SSD models, which LiteRT's
// builtin resolver runs (kernels/detection_postprocess.cc): it decodes
// center-size box encodings against the anchors and selects detections by
// class-agnostic (fast) or per-class (regular) non-max suppression.
// Its custom_options are a FlexBuffer map (flex.go).
const (
	opDetectionPostProcess = -2
	nameDetectionPost      = "TFLite_Detection_PostProcess"
)

// builtinCustom are the custom operators LiteRT's builtin resolver
// registers, which a model may use without the caller's kernel.
var builtinCustom = map[string]int32{nameDetectionPost: opDetectionPostProcess}

type detectOpts struct {
	maxDet, maxClassesPerDet, detPerClass, numClasses int
	regular                                           bool
	scoreThr, iouThr                                  float32
	scale                                             [4]float64 // y, x, h, w
}

func readDetectOpts(o op) (detectOpts, error) {
	m, err := flexMap(o.customOpts)
	if err != nil {
		return detectOpts{}, fmt.Errorf("operator %s: %w", o.name, err)
	}
	get := func(k string, d float64) float64 {
		if v, ok := m[k]; ok {
			return v
		}
		return d
	}
	d := detectOpts{maxDet: int(get("max_detections", 0)), maxClassesPerDet: int(get("max_classes_per_detection", 1)),
		detPerClass: int(get("detections_per_class", 100)), numClasses: int(get("num_classes", 0)),
		regular: get("use_regular_nms", 0) != 0, scoreThr: float32(get("nms_score_threshold", 0)), iouThr: float32(get("nms_iou_threshold", 0)),
		scale: [4]float64{get("y_scale", 0), get("x_scale", 0), get("h_scale", 0), get("w_scale", 0)}}
	if d.maxDet <= 0 || d.numClasses <= 0 || d.maxClassesPerDet <= 0 || d.detPerClass <= 0 || d.iouThr < 0 || d.iouThr > 1 {
		return d, fmt.Errorf("operator %s: invalid options %v", o.name, m)
	}
	for _, s := range d.scale {
		if s <= 0 {
			return d, fmt.Errorf("operator %s: y_scale, x_scale, h_scale and w_scale must be positive", o.name)
		}
	}
	return d, nil
}

// detectIoU is detection_postprocess.cc's ComputeIntersectionOverUnion of
// corner boxes (ymin, xmin, ymax, xmax).
func detectIoU(b []float32, i, j int) float32 {
	bi, bj := b[4*i:4*i+4], b[4*j:4*j+4]
	ai := (bi[2] - bi[0]) * (bi[3] - bi[1])
	aj := (bj[2] - bj[0]) * (bj[3] - bj[1])
	if ai <= 0 || aj <= 0 {
		return 0
	}
	inter := max(min(bi[2], bj[2])-max(bi[0], bj[0]), 0) * max(min(bi[3], bj[3])-max(bi[1], bj[1]), 0)
	return inter / (ai + aj - inter)
}

// singleClassNMS is NonMaxSuppressionSingleClassHelper: the indices of the
// boxes kept, best score first.
func singleClassNMS(d detectOpts, boxes, scores []float32, maxDet int) []int {
	var keep []int
	for i, s := range scores {
		if s >= d.scoreThr {
			keep = append(keep, i)
		}
	}
	slices.SortStableFunc(keep, func(a, b int) int { return cmp.Compare(scores[b], scores[a]) })
	outSize := min(len(keep), maxDet)
	active := make([]bool, len(keep))
	for i := range active {
		active[i] = true
	}
	nActive := len(keep)
	var sel []int
	for i := range keep {
		if nActive == 0 || len(sel) >= outSize {
			break
		}
		if !active[i] {
			continue
		}
		sel = append(sel, keep[i])
		active[i] = false
		nActive--
		for j := i + 1; j < len(keep); j++ {
			if active[j] && detectIoU(boxes, keep[i], keep[j]) > d.iouThr {
				active[j] = false
				nActive--
			}
		}
	}
	return sel
}

// detectionPostProcess runs the operator: box_encodings [1, boxes, >=4],
// class_predictions [1, boxes, classes (+ background)], anchors [boxes, 4];
// detection_boxes, _classes, _scores and num_detections.
func detectionPostProcess(o op, a []*Tensor, n int) ([]*Tensor, error) {
	if len(a) != 3 || n != 4 || a[0] == nil || a[1] == nil || a[2] == nil {
		return nil, fmt.Errorf("operator %s takes 3 inputs and gives 4 outputs", o.name)
	}
	d, err := readDetectOpts(o)
	if err != nil {
		return nil, err
	}
	enc, cls, anc := a[0], a[1], a[2]
	if len(enc.Shape) != 3 || enc.Shape[0] != 1 || enc.Shape[2] < 4 || len(cls.Shape) != 3 || len(anc.Shape) != 2 || anc.Shape[1] != 4 {
		return nil, fmt.Errorf("operator %s: inputs of shapes %v, %v and %v", o.name, enc.Shape, cls.Shape, anc.Shape)
	}
	nb, coords := enc.Shape[1], enc.Shape[2]
	withBg := cls.Shape[2]
	if cls.Shape[1] != nb || anc.Shape[0] != nb || withBg < d.numClasses || withBg > d.numClasses+1 {
		return nil, fmt.Errorf("operator %s: %d boxes, %d anchors, %d class scores for %d classes", o.name, nb, anc.Shape[0], withBg, d.numClasses)
	}
	off := withBg - d.numClasses
	ef, cf, af := floatsOf(enc), floatsOf(cls), floatsOf(anc)
	boxes := make([]float32, 4*nb)
	for i := range nb {
		e, an := ef[i*coords:], af[i*4:]
		yc := float32(float64(e[0])/d.scale[0]*float64(an[2]) + float64(an[0]))
		xc := float32(float64(e[1])/d.scale[1]*float64(an[3]) + float64(an[1]))
		hh := float32(0.5 * math.Exp(float64(e[2])/d.scale[2]) * float64(an[2]))
		hw := float32(0.5 * math.Exp(float64(e[3])/d.scale[3]) * float64(an[3]))
		copy(boxes[4*i:], []float32{yc - hh, xc - hw, yc + hh, xc + hw})
	}
	slots := d.maxDet
	if !d.regular {
		slots = d.maxDet * d.maxClassesPerDet
	}
	ob := &Tensor{Type: Float32, Shape: []int{1, slots, 4}, F: make([]float32, 4*slots)}
	oc := &Tensor{Type: Float32, Shape: []int{1, slots}, F: make([]float32, slots)}
	os := &Tensor{Type: Float32, Shape: []int{1, slots}, F: make([]float32, slots)}
	count := 0
	if d.regular {
		type info struct {
			index, class int
			score        float32
		}
		var sorted []info
		cs := make([]float32, nb)
		for c := range d.numClasses {
			for r := range nb {
				cs[r] = cf[r*withBg+c+off]
			}
			for _, s := range singleClassNMS(d, boxes, cs, d.detPerClass) {
				sorted = append(sorted, info{s, c, cs[s]})
			}
			slices.SortStableFunc(sorted, func(x, y info) int { return cmp.Compare(y.score, x.score) })
			sorted = sorted[:min(len(sorted), d.maxDet)]
		}
		for k, s := range sorted {
			copy(ob.F[4*k:], boxes[4*s.index:4*s.index+4])
			oc.F[k], os.F[k] = float32(s.class), s.score
		}
		count = len(sorted)
	} else {
		per := min(d.maxClassesPerDet, d.numClasses)
		best := make([]float32, nb)
		order := make([][]int, nb)
		for r := range nb {
			row := cf[r*withBg+off : r*withBg+off+d.numClasses]
			idx := make([]int, d.numClasses)
			for i := range idx {
				idx[i] = i
			}
			slices.SortStableFunc(idx, func(x, y int) int { return cmp.Compare(row[y], row[x]) })
			order[r], best[r] = idx[:per], row[idx[0]]
		}
		for k, s := range singleClassNMS(d, boxes, best, d.maxDet) {
			row := cf[s*withBg+off:]
			for c, ci := range order[s] {
				j := d.maxClassesPerDet*k + c
				copy(ob.F[4*j:], boxes[4*s:4*s+4])
				oc.F[j], os.F[j] = float32(ci), row[ci]
			}
			count++
		}
	}
	return []*Tensor{ob, oc, os, {Type: Float32, Shape: []int{1}, F: []float32{float32(count)}}}, nil
}

// floatsOf is a tensor's values as float32.
func floatsOf(t *Tensor) []float32 {
	if t.F != nil {
		return t.F
	}
	out := make([]float32, t.Len())
	for i := range out {
		out[i] = t.f(i)
	}
	return out
}
