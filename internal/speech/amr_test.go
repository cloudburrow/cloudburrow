package speech

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// TestAMRDecode (#1799): JFK's sample encoded as AMR (12.2 kbit/s, by
// opencore-amr's own amrnb-enc) and AMR-WB (23.85 kbit/s, by
// vo-amrwbenc's amrwb-enc) decodes, through amr.wasm, to exactly the
// samples opencore-amr's native amrnb-dec and amrwb-dec write (their
// SHA-256 here); auto decoding detects the magic, an explicit config reads
// the frames without it, and bad data is refused.
func TestAMRDecode(t *testing.T) {
	for _, tc := range []struct {
		file, enc string
		rate, n   int
		sha       string
	}{
		{"testdata/jfk.amr", "AMR", 8000, 88000, "7e1a5b596ea50571ddc6c17f4358ed226b5409fcd6d2a34a7d643859a3c7296a"},
		{"testdata/jfk.awb", "AMR_WB", 16000, 176000, "ef98525847d7c381b097107249ba08e272306e2aa507605f346638d54981077a"},
	} {
		b, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		pcm, rate, err := amrPCM(b, tc.enc == "AMR_WB", MaxSeconds)
		if err != nil || rate != tc.rate || len(pcm) != tc.n {
			t.Fatalf("%s: %d samples at %d Hz, %v", tc.file, len(pcm), rate, err)
		}
		raw := make([]byte, 2*len(pcm))
		for i, v := range pcm {
			binary.LittleEndian.PutUint16(raw[2*i:], uint16(int16(v*32768)))
		}
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != tc.sha {
			t.Errorf("%s: samples differ from opencore-amr's native decoder", tc.file)
		}
		auto, err := Decode(speechconfig.Decoding{Auto: true}, b)
		if err != nil || len(auto) != 11*SampleRate {
			t.Errorf("%s auto: %d samples, %v", tc.file, len(auto), err)
		}
		magic := len("#!AMR\n")
		if tc.enc == "AMR_WB" {
			magic = len("#!AMR-WB\n")
		}
		bare, err := Decode(speechconfig.Decoding{Encoding: tc.enc}, b[magic:])
		if err != nil || len(bare) != len(auto) {
			t.Errorf("%s without the magic: %d samples, %v", tc.file, len(bare), err)
		}
		if _, err := Decode(speechconfig.Decoding{Encoding: tc.enc}, b[:len(b)-3]); err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s truncated: %v", tc.file, err)
		}
		if _, _, err := amrPCM(b, tc.enc == "AMR_WB", 5); !errors.Is(err, ErrTooLong) {
			t.Errorf("%s over 5 s: %v", tc.file, err)
		}
	}
	if _, err := Decode(speechconfig.Decoding{Encoding: "AMR"}, []byte("#!AMR-WB\n\x44")); err == nil {
		t.Error("AMR-WB data as AMR is accepted")
	}
	if _, err := Decode(speechconfig.Decoding{Auto: true}, []byte("#!AMR_MC1.0\n\x00\x00\x00\x01")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("multi-channel AMR: %v", err)
	}
}
