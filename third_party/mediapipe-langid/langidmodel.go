// Package langidmodel holds MediaPipe's Language Detector model
// (language_detector.tflite, Google's, Apache-2.0), which internal/langid
// runs in pure Go. PROVENANCE.md records its source and digest.
package langidmodel

import _ "embed"

// Model is language_detector.tflite, with its labels.txt zipped on.
//
//go:embed language_detector.tflite
var Model []byte
