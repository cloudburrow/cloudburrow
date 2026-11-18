package localai

// The MediaPipe detectors Vision's OBJECT_LOCALIZATION and FACE_DETECTION
// run locally (internal/imagedetect, #1799), approved by the maintainer
// 2026-11-18. Like EfficientNetLite0 they are file downloads, not API
// calls: fetched once, the first time an image needs one, into
// <state-dir>/models and checked against the SHA-256 pinned here, measured
// on the files downloaded on 2026-11-18. They are not catalogue entries.

// ModalityObjectDetection finds objects or faces in an image.
const ModalityObjectDetection Modality = "object-detection"

// EfficientDetLite0Repo is where MediaPipe publishes the EfficientDet-Lite0
// object detector, as the MediaPipe object detector page lists it
// (https://ai.google.dev/edge/mediapipe/solutions/vision/object_detector,
// read 2026-11-18).
const EfficientDetLite0Repo = "mediapipe-models/object_detector/efficientdet_lite0"

// EfficientDetLite0 is MediaPipe's float32 EfficientDet-Lite0 object
// detector (Apache-2.0, by Google), trained on COCO: its labels are COCO's
// 80 categories, carried in the file.
var EfficientDetLite0 = Model{
	ID:        "mediapipe-efficientdet-lite0-float32-1",
	Repo:      EfficientDetLite0Repo,
	Publisher: PublisherGoogle,
	Access:    AccessOpen,
	License:   "apache-2.0",
	Modality:  ModalityObjectDetection,
	Artifact:  "efficientdet_lite0.tflite",
	Runtime:   "tflitemodel",
	Official:  true,
	Notes:     "MediaPipe's EfficientDet-Lite0 object detector (Apache-2.0, Google), fetched on demand for Vision OBJECT_LOCALIZATION.",
	Pin: &Pin{
		Revision: "float32-1",
		BaseURL:  "https://storage.googleapis.com/" + EfficientDetLite0Repo + "/float32/1",
		Files: []PinnedFile{
			{Path: "efficientdet_lite0.tflite", Bytes: 13836895,
				SHA256: "40338edf5ec70d43e318b0a716a84d4564cd1802759a7a07170c7e43796dbf58"},
		},
	},
}

// BlazeFaceShortRangeRepo is where MediaPipe publishes the BlazeFace
// short-range face detector, as the MediaPipe face detector page lists it
// (https://ai.google.dev/edge/mediapipe/solutions/vision/face_detector,
// read 2026-11-18); MediaPipe publishes it in float16 only.
const BlazeFaceShortRangeRepo = "mediapipe-models/face_detector/blaze_face_short_range"

// BlazeFaceShortRange is MediaPipe's BlazeFace short-range face detector
// (float16, Apache-2.0, by Google).
var BlazeFaceShortRange = Model{
	ID:        "mediapipe-blaze-face-short-range-float16-1",
	Repo:      BlazeFaceShortRangeRepo,
	Publisher: PublisherGoogle,
	Access:    AccessOpen,
	License:   "apache-2.0",
	Modality:  ModalityObjectDetection,
	Artifact:  "blaze_face_short_range.tflite",
	Runtime:   "tflitemodel",
	Official:  true,
	Notes:     "MediaPipe's BlazeFace short-range face detector (Apache-2.0, Google), fetched on demand for Vision FACE_DETECTION.",
	Pin: &Pin{
		Revision: "float16-1",
		BaseURL:  "https://storage.googleapis.com/" + BlazeFaceShortRangeRepo + "/float16/1",
		Files: []PinnedFile{
			{Path: "blaze_face_short_range.tflite", Bytes: 229746,
				SHA256: "b4578f35940bf5a1a655214a1cce5cab13eba73c1297cd78e1a04c2380b0152f"},
		},
	},
}
