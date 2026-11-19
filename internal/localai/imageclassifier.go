package localai

// EfficientNetLite0Repo is where Google's MediaPipe publishes the
// EfficientNet-Lite0 image classifier: its storage.googleapis.com bucket
// and path, as the MediaPipe image classifier page lists it
// (https://ai.google.dev/edge/mediapipe/solutions/vision/image_classifier,
// read 2026-11-18).
const EfficientNetLite0Repo = "mediapipe-models/image_classifier/efficientnet_lite0"

// EfficientNetLite0 is MediaPipe's float32 EfficientNet-Lite0 image
// classifier (Apache-2.0, by Google), which Vision's LABEL_DETECTION runs
// locally (internal/imagelabel, #1799), approved by the maintainer
// 2026-11-18. It is a file download, not an API call: fetched once, the
// first time an image is labelled, into <state-dir>/models and checked
// against the SHA-256 pinned here, measured on the file downloaded on
// 2026-11-18. Its 1,000 labels are ImageNet's, carried in the file. It is
// not a catalogue entry: no Local models screen manages it, since
// LABEL_DETECTION fetches what it needs.
var EfficientNetLite0 = Model{
	ID:        "mediapipe-efficientnet-lite0-float32-1",
	Repo:      EfficientNetLite0Repo,
	Publisher: PublisherGoogle,
	Access:    AccessOpen,
	License:   "apache-2.0",
	Modality:  ModalityImageClassification,
	Artifact:  "efficientnet_lite0.tflite",
	Runtime:   "tflitemodel",
	Official:  true,
	Notes:     "MediaPipe's EfficientNet-Lite0 image classifier (Apache-2.0, Google), fetched on demand for Vision LABEL_DETECTION.",
	Pin: &Pin{
		Revision: "float32-1",
		BaseURL:  "https://storage.googleapis.com/" + EfficientNetLite0Repo + "/float32/1",
		Files: []PinnedFile{
			{Path: "efficientnet_lite0.tflite", Bytes: 18582189,
				SHA256: "6c7ab0a6e5dcbf38a8c33b960996a55a3b4300b36a018c4545801de3a3c8bde0"},
		},
	},
}
