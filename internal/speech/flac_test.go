package speech

import (
	"errors"
	"math"
	"os"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// The FLAC files in testdata were made from jfk.wav (whisper.cpp's sample,
// MIT) with ffmpeg 9's flac encoder: jfk.flac at -compression_level 8 (LPC subframes up to order 12,
// 16 kHz mono 16-bit), jfk-stereo24.flac from 0.5 s for 1.5 s, upsampled to
// 44.1 kHz stereo at 24 bits (stereo decorrelation).

// TestDecodeFLACIsLossless: jfk.flac decodes to exactly jfk.wav's samples.
func TestDecodeFLACIsLossless(t *testing.T) {
	wav, err := os.ReadFile("testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	fl, err := os.ReadFile("testdata/jfk.flac")
	if err != nil {
		t.Fatal(err)
	}
	want, err := embedding.DecodeWAVSpeech(wav)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFLAC(fl)
	if err != nil {
		t.Fatalf("DecodeFLAC: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d", len(got), len(want))
	}
	for i := range got {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("sample %d is %v, want %v", i, got[i], want[i])
		}
	}
	// Through Decode, sniffed and named explicitly.
	for _, d := range []speechconfig.Decoding{{Auto: true}, {Encoding: "FLAC"}} {
		if s, err := Decode(d, fl); err != nil || len(s) != len(want) {
			t.Errorf("Decode %+v: %d, %v", d, len(s), err)
		}
	}
}

// TestDecodeFLACStereo24: a 44.1 kHz stereo 24-bit stream is averaged to
// mono, resampled to 16 kHz and follows the WAV's samples.
func TestDecodeFLACStereo24(t *testing.T) {
	wav, _ := os.ReadFile("testdata/jfk.wav")
	fl, err := os.ReadFile("testdata/jfk-stereo24.flac")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := embedding.DecodeWAVSpeech(wav)
	got, err := DecodeFLAC(fl)
	if err != nil {
		t.Fatalf("DecodeFLAC: %v", err)
	}
	if len(got) < 23900 || len(got) > 24100 {
		t.Fatalf("%d samples, want 1.5 s at 16 kHz", len(got))
	}
	// Correlation with the source over the same 1.5 s.
	src := want[8000 : 8000+len(got)]
	var dot, a, b float64
	for i := range got {
		dot += float64(got[i]) * float64(src[i])
		a += float64(got[i]) * float64(got[i])
		b += float64(src[i]) * float64(src[i])
	}
	if c := dot / math.Sqrt(a*b); c < 0.98 {
		t.Errorf("correlation with the source %.3f, want ≥ 0.98", c)
	}
}

func TestDecodeFLACRefuses(t *testing.T) {
	fl, _ := os.ReadFile("testdata/jfk.flac")
	if _, _, err := embedding.DecodeFLACFile(fl, 5); !embedding.IsTooLong(err) {
		t.Errorf("11 s against a 5 s limit: %v", err)
	}
	for name, data := range map[string][]byte{
		"truncated":      fl[:len(fl)/2],
		"no streaminfo":  []byte("fLaC\x81\x00\x00\x00"),
		"not flac":       []byte("RIFF"),
		"header only":    fl[:42],
		"corrupt frames": append(append([]byte{}, fl[:8300]...), make([]byte, 64)...),
	} {
		if s, err := DecodeFLAC(data); err == nil {
			t.Errorf("%s: %d samples, no error", name, len(s))
		} else if errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v is not a data error", name, err)
		}
	}
}
