# go-mp3, extended to MPEG-2.5

Source: `github.com/hajimehoshi/go-mp3` v0.3.4 (commit `26f0b17459ab4b2b3d9d2b484a27a4eab3cca17b`,
2022-11-02, the Go module proxy's `v0.3.4.zip`, go.sum `h1:NUP7pBYH8OguP4diaTZ9wJbUbk3tC0KlfzsEpWmYj68=`),
by Hajime Hoshi and the authors in `AUTHORS`, Apache License 2.0 (`LICENSE`).

CloudBurrow decodes MP3 audio with it for Vertex AI EmbedContent (#1721). Upstream decodes MPEG-1
and MPEG-2 Layer III and refuses MPEG-2.5 (8, 11.025 and 12 kHz), and has had no release since
2022, so its decoder is copied here (import path
`github.com/cloudburrow/cloudburrow/third_party/go-mp3`) and extended (#1734). Copied: `decode.go`,
`source.go` and the non-test files of `internal/`; left out: the tests, `example/` and
`fuzzing_test.go`. The import paths `github.com/hajimehoshi/go-mp3/internal/...` are rewritten to
this directory's.

Changes (each marked "Modified by CloudBurrow" or "added by CloudBurrow" in the file, as the
Apache License asks):

- `internal/consts/consts.go`: `SfBandIndices` gains the MPEG-2.5 scalefactor bands (11.025, 12
  and 8 kHz; ffmpeg's `band_size_long` and `band_size_short`), and its comments name the sample
  rates the tables are for (upstream labels them as layers).
- `internal/frameheader/frameheader.go`: `SamplingFrequencyValue` quarters the MPEG-1 rates for
  MPEG-2.5; `VersionIndex`, `SfBandIndices` and `MixedLongBands` are added.
- `internal/frame/frame.go`: the MPEG-2.5 refusal is removed; the band tables come from the
  frame's version; a mixed block's long part is 6 bands (72 lines) at 8 kHz, whose bands are twice
  as wide, as ffmpeg's decoder takes it, and 8 bands (36 lines) otherwise.
- `internal/maindata/huffman.go`: the band tables come from the frame's version, and a short
  block's region 1 starts at the third short band (72 lines at 8 kHz, 36 otherwise), as ffmpeg's
  `init_short_region` does.
- `consts.UnexpectedEOF` literals name their field (`go vet`'s composites check, which applies now
  that the type is in the same module).

Checked against ffmpeg 9: MPEG-2.5 MP3s from libmp3lame at 8 kHz mono and 11.025 and 12 kHz joint
stereo, with start and short blocks, decode to ffmpeg's samples within one 16-bit step (mono) and
two a channel (stereo, upstream's float32 mid/side), `internal/embedding`'s
`TestDecodeMPEG25MP3`; the MPEG-1 and MPEG-2 tests are unchanged.

To update: copy the new version's sources the same way, reapply the changes above, record the
version, sum and commit in `dependencies.json`, and run `go test ./internal/embedding/`.
