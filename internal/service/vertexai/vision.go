package vertexai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/gif"  // Vision reads GIF
	_ "image/jpeg" // and JPEG
	_ "image/png"  // and PNG
	"io"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/ocr"
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
//     pixels an inch, and answer textAnnotations (one entry, the whole
//     text) and fullTextAnnotation (text, and one page with the image's
//     width and height). Tesseract's word and block boxes are not
//     returned: no boundingPoly, no per-word textAnnotations and no
//     blocks, paragraphs, words or symbols under the page (docs/gaps.md).
//   - IMAGE_PROPERTIES is computed from the pixels (imageprops.go, #1799):
//     imagePropertiesAnnotation's dominant colors and a whole-image
//     cropHintsAnnotation.
//   - Every other feature (FACE_DETECTION, LANDMARK_DETECTION,
//     LOGO_DETECTION, LABEL_DETECTION, OBJECT_LOCALIZATION,
//     SAFE_SEARCH_DETECTION, CROP_HINTS, WEB_DETECTION,
//     PRODUCT_SEARCH) has no local model: that image's response is an
//     UNIMPLEMENTED error naming it, and nothing is read.
//   - image.content (base64) alone: image.source, a URL Vision would fetch,
//     is refused. PNG, JPEG and GIF are decoded; other data is "Bad image
//     data." as Vision answers it.

// visionServedFeatures are the features answered, true for those read
// with OCR.
var visionServedFeatures = map[string]bool{"TEXT_DETECTION": true, "DOCUMENT_TEXT_DETECTION": true, "IMAGE_PROPERTIES": false}

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
		text, props := false, false
		for _, f := range ir.Features {
			t := strings.ToUpper(f.Type)
			ocr, ok := visionServedFeatures[t]
			switch {
			case !ok:
				unserved = append(unserved, t)
			case ocr:
				text = true
			default:
				props = true
			}
		}
		if len(unserved) > 0 {
			fail(12, "Not implemented here: Vision feature "+strings.Join(unserved, ", ")+": CloudBurrow has no local model for it "+
				"(TEXT_DETECTION and DOCUMENT_TEXT_DETECTION are read with local Tesseract OCR, IMAGE_PROPERTIES from the pixels) "+
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
			res, err := ocr.Read(r.Context(), img, 300)
			if err != nil {
				fail(13, "Internal error: reading the image's text: "+err.Error())
				continue
			}
			out = visionTextResponse(res.Text, img.Bounds())
		}
		if props {
			for k, v := range imageProperties(img) {
				out[k] = v
			}
		}
		responses = append(responses, out)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})
}

// visionTextResponse is an AnnotateImageResponse of text: empty, as
// Vision's, when there is none.
func visionTextResponse(text string, b image.Rectangle) map[string]any {
	if strings.TrimSpace(text) == "" {
		return map[string]any{}
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return map[string]any{
		"textAnnotations": []any{map[string]any{"description": text}},
		"fullTextAnnotation": map[string]any{
			"text":  text,
			"pages": []any{map[string]any{"width": b.Dx(), "height": b.Dy()}},
		},
	}
}
