package vertexai

import (
	"context"
	"encoding/base64"
	"image"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/imagedetect"
)

// TestObjectAnnotations: Vision's LocalizedObjectAnnotation shape, the
// name capitalised, four normalised corners clockwise from the top left
// with zero coordinates omitted, no mid.
func TestObjectAnnotations(t *testing.T) {
	got := objectAnnotations([]imagedetect.Object{{Name: "traffic light", Score: 0.75, Box: imagedetect.Box{XMin: 0, YMin: 0.25, XMax: 0.5, YMax: 1}}})
	a := got[0].(map[string]any)
	if a["name"] != "Traffic light" || a["score"] != float32(0.75) || a["mid"] != nil {
		t.Errorf("annotation %v", a)
	}
	want := []any{map[string]any{"y": float32(0.25)}, map[string]any{"x": float32(0.5), "y": float32(0.25)},
		map[string]any{"x": float32(0.5), "y": float32(1)}, map[string]any{"y": float32(1)}}
	if vs := a["boundingPoly"].(map[string]any)["normalizedVertices"]; !reflect.DeepEqual(vs, want) {
		t.Errorf("vertices %v", vs)
	}
}

// TestFaceAnnotations: Vision's FaceAnnotation shape in pixels: the
// detector's box as fdBoundingPoly, it scaled 1.5 times and clipped as
// boundingPoly, six landmarks, the roll of the eye line, and every
// likelihood UNKNOWN.
func TestFaceAnnotations(t *testing.T) {
	f := imagedetect.Face{Score: 0.9, Box: imagedetect.Box{XMin: 0.25, YMin: 0.25, XMax: 0.75, YMax: 0.75}}
	f.Keypoints = [6]imagedetect.Point{{X: 0.4, Y: 0.4}, {X: 0.6, Y: 0.6}, {X: 0.5, Y: 0.5}, {X: 0.5, Y: 0.6}, {X: 0.3, Y: 0.45}, {X: 0.7, Y: 0.45}}
	a := faceAnnotations([]imagedetect.Face{f}, image.Rect(0, 0, 200, 100))[0].(map[string]any)
	poly := func(k string) []any { return a[k].(map[string]any)["vertices"].([]any) }
	if fd := poly("fdBoundingPoly"); !reflect.DeepEqual(fd, []any{vertex(50, 25), vertex(150, 25), vertex(150, 75), vertex(50, 75)}) {
		t.Errorf("fdBoundingPoly %v", fd)
	}
	if b := poly("boundingPoly"); !reflect.DeepEqual(b, []any{vertex(25, 13), vertex(175, 13), vertex(175, 88), vertex(25, 88)}) {
		t.Errorf("boundingPoly %v", b)
	}
	marks := a["landmarks"].([]any)
	if len(marks) != 6 || marks[0].(map[string]any)["type"] != "LEFT_EYE" || marks[5].(map[string]any)["type"] != "RIGHT_EAR_TRAGION" {
		t.Errorf("landmarks %v", marks)
	}
	if p := marks[0].(map[string]any)["position"].(map[string]any); p["x"] != float32(80) || p["y"] != float32(40) || p["z"] != nil {
		t.Errorf("left eye %v", p)
	}
	// the eyes are 40 px apart across and 20 px down: atan(0.5)
	if r, _ := a["rollAngle"].(float32); r < 26.56 || r > 26.57 {
		t.Errorf("rollAngle %v", a["rollAngle"])
	}
	if a["detectionConfidence"] != float32(0.9) || a["panAngle"] != nil || a["tiltAngle"] != nil {
		t.Errorf("annotation %v", a)
	}
	for _, k := range faceLikelihoods {
		if a[k] != "UNKNOWN" {
			t.Errorf("%s %v", k, a[k])
		}
	}
	if len(faceLikelihoods) != 7 {
		t.Errorf("%d likelihoods", len(faceLikelihoods))
	}
}

// TestAnnotateImagesDetectionsWithoutModel: with no detector configured,
// OBJECT_LOCALIZATION and FACE_DETECTION are that image's UNIMPLEMENTED
// error.
func TestAnnotateImagesDetectionsWithoutModel(t *testing.T) {
	imagedetect.SetObjectSource(nil)
	imagedetect.SetFaceSource(nil)
	png, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	img := `{"content":"` + base64.StdEncoding.EncodeToString(png) + `"}`
	rs := annotate(t, `{"requests":[{"image":`+img+`,"features":[{"type":"OBJECT_LOCALIZATION"}]},{"image":`+img+`,"features":[{"type":"FACE_DETECTION"}]}]}`)
	for i, r := range rs {
		if e, _ := r["error"].(map[string]any); e["code"] != 12.0 || !strings.Contains(e["message"].(string), "detection model") {
			t.Errorf("response %d: %v", i, r)
		}
	}
}

// TestAnnotateImagesDetections runs both MediaPipe detectors
// (CLOUDBURROW_TEST_OBJECT_MODEL, CLOUDBURROW_TEST_FACE_MODEL) through
// images:annotate on image_big.png (one face, measured 2026-11-18, and no
// object) and, when CLOUDBURROW_TEST_MEDIAPIPE_IMAGES names a directory
// holding it, MediaPipe's cats_and_dogs.jpg with maxResults 3.
func TestAnnotateImagesDetections(t *testing.T) {
	op, fp := os.Getenv("CLOUDBURROW_TEST_OBJECT_MODEL"), os.Getenv("CLOUDBURROW_TEST_FACE_MODEL")
	if op == "" || fp == "" {
		t.Skip("set CLOUDBURROW_TEST_OBJECT_MODEL and CLOUDBURROW_TEST_FACE_MODEL to MediaPipe's efficientdet_lite0.tflite and blaze_face_short_range.tflite")
	}
	imagedetect.SetObjectSource(func(context.Context) (string, error) { return op, nil })
	imagedetect.SetFaceSource(func(context.Context) (string, error) { return fp, nil })
	t.Cleanup(func() { imagedetect.SetObjectSource(nil); imagedetect.SetFaceSource(nil) })
	content := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return `{"content":"` + base64.StdEncoding.EncodeToString(b) + `"}`
	}
	img := content("../../tflitemodel/testdata/image_big.png")
	rs := annotate(t, `{"requests":[{"image":`+img+`,"features":[{"type":"FACE_DETECTION"},{"type":"object_localization"}]}]}`)
	fa, _ := rs[0]["faceAnnotations"].([]any)
	if len(fa) != 1 {
		t.Fatalf("faces: %v", rs[0])
	}
	if c, _ := fa[0].(map[string]any)["detectionConfidence"].(float64); c < 0.565 || c > 0.567 {
		t.Errorf("face: %v", fa[0])
	}
	if oa, ok := rs[0]["localizedObjectAnnotations"]; ok { // none: omitted, as in proto3 JSON
		t.Errorf("objects: %v", oa)
	}
	dir := os.Getenv("CLOUDBURROW_TEST_MEDIAPIPE_IMAGES")
	if dir == "" {
		return
	}
	rs = annotate(t, `{"requests":[{"image":`+content(filepath.Join(dir, "cats_and_dogs.jpg"))+`,"features":[{"type":"OBJECT_LOCALIZATION","maxResults":3}]}]}`)
	oa, _ := rs[0]["localizedObjectAnnotations"].([]any)
	if len(oa) != 3 || oa[0].(map[string]any)["name"] != "Dog" || oa[2].(map[string]any)["name"] != "Cat" {
		t.Errorf("cats and dogs: %v", rs[0])
	}
}
