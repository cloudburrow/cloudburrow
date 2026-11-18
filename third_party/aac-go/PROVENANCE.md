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
- `sbr_dsp.go`: the 32-band analysis and 64-band synthesis QMF banks (and the 32-band synthesis
  bank of downsampled SBR), dequantisation (with
  coupling), HF generation (covariance LPC, chirp factors) and HF adjustment (limiter, gain boost,
  noise, sinusoids, smoothing).
- `ps.go`: parametric stereo with 10 or 20 stereo bands: IID (coarse and fine) and ICC, the 20-band
  hybrid filterbank, decorrelation (fractional-delay all-pass, delays, transient ducking) and
  mixing procedure A with time interpolation.
- `sbr_huff.go`, `sbr_tables.go`: the Huffman trees and tables. The tables come from PacketVideo's
  OpenCORE AAC decoder, Apache License 2.0, as Android ships it: the SBR Huffman trees from
  `sbr_code_book_envlevel.cpp`, the PS ones from `ps_read_data.cpp`, the noise table from
  `sbr_envelope_calc_tbl.cpp` (Q15), and the QMF window from `qmf_filterbank_coeff.cpp` and
  `calc_sbr_synfilterbank.cpp` (below). Only these tables were taken; OpenCORE's code was not used.
  No GPL or LGPL decoder (ffmpeg, FAAD2) or FDK source was read; ffmpeg was only run, to make the
  fixtures and the reference decodes.

### OpenCORE (the tables' source)

Downloaded 2026-11-17, read only as data (never built or run), with the user's approval of that
day: `https://android.googlesource.com/platform/frameworks/av/+archive/android-4.1.1_r1/media/libstagefright/codecs/aacdec.tar.gz`
(the directory `media/libstagefright/codecs/aacdec` at tag `android-4.1.1_r1`, with its `NOTICE`
and `MODULE_LICENSE_APACHE2`). The tables had earlier been obtained file by file; with the whole
directory at hand:

- Every Huffman tree in `sbr_tables.go` (the ten SBR books, `bookSbrEnvLevel10T` to
  `bookSbrNoiseBalance11T`, and the six PS books, `aBookPsIidTimeDecode` to
  `aBookPsIidFineFreqDecode`) and the noise table (`rPxx`) were compared entry by entry with the
  files: all identical. The limiter gains and the gain smoothing filter in `sbr_dsp.go` match
  `sbr_envelope_calc_tbl.cpp`'s `limGains` and `fir_table[4]`.
- The QMF window (ISO/IEC 14496-3 Table 4.A.90) is not in OpenCORE in full. It has the even
  coefficients c[2n+128j], 1 <= n <= 31, to nine digits (`sbrDecoderFilterbankCoefficients_an_filt`;
  `_an_filt_LC` is the same values times sqrt(2), to within 4e-15, so no more precise), the
  coefficients c[64k] to nine digits as constants in `calc_sbr_synfilterbank.cpp`, and
  c[n+64m], n = 1 to 31, in 16 bits (times 10/9, `sbrDecoderFilterbankCoefficients`). The table
  had interpolated c[64k]; they are now OpenCORE's, which cut the larger HE-AAC differences from
  ffmpeg two to four times (table below). c[1] is now the 16-bit value less that table's mean
  rounding offset; the other odd coefficients stay interpolated (8-point Lagrange on the even
  ones), and the 16-bit table confirms them to its precision (their rounding offsets match the even
  coefficients' in every 64-coefficient segment, to within 0.2 of a 16-bit step). The exact table
  was looked for and not found in an allowed source: ETSI TS 126 404 V18.0.0 §5.2 refers to a
  figure that does not carry it, US 8775193's annex is the low-delay window, and the only other copy
  found is ffmpeg's (LGPL, not read).
- `third_party/aac-go/NOTICE` carries OpenCORE's copyright line, the opening of its `NOTICE`, and
  the ISO/IEC software copyright notice its table files require in derivative works (Apache-2.0
  §4(d)); the root `NOTICE` names it.

Changed: `decoder.go` splits `configure` (`configureCore`) and `decodeRawDataBlock`
(`decodeFrame`, float samples) so both entry points share them, and reads SBR fill elements
(extension types 13 and 14) instead of skipping them. ADTS `Decode` finds SBR and PS in an AAC-LC
stream (implicit signalling), and then returns the doubled rate and, for mono, two channels, as
`SampleRate` and `Channels` report.

API: `NewDecoderASC(asc []byte) (*Decoder, error)` for raw access units; `DecodeAU(au []byte)
([]float32, error)`, interleaved, a full-scale 16-bit sample being 1.0; `OutputSampleRate() int`;
`OutputChannels() int` (2 for any SBR stream with a mono core, as ffmpeg decodes it). Downsampled
SBR (an AudioSpecificConfig whose SBR rate is the core's) decodes at the core rate through a
32-band synthesis bank.

Refused with `ErrUnsupported`, frame by frame (so a stream that switches to one mid-way fails
with an error from `DecodeAU`/`Decode`): HE-AAC v2 parametric stereo with 34 stereo bands
(`iid_mode` or `icc_mode` 2 or 5), mixing procedure B (`icc_mode` 3 to 5), and IPD/OPD (a
`ps_extension` with id 0 and `enable_ipdopd`). Their normative tables (the 34-band hybrid
filterbank's prototype filters and band maps, the IPD/OPD Huffman books, mixing B's details) had
no allowed source. With the whole OpenCORE directory read (2026-11-17), it still has none of them:
for 34-band streams it averages the 34 IID/ICC indices down to 20 (`map34IndexTo20` in
`ps_decode_bs_utils.cpp`) and filters with the 20-band bank, which is not the normative output and
would not match ffmpeg; `ps_init_stereo_mixing.cpp` says "type Rb is not supported"; and
`ps_read_data.cpp` skips the whole `ps_extension` by its byte count, so no IPD/OPD books or
dequantisation. 3GPP's enhanced aacPlus is baseline PS only too, a search for a permissively
licensed decoder with full PS found none (FDK AAC, under its own non-permissive licence and not
read, is described as baseline as well), and ATSC A/52 is unrelated. What is missing, all in
ISO/IEC 14496-3 subpart 8 (§8.6.4 and Annex 8.B), not available here: the IPD and OPD Huffman
books (time and frequency) and their quantisation, the 34-band hybrid analysis and synthesis
filters (the 12-, 8- and 4-band splits of QMF bands 0 to 4) with the 34-band parameter-to-band
map and the decorrelator's 34-band settings, and mixing procedure B's angle tables and rotation.
They were not written from memory, and no encoder at hand emits them
(`aac_at` always sends `iid_mode` and `icc_mode` 1 without extensions, at every bitrate tried), so
they could not have been checked against ffmpeg either. Also refused, out of scope: 960-sample
frames and channel configuration 0 (a program_config_element).

Tests (`hev2_test.go`, `testdata/`, about 1.1 MB): five AudioToolbox fixtures (HE-AAC mono 44.1
kHz and stereo 48 kHz in MP4, HE-AAC v2 in MP4, and HE-AAC and HE-AAC v2 in ADTS with implicit
signalling), 1.5 s of HE-AAC v2 speech (the JFK clip in `test/compat/testdata`, which
exercises PS frames with `num_env` 0: the last parameters hold for the frame; that path once
decoded to silence), and four derived from the tone fixtures (downsampled SBR, and three with SBR header_extra2 values
`aac_at` never sends: limiter bands 0, 1 and 3, limiter gains 0, 1 and 3, `bs_interpol_freq` 0 and
gain smoothing on), against ffmpeg 9's native decoder, sample by sample; how each was made is in
`TestHEAACMatchesFFmpeg`'s comment. Measured max and RMS differences (full scale 1.0):

| fixture | max | RMS | before c[64k] (max, RMS) | test tolerance |
|---|---|---|---|---|
| he-mono-44k.m4a | 4.52e-5 | 1.82e-5 | 1.77e-4, 2.72e-5 | 6e-5, 2.5e-5 |
| he-stereo-48k.m4a | 3.78e-5 | 1.80e-5 | 6.62e-5, 1.81e-5 | 5e-5, 2.5e-5 |
| hev2-44k.m4a | 2.05e-4 | 2.01e-5 | 8.83e-4, 8.15e-5 | 3e-4, 3e-5 |
| he-implicit-44k.aac | 5.67e-4 | 4.03e-5 | 1.58e-3, 1.59e-4 | 8e-4, 6e-5 |
| hev2-implicit-48k.aac | 1.31e-4 | 1.95e-5 | 1.30e-4, 2.01e-5 | 2e-4, 3e-5 |
| he-ds-22k.m4a (downsampled) | 4.23e-5 | 1.81e-5 | 1.70e-4, 2.72e-5 | 6e-5, 2.5e-5 |
| he-lim0-44k.aac (limiter 0/0, interpol 0, smoothing) | 4.19e-4 | 3.11e-5 | 7.84e-4, 5.93e-5 | 6e-4, 4.5e-5 |
| he-lim1-44k.aac (limiter 1/3, smoothing) | 1.38e-3 | 9.16e-5 | 2.84e-3, 3.05e-4 | 2e-3, 1.3e-4 |
| he-lim3-44k.aac (limiter 3/1, interpol 0) | 6.84e-4 | 5.08e-5 | 2.85e-3, 2.99e-4 | 1e-3, 7e-5 |
| hev2-speech-44k.m4a (1.5 s of speech) | 4.43e-5 | 1.78e-5 | 7.61e-5, 1.80e-5 | 6e-5, 2.5e-5 |

Six of the ten are within 1e-4, with an RMS (1.8e-5) at the rounding of ffmpeg's 16-bit output.
The larger differences are in SBR bands whose source is near silent (80 to 90 dB below the
frame's peak) and whose gains are tens of dB (the limiter's G_max reaches 7866 in
he-implicit-44k), where the output follows the core decoder's last bits and any window error is
amplified. They were blamed earlier on ffmpeg's float32 arithmetic; the c[64k] fix shows that most
of them were the interpolated window, and the odd coefficients, still interpolated (about 1e-5
off), are the one known remaining source. `TestHEAACImplicitADTS` checks that `Decode` matches `DecodeAU` on the ADTS
fixtures; `TestDecodeAUMatchesDecodeLC` checks that plain AAC-LC through `DecodeAU` matches
`Decode`; `TestPSRefusals` checks the refusals above.

### Channel configurations 3 to 6 (#1651)

`configureCore` takes channel configurations 1 to 6 (mono to 5.1; configuration 7, and a
`program_config_element`'s layout, stay `ErrUnsupported`), and `decodeFrame` decodes `lfe_channel_element()` as the single channel element
it is (ISO/IEC 14496-3 Table 4.3). SBR over more than two channels is `ErrUnsupported`, explicit
or found in the stream. A 5.1 AAC-LC track decodes within 1.8e-7 of ffmpeg 9 after ffmpeg's
downmix (`internal/embedding/decoders_test.go`).

## Checked

`go test ./third_party/aac-go/` (HE-AAC, above).

`internal/embedding`'s `TestDecodeVideoAudioTrack`: an AAC-LC track (16 kHz stereo, mid/side, short
blocks) decodes to ffmpeg 9's samples within one 16-bit step a channel, as before the copy.

## Updating

Download the new version with `go mod download`, copy the same files, rewrite the imports, record
the version, sum and commit here and in `dependencies.json`, and run `go test ./internal/embedding/`.
