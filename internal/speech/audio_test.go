package speech

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

func TestDecode(t *testing.T) {
	wav, err := os.ReadFile("testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Decode(speechconfig.Decoding{Auto: true}, wav)
	if err != nil || len(s) != 176000 {
		t.Fatalf("WAV: %d samples, %v", len(s), err)
	}
	// The same file with an explicit LINEAR16 encoding: its header is read.
	if s2, err := Decode(speechconfig.Decoding{Encoding: "LINEAR16", SampleRate: 16000, Channels: 1}, wav); err != nil || len(s2) != len(s) {
		t.Errorf("WAV as LINEAR16: %d, %v", len(s2), err)
	}
	// Headerless stereo LINEAR16 at 8 kHz: its first channel, resampled.
	pcm := make([]byte, 8000*2*2)
	for i := 0; i < 8000; i++ {
		v := int16(10000 * math.Sin(float64(i)/10))
		binary.LittleEndian.PutUint16(pcm[4*i:], uint16(v))
		binary.LittleEndian.PutUint16(pcm[4*i+2:], uint16(v))
	}
	if s, err := Decode(speechconfig.Decoding{Encoding: "LINEAR16", SampleRate: 8000, Channels: 2}, pcm); err != nil || len(s) < 15900 || len(s) > 16100 {
		t.Errorf("headerless LINEAR16: %d, %v", len(s), err)
	}
	if s, err := Decode(speechconfig.Decoding{Encoding: "MULAW", SampleRate: 8000, Channels: 1}, make([]byte, 800)); err != nil || len(s) < 1590 {
		t.Errorf("MULAW: %d, %v", len(s), err)
	}
	if _, err := Decode(speechconfig.Decoding{Encoding: "LINEAR16", SampleRate: 4000, Channels: 1}, pcm); err == nil {
		t.Error("4 kHz LINEAR16 accepted")
	}
	for name, data := range map[string][]byte{"AMR": []byte("#!AMR\n\x3c"), "AMR_WB": []byte("#!AMR-WB\n\x3c")} {
		if _, err := Decode(speechconfig.Decoding{Auto: true}, data); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Decode(speechconfig.Decoding{Auto: true}, []byte("hello world, not audio")); err == nil || errors.Is(err, ErrUnsupported) {
		t.Errorf("text: %v", err)
	}
	if _, err := Decode(speechconfig.Decoding{Encoding: "LINEAR16", SampleRate: 8000, Channels: 1}, make([]byte, 2*8000*(MaxSeconds+1))); !errors.Is(err, ErrTooLong) {
		t.Errorf("31 minutes: %v", err)
	}
}
