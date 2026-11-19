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

Full parametric stereo (#1754) is decoded too: see below. Refused, out of scope: 960-sample
frames.

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

### Full parametric stereo (#1754)

`ps.go` now decodes the whole PS tool: 34 stereo bands (`iid_mode`/`icc_mode` 2 and 5) with the
34-band hybrid filterbank (QMF band 0 into 12, band 1 into 8, bands 2 to 4 into 4 each), mapping
10- and 20-band parameters to 34 when the other parameter has 34, and switching configurations
mid-stream (the decorrelator cleared, the last mixing coefficients mapped); IPD/OPD (`ps_extension`
id 0, `enable_ipdopd`) with modulo-8 delta decoding, phase smoothing over the current and two
previous envelopes (weights 1, 1/2, 1/4) and the phases applied to the mixing coefficients
(conjugated for negative-frequency hybrid bands); and mixing procedure B (`icc_mode` 3 to 5),
the rotation by the principal-axis angle alpha and the eigenvalue split gamma, with rho floored
at 0.05. Fine IID (`iid_mode` 3 to 5) was already decoded. New file `ps_tables.go`: the 12-, 8-
(34-band) and 4-band hybrid prototype filters, the 34-band sub-subband-to-parameter map and
centre frequencies, the QMF-band-to-parameter map, the 20-to-34 map, and the four IPD/OPD Huffman
codes.

Where they come from: no source with these tables was allowed (#1754's text), so they were written
from knowledge of ISO/IEC 14496-3 Annex 8.B (not copied from any decoder's source; no ffmpeg, FAAD2
or FDK source was read) and accepted only because the result matches ISO's own conformance
references. Two details were found by that check rather than recalled: mixing B floors rho at
0.05 (without it the SNR is 8 dB; with it 102 dB), and the hybrid centre frequencies of the odd
QMF bands are a band higher than their filters' outputs (as the table says).

Also fixed: an SBR extension before the first SBR header is now skipped to its byte count
(it was left unread, so the rest of the frame was misparsed; ISO's streams start that way).

Checked against the ISO/IEC 14496-26:2024 (2nd edition) audio conformance package on ISO's
Publicly Available Standards server, `https://standards.iso.org/iso-iec/14496/-26/ed-2/en/`
(`compressedMp4.zip`, and `referencesWav.zip`'s `add-opt/*_ur.wav`, the unrestricted-PS references,
24-bit 32 kHz). **ISO's files are not in the repository** (they are ISO's copyright): the tests fetch
them at test time with `internal/isoconformance`, which reads each member out of its zip by HTTP
range requests (the zips are 1.2 and 9.8 GB; the six members about 5.3 MB, about 8 MB fetched with
the zips' directories) into the user's cache directory, `os.UserCacheDir()/cloudburrow-test/iso-14496-26-ed2`
(`~/.cache/...` on Linux, `~/Library/Caches/...` on macOS), the place the other pinned test downloads
use, outside the repository. A file is downloaded only when `CLOUDBURROW_TEST_DOWNLOADS` is set;
otherwise a test that needs a file not cached skips, naming it and the variable. Each file is checked
against its pinned size, SHA-256 and ISO's MD5 (from the package's
`mpeg4audio-conformance2ndEd_20230125.md5`) before it is cached and each time it is read, and the zips'
sizes are pinned too. The pins are in `internal/isoconformance/isoconformance.go` and in
`dependencies.json` (`components.testOracles.iso14496_26Conformance`), held equal by
`TestDependenciesRecordThePins`. SHA-256s measured on the members fetched 2026-11-19:

| file | bytes | SHA-256 | ISO's MD5 | PS tools |
|---|---|---|---|---|
| al_sbr_ps_00.mp4 to _02, _05 | | (not pinned) | | baseline: 10/20 bands, IID and ICC |
| al_sbr_ps_03.mp4 | 28612 | 717153ee39f505e897e3dc22357b39dbf0470c8033c7e0c76d5b3b68d6d8e238 | 17f7d06f1c0dee51e7ca4a01d06b453b | fine IID, mixing B, 20 bands |
| al_sbr_ps_03_ur.wav | 1302716 | 3b905534a0e90922dcde26048a0023f405bd1f496d6e35586fddda5502dbaafa | 0f4d0ddcd52d8ca7ea4de16a3c6aefd1 | |
| al_sbr_ps_04.mp4 | 28612 | 89669b9f796678db2a9bf13cc33a0617e6864bf1b0152fa717ba3616d869a749 | 596100860fe4ed5e53fcfcd5a1648f73 | IPD/OPD, 20 bands |
| al_sbr_ps_04_ur.wav | 1302716 | d00d6d52a362a31b362010e30f422898eb48ecd8829cfdbbbe0fabd927c41d35 | 8d885cbc26f0a2764f83d17522de6562 | |
| al_sbr_ps_06.mp4 | 56598 | 42d273c6260d2541d07e297257aa3238ebbcc1cb9eac5cf71bbfba69a6238132 | 726dd94f7c29da1dc11314c837c37400 | 34-band ICC, then 34-band IID; 20/34 switching |
| al_sbr_ps_06_ur.wav | 2605244 | e2ef76b498971db8871190e2102eadc747870ca00c83d0e9add7fb9be7d4e817 | a170700536bd15130a58d2cbe22ee99d | |

All seven streams (every access unit, whole files) decode to their `_ur` WAVs within a max of
3.6e-6 and an RMS of 5e-7 (102.4 dB SNR, the 24-bit references' rounding plus float
differences), the four baseline ones as before. `TestPSFullConformance` decodes the three full-PS
streams and compares them, over windows where the `_ur` and `_bl` (baseline) references differ, with
the reference's samples, cut at test time from the WAV's `data` chunk (raw 24-bit little-endian
stereo): al_sbr_ps_03 from sample frame 126976 for 32000 frames, al_sbr_ps_04 from 118784 for 20000,
al_sbr_ps_06 from 229376 and from 344064 for 16000 each (max 1e-5, RMS 1e-6; measured 2026-11-19:
max 3.5e-6, RMS 5.2e-7, 102.4 to 102.6 dB SNR). These are the windows an earlier branch cut with
Python and committed; cut here, they are byte for byte the same. It also checks that each stream
decodes to the reference's length. `TestPSFullSyntax` replaces `TestPSRefusals`. CI's check job
fetches the files into its cache (`actions/cache`) before `make test-race`, so the test runs there.

ISO's PS streams signal their mono core with a program_config_element (channel configuration 0, one
SCE). `ChannelNames` named that program's one channel while PS made the output stereo, so
`internal/embedding` downmixed the stereo samples as one channel (the audio twice as long); it now
returns nil for any mono or stereo output (`TestFullParametricStereoTrack` in `internal/embedding`,
and the EmbedContent compat test
`TestEmbedContentEmbedsAFullParametricStereoTrackThroughThePredictionClient`, which muxes al_sbr_ps_06
as the audio track of `test/compat/testdata/turtle-jfk.mp4` at test time).

AudioToolbox could not be the reference: its decoder implements baseline PS only (an ADTS remux
of these streams, `afconvert -f WAVE -d LEI24`, matches the `_bl` references at 122 dB and the `_ur`
ones at 11 to 28 dB), and its encoder (`aac_at`) emits only `iid_mode`/`icc_mode` 1 without
extensions. No GPL tool made any expected output.

### Channel configurations 0, 3 to 7, 11 and 12, and multichannel SBR (#1651)

`configureCore` takes channel configurations 1 to 7 (mono to 7.1; configuration 7 is eight channels,
SCE CPE CPE CPE LFE, as AudioToolbox writes 7.1), 11 (SCE CPE CPE SCE LFE, 6.1, as AudioToolbox
writes it) and 12 (SCE CPE CPE CPE LFE, 7.1, as ffmpeg 9 writes it; ISO/IEC 14496-3:2019 Table
1.19), and configuration 0: `pce.go`, written for
CloudBurrow from the `program_config_element()` syntax (ISO/IEC 14496-3 Table 4.2), parses the PCE
of the AudioSpecificConfig (byte-aligned from the ASC's start), names each listed element's speakers
(front: a centre, then up to two pairs; one side pair; behind, a pair and a centre; one LFE; anything
else `ErrUnsupported`), and puts each frame's channels in the PCE's order, matching elements by kind
and `element_instance_tag`. `decodeFrame` decodes `lfe_channel_element()` as the single channel
element it is (Table 4.3). `ChannelNames` gives the speakers (configuration 7's second pair is the
side pair, as ffmpeg 9 lays it out). SBR now runs over every SCE and CPE of a multichannel stream
(the LFE, which carries no SBR data, through the QMF banks alone), as it already ran per element.
Checked against ffmpeg 9 after its downmix (`internal/embedding/decoders_test.go`): 5.1 AAC-LC within
1.8e-7, ffmpeg's 7.1(wide) and 6.1 (PCEs) 2.4e-7 and 1.8e-7, ffmpeg's 7.1 (configuration 12),
AudioToolbox's 7.1 (configuration 7) and 6.1 (configuration 11) 1.8e-7, AudioToolbox's 5.1 HE-AAC
1.3e-5; `TestParseASCProgramConfig`.

## Checked

`go test ./third_party/aac-go/` (HE-AAC, above).

`internal/embedding`'s `TestDecodeVideoAudioTrack`: an AAC-LC track (16 kHz stereo, mid/side, short
blocks) decodes to ffmpeg 9's samples within one 16-bit step a channel, as before the copy.

## Updating

Download the new version with `go mod download`, copy the same files, rewrite the imports, record
the version, sum and commit here and in `dependencies.json`, and run `go test ./internal/embedding/`.
