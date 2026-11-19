package embedding

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// soundTrackOf decodes the sound track of a test file at its own rate.
func soundTrackOf(t *testing.T, file string) ([]float32, int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range f.Moov.Traks {
		if tr.Mdia.Hdlr.HandlerType == "soun" {
			got, rate, err := decodeSoundTrack(f, tr, b)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			return got, rate
		}
	}
	t.Fatalf("%s: no sound track", file)
	return nil, 0
}

// TestDecodeVideoAudioTrack (#1732): the audio track of an MP4 or MOV
// decodes to ffmpeg 9's samples (testdata/va-*.ffmpeg.s16, `ffmpeg -i f
// -vn -f s16le`), the AAC encoder's priming dropped by the edit list as
// ffmpeg drops it: ffmpeg's AAC-LC (16 kHz stereo, mid/side, short blocks)
// within one 16-bit step a channel, an MP3 track (16 kHz mono) within one
// step, QuickTime sowt PCM exactly. Each is 1.5 seconds. Since #1739, a
// 48 kHz stereo Opus track (libopus 64 kbit/s, its 312-sample pre-skip in
// the edit list) is ffmpeg's libopus decode (`ffmpeg -c:a libopus -i f -vn
// -t 1.5 -f s16le`) within 1.5 steps a channel: 0.71 in float, then
// ffmpeg's rounding to 16 bits. va-opus.mp4 is
//
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1.5 -f lavfi -i "aevalsrc=0.3*sin(2*PI*440*t)+0.2*sin(2*PI*1234*t)*sin(2*PI*3*t)|0.3*sin(2*PI*660*t):s=48000:d=1.5" -c:v libx264 -c:a libopus -b:a 64k -shortest -map_metadata -1 va-opus.mp4
func TestDecodeVideoAudioTrack(t *testing.T) {
	for _, c := range []struct {
		file     string
		channels int
		steps    float64 // per channel
		rate     int
	}{
		{"va-aac.mp4", 2, 1, 16000},
		{"va-mp3.mp4", 1, 1, 16000},
		{"va-pcm.mov", 1, 0, 16000},
		{"va-opus.mp4", 2, 1.5, 48000},
	} {
		got, rate := soundTrackOf(t, c.file)
		ref, err := os.ReadFile(filepath.Join("testdata", strings.TrimSuffix(c.file, filepath.Ext(c.file))+".ffmpeg.s16"))
		if err != nil {
			t.Fatal(err)
		}
		n := len(ref) / 2 / c.channels
		if rate != c.rate || len(got) != n {
			t.Fatalf("%s: %d samples at %d Hz, want ffmpeg's %d at %d", c.file, len(got), rate, n, c.rate)
		}
		var maxDiff float64
		for i := range n {
			l := int16(binary.LittleEndian.Uint16(ref[2*c.channels*i:]))
			want := float64(l) / 32768
			if c.channels == 2 {
				r := int16(binary.LittleEndian.Uint16(ref[2*c.channels*i+2:]))
				// Since #1739 AAC is decoded to float, as ffmpeg decodes it,
				// unclipped; the 16-bit reference is clipped at full scale.
				if l == math.MaxInt16 || l == math.MinInt16 || r == math.MaxInt16 || r == math.MinInt16 {
					continue
				}
				want = math.Sqrt2 * (want + float64(r)/32768) / 2 // as downmix does
			}
			maxDiff = max(maxDiff, math.Abs(want-float64(got[i])))
		}
		limit := c.steps
		if c.channels == 2 {
			limit *= math.Sqrt2 // the downmix of two channels each within c.steps
		}
		if maxDiff*32768 > limit+1e-3 {
			t.Errorf("%s: %.2f 16-bit steps from ffmpeg's decode, want at most %.2f", c.file, maxDiff*32768, limit)
		}
	}

	// Perceptual noise substitution (ffmpeg's AAC encoder's default) is
	// noise each decoder draws itself: the level is the encoder's.
	b, err := os.ReadFile(filepath.Join("testdata", "va-pns.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	clip, err := DecodeVideoAudio("video/mp4", b, VideoOptions{})
	if err != nil || len(clip) != 24000 {
		t.Fatalf("PNS: %d samples, %v; want 24000", len(clip), err)
	}
	var ss float64
	for _, v := range clip[4000:20000] {
		ss += float64(v) * float64(v)
	}
	// 0.3 sin in one channel and uniform noise of ±0.1 in the other,
	// downmixed by √2/2: √(0.045 + 0.0033) · 0.7071 = 0.155.
	if rms := math.Sqrt(ss / 16000); math.Abs(rms-0.155) > 0.01 {
		t.Errorf("PNS: RMS %.3f, want 0.155", rms)
	}

	// Through DecodeVideoAudio, at 16 kHz, between video_metadata's offsets.
	b, err = os.ReadFile(filepath.Join("testdata", "va-aac.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if clip, err := DecodeVideoAudio("video/mp4", b, VideoOptions{}); err != nil || len(clip) != 24000 {
		t.Errorf("whole: %d samples, %v; want 24000", len(clip), err)
	}
	if clip, err := DecodeVideoAudio("video/mp4", b, VideoOptions{Start: 500 * time.Millisecond, End: time.Second}); err != nil || len(clip) != 8000 {
		t.Errorf("0.5s to 1s: %d samples, %v; want 8000", len(clip), err)
	}
	// A video with no sound track has no clip.
	b, err = os.ReadFile(filepath.Join("testdata", "va-silent.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if clip, err := DecodeVideoAudio("video/mp4", b, VideoOptions{}); err != nil || clip != nil {
		t.Errorf("no sound track: %d samples, %v", len(clip), err)
	}
}

// TestDecodeVideoAudioRefuses: a track this build does not decode is
// refused naming its codec.
func TestDecodeVideoAudioRefuses(t *testing.T) {
	for file, want := range map[string]string{
		"va-ima4.mov": `codec "ima4"`,
	} {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeVideoAudio("video/mp4", b, VideoOptions{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error naming %q", file, err, want)
		}
	}
	if _, err := DecodeVideoAudio("video/mp4", []byte("not a video"), VideoOptions{}); err == nil {
		t.Error("not a video: no error")
	}
}

// TestDecodeVideoAudioTrackCodecs (#1739): HE-AAC (SBR, and SBR with
// parametric stereo), AC-3, E-AC-3 and surround Opus tracks decode as
// ffmpeg 9 decodes them, within the stated tolerance. The HE-AAC tracks are compared at
// their own rate, downmixed (testdata/va-heaac*.ffmpeg-mono.s16: `ffmpeg -i
// f -vn -ac 1 -f f32le`, then rounded to 16 bits by `ffmpeg -f f32le -ar
// 44100 -ac 1 -i x.f32 -f s16le`, since a 16-bit -ac 1 normalises the
// downmix; within half a 16-bit step more);
// the 5.1 AC-3 and E-AC-3 ones through DecodeVideoAudio, downmixed with the
// bitstream's centre and surround mix levels as ffmpeg's resampler is given
// them and resampled to 16 kHz (testdata/va-*ac3.ffmpeg-16k.f32, `ffmpeg -i
// f -vn -ac 1 -ar 16000 -f f32le`). (The resampler is checked on its own in
// TestResampleKeepsATone; at 44.1 kHz to 16 kHz its table interpolation
// and libswresample's polyphase filter differ by up to 1e-2 near the
// cutoff, which is why the HE-AAC tracks are compared before it.) The
// files, 1.5 seconds each:
//
//	S="aevalsrc=0.3*sin(2*PI*440*t)+0.1*sin(2*PI*7000*t)+0.05*sin(2*PI*(3000+4000*t)*t)|0.3*sin(2*PI*660*t)+0.08*sin(2*PI*9500*t):s=44100:d=1.5"
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1.5 -f lavfi -i "$S" -c:v libx264 -c:a aac_at -profile:a 4 -b:a 48k -shortest -map_metadata -1 va-heaac.mp4
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1.5 -f lavfi -i "$S" -c:v libx264 -c:a aac_at -profile:a 28 -b:a 32k -shortest -map_metadata -1 va-heaacv2.mp4
//	S6="aevalsrc=0.3*sin(2*PI*440*t)|0.3*sin(2*PI*660*t)|0.25*sin(2*PI*330*t)|0.2*sin(2*PI*60*t)|0.15*sin(2*PI*1500*t)*sin(2*PI*2*t)|0.15*sin(2*PI*2500*t):s=48000:d=1.5:channel_layout=5.1"
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1.5 -f lavfi -i "$S6" -c:v libx264 -c:a ac3 -b:a 192k -shortest -map_metadata -1 va-ac3.mp4
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1.5 -f lavfi -i "$S6" -c:v libx264 -c:a eac3 -b:a 192k -shortest -map_metadata -1 va-eac3.mp4
//	ffmpeg -f lavfi -i color=c=blue:s=64x36:r=2:d=1.5 -f lavfi -i "$S6" -c:v libx264 -c:a libopus -b:a 192k -shortest -map_metadata -1 va-opus51.mp4
//
// The 5.1 Opus one (mapping family 1, #1739) is compared with ffmpeg's
// libopus decode (`ffmpeg -c:a libopus -i f -vn -t 1.5 -ac 1 -ar 16000 -f f32le`),
// downmixed with the default levels.//
// (aac_at is macOS's AudioToolbox encoder: profile 4 is HE-AAC, 28 HE-AACv2.)
func TestDecodeVideoAudioTrackCodecs(t *testing.T) {
	for _, c := range []struct {
		file    string
		maxDiff float64 // per sample, full scale 1
	}{
		{"va-heaac.mp4", 5e-4},   // SBR is float: measured 3.2e-4, RMS 3.5e-5
		{"va-heaacv2.mp4", 1e-3}, // measured 5.8e-4, RMS 7.1e-5
		{"va-ac3.mp4", 1e-3},     // measured 5.7e-4 (RMS 7.7e-5): ffmpeg's dither differs from ours
		{"va-eac3.mp4", 1e-3},    // measured 2.7e-4 (RMS 3.3e-5)
		{"va-opus51.mp4", 1e-4},  // libopus transpiled: measured 3.6e-5, RMS 9.2e-6
	} {
		b, err := os.ReadFile(filepath.Join("testdata", c.file))
		if err != nil {
			t.Fatal(err)
		}
		var got, want []float32
		skip := 0
		if strings.HasPrefix(c.file, "va-heaac") {
			got, _ = soundTrackOf(t, c.file)
			ref := readTestdata(t, strings.TrimSuffix(c.file, ".mp4")+".ffmpeg-mono.s16")
			want = make([]float32, len(ref)/2)
			for i := range want {
				want[i] = float32(int16(binary.LittleEndian.Uint16(ref[2*i:]))) / 32768
			}
		} else {
			if got, err = DecodeVideoAudio("video/mp4", b, VideoOptions{}); err != nil {
				t.Fatalf("%s: %v", c.file, err)
			}
			ref := readTestdata(t, strings.TrimSuffix(c.file, ".mp4")+".ffmpeg-16k.f32")
			want = make([]float32, len(ref)/4)
			for i := range want {
				want[i] = math.Float32frombits(binary.LittleEndian.Uint32(ref[4*i:]))
			}
			// The first and last 32 samples (the resampling filter's
			// half-length), where libswresample's handling of the stream's
			// ends differs from resample's, are left out.
			skip = resampleZeros
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d samples, ffmpeg %d", c.file, len(got), len(want))
		}
		var maxDiff, ss float64
		for i := skip; i < len(got)-skip; i++ {
			d := math.Abs(float64(got[i] - want[i]))
			maxDiff = max(maxDiff, d)
			ss += d * d
		}
		t.Logf("%s: max %.2e, RMS %.2e from ffmpeg", c.file, maxDiff, math.Sqrt(ss/float64(len(got)-2*skip)))
		if maxDiff > c.maxDiff {
			t.Errorf("%s: %.2e from ffmpeg's decode, want at most %.0e", c.file, maxDiff, c.maxDiff)
		}
	}
}
