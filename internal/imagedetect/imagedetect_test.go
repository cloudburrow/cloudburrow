package imagedetect

import (
	"context"
	"errors"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "golang.org/x/image/webp"
)

func TestIoU(t *testing.T) {
	a := Box{0, 0, 2, 2}
	if got := iou(a, Box{1, 1, 3, 3}); math.Abs(float64(got)-1.0/7) > 1e-6 {
		t.Errorf("overlap %v, want 1/7", got)
	}
	if got := iou(a, Box{2, 0, 4, 2}); got != 0 {
		t.Errorf("touching boxes %v", got)
	}
	if got := iou(a, a); got != 1 {
		t.Errorf("same box %v", got)
	}
}

// TestNMS: the DEFAULT algorithm keeps the most probable of overlapping
// boxes, keeps boxes that overlap by at most the threshold, and stops at
// max.
func TestNMS(t *testing.T) {
	ds := []detection{
		{box: Box{0, 0, 1, 1}, score: 0.6, class: 1},
		{box: Box{0, 0, 1, 0.9}, score: 0.9, class: 2},
		{box: Box{2, 2, 3, 3}, score: 0.7, class: 3},
		{box: Box{0.5, 0, 1.5, 1}, score: 0.8, class: 4}, // IoU 1/3 with the first two
	}
	var classes []int
	for _, d := range nms(append([]detection(nil), ds...), 0.3, 0) {
		classes = append(classes, d.class)
	}
	if !reflect.DeepEqual(classes, []int{2, 3}) {
		t.Errorf("IoU 0.3: %v", classes)
	}
	classes = nil
	for _, d := range nms(append([]detection(nil), ds...), 0.5, 2) {
		classes = append(classes, d.class)
	}
	if !reflect.DeepEqual(classes, []int{2, 4}) {
		t.Errorf("IoU 0.5, max 2: %v", classes)
	}
}

// TestWeightedNMS: overlapping boxes merge into their score-weighted
// mean with the best score; a box apart stays.
func TestWeightedNMS(t *testing.T) {
	ds := []detection{
		{box: Box{0, 0, 1, 1}, score: 0.25, keypoints: []Point{{0, 0}}},
		{box: Box{0.1, 0, 1.1, 1}, score: 0.75, keypoints: []Point{{1, 1}}},
		{box: Box{5, 5, 6, 6}, score: 0.5, keypoints: []Point{{5, 5}}},
	}
	out := weightedNMS(ds, 0.5)
	if len(out) != 2 {
		t.Fatalf("%d detections", len(out))
	}
	m := out[0]
	if m.score != 0.75 || math.Abs(float64(m.box.XMin-0.075)) > 1e-6 || math.Abs(float64(m.box.XMax-1.075)) > 1e-6 ||
		m.keypoints[0] != (Point{0.75, 0.75}) {
		t.Errorf("merged %+v", m)
	}
	if out[1].box != (Box{5, 5, 6, 6}) {
		t.Errorf("apart %+v", out[1])
	}
}

// TestFaceAnchors: the short-range options give 896 anchors, two a cell
// on the 16×16 map, then six a cell on the 8×8 map.
func TestFaceAnchors(t *testing.T) {
	a := faceAnchors()
	if len(a) != 896 {
		t.Fatalf("%d anchors", len(a))
	}
	for i, want := range map[int]anchor{
		0: {1.0 / 32, 1.0 / 32, 1, 1}, 1: {1.0 / 32, 1.0 / 32, 1, 1}, 2: {3.0 / 32, 1.0 / 32, 1, 1},
		511: {31.0 / 32, 31.0 / 32, 1, 1}, 512: {1.0 / 16, 1.0 / 16, 1, 1}, 517: {1.0 / 16, 1.0 / 16, 1, 1},
		518: {3.0 / 16, 1.0 / 16, 1, 1}, 895: {15.0 / 16, 15.0 / 16, 1, 1},
	} {
		if a[i] != want {
			t.Errorf("anchor %d %+v, want %+v", i, a[i], want)
		}
	}
}

// TestImageInputLetterbox: a wide white image is centred in the square,
// its padding -1 (pixel value 0) and the image 1.
func TestImageInputLetterbox(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := range 4 {
		for x := range 8 {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	in, padX, padY := imageInput(img, 8, true)
	if padX != 0 || padY != 0.25 {
		t.Fatalf("padding %v %v", padX, padY)
	}
	at := func(x, y int) float32 { return in[(y*8+x)*3] }
	if at(3, 0) != -1 || at(3, 7) != -1 || at(3, 3) != 1 || at(0, 4) != 1 {
		t.Errorf("rows: %v %v %v %v", at(3, 0), at(3, 7), at(3, 3), at(0, 4))
	}
	if got := unletter(0.25, padY); got != 0 {
		t.Errorf("unletter top %v", got)
	}
	if got := unletter(0.75, padY); got != 1 {
		t.Errorf("unletter bottom %v", got)
	}
	st, _, _ := imageInput(img, 8, false)
	for i, v := range st {
		if v != 1 {
			t.Fatalf("stretched input %d is %v", i, v)
		}
	}
}

func TestDefaultWithoutSource(t *testing.T) {
	SetObjectSource(nil)
	SetFaceSource(nil)
	if _, err := DefaultObjects(context.Background()); !errors.Is(err, ErrNoModel) {
		t.Errorf("objects: %v", err)
	}
	if _, err := DefaultFaces(context.Background()); !errors.Is(err, ErrNoModel) {
		t.Errorf("faces: %v", err)
	}
	SetFaceSource(func(context.Context) (string, error) { return "", errors.New("offline") })
	if _, err := DefaultFaces(context.Background()); !errors.Is(err, ErrFetch) {
		t.Errorf("failed fetch: %v", err)
	}
	SetFaceSource(nil)
}

// loadModels loads the MediaPipe detectors that
// CLOUDBURROW_TEST_OBJECT_MODEL (efficientdet_lite0.tflite, float32 1) and
// CLOUDBURROW_TEST_FACE_MODEL (blaze_face_short_range.tflite, float16 1)
// name, or skips: the files are fetched on demand, not kept in the repo.
func loadModels(t *testing.T) (*ObjectDetector, *FaceDetector) {
	t.Helper()
	op, fp := os.Getenv("CLOUDBURROW_TEST_OBJECT_MODEL"), os.Getenv("CLOUDBURROW_TEST_FACE_MODEL")
	if op == "" || fp == "" {
		t.Skip("CLOUDBURROW_TEST_OBJECT_MODEL and CLOUDBURROW_TEST_FACE_MODEL name MediaPipe's efficientdet_lite0.tflite and blaze_face_short_range.tflite")
	}
	read := func(p string) []byte {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	od, err := LoadObjectDetector(read(op))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := LoadFaceDetector(read(fp))
	if err != nil {
		t.Fatal(err)
	}
	return od, fd
}

func decode(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func near(a, b, tol float32) bool { return math.Abs(float64(a-b)) <= float64(tol) }

// TestObjectDetectorLabels: COCO's 90 category IDs, 80 of them named.
func TestObjectDetectorLabels(t *testing.T) {
	od, _ := loadModels(t)
	named := 0
	for _, l := range od.Labels() {
		if l != unnamedLabel {
			named++
		}
	}
	if len(od.Labels()) != 90 || named != 80 || od.Labels()[0] != "person" || len(od.anchors) != 19206 {
		t.Errorf("%d labels, %d named, first %q, %d anchors", len(od.Labels()), named, od.Labels()[0], len(od.anchors))
	}
}

// TestDetectFixedImages runs both detectors on the repo's fixed images,
// whose answers were measured on 2026-11-18: no object and no face in the
// scanned invoice or the WebP scene, and one face (a false positive, kept
// as measured) in tflitemodel's image_big.png. The answer is the same
// twice.
func TestDetectFixedImages(t *testing.T) {
	od, fd := loadModels(t)
	for _, c := range []struct {
		path  string
		faces int
	}{
		{"../ocr/testdata/scan.png", 0},
		{"../../test/compat/testdata/scene-lossy.webp", 0},
		{"../tflitemodel/testdata/image_big.png", 1},
	} {
		img := decode(t, c.path)
		objs, err := od.Detect(img, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != 0 {
			t.Errorf("%s: objects %+v", c.path, objs)
		}
		fs, err := fd.Detect(img, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) != c.faces {
			t.Fatalf("%s: faces %+v", c.path, fs)
		}
		again, _ := fd.Detect(img, 0)
		if !reflect.DeepEqual(fs, again) {
			t.Errorf("%s: not deterministic", c.path)
		}
	}
	fs, _ := fd.Detect(decode(t, "../tflitemodel/testdata/image_big.png"), 0)
	if f := fs[0]; !near(f.Score, 0.5662, 1e-3) || !near(f.Box.XMin, 0.0705, 1e-3) || !near(f.Box.YMax, 1.1257, 1e-3) {
		t.Errorf("image_big.png face %+v", f)
	}
}

// TestMediaPipeImages runs both detectors on MediaPipe's own test images
// (storage.googleapis.com/mediapipe-assets/portrait.jpg and
// cats_and_dogs.jpg, not kept in the repo: their licence is not stated)
// when CLOUDBURROW_TEST_MEDIAPIPE_IMAGES names a directory holding them.
// The portrait's face is checked against MediaPipe's expected detection
// for its short-range model (mediapipe/tasks/testdata/vision/
// portrait_expected_detection.pbtxt: a 234×234 box at 283,115 of the
// 820×1024 image, and its keypoints), within 0.02 of the image; MediaPipe
// checks its keypoints within 0.01, and the measured differences, up to
// 0.016, are UNVERIFIED (JPEG decoding or resampling; MediaPipe's test
// re-read 2026-11-18). The objects were
// measured on 2026-11-18.
func TestMediaPipeImages(t *testing.T) {
	dir := os.Getenv("CLOUDBURROW_TEST_MEDIAPIPE_IMAGES")
	if dir == "" {
		t.Skip("CLOUDBURROW_TEST_MEDIAPIPE_IMAGES names a directory with MediaPipe's portrait.jpg and cats_and_dogs.jpg")
	}
	od, fd := loadModels(t)
	portrait := decode(t, filepath.Join(dir, "portrait.jpg"))
	fs, err := fd.Detect(portrait, 0)
	if err != nil || len(fs) != 1 {
		t.Fatalf("portrait faces %+v %v", fs, err)
	}
	f := fs[0]
	want := Box{283.0 / 820, 115.0 / 1024, (283.0 + 234) / 820, (115.0 + 234) / 1024}
	if !near(f.Box.XMin, want.XMin, 0.02) || !near(f.Box.YMin, want.YMin, 0.02) || !near(f.Box.XMax, want.XMax, 0.02) || !near(f.Box.YMax, want.YMax, 0.02) {
		t.Errorf("portrait box %+v, MediaPipe's %+v", f.Box, want)
	}
	for i, p := range []Point{{0.44416338, 0.17643969}, {0.55514044, 0.17731678}, {0.5046702, 0.2265771},
		{0.50227845, 0.2719954}, {0.36063305, 0.20143759}, {0.6084143, 0.20409837}} {
		if !near(f.Keypoints[i].X, p.X, 0.02) || !near(f.Keypoints[i].Y, p.Y, 0.02) {
			t.Errorf("portrait keypoint %d %+v, MediaPipe's %+v", i, f.Keypoints[i], p)
		}
	}
	objs, err := od.Detect(portrait, 0)
	if err != nil || len(objs) != 2 || objs[0].Name != "person" || !near(objs[0].Score, 0.9513, 1e-3) || objs[1].Name != "tie" {
		t.Errorf("portrait objects %+v %v", objs, err)
	}
	objs, err = od.Detect(decode(t, filepath.Join(dir, "cats_and_dogs.jpg")), 0)
	var names []string
	for _, o := range objs {
		names = append(names, o.Name)
	}
	if err != nil || !reflect.DeepEqual(names, []string{"dog", "dog", "cat", "cat"}) || !near(objs[0].Score, 0.7154, 1e-3) {
		t.Errorf("cats and dogs %+v %v", objs, err)
	}
	if objs, _ := od.Detect(decode(t, filepath.Join(dir, "cats_and_dogs.jpg")), 2); len(objs) != 2 {
		t.Errorf("max 2: %d objects", len(objs))
	}
}
