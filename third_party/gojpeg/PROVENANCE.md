# Go's image/jpeg, extended to arithmetic coding and 12-bit samples

Source: Go's standard library `image/jpeg`, from the go1.27.4 toolchain (Homebrew's
`go 1.27.4`, `$(go env GOROOT)/src/image/jpeg`), by The Go Authors, BSD 3-Clause licence
(`LICENSE`, the toolchain's own).

CloudBurrow decodes JPEG images with it for Vertex AI EmbedContent (#1454, #1651). Go's decoder
refuses arithmetic-coded JPEG (SOF9, SOF10) and 12-bit sample precision, both of which Google's
embedding models accept, so its decoder is copied here (import path
`github.com/cloudburrow/cloudburrow/third_party/gojpeg`) and extended. Copied: `reader.go`,
`scan.go`, `huffman.go` and `dct.go`; left out: the encoder (`writer.go`) and the tests, which
read the standard library's shared `image/testdata`.

Changes (each marked "Modified by CloudBurrow" in the file):

- `reader.go`: SOF9 and SOF10 (arithmetic sequential and progressive) and DAC markers are read;
  SOF1, SOF2, SOF9 and SOF10 frames may have 12-bit precision (baseline SOF0 stays 8-bit only);
  a 12-bit image is returned by `output12`; `image/internal/imageutil.DrawYCbCr`, which a copy
  cannot import, is replaced by `drawYCbCr`.
- `scan.go`: a scan's components are a named type (`scanComp`); an arithmetic-coded scan's
  blocks are decoded by `arithBlock`, its statistics reset at the scan's start and each restart;
  a 12-bit block is stored by `store12`, and its image keeps 16-bit planes next to Go's 8-bit
  ones.
- `ext.go` (new, written for CloudBurrow from ITU-T T.81 | ISO/IEC 10918-1, its Table D.3 read
  from the 1992 recommendation): the arithmetic decoder and probability estimation (Annex D),
  the DC and AC statistical models with DAC conditioning (F.1.4.4, F.2.4), the progressive
  first and refinement passes (G.1.3), a floating-point IDCT for 12-bit blocks (A.3.3), and the
  12-bit output (Gray16, or RGBA64 through JFIF's YCbCr conversion, chroma replicated as Go's
  YCbCr image is).

Measured (`ext_test.go`): every arithmetic-coded fixture decodes to exactly the pixels Go's
`image/jpeg` decodes from its Huffman-coded twin (libjpeg-turbo 3.2.1's `jpegtran -arithmetic`
transcodes coefficients losslessly), sequential and progressive, 4:2:0, 4:2:2, 4:4:4 and
grayscale, with and without restart intervals; 12-bit fixtures in all four frame types decode
alike, within 1 (grayscale) and 2 (colour) twelve-bit steps of `djpeg -dct float -nosmooth`, and
within 1 and 3 of its default integer IDCT. The fixtures in `testdata/` were written by
libjpeg-turbo 3.2.1's `cjpeg` and `jpegtran` from ffmpeg's `mandelbrot` source and checked
against its `djpeg`, all run as tools; no libjpeg code is copied.

Still `UnsupportedError` (UNIMPLEMENTED in EmbedContent): lossless (SOF3, SOF11) and
hierarchical (SOF5 to SOF7, SOF13 to SOF15) frames, 12-bit baseline frames, and 12-bit CMYK or
YCbCrK.
