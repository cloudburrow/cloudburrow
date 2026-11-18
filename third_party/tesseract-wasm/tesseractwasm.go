// Package tesseractwasm holds Tesseract OCR compiled to a WASI WebAssembly
// module (tesseract.wasm, built by build.sh from the pinned Tesseract and
// Leptonica releases) and the tessdata_fast language data CloudBurrow
// embeds: English and orientation and script detection. internal/ocr runs
// the module on wazero. PROVENANCE.md records every input.
package tesseractwasm

import "embed"

// Module is tesseract.wasm.
//
//go:embed tesseract.wasm
var Module []byte

// Tessdata holds tessdata/eng.traineddata.gz and tessdata/osd.traineddata.gz:
// tessdata_fast 4.1.0's files, gzipped (gzip -9 -n) to keep each tracked
// file under 5 MB (test/repo's TestNoLargeTrackedFiles).
//
//go:embed tessdata/eng.traineddata.gz tessdata/osd.traineddata.gz
var Tessdata embed.FS

// TessdataSHA256 is each uncompressed file's SHA-256, as published.
var TessdataSHA256 = map[string]string{
	"eng.traineddata": "7d4322bd2a7749724879683fc3912cb542f19906c83bcc1a52132556427170b2",
	"osd.traineddata": "9cf5d576fcc47564f11265841e5ca839001e7e6f38ff7f7aacf46d15a96b00ff",
}
