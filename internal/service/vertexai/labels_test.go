package vertexai

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/imagelabel"
)

// TestLabelAnnotations: Vision's EntityAnnotation shape, most probable
// first, topicality equal to score, no mid, and maxResults (10 omitted).
func TestLabelAnnotations(t *testing.T) {
	var labels []imagelabel.Label
	for i := range 20 {
		labels = append(labels, imagelabel.Label{Description: string(rune('a' + i)), Score: float32(20-i) / 210})
	}
	for _, tc := range []struct{ max, want int }{{0, 10}, {-1, 10}, {3, 3}, {50, 20}} {
		got := labelAnnotations(labels, tc.max)
		if len(got) != tc.want {
			t.Fatalf("maxResults %d: %d labels, want %d", tc.max, len(got), tc.want)
		}
		first := got[0].(map[string]any)
		if first["description"] != "a" || first["score"] != first["topicality"] || first["mid"] != nil {
			t.Errorf("first label: %v", first)
		}
	}
}

// TestAnnotateImagesLabelsWithoutModel: with no label model configured,
// LABEL_DETECTION is that image's UNIMPLEMENTED error.
func TestAnnotateImagesLabelsWithoutModel(t *testing.T) {
	imagelabel.SetSource(nil)
	png, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	rs := annotate(t, `{"requests":[{"image":{"content":"`+base64.StdEncoding.EncodeToString(png)+`"},"features":[{"type":"LABEL_DETECTION"}]}]}`)
	if e, _ := rs[0]["error"].(map[string]any); e["code"] != 12.0 || !strings.Contains(e["message"].(string), "label model") {
		t.Errorf("no model: %v", rs[0])
	}
}

// TestAnnotateImagesLabels runs the model (CLOUDBURROW_TEST_LABEL_MODEL)
// through images:annotate: the scanned invoice's top label, maxResults,
// and LABEL_DETECTION with IMAGE_PROPERTIES in one request.
func TestAnnotateImagesLabels(t *testing.T) {
	path := os.Getenv("CLOUDBURROW_TEST_LABEL_MODEL")
	if path == "" {
		t.Skip("set CLOUDBURROW_TEST_LABEL_MODEL to MediaPipe's efficientnet_lite0.tflite to run the label model")
	}
	imagelabel.SetSource(func(context.Context) (string, error) { return path, nil })
	t.Cleanup(func() { imagelabel.SetSource(nil) })
	png, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	img := `{"content":"` + base64.StdEncoding.EncodeToString(png) + `"}`
	rs := annotate(t, `{"requests":[{"image":`+img+`,"features":[{"type":"LABEL_DETECTION"}]},`+
		`{"image":`+img+`,"features":[{"type":"label_detection","maxResults":3},{"type":"IMAGE_PROPERTIES"}]}]}`)
	la, _ := rs[0]["labelAnnotations"].([]any)
	if len(la) != 10 {
		t.Fatalf("default maxResults: %v", rs[0])
	}
	first := la[0].(map[string]any)
	if first["description"] != "web site" || first["score"] != first["topicality"] || first["mid"] != nil {
		t.Errorf("first label: %v", first)
	}
	la3, _ := rs[1]["labelAnnotations"].([]any)
	if len(la3) != 3 || la3[0].(map[string]any)["score"] != first["score"] || rs[1]["imagePropertiesAnnotation"] == nil {
		t.Errorf("maxResults 3 with IMAGE_PROPERTIES: %v", rs[1])
	}
}
