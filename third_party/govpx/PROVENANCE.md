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

## `vp9hbd`: CloudBurrow's VP9 decoder for every profile

`vp9hbd` (import path `github.com/cloudburrow/cloudburrow/third_party/govpx/vp9hbd`) is not
copied from govpx: CloudBurrow wrote it, for VP9 profiles 1 to 3 (10 and 12 bits a sample, 4:2:2,
4:4:0 and 4:4:4), which govpx's decoder does not decode. It decodes profile 0 too. It follows the
normative decoding process of the *VP9 Bitstream & Decoding Process Specification* v0.6
(31 March 2016, Google; https://www.webmproject.org/vp9/), section by section (the code cites the
sections: 6 syntax, 7 semantics, 8 decoding process, 9 parsing, Annex B superframes), with every
sample a `uint16`. It is standard library only: no cgo, no assembly, no other module.

From govpx it reuses, by import and unchanged, the constant tables of `internal/vp9/tables`
(default and keyframe probabilities, scans, coefficient bands, Pareto table, quantizer lookups for
8, 10 and 12 bits, motion vector reference positions, interpolation kernels). `txfm1d.go` holds
govpx's 1-D inverse DCT and ADST (`internal/vp9/dsp`, ports of libvpx's `vpx_dsp/inv_txfm.c`,
BSD 3-Clause) widened from `int16` to `int32` arithmetic, as libvpx's high-bitdepth build computes
them. No GPL or LGPL code (ffmpeg's, libav's) was read; ffmpeg was only run, to make the test clips
and their reference decodes.

Where the specification text and libvpx differ, it does what libvpx (the reference decoder) does:

1. In 4:2:2 and 4:4:0 the chroma area of an 8x16 or 16x8 block is 4x16 or 16x4, for which
   `get_plane_block_size` gives `BLOCK_INVALID`; the plane's size in 4x4 units is computed from
   the block's size and the subsampling instead (libvpx's `n4_w`, `n4_h`), and the chroma
   transform size is the largest that fits it (libvpx's `uv_txsize_lookup`).
2. Spec typos: key and intra-only frames read partitions with the keyframe probabilities, and the
   loop filter level lookup applies the deltas when `loop_filter_delta_enabled` is 1.

It also refuses frames beyond level 6.2's size (16384 a side, 35651584 samples), references out of
the 7.2 scaling range or of another bit depth or subsampling, and turns any panic on corrupt data
into an error.

Checked by `go test ./third_party/govpx/vp9hbd/`: 23 clips in its `testdata` (one per pixel
format yuv420p, yuv422p, yuv440p, yuv444p, yuv420p10le, yuv420p12le, yuv422p10le, yuv444p12le,
yuv440p10le, with hidden alternate reference frames in superframes and compound prediction; two
tile columns; lossless; error resilient with segmentation; per-segment quantizers; a 17x9 frame;
`show_existing_frame`; a size change at a keyframe; 60 frames of `internal/embedding`'s turtle
clip; reference scaling and intra-only frames made by rewriting libvpx streams' headers) match
ffmpeg 9's decode frame for frame by MD5. The test file records the command that made each clip.
