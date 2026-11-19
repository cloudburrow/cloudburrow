package speech

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
)

// TestTranscribeBias reads testdata/adaptation.wav, generated with macOS's
// say ("We met Siobhan Nyhuis at the Kildrummy station."), whose two rare
// names whisper misspells unaided: biased towards Nyhuis (a phrase with a
// boost, speechbias.go's mapping of boost 10) it spells that one, and still
// misspells Kildrummy, which nothing boosts.
func TestTranscribeBias(t *testing.T) {
	modelForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b, err := os.ReadFile("testdata/adaptation.wav")
	if err != nil {
		t.Fatal(err)
	}
	samples, err := embedding.DecodeAudio("audio/wav", b)
	if err != nil {
		t.Fatal(err)
	}
	text := func(opt Options) string {
		res, err := Transcribe(ctx, samples, opt)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range res.Segments {
			out = append(out, s.Text)
		}
		return strings.Join(out, " ")
	}
	plain := text(Options{Language: "en"})
	if strings.Contains(plain, "Nyhuis") || strings.Contains(plain, "Kildrummy") {
		t.Fatalf("unadapted transcript %q already has the rare names: the test proves nothing", plain)
	}
	biased := text(Options{Language: "en", Prompt: "Nyhuis", Phrases: []Phrase{{Text: "Nyhuis", Bias: 10}}})
	if !strings.Contains(biased, "Nyhuis") {
		t.Errorf("boosted transcript %q lacks Nyhuis", biased)
	}
	if strings.Contains(biased, "Kildrummy") {
		t.Errorf("boosted transcript %q has Kildrummy, which was not boosted", biased)
	}
}

func TestBiasSpec(t *testing.T) {
	got := biasSpec([]Phrase{{Text: "  New   York ", Bias: 7.5}, {Text: "zero", Bias: 0}, {Text: " ", Bias: 3}})
	if got != "7.5\tNew York\n" {
		t.Fatalf("biasSpec = %q", got)
	}
}

func TestCutPoint(t *testing.T) {
	s := make([]float32, 100*SampleRate)
	for i := range s {
		s[i] = 0.5
	}
	// A silent 100 ms at 95 s: a 100 s piece ends there, in its last tenth.
	for i := 95 * SampleRate; i < 95*SampleRate+SampleRate/10; i++ {
		s[i] = 0
	}
	if got := CutPoint(s, 0, 100*SampleRate); got != len(s) {
		t.Errorf("whole audio: %d", got)
	}
	got := CutPoint(s, 0, 99*SampleRate)
	if got < 95*SampleRate || got > 95*SampleRate+SampleRate/10 {
		t.Errorf("cut at %v s, want the silence at 95 s", float64(got)/SampleRate)
	}
}
