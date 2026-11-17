# aac-go, copied

Source: `github.com/arabian9ts/aac-go` v0.1.0 (commit `c4cc4c5fae73af12eb0a54aa745f10d105c825f6`,
tagged 2026-08-11; the Go module proxy's zip, go.sum `h1:e298pHWu+Xcv6VPU2hmS1UTmOq5C9KHD38eQcJR4K8c=`),
Apache License 2.0 (`LICENSE`, `NOTICE`, as upstream ships them). A pure Go AAC-LC encoder and
decoder, standard library only.

CloudBurrow decodes a video's AAC-LC audio track with it for Vertex AI EmbedContent's
`audio_track_extraction` (#1732, `internal/embedding/videoaudio.go`). It was required as a module
until #1739, which copied it here so CloudBurrow owns its fixes: upstream has one release and no
activity since, and the decoder is where HE-AAC (SBR, PS) would be added.

## What is copied

Every non-test Go file of the module root, `adts`, `wav` and `internal/{bits,coder,tables,transform}`,
and `LICENSE` and `NOTICE`. Left out: `cmd/` (its `aacenc` and `aacdec` tools), `scripts/`,
`testdata/`, the tests and `README.md`.

## Changes

The import paths `github.com/arabian9ts/aac-go/...` are rewritten to
`github.com/cloudburrow/cloudburrow/third_party/aac-go/...`.

### HE-AAC v1 and v2 decoding (#1739)

New files, written for CloudBurrow from ISO/IEC 14496-3 (§4.6.18 SBR, §8.6.4 PS):

- `hev2.go`: `ParseASC` (an AudioSpecificConfig: object types 2, 5 and 29, the GASpecificConfig,
  and the backward-compatible `0x2b7`/`0x548` SBR and PS extensions), `NewDecoderASC`, `DecodeAU`,
  `OutputSampleRate`, `OutputChannels`, and running SBR and PS after the core.
- `sbr.go`: `sbr_extension_data()` parsing (header, SCE and CPE, coupling, grid, delta coding,
  inverse filtering, envelopes, noise floors, sinusoids, the PS extension), and the master, high,
  low, noise and limiter band tables and the patches.
- `sbr_dsp.go`: the 32-band analysis and 64-band synthesis QMF banks, dequantisation (with
  coupling), HF generation (covariance LPC, chirp factors) and HF adjustment (limiter, gain boost,
  noise, sinusoids, smoothing).
- `ps.go`: parametric stereo with 10 or 20 stereo bands: IID (coarse and fine) and ICC, the 20-band
  hybrid filterbank, decorrelation (fractional-delay all-pass, delays, transient ducking) and
  mixing procedure A with time interpolation.
- `sbr_huff.go`, `sbr_tables.go`: the Huffman trees and tables. The tables come from PacketVideo's
  OpenCORE AAC decoder, Apache License 2.0, as Android ships it
  (https://android.googlesource.com/platform/frameworks/av/+/android-4.1.1_r1/media/libstagefright/codecs/aacdec/):
  the SBR Huffman trees from `sbr_code_book_envlevel.cpp`, the PS ones from `ps_read_data.cpp`,
  the noise table from `sbr_envelope_calc_tbl.cpp` (Q15), and the QMF window from
  `qmf_filterbank_coeff.cpp`, whose 155 even coefficients give the rest of the 640 by the window's
  symmetry and 8-point Lagrange interpolation (leave-one-out error below 1.3e-5, 3.7e-5 at c[2];
  `sbr_tables.go`'s header says how). Only these tables were taken; OpenCORE's code was not
  used. No GPL or LGPL decoder (ffmpeg, FAAD2) or FDK source was read; ffmpeg was only run, to make
  the fixtures and the reference decodes.

Changed: `decoder.go` splits `configure` (`configureCore`) and `decodeRawDataBlock`
(`decodeFrame`, float samples) so both entry points share them, and reads SBR fill elements
(extension types 13 and 14) instead of skipping them. ADTS `Decode` finds SBR and PS in an AAC-LC
stream (implicit signalling), and then returns the doubled rate and, for mono, two channels, as
`SampleRate` and `Channels` report.

API: `NewDecoderASC(asc []byte) (*Decoder, error)` for raw access units; `DecodeAU(au []byte)
([]float32, error)`, interleaved, a full-scale 16-bit sample being 1.0; `OutputSampleRate() int`;
`OutputChannels() int` (2 for any SBR stream with a mono core, as ffmpeg decodes it). Not
supported, with an unsupported error: downsampled SBR, 960-sample frames, PS with 34 stereo bands
or mixing procedure B (`icc_mode` 3 to 5), channel configuration 0. IPD/OPD is parsed and not
applied (the baseline profile does not use it).

Tests (`hev2_test.go`, `testdata/`, about 710 KB): five AudioToolbox fixtures (HE-AAC mono 44.1
kHz and stereo 48 kHz in MP4, HE-AAC v2 in MP4, and HE-AAC and HE-AAC v2 in ADTS with implicit
signalling) against ffmpeg 9's native decoder, sample by sample; the ffmpeg commands are in
`TestHEAACMatchesFFmpeg`'s comment. Measured max and RMS differences (full scale 1.0):

| fixture | max | RMS | test tolerance |
|---|---|---|---|
| he-mono-44k.m4a | 1.77e-4 | 2.72e-5 | 2.5e-4, 3.5e-5 |
| he-stereo-48k.m4a | 6.62e-5 | 1.81e-5 | 1e-4, 2.5e-5 |
| hev2-44k.m4a | 8.83e-4 | 8.15e-5 | 1.2e-3, 1.1e-4 |
| he-implicit-44k.aac | 1.58e-3 | 1.59e-4 | 2e-3, 2e-4 |
| hev2-implicit-48k.aac | 1.30e-4 | 2.01e-5 | 2e-4, 3e-5 |

The remaining differences are in the SBR high band around the 9 kHz burst's onsets (gains about
0.1 dB apart in a few QMF bands); their cause is not yet found. `TestHEAACImplicitADTS` checks
that `Decode` matches `DecodeAU` on the ADTS fixtures; `TestDecodeAUMatchesDecodeLC` checks that
plain AAC-LC through `DecodeAU` matches `Decode`.

## Checked

`go test ./third_party/aac-go/` (HE-AAC, above).

`internal/embedding`'s `TestDecodeVideoAudioTrack`: an AAC-LC track (16 kHz stereo, mid/side, short
blocks) decodes to ffmpeg 9's samples within one 16-bit step a channel, as before the copy.

## Updating

Download the new version with `go mod download`, copy the same files, rewrite the imports, record
the version, sum and commit here and in `dependencies.json`, and run `go test ./internal/embedding/`.
