# govpx's VP9 decoder, for Go 1.26

Source: `github.com/thesyncim/govpx` v0.0.0-20261018202535-159dce5b931a (commit
`159dce5b931af4f7c600f157d64c2a813092ec57`, the Go module proxy's zip, go.sum
`h1:NIUp9Ir2/cvq/yY3XnNLQG68w4hxFoW2DnkBVu/lu8o=`), BSD 3-Clause (`LICENSE`), a pure Go port of
libvpx (`LICENSE.libvpx`, `PATENTS.libvpx`, `NOTICE`, all as upstream ships them).

CloudBurrow decodes VP9 video with it for Vertex AI EmbedContent (#1731). Upstream's `go.mod` says
`go 1.27.0`, and CloudBurrow builds with Go 1.26 too, so the module cannot be required; its decoder
is copied here (import path `github.com/cloudburrow/cloudburrow/third_party/govpx`; a `replace`
directive would make `go install ...@latest` refuse the module). It is standard library only: no
cgo, no assembly, no other module.

## What is copied

The root package's decoder files (`codec.go`, `doc.go`, `errors.go`, `image.go`, `options.go`,
`streaminfo.go`, `vp8_decoder.go`, `vp9_decoder*.go`, `vp9_superframe.go`) and the non-test files
of `internal/vp9/{tables,bitstream,common,dsp,decoder,rtp}`, `internal/vpx/{errors,buffers,rtp}`,
`internal/pixel` and `internal/vp8/{common,decoder,tables,boolcoder,dsp}` (VP9's post-processing is
built on the VP8 decoder's, and the VP9 decoder embeds its types). The import paths
`github.com/thesyncim/govpx/...` are rewritten to this directory's.

Left out: both encoders (`internal/vp8/encoder`, `internal/vp9/encoder`, every `vp8_encoder*` and
`vp9_encoder*` root file, rate control, SVC and the RTP packetizers), the root RTP, SDP, temporal and
timing files, `internal/{coracle,testutil,upstream}`, `internal/vpx/{arith,geometry,ivf,ratecontrol,conformance}`,
every file built only with `GOEXPERIMENT=simd` (upstream's only use of Go 1.27) or the
`govpx_oracle_trace`, `govpx_phase_stats` or `ignore` tags, upstream's own `third_party/goav1`
(used only by those SIMD files), the tests, `cmd/`, `testdata/`, `benchmarks/`, `docs/` and
`scripts/`.

## Changes, each marked `CloudBurrow:` in the file

1. **Build constraints removed** from the 35 portable files that carried
   `//go:build !goexperiment.simd || ... || purego` (in `internal/pixel`,
   `internal/vp9/dsp/intrapred_fixed_nosimd.go` and `transform_nosimd.go`,
   `internal/vp8/decoder/loopfilter_macro_internal_generic.go` and `internal/vp8/dsp`): their SIMD
   alternatives are not copied, so the plain Go versions always build, with no tag and under any
   `GOEXPERIMENT`. The architecture-only constraints of `internal/vp9/bitstream/load_*.go` are kept.
2. **`vp9_decoder_cloudburrow_shared.go`** (new) holds, verbatim, the declarations the decoder shares
   with upstream's encoder files, which are not copied, each under the upstream file and line range
   it comes from: the `runtime.procyield` linkname (`vp8_encoder.go`), the row-worker idle
   constants (`vp8_encoder_row_worker.go`), the `VP9RTPMax*` constants (`vp9_rtp.go`), the disabled
   phase-statistics stubs (`vp9_phase_stats_disabled.go`), `vp9IntraPredictWidth4x4`
   (`vp9_encoder_key_modes.go`), `vp9RowMTSync` (`vp9_encoder_tile_workers.go`) and `vp9LfSync`
   with the decoder's loop-filter row workers (`vp9_encoder_lf_workers.go`).

No Go 1.27 language feature or library function is used by what is copied, so nothing else is
changed: it builds and vets with Go 1.26.8 and 1.27.4, for darwin and linux on amd64 and arm64.

## Checked

Every frame of a libvpx two-pass VP9 clip with hidden alternate reference frames in superframes
(190 coded frames, 180 shown) is byte for byte ffmpeg 9's decode (`internal/embedding`'s
`TestVP9AndAV1MatchFFmpeg`, by each frame's MD5); also with 4 threads.

## Updating

Download the new version with `go mod download`, copy the same files, reapply the two changes,
record the version, sum and commit here and in `dependencies.json`, and run
`go test ./internal/embedding/ ./test/repo/`. Once CloudBurrow builds with Go 1.27 only, the
module can be required instead.
