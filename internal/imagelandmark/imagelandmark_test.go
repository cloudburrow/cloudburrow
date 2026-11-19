package imagelandmark

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestMerge: each name's best score over every model, from Threshold,
// most probable first, equal scores by name.
func TestMerge(t *testing.T) {
	got := merge([]map[string]float32{
		{"Golden Gate Bridge": 0.99, "Alcatraz": 0.71, "Coit Tower": 0.69},
		{"Golden Gate Bridge": 0.5, "Sunshine Plaza": 0.98, "Bora Bora": 0.71},
	})
	want := []Landmark{{"Golden Gate Bridge", 0.99}, {"Sunshine Plaza", 0.98}, {"Alcatraz", 0.71}, {"Bora Bora", 0.71}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestReadLabels: the English list from the zip appended to the model,
// one label a line in output order, a trailing newline or none.
func TestReadLabels(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("TFL3 model bytes")
	zw := zip.NewWriter(&buf)
	zw.SetOffset(int64(buf.Len()))
	w, _ := zw.Create(labelsFile)
	_, _ = w.Write([]byte("Mount Rushmore National Memorial\r\nOld Faithful\nGolden Gate Bridge"))
	_ = zw.Close()
	got, err := readLabels(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "Mount Rushmore National Memorial" || got[2] != "Golden Gate Bridge" {
		t.Errorf("labels %q", got)
	}
	if _, err := readLabels([]byte("no zip")); err == nil {
		t.Error("a model without labels was accepted")
	}
}

// TestDefaultWithoutSource: no source is ErrNoModel; a failed fetch is
// ErrFetch, and retried.
func TestDefaultWithoutSource(t *testing.T) {
	t.Cleanup(func() { SetSource(nil) })
	SetSource(nil)
	if _, err := Default(context.Background()); !errors.Is(err, ErrNoModel) {
		t.Fatalf("no source: %v", err)
	}
	calls := 0
	SetSource(func(context.Context) ([]string, error) { calls++; return nil, errors.New("offline") })
	for range 2 {
		if _, err := Default(context.Background()); !errors.Is(err, ErrFetch) {
			t.Fatalf("failed fetch: %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("%d fetches, want a retry", calls)
	}
}

// TestRecognizeFixedImages runs Google's six landmark classifiers
// (CLOUDBURROW_TEST_LANDMARK_MODELS, a directory holding each
// landmarks_classifier_<continent>_v1.tflite) on photographs not kept in
// the repo (CLOUDBURROW_TEST_LANDMARK_IMAGES: Wikimedia Commons'
// GoldenGateBridge-001.jpg, Statue_of_Liberty_7.jpg and
// Tour_Eiffel_Wikimedia_Commons_(cropped).jpg at 960 pixels wide, saved as
// ggb.jpg, liberty.jpg and eiffel.jpg), and on the repo's scanned page:
// the measured top landmark of each (2026-11-18), the page's a false one.
func TestRecognizeFixedImages(t *testing.T) {
	dir := os.Getenv("CLOUDBURROW_TEST_LANDMARK_MODELS")
	if dir == "" {
		t.Skip("set CLOUDBURROW_TEST_LANDMARK_MODELS to a directory of Google's landmarks_classifier_<continent>_v1.tflite")
	}
	var cs []*Classifier
	for _, r := range []string{"africa", "asia", "europe", "north-america", "oceania-antarctica", "south-america"} {
		data, err := os.ReadFile(filepath.Join(dir, "landmarks_classifier_"+r+"_v1.tflite"))
		if err != nil {
			t.Fatal(err)
		}
		c, err := Load(data)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		cs = append(cs, c)
	}
	top := func(path string) Landmark {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		img, _, err := image.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		ls, err := Recognize(cs, img)
		if err != nil {
			t.Fatal(err)
		}
		if len(ls) == 0 {
			t.Fatalf("%s: no landmark", path)
		}
		for i := 1; i < len(ls); i++ {
			if ls[i].Score > ls[i-1].Score || ls[i].Score < Threshold {
				t.Fatalf("%s: order or threshold: %v", path, ls[:i+1])
			}
		}
		return ls[0]
	}
	// The classifiers cannot tell a document from a landmark (the gap
	// docs/gaps.md names): the page's best is a museum, at 0.950.
	if l := top("../ocr/testdata/scan.png"); l.Description != "Museum of Archeology and Ethnology USP" || l.Score < 0.94 || l.Score > 0.96 {
		t.Errorf("scanned page: %v", l)
	}
	imgs := os.Getenv("CLOUDBURROW_TEST_LANDMARK_IMAGES")
	if imgs == "" {
		return
	}
	for file, want := range map[string]string{"ggb.jpg": "Golden Gate Bridge", "liberty.jpg": "Liberty Island", "eiffel.jpg": "Eiffel Tower"} {
		if l := top(filepath.Join(imgs, file)); l.Description != want {
			t.Errorf("%s: %v, want %s", file, l, want)
		}
	}
}
