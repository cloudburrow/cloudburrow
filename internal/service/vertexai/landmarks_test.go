package vertexai

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/imagelandmark"
)

// TestLandmarkAnnotations: description and score, at most maxResults, 10
// when it is omitted or negative; no mid, locations or boundingPoly.
func TestLandmarkAnnotations(t *testing.T) {
	var ls []imagelandmark.Landmark
	for i := range 12 {
		ls = append(ls, imagelandmark.Landmark{Description: string(rune('A' + i)), Score: 0.99 - float32(i)/100})
	}
	if got := landmarkAnnotations(ls, 0); len(got) != 10 {
		t.Errorf("default: %d", len(got))
	}
	if got := landmarkAnnotations(ls, -1); len(got) != 10 {
		t.Errorf("negative: %d", len(got))
	}
	got := landmarkAnnotations(ls, 2)
	if len(got) != 2 {
		t.Fatalf("maxResults 2: %v", got)
	}
	a := got[0].(map[string]any)
	if a["description"] != "A" || a["score"] != float32(0.99) || len(a) != 2 {
		t.Errorf("first: %v", a)
	}
	if got := landmarkAnnotations(nil, 0); len(got) != 0 {
		t.Errorf("none: %v", got)
	}
}

// TestAnnotateImagesLandmarksWithoutModel: with no landmark models
// configured, LANDMARK_DETECTION is that image's UNIMPLEMENTED error.
func TestAnnotateImagesLandmarksWithoutModel(t *testing.T) {
	imagelandmark.SetSource(nil)
	png, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	rs := annotate(t, `{"requests":[{"image":{"content":"`+base64.StdEncoding.EncodeToString(png)+`"},"features":[{"type":"LANDMARK_DETECTION"}]}]}`)
	if e, _ := rs[0]["error"].(map[string]any); e["code"] != 12.0 || !strings.Contains(e["message"].(string), "landmark model") {
		t.Errorf("response: %v", rs[0])
	}
}

// TestAnnotateImagesLandmarks runs Google's six landmark classifiers
// (CLOUDBURROW_TEST_LANDMARK_MODELS, as internal/imagelandmark's test
// takes them) through images:annotate on the scanned page with
// maxResults 1, and, when CLOUDBURROW_TEST_LANDMARK_IMAGES names their
// directory, on the Golden Gate Bridge photograph.
func TestAnnotateImagesLandmarks(t *testing.T) {
	dir := os.Getenv("CLOUDBURROW_TEST_LANDMARK_MODELS")
	if dir == "" {
		t.Skip("set CLOUDBURROW_TEST_LANDMARK_MODELS to a directory of Google's landmarks_classifier_<continent>_v1.tflite")
	}
	imagelandmark.SetSource(func(context.Context) ([]string, error) {
		var ps []string
		for _, r := range []string{"africa", "asia", "europe", "north-america", "oceania-antarctica", "south-america"} {
			ps = append(ps, filepath.Join(dir, "landmarks_classifier_"+r+"_v1.tflite"))
		}
		return ps, nil
	})
	t.Cleanup(func() { imagelandmark.SetSource(nil) })
	content := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return `{"content":"` + base64.StdEncoding.EncodeToString(b) + `"}`
	}
	rs := annotate(t, `{"requests":[{"image":`+content("../../ocr/testdata/scan.png")+`,"features":[{"type":"landmark_detection","maxResults":1}]}]}`)
	la, _ := rs[0]["landmarkAnnotations"].([]any)
	if len(la) != 1 {
		t.Fatalf("scanned page: %v", rs[0])
	}
	imgs := os.Getenv("CLOUDBURROW_TEST_LANDMARK_IMAGES")
	if imgs == "" {
		return
	}
	rs = annotate(t, `{"requests":[{"image":`+content(filepath.Join(imgs, "ggb.jpg"))+`,"features":[{"type":"LANDMARK_DETECTION"}]}]}`)
	la, _ = rs[0]["landmarkAnnotations"].([]any)
	if len(la) == 0 || len(la) > 10 || la[0].(map[string]any)["description"] != "Golden Gate Bridge" {
		t.Errorf("Golden Gate Bridge: %v", rs[0])
	}
}
