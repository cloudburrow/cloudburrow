package embedding

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

func readF32(t *testing.T, file string) []float32 {
	t.Helper()
	ref := readTestdata(t, file)
	out := make([]float32, len(ref)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(ref[4*i:]))
	}
	return out
}

func maxAbsDiff(t *testing.T, name string, got, want []float32) float64 {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d samples, ffmpeg %d", name, len(got), len(want))
	}
	var d float64
	for i := range got {
		d = max(d, math.Abs(float64(got[i]-want[i])))
	}
	return d
}

// TestDecodeADPCMWAV (#1651) decodes IMA and Microsoft ADPCM WAVs, mono and
// stereo, sample for sample as ffmpeg 9 does (`ffmpeg -i f -f f32le`,
// testdata/*.raw.f32, every channel). Made with ffmpeg 9:
//
//	S1="aevalsrc=0.3*sin(2*PI*440*t)+0.1*sin(2*PI*3000*t):s=16000:d=0.53"
//	S2="aevalsrc=0.3*sin(2*PI*440*t)+0.1*sin(2*PI*3000*t)|0.25*sin(2*PI*660*t):s=16000:d=0.53"
//	ffmpeg -f lavfi -i "$S1" -c:a adpcm_ima_wav -map_metadata -1 -fflags +bitexact -flags +bitexact wav-ima.wav
//	("$S2": wav-ima-st.wav; -c:a adpcm_ms: wav-ms.wav, wav-ms-st.wav)
//
// 3-bit IMA ADPCM (#1651), which ffmpeg does not encode, was written by a
// small encoder made for the fixtures (scratch, not kept): a 1-second
// signal of S1's kind, mono in 260-byte blocks (four bytes of padding
// after 21 groups) and stereo in 512-byte blocks, wav-ima3.wav and
// wav-ima3-st.wav; ffmpeg's decode (`-f f32le`) is the reference. 2- and
// 5-bit IMA ADPCM (#1651) the same way, mono in 258- and 256-byte blocks
// (2 and 12 bytes of padding) and stereo in 512-byte blocks:
// wav-ima2.wav, wav-ima2-st.wav, wav-ima5.wav, wav-ima5-st.wav.
//
// GSM 6.10 (#1651), which ffmpeg does not encode: Microsoft-packed blocks
// of frames of random parameters written by a scratch generator (not
// kept), xmaxc at most 36 so the synthesis does not overflow (there this
// decoder saturates as GSM 06.10 specifies and ffmpeg 9 wraps): wav-gsm.wav
// (50 blocks, lags 40 to 120) and wav-gsm-lags.wav (24 blocks, any lag,
// out-of-range ones clamped as ffmpeg does). wav-gsm-ffmpeg.wav is ffmpeg's
// decode as 16-bit PCM, for the compat test.
func TestDecodeADPCMWAV(t *testing.T) {
	for _, name := range []string{"wav-ima", "wav-ima-st", "wav-ms", "wav-ms-st", "wav-ima3", "wav-ima3-st", "wav-ima2", "wav-ima2-st", "wav-ima5", "wav-ima5-st", "wav-gsm", "wav-gsm-lags"} {
		w, err := decodeWAV(readTestdata(t, name+".wav"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := maxAbsDiff(t, name, w.samples, readF32(t, name+".raw.f32")); d != 0 {
			t.Errorf("%s: %.2e from ffmpeg's decode, want 0", name, d)
		}
		if _, err := DecodeAudio("audio/wav", readTestdata(t, name+".wav")); err != nil {
			t.Errorf("%s: DecodeAudio: %v", name, err)
		}
	}
}

// TestDecodeOddWidthPCMWAV: ffmpeg reads 12-bit PCM as its 16-bit
// container (checked: `ffmpeg -i s12.wav -f s16le` equals the 16-bit file
// it was patched from).
func TestDecodeOddWidthPCMWAV(t *testing.T) {
	a := wavBytes(1, 1, 16000, 16, 0, 16)
	for i := 44; i+1 < len(a); i += 2 {
		binary.LittleEndian.PutUint16(a[i:], uint16(i*16))
	}
	b := append([]byte(nil), a...)
	b[34] = 12
	wa, err := decodeWAV(a)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := decodeWAV(b)
	if err != nil {
		t.Fatalf("12-bit: %v", err)
	}
	if d := maxAbsDiff(t, "12-bit", wb.samples, wa.samples); d != 0 {
		t.Errorf("12-bit PCM differs from its 16-bit container by %.2e", d)
	}
}

// TestDecodeFLACAndALACTracks (#1651) decodes videos' FLAC and ALAC audio
// tracks, and a 5.1 AAC-LC one (channel configuration 6, with its LFE), as
// ffmpeg 9 does: stereo 16-bit against ffmpeg's decode of every
// channel (`-f f32le`, downmixed here as the stereo path does), and 5.1
// 24-bit against ffmpeg's own downmix (`-ac 1 -ar 16000 -f f32le`), which
// also checks each codec's channel order. Made with ffmpeg 9 (S2 as above,
// S6 the 5.1 source of TestDecodeMultichannelAndG711WAV, d=0.5):
//
//	V="-f lavfi -i color=c=blue:s=64x36:r=2:d=0.5"
//	ffmpeg $V -f lavfi -i "$S2" -c:v libx264 -c:a flac -sample_fmt s16 -shortest ... va-flac.mp4
//	ffmpeg $V -f lavfi -i "$S2" -c:v libx264 -c:a alac -sample_fmt s16p -shortest ... va-alac-st.mp4
//	ffmpeg $V -f lavfi -i "$S6" -c:v libx264 -c:a flac -sample_fmt s32 -shortest ... va-flac51-24.mp4
//	ffmpeg $V -f lavfi -i "$S6" -c:v libx264 -c:a alac -sample_fmt s32p -shortest ... va-alac51-24.mp4
//	ffmpeg $V -f lavfi -i "$S6" -c:v libx264 -c:a aac -aac_pns 0 -shortest ... va-aac51.mp4 (no PNS: its noise is random)
func TestDecodeFLACAndALACTracks(t *testing.T) {
	for _, c := range []struct {
		file, ref string
		stereo    bool
	}{
		{"va-flac.mp4", "va-flac.raw.f32", true},
		{"va-alac-st.mp4", "va-alac-st.raw.f32", true},
		{"va-alac.mp4", "", false},
		{"va-flac51-24.mp4", "va-flac51-24.ffmpeg-16k.f32", false},
		{"va-alac51-24.mp4", "va-alac51-24.ffmpeg-16k.f32", false},
		{"va-aac51.mp4", "va-aac51.ffmpeg-16k.f32", false}, // measured 1.8e-7
	} {
		got, err := DecodeVideoAudio("video/mp4", readTestdata(t, c.file), VideoOptions{})
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if c.ref == "" {
			continue
		}
		want := readF32(t, c.ref)
		tol := 1e-6
		if c.stereo {
			if want, err = downmixNamed(want, []string{"FL", "FR"}); err != nil {
				t.Fatal(err)
			}
			tol = 0
		}
		d := maxAbsDiff(t, c.file, got, want)
		t.Logf("%s: max %.2e from ffmpeg", c.file, d)
		if d > tol {
			t.Errorf("%s: %.2e from ffmpeg's decode, want at most %.0e", c.file, d, tol)
		}
	}
}

// TestFLACFrameErrors: truncated or corrupt frames are invalid data.
func TestFLACFrameErrors(t *testing.T) {
	si := flacStreamInfo{rate: 16000, bits: 16, channels: 1}
	for _, b := range [][]byte{nil, {0xff}, {0xff, 0xf8, 0x00}, {0xff, 0xf8, 0x69, 0x08, 0x00, 0x00}} {
		if _, err := decodeFLACFrame(b, si); err == nil || errors.Is(err, ErrNotDecoded) {
			t.Errorf("%x: %v; want invalid data", b, err)
		}
	}
}

// TestDecodeG711Tracks (#1651) decodes QuickTime µ-law (ulaw) and A-law
// (alaw) tracks sample for sample as ffmpeg 9 does: mono against ffmpeg's
// decode (`-ac 1 -ar 16000 -f f32le`), stereo against its decode of every
// channel (`-f f32le`) downmixed as the stereo path does. Made with ffmpeg 9
// (S1 and S2 as in TestDecodeADPCMWAV, d=0.5):
//
//	ffmpeg $V -f lavfi -i "$S1" -c:v libx264 -c:a pcm_mulaw -shortest ... va-ulaw16k.mov
//	ffmpeg $V -f lavfi -i "$S2" -c:v libx264 -c:a pcm_alaw -shortest ... va-alaw-st.mov
func TestDecodeG711Tracks(t *testing.T) {
	got, err := DecodeVideoAudio("video/quicktime", readTestdata(t, "va-ulaw16k.mov"), VideoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := maxAbsDiff(t, "ulaw", got, readF32(t, "va-ulaw16k.ffmpeg-16k.f32")); d != 0 {
		t.Errorf("µ-law: %.2e from ffmpeg's decode, want 0", d)
	}
	got, err = DecodeVideoAudio("video/quicktime", readTestdata(t, "va-alaw-st.mov"), VideoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := downmixNamed(readF32(t, "va-alaw-st.raw.f32"), []string{"FL", "FR"})
	if err != nil {
		t.Fatal(err)
	}
	if d := maxAbsDiff(t, "alaw", got, want); d > 1e-7 { // the stereo mean's rounding
		t.Errorf("A-law: %.2e from ffmpeg's decode, want at most 1e-7", d)
	}
	// 8 kHz µ-law, resampled.
	if got, err := DecodeVideoAudio("video/quicktime", readTestdata(t, "va-ulaw.mov"), VideoOptions{}); err != nil || len(got) != AudioSampleRate/2 {
		t.Errorf("8 kHz µ-law: %d samples, %v; want %d", len(got), err, AudioSampleRate/2)
	}
}

// TestUndecodedTrackCodec: a codec still not decoded (QuickTime IMA ADPCM,
// ima4) is ErrNotDecoded, not invalid data.
func TestUndecodedTrackCodec(t *testing.T) {
	_, err := DecodeVideoAudio("video/quicktime", readTestdata(t, "va-ima4.mov"), VideoOptions{})
	if !errors.Is(err, ErrNotDecoded) || !strings.Contains(err.Error(), "ima4") {
		t.Errorf("ima4 track: %v; want ErrNotDecoded naming ima4", err)
	}
}

// TestDecodeMultichannelAACTracks (#1651): 7.1 AAC-LC as ffmpeg writes it
// (channel configuration 0, a program_config_element listing a centre, two
// front pairs, a back pair and the LFE) against ffmpeg 9's own downmix
// (`-ac 1 -ar 16000 -f f32le`; the file is 16 kHz, so nothing is
// resampled), and 5.1 HE-AAC (SBR over every element, the LFE upsampled
// without SBR data) at its own 48 kHz against ffmpeg's downmix (`-vn -ac 1
// -f f32le`). Made with ffmpeg 9 (S8 the 7.1 source of
// TestDecodeMultichannelAndG711WAV):
//
//	ffmpeg $V -f lavfi -i "$S8" -c:v libx264 -c:a aac -shortest ... va-aac71.mp4
//	S6="aevalsrc=0.3*sin(2*PI*440*t)+0.05*sin(2*PI*7000*t)|0.3*sin(2*PI*660*t)+0.05*sin(2*PI*9000*t)|0.25*sin(2*PI*330*t)|0.2*sin(2*PI*60*t)|0.15*sin(2*PI*1500*t)*sin(2*PI*2*t)|0.15*sin(2*PI*2500*t)+0.04*sin(2*PI*8000*t):s=48000:d=1:channel_layout=5.1(side)"
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1 -f lavfi -i "$S6" -c:v libx264 -c:a aac_at -profile:a 4 -b:a 128k -shortest -map_metadata -1 va-heaac51.mp4
//	S8W="aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.25*sin(2*PI*330*t)|0.2*sin(2*PI*60*t)|0.15*sin(2*PI*1500*t)|0.15*sin(2*PI*2500*t)|0.1*sin(2*PI*900*t)|0.1*sin(2*PI*1200*t):s=48000:d=1:channel_layout=7.1(wide)"
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1 -f lavfi -i "$S8W" -c:v libx264 -c:a aac_at -b:a 256k -shortest -map_metadata -1 va-aac71-cfg7.mp4 (channel configuration 7)
//
//	S7="aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.25*sin(2*PI*330*t)|0.2*sin(2*PI*60*t)|0.15*sin(2*PI*1500*t)|0.15*sin(2*PI*2500*t)|0.1*sin(2*PI*900*t):s=48000:d=0.5:channel_layout=6.1"
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=0.5 -f lavfi -i "$S7" -c:v libx264 -c:a aac_at -b:a 192k -shortest -map_metadata -1 va-aac61-cfg11.mp4 (configuration 11)
//	(S8 with channel_layout=7.1 and S7 at s=16000) ffmpeg $V ... -c:a aac -aac_pns 0 ... va-aac71-cfg12.mp4 (configuration 12), va-aac61-pce.mp4 (a PCE)
//
// (va-aac71-cfg7, va-aac61-cfg11 and va-heaac51.ffmpeg-mono.f32: `ffmpeg -i f -vn -ac 1 -f f32le`;
// va-aac71-cfg12 and va-aac61-pce.ffmpeg-16k.f32: `-ac 1 -ar 16000 -f f32le`.)
func TestDecodeMultichannelAACTracks(t *testing.T) {
	got, err := DecodeVideoAudio("video/mp4", readTestdata(t, "va-aac71.mp4"), VideoOptions{})
	if err != nil {
		t.Fatalf("7.1 AAC: %v", err)
	}
	d := maxAbsDiff(t, "7.1 AAC", got, readF32(t, "va-aac71.ffmpeg-16k.f32"))
	t.Logf("7.1 AAC: max %.2e from ffmpeg", d)
	if d > 1e-6 {
		t.Errorf("7.1 AAC: %.2e from ffmpeg's decode, want at most 1e-6", d)
	}
	// ffmpeg's 7.1 (channel configuration 12) and 6.1 (a PCE with a back
	// centre).
	for _, f := range []string{"va-aac71-cfg12", "va-aac61-pce"} {
		got, err := DecodeVideoAudio("video/mp4", readTestdata(t, f+".mp4"), VideoOptions{})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		d := maxAbsDiff(t, f, got, readF32(t, f+".ffmpeg-16k.f32"))
		t.Logf("%s: max %.2e from ffmpeg", f, d)
		if d > 1e-6 {
			t.Errorf("%s: %.2e from ffmpeg's decode, want at most 1e-6", f, d)
		}
	}
	// Channel configurations 7 and 11, as AudioToolbox writes 7.1 and 6.1:
	// configuration 7's second pair is a side pair (a front-wide one would
	// be 0.12 off).
	for _, f := range []string{"va-aac71-cfg7", "va-aac61-cfg11"} {
		got, rate := soundTrackOf(t, f+".mp4")
		d := maxAbsDiff(t, f, got, readF32(t, f+".ffmpeg-mono.f32"))
		t.Logf("%s: max %.2e from ffmpeg", f, d)
		if rate != 48000 || d > 1e-6 {
			t.Errorf("%s: %d Hz, %.2e from ffmpeg's decode, want 48000 Hz, at most 1e-6", f, rate, d)
		}
	}
	got, rate := soundTrackOf(t, "va-heaac51.mp4")
	if rate != 48000 {
		t.Errorf("5.1 HE-AAC: %d Hz, want 48000", rate)
	}
	d = maxAbsDiff(t, "5.1 HE-AAC", got, readF32(t, "va-heaac51.ffmpeg-mono.f32"))
	t.Logf("5.1 HE-AAC: max %.2e from ffmpeg", d)
	if d > 1e-3 {
		t.Errorf("5.1 HE-AAC: %.2e from ffmpeg's decode, want at most 1e-3", d)
	}
}

// TestDecodeMP3WAV (#1651) decodes MP3 in a WAV (WAVE_FORMAT_MPEGLAYER3) as
// ffmpeg 9 does (`ffmpeg -i f.wav -f s16le`, testdata/*.ffmpeg.s16): every
// frame, nothing trimmed, mono within one 16-bit step and joint stereo
// within two a channel, as TestDecodeMPEG25MP3. Made with ffmpeg 9 (S1 and
// S2 of TestDecodeADPCMWAV, S2 at 44.1 kHz):
//
//	ffmpeg -f lavfi -i "$S1" -c:a libmp3lame -b:a 32k -map_metadata -1 -fflags +bitexact -flags +bitexact -f wav wav-mp3.wav
//	ffmpeg -f lavfi -i "$S2" -c:a libmp3lame -b:a 96k ... -f wav wav-mp3-st.wav
func TestDecodeMP3WAV(t *testing.T) {
	for _, c := range []struct {
		file           string
		rate, channels int
		tol            float64
	}{
		{"wav-mp3", 16000, 1, 1},
		{"wav-mp3-st", 44100, 2, 2},
	} {
		w, err := decodeWAV(readTestdata(t, c.file+".wav"))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		ref := readTestdata(t, c.file+".ffmpeg.s16")
		if w.rate != c.rate || w.channels != c.channels || len(w.samples) != len(ref)/2 {
			t.Fatalf("%s: %d samples, %d channels at %d Hz; want ffmpeg's %d, %d at %d Hz",
				c.file, len(w.samples), w.channels, w.rate, len(ref)/2, c.channels, c.rate)
		}
		var d float64
		for i, v := range w.samples {
			want := float64(int16(binary.LittleEndian.Uint16(ref[2*i:])))
			d = max(d, math.Abs(math.Round(float64(v)*32768)-want))
		}
		if d > c.tol {
			t.Errorf("%s: %v 16-bit steps from ffmpeg's decode, want at most %v", c.file, d, c.tol)
		}
		if _, err := DecodeAudio("audio/wav", readTestdata(t, c.file+".wav")); err != nil {
			t.Errorf("%s: DecodeAudio: %v", c.file, err)
		}
	}
}
