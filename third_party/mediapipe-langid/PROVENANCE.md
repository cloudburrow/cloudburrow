# mediapipe-langid: MediaPipe's Language Detector model

CloudBurrow identifies the language of text for BigQuery ML's `ML.TRANSLATE` (#1799):
`DETECT_LANGUAGE`, and `TRANSLATE_TEXT`'s `detected_language_code`. Google's Cloud Translation
detector is served only by the Translation API, which CloudBurrow never calls, so it uses the
language identification model Google publishes for on-device use, MediaPipe's Language Detector,
which `internal/langid` runs in pure Go on `internal/tflitemodel`. No cgo: the Linux CLI stays
static.

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| `language_detector.tflite` (with its `labels.txt`, 110 languages and `unknown`, zipped onto the FlatBuffer) | float32, version 1 (the latest the task page links, 2026-11-18) | https://storage.googleapis.com/mediapipe-models/language_detector/language_detector/float32/1/language_detector.tflite, linked from https://ai.google.dev/edge/mediapipe/solutions/text/language_detector | `7db4f23dfe1ad8966b050b419a865da451143fd43eb6b606a256aadeeb1e5417` (315294 bytes) | Apache-2.0, as its model card states (https://storage.googleapis.com/mediapipe-assets/LanguageDetector%20Model%20Card.pdf: authors Learn2Compress Team and Conceptual Understanding of Learning Architectures Team, Google Research; card by the MediaPipe Team, May 10, 2023) |

The model uses two MediaPipe custom operators, which `internal/langid/ops.go` ports to Go from
MediaPipe's sources (https://github.com/google-ai-edge/mediapipe,
`mediapipe/tasks/cc/text/language_detector/custom_ops/`: `ngram_hash.cc`,
`utils/ngram_hash_ops_utils.cc`, `utils/hash/murmur.cc` and `kmeans_embedding_lookup.cc`,
Copyright 2023 The MediaPipe Authors, Apache-2.0; read 2026-11-18). No MediaPipe code is
copied; the port follows the kernels' arithmetic. One difference: MediaPipe tests letters with
its Plan 9 `utf` library's `isalpharune`, the port with Go's `unicode.IsLetter`, which can
differ on a few runes.

The licence text is the Apache License 2.0, as in the repository's `LICENSE`.
