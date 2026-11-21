package vertexai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"  // Vision reads GIF
	_ "image/jpeg" // and JPEG
	_ "image/png"  // and PNG
	"io"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/imagedetect"
	"github.com/cloudburrow/cloudburrow/internal/imagelabel"
	"github.com/cloudburrow/cloudburrow/internal/ocr"
	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// The text features of Cloud Vision's images:annotate (#1773), for
// BigQuery ML's ML.ANNOTATE_IMAGE over a CLOUD_AI_VISION_V1 remote model,
// which the BigQuery front sends here, through cloudburrow-host, instead
// of to vision.googleapis.com. The request and response are Vision v1's
// BatchAnnotateImagesRequest and BatchAnnotateImagesResponse
// (https://cloud.google.com/vision/docs/reference/rest/v1/images/annotate,
// read 2026-11-18), for the subset a local model can answer:
//
//   - TEXT_DETECTION and DOCUMENT_TEXT_DETECTION read the image with
//     Tesseract (internal/ocr, the LSTM engine Google developed), at 300
//     pixels an inch, and answer textAnnotations (the whole text with its
//     bounds, then one entry a word with its boundingPoly) and
//     fullTextAnnotation (text, and one page with the image's width and
//     height, its blocks, paragraphs, words and symbols with their
//     boundingBox and confidence, from Tesseract's result iterator, #1799;
//     each word's last symbol carries detectedBreak SPACE, EOL_SURE_SPACE
//     at a line's end or LINE_BREAK at a paragraph's). UNVERIFIED (the
//     AnnotateImageResponse reference, re-read 2026-11-18, defines the
//     fields but not these choices): no locale or detectedLanguages
//     (Tesseract names no language); box corners are Tesseract's pixel
//     edges; Vision's TEXT_DETECTION and DOCUMENT_TEXT_DETECTION segment a
//     page differently from each other, here both are Tesseract's.
//   - IMAGE_PROPERTIES is computed from the pixels (imageprops.go, #1799):
//     imagePropertiesAnnotation's dominant colors and a whole-image
//     cropHintsAnnotation.
//   - LABEL_DETECTION is answered by Google's MediaPipe EfficientNet-Lite0
//     classifier run locally (labels.go, #1799): labelAnnotations of
//     ImageNet classes, at most the feature's maxResults (10 by default).
//   - OBJECT_LOCALIZATION and FACE_DETECTION are answered by Google's
//     MediaPipe EfficientDet-Lite0 and BlazeFace detectors run locally
//     (detections.go, #1799): localizedObjectAnnotations of COCO's 80
//     categories, and faceAnnotations with six landmarks.
//   - Every other feature (LANDMARK_DETECTION, LOGO_DETECTION,
//     SAFE_SEARCH_DETECTION, CROP_HINTS, WEB_DETECTION,
//     PRODUCT_SEARCH) has no local model: that image's response is an
//     UNIMPLEMENTED error naming it, and nothing is read. Re-checked
//     2026-11-21 for logos and landmarks: Google publishes no logo model
//     (MediaPipe's, Kaggle's and TF Hub's lists have none); its on-device
//     landmark classifiers cannot answer "no landmark" (measured, #1799);
//     DELF and DELG (tensorflow/models, Apache-2.0, ungated) are retrieval
//     embeddings, which need a TensorFlow runtime and an index of labelled
//     landmark images with Knowledge Graph mids that Google does not
//     publish.
//   - image.content (base64) alone: image.source, a URL Vision would fetch,
//     is refused. PNG, JPEG and GIF are decoded; other data is "Bad image
//     data." as Vision answers it.

// visionServedFeatures are the features answered, and how.
var visionServedFeatures = map[string]visionKind{"TEXT_DETECTION": visionText, "DOCUMENT_TEXT_DETECTION": visionText,
	"IMAGE_PROPERTIES": visionProps, "LABEL_DETECTION": visionLabel, "OBJECT_LOCALIZATION": visionObject, "FACE_DETECTION": visionFace}

// visionKind is how a feature is answered.
type visionKind int

const (
	visionText   visionKind = iota + 1 // local Tesseract OCR
	visionProps                        // from the pixels
	visionLabel                        // the local image classifier
	visionObject                       // the local object detector
	visionFace                         // the local face detector
)

// visionStatus is a google.rpc.Status.
type visionStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// annotateImages serves POST /v1/images:annotate.
func (s *Server) annotateImages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Requests []struct {
			Image struct {
				Content string          `json:"content"`
				Source  json.RawMessage `json:"source"`
			} `json:"image"`
			Features []struct {
				Type       string `json:"type"`
				MaxResults int    `json:"maxResults"`
			} `json:"features"`
		} `json:"requests"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil || json.Unmarshal(body, &req) != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("malformed request body: expected a BatchAnnotateImagesRequest"))
		return
	}
	if len(req.Requests) == 0 {
		apierror.WriteJSON(w, apierror.InvalidArgument("No requests specified."))
		return
	}
	responses := make([]any, 0, len(req.Requests))
	for _, ir := range req.Requests {
		fail := func(code int, msg string) {
			responses = append(responses, map[string]any{"error": visionStatus{Code: code, Message: msg}})
		}
		if len(ir.Features) == 0 {
			fail(3, "No feature specified.")
			continue
		}
		var unserved []string
		text, props, labels, maxLabels := false, false, false, 0
		objects, maxObjects, faces, maxFaces := false, 0, false, 0
		for _, f := range ir.Features {
			t := strings.ToUpper(f.Type)
			switch visionServedFeatures[t] {
			case visionText:
				text = true
			case visionProps:
				props = true
			case visionLabel:
				labels, maxLabels = true, max(maxLabels, f.MaxResults)
			case visionObject:
				objects, maxObjects = true, max(maxObjects, f.MaxResults)
			case visionFace:
				faces, maxFaces = true, max(maxFaces, f.MaxResults)
			default:
				unserved = append(unserved, t)
			}
		}
		if len(unserved) > 0 {
			fail(12, "Not implemented here: Vision feature "+strings.Join(unserved, ", ")+": CloudBurrow has no local model for it "+
				"(TEXT_DETECTION and DOCUMENT_TEXT_DETECTION are read with local Tesseract OCR, IMAGE_PROPERTIES from the pixels, "+
				"LABEL_DETECTION with a local image classifier, OBJECT_LOCALIZATION and FACE_DETECTION with local detectors) "+
				"and never calls Google. Nothing was read.")
			continue
		}
		if len(ir.Image.Source) > 0 && string(ir.Image.Source) != "null" {
			fail(12, "Not implemented here: image.source: send the image's bytes as image.content.")
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(ir.Image.Content)
		if err != nil || len(raw) == 0 {
			fail(3, "Bad image data.")
			continue
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			fail(3, "Bad image data.")
			continue
		}
		out := map[string]any{}
		if text {
			res, err := ocr.ReadLayout(r.Context(), img, 300)
			if err != nil {
				fail(13, "Internal error: reading the image's text: "+err.Error())
				continue
			}
			out = visionTextResponse(res.Text, res.Blocks, img.Bounds())
		}
		if props {
			for k, v := range imageProperties(img) {
				out[k] = v
			}
		}
		if labels {
			la, err := visionLabels(r.Context(), img, maxLabels)
			if err != nil {
				fail(labelErrorCode(err), "Labelling the image: "+err.Error())
				continue
			}
			out["labelAnnotations"] = la
		}
		if objects {
			oa, err := visionObjects(r.Context(), img, maxObjects)
			if err != nil {
				fail(labelErrorCode(err), "Localizing the image's objects: "+err.Error())
				continue
			}
			if len(oa) > 0 { // proto3 JSON omits an empty list
				out["localizedObjectAnnotations"] = oa
			}
		}
		if faces {
			fa, err := visionFaces(r.Context(), img, maxFaces)
			if err != nil {
				fail(labelErrorCode(err), "Detecting the image's faces: "+err.Error())
				continue
			}
			if len(fa) > 0 {
				out["faceAnnotations"] = fa
			}
		}
		responses = append(responses, out)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})
}

// labelErrorCode is the google.rpc.Code of a failure to label an image or
// detect its objects or faces: UNIMPLEMENTED when this process has no such
// model or the model holds an
// operator the local runtime does not run, UNAVAILABLE when it could not be
// fetched, INTERNAL otherwise.
func labelErrorCode(err error) int {
	switch {
	case errors.Is(err, imagelabel.ErrNoModel), errors.Is(err, imagedetect.ErrNoModel), tflitemodel.IsUnsupported(err):
		return 12
	case errors.Is(err, imagelabel.ErrFetch), errors.Is(err, imagedetect.ErrFetch):
		return 14
	}
	return 13
}

// visionTextResponse is an AnnotateImageResponse of text: empty, as
// Vision's, when there is none.
func visionTextResponse(text string, blocks []ocr.Block, b image.Rectangle) map[string]any {
	if strings.TrimSpace(text) == "" {
		return map[string]any{}
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	all := map[string]any{"description": text}
	annotations := []any{all}
	var pageConf float64
	var nwords int
	var minP, maxP image.Point
	first := true
	grow := func(q ocr.Quad) {
		for _, p := range q {
			if first {
				minP, maxP, first = p, p, false
				continue
			}
			minP.X, minP.Y = min(minP.X, p.X), min(minP.Y, p.Y)
			maxP.X, maxP.Y = max(maxP.X, p.X), max(maxP.Y, p.Y)
		}
	}
	vblocks := make([]any, 0, len(blocks))
	for _, bl := range blocks {
		paras := make([]any, 0, len(bl.Paragraphs))
		for _, p := range bl.Paragraphs {
			words := make([]any, 0, len(p.Words))
			for wi, w := range p.Words {
				syms := make([]any, 0, len(w.Symbols))
				var word strings.Builder
				for si, sy := range w.Symbols {
					word.WriteString(sy.Text)
					vs := map[string]any{"boundingBox": visionPoly(sy.Box), "text": sy.Text, "confidence": float32(sy.Confidence)}
					if si == len(w.Symbols)-1 {
						brk := "SPACE"
						switch {
						case sy.LineEnd && wi == len(p.Words)-1:
							brk = "LINE_BREAK"
						case sy.LineEnd:
							brk = "EOL_SURE_SPACE"
						}
						vs["property"] = map[string]any{"detectedBreak": map[string]any{"type": brk}}
					}
					syms = append(syms, vs)
				}
				if word.Len() == 0 {
					continue
				}
				grow(w.Box)
				pageConf += w.Confidence
				nwords++
				annotations = append(annotations, map[string]any{"description": word.String(), "boundingPoly": visionPoly(w.Box)})
				words = append(words, map[string]any{"boundingBox": visionPoly(w.Box), "symbols": syms, "confidence": float32(w.Confidence)})
			}
			paras = append(paras, map[string]any{"boundingBox": visionPoly(p.Box), "words": words, "confidence": float32(p.Confidence)})
		}
		vblocks = append(vblocks, map[string]any{"boundingBox": visionPoly(bl.Box), "paragraphs": paras, "blockType": "TEXT",
			"confidence": float32(bl.Confidence)})
	}
	page := map[string]any{"width": b.Dx(), "height": b.Dy(), "blocks": vblocks}
	if nwords > 0 {
		all["boundingPoly"] = visionPoly(ocr.Quad{minP, {maxP.X, minP.Y}, maxP, {minP.X, maxP.Y}})
		page["confidence"] = float32(pageConf / float64(nwords))
	}
	return map[string]any{
		"textAnnotations":    annotations,
		"fullTextAnnotation": map[string]any{"text": text, "pages": []any{page}},
	}
}

// visionPoly is a BoundingPoly of q's corners.
func visionPoly(q ocr.Quad) map[string]any {
	vs := make([]any, 0, 4)
	for _, p := range q {
		vs = append(vs, vertex(p.X, p.Y))
	}
	return map[string]any{"vertices": vs}
}
