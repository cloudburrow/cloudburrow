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

// TestDetectorsArePinned: each detector is one file, at MediaPipe's own
// URL on storage.googleapis.com, with its SHA-256 and size.
func TestDetectorsArePinned(t *testing.T) {
	for _, c := range []struct {
		m     Model
		bytes int64
		url   string
	}{
		{EfficientDetLite0, 13836895, "https://storage.googleapis.com/mediapipe-models/object_detector/efficientdet_lite0/float32/1/efficientdet_lite0.tflite"},
		{BlazeFaceShortRange, 229746, "https://storage.googleapis.com/mediapipe-models/face_detector/blaze_face_short_range/float16/1/blaze_face_short_range.tflite"},
	} {
		m := c.m
		if !m.Official || m.Publisher != PublisherGoogle || m.License != "apache-2.0" || m.Pin == nil || len(m.Pin.Files) != 1 {
			t.Fatalf("%s: %+v", m.ID, m)
		}
		f := m.Pin.Files[0]
		if len(f.SHA256) != 64 || f.Bytes != c.bytes {
			t.Errorf("%s: incomplete pin: %+v", m.ID, f)
		}
		if got := m.Pin.URL(m.Repo, f); got != c.url {
			t.Errorf("%s: URL %s", m.ID, got)
		}
	}
}

// TestSpeakerModelIsPinned: 3D-Speaker's CAM++ is one ONNX file, at
// sherpa-onnx's GitHub release, with its SHA-256 and size, a community
// export under Apache-2.0 run on ONNX Runtime.
func TestSpeakerModelIsPinned(t *testing.T) {
	m := SpeakerCAMPlusPlus
	if m.Official || m.Publisher != PublisherCommunity || m.Access != AccessOpen || m.License != "apache-2.0" ||
		m.Runtime != RuntimeONNX || m.Pin == nil || len(m.Pin.Files) != 1 {
		t.Fatalf("SpeakerCAMPlusPlus: %+v", m)
	}
	f := m.Pin.Files[0]
	if len(f.SHA256) != 64 || f.Bytes != 28281165 {
		t.Errorf("incomplete pin: %+v", f)
	}
	if got := m.Pin.URL(m.Repo, f); got != "https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx" {
		t.Errorf("URL %s", got)
	}
}
