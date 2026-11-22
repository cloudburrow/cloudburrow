//go:build onnx

package speaker_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/speaker"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// TestCAMPlusPlusTwoSpeakers runs the pinned CAM++ model (its path in
// CLOUDBURROW_SPEAKER_MODEL; skipped without it) on testdata's two macOS
// voices (Samantha, Daniel, Samantha, Daniel), the words' spans its
// voiced 100 ms frames: two speakers, alternating.
func TestCAMPlusPlusTwoSpeakers(t *testing.T) {
	path := os.Getenv("CLOUDBURROW_SPEAKER_MODEL")
	if path == "" {
		t.Skip("CLOUDBURROW_SPEAKER_MODEL is not set")
	}
	speaker.SetModelSource(func(context.Context) (string, error) { return path, nil })
	data, err := os.ReadFile("../speech/testdata/twospeakers.wav")
	if err != nil {
		t.Fatal(err)
	}
	samples, err := speech.Decode(speechconfig.Decoding{Auto: true}, data)
	if err != nil {
		t.Fatal(err)
	}
	var spans []speaker.Span
	const step = 1600
	for i := 0; i+step <= len(samples); i += step {
		var e float64
		for _, s := range samples[i : i+step] {
			e += float64(s) * float64(s)
		}
		if e/step > 1e-4 {
			spans = append(spans, speaker.Span{Start: time.Duration(i) * time.Second / 16000, End: time.Duration(i+step) * time.Second / 16000})
		}
	}
	labels, err := speaker.Diarize(context.Background(), samples, spans, speaker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var turns []int
	for _, l := range labels {
		if len(turns) == 0 || turns[len(turns)-1] != l {
			turns = append(turns, l)
		}
	}
	t.Logf("turns: %v", turns)
	if n := speaker.Speakers(labels); n != 2 {
		t.Errorf("%d speakers, want 2: %v", n, labels)
	}
	for _, opt := range []speaker.Options{{Max: 1}, {Min: 3}} {
		labels, err := speaker.Diarize(context.Background(), samples, spans, opt)
		if err != nil {
			t.Fatal(err)
		}
		want := max(opt.Min, 1)
		if n := speaker.Speakers(labels); n != want {
			t.Errorf("%+v: %d speakers, want %d", opt, n, want)
		}
	}
}
