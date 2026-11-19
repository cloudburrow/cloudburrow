package speech

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// modelForTest fetches the pinned whisper model through internal/localai
// into the user's cache directory, so repeated runs fetch it once. It skips
// the test in -short mode or when the network is not reachable.
func modelForTest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("fetches the 82 MB whisper model")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	cache := localai.Cache{Dir: filepath.Join(dir, "cloudburrow-test", "models")}
	SetModelSource(func(ctx context.Context) (string, error) {
		p, err := cache.FetchPinnedFile(ctx, localai.Whisper, localai.Whisper.Pin.Files[0])
		if err != nil {
			t.Skipf("fetching the whisper model: %v", err)
		}
		return p, nil
	})
	t.Cleanup(func() { SetModelSource(nil) })
}

func jfk(t *testing.T) []float32 {
	t.Helper()
	b, err := os.ReadFile("testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	s, err := embedding.DecodeAudio("audio/wav", b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTranscribe reads whisper.cpp's own sample, eleven seconds of John F.
// Kennedy's inaugural address, in English and with language detection, with
// word offsets and confidences.
func TestTranscribe(t *testing.T) {
	modelForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	samples := jfk(t)
	start := time.Now()
	res, err := Transcribe(ctx, samples, Options{Language: "en", Words: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%.1f s of audio in %v: %+v", float64(len(samples))/SampleRate, time.Since(start), res)
	var text []string
	for _, s := range res.Segments {
		text = append(text, s.Text)
		if s.Confidence <= 0 || s.Confidence > 1 || s.End <= s.Start {
			t.Errorf("segment %+v", s)
		}
	}
	got := strings.ToLower(strings.Join(text, " "))
	for _, w := range []string{"ask not what your country can do for you", "what you can do for your country"} {
		if !strings.Contains(got, w) {
			t.Errorf("transcript %q lacks %q", got, w)
		}
	}
	if res.Language != "en" || len(res.Segments) == 0 || len(res.Segments[0].Words) < 5 {
		t.Fatalf("result %+v", res)
	}
	w := res.Segments[0].Words[0]
	if w.Text != "And" || w.End <= w.Start || w.Confidence <= 0 {
		t.Errorf("first word %+v", w)
	}
	auto, err := Transcribe(ctx, samples, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if auto.Language != "en" {
		t.Errorf("detected language %q", auto.Language)
	}
	if ok, err := LanguageKnown(ctx, "de"); !ok || err != nil {
		t.Errorf("LanguageKnown(de) = %v, %v", ok, err)
	}
	if ok, _ := LanguageKnown(ctx, "xx"); ok {
		t.Error("LanguageKnown(xx)")
	}
}

func TestTranscribeWithoutModel(t *testing.T) {
	SetModelSource(nil)
	if _, err := Transcribe(context.Background(), []float32{0, 0}, Options{}); err != ErrNoModel {
		t.Fatalf("err = %v", err)
	}
}
