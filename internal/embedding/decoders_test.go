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
func TestDecodeADPCMWAV(t *testing.T) {
	for _, name := range []string{"wav-ima", "wav-ima-st", "wav-ms", "wav-ms-st"} {
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

// TestUndecodedTrackCodec: a codec still not decoded (QuickTime µ-law) is
// ErrNotDecoded, not invalid data.
func TestUndecodedTrackCodec(t *testing.T) {
	_, err := DecodeVideoAudio("video/quicktime", readTestdata(t, "va-ulaw.mov"), VideoOptions{})
	if !errors.Is(err, ErrNotDecoded) || !strings.Contains(err.Error(), "ulaw") {
		t.Errorf("µ-law track: %v; want ErrNotDecoded naming ulaw", err)
	}
}

// TestUndecodedAACLayouts: 7.1 AAC as ffmpeg writes it (a
// program_config_element, channel configuration 0) is ErrNotDecoded.
func TestUndecodedAACLayouts(t *testing.T) {
	_, err := DecodeVideoAudio("video/mp4", readTestdata(t, "va-aac71.mp4"), VideoOptions{})
	if !errors.Is(err, ErrNotDecoded) || !strings.Contains(err.Error(), "channel configuration 0") {
		t.Errorf("7.1 AAC: %v; want ErrNotDecoded naming channel configuration 0", err)
	}
}
