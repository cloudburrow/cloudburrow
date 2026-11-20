package embedding

import (
	"bytes"
	"math"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/cloudburrow/cloudburrow/internal/isoconformance"
)

// TestFullParametricStereoTrack (#1754): an MP4 sound track of HE-AAC v2
// with the full parametric stereo tool, ISO/IEC 14496-26's al_sbr_ps_06
// (34 stereo bands; a mono core signalled by a program_config_element),
// decodes to its stereo output downmixed, the same as ISO's reference
// decode (al_sbr_ps_06_ur.wav) read as a WAV part: the same length and
// within 90 dB. A program's single SCE named as one channel once made the
// downmix read the stereo output as mono, twice as long. ISO's files are
// fetched at test time (internal/isoconformance), never committed; the
// test skips when they are not cached and CLOUDBURROW_TEST_DOWNLOADS is
// unset.
func TestFullParametricStereoTrack(t *testing.T) {
	stream := isoconformance.File(t, "al_sbr_ps_06.mp4")
	ref, err := DecodeAudio("audio/wav", isoconformance.File(t, "al_sbr_ps_06_ur.wav"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	mono, rate, err := decodeSoundTrack(f, f.Moov.Trak, stream)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 32000 {
		t.Fatalf("rate %d, want 32000", rate)
	}
	got := resample(mono, rate, AudioSampleRate)
	if len(got) != len(ref) {
		t.Fatalf("%d samples at 16 kHz, the reference WAV %d", len(got), len(ref))
	}
	var se, sig float64
	for i := range got {
		d := float64(got[i] - ref[i])
		se += d * d
		sig += float64(ref[i]) * float64(ref[i])
	}
	snr := 10 * math.Log10(sig/se)
	t.Logf("%d samples, SNR %.1f dB against the reference WAV", len(got), snr)
	if snr < 90 {
		t.Errorf("SNR %.1f dB against the reference WAV, want at least 90", snr)
	}
}
