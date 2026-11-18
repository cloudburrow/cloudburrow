package localai

import "testing"

// TestEfficientNetLite0IsPinned: one file, at MediaPipe's own URL on
// storage.googleapis.com, with its SHA-256 and size.
func TestEfficientNetLite0IsPinned(t *testing.T) {
	m := EfficientNetLite0
	if !m.Official || m.Publisher != PublisherGoogle || m.License != "apache-2.0" || m.Pin == nil || len(m.Pin.Files) != 1 {
		t.Fatalf("EfficientNetLite0: %+v", m)
	}
	f := m.Pin.Files[0]
	if len(f.SHA256) != 64 || f.Bytes != 18582189 {
		t.Errorf("incomplete pin: %+v", f)
	}
	if got := m.Pin.URL(m.Repo, f); got != "https://storage.googleapis.com/mediapipe-models/image_classifier/efficientnet_lite0/float32/1/efficientnet_lite0.tflite" {
		t.Errorf("URL %s", got)
	}
}
