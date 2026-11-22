package speaker

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// TestFbankTone: a 1 kHz tone's energy is in the mel bin whose centre is
// nearest 1 kHz, and 1 s of audio is 98 frames (snip_edges).
func TestFbankTone(t *testing.T) {
	samples := make([]float32, sampleRate)
	for i := range samples {
		samples[i] = float32(0.5 * math.Sin(2*math.Pi*1000*float64(i)/sampleRate))
	}
	f := Fbank(samples)
	if len(f) != 98 || NumFrames(399) != 0 || NumFrames(400) != 1 {
		t.Fatalf("frames: %d", len(f))
	}
	lo, hi := melScale(lowFreq), melScale(sampleRate/2)
	delta := (hi - lo) / float64(NumMelBins+1)
	want, best := 0, math.Inf(1)
	for m := 0; m < NumMelBins; m++ {
		if d := math.Abs(lo + float64(m+1)*delta - melScale(1000)); d < best {
			want, best = m, d
		}
	}
	top := 0
	for m, v := range f[50] {
		if v > f[50][top] {
			top = m
		}
	}
	if top != want {
		t.Errorf("peak in bin %d, want %d", top, want)
	}
	n := FbankMeanNormalized(samples)
	var mean float64
	for _, row := range n {
		mean += float64(row[top])
	}
	if math.Abs(mean/float64(len(n))) > 1e-4 {
		t.Errorf("normalised mean %v", mean)
	}
}

// fakeEmbedder embeds a window as the direction of its first frame's
// first bin's sign: the test's two "voices" are a positive and a negative
// signal, so their embeddings are orthogonal-ish unit vectors with noise.
func fakeEmbedder(voices func(start int) int) embedder {
	r := rand.New(rand.NewPCG(1, 2))
	call := 0
	return func(ctx context.Context, feats [][][]float32) ([][]float32, error) {
		out := make([][]float32, len(feats))
		for i := range feats {
			v := make([]float32, 8)
			v[voices(call)] = 1
			for d := range v {
				v[d] += float32(0.1 * r.NormFloat64())
			}
			out[i] = v
			call++
		}
		return out, nil
	}
}

// TestDiarizeBounds: two alternating voices are two speakers, numbered in
// the order they speak; maxSpeakers 1 makes one, minSpeakers 3 three.
func TestDiarizeBounds(t *testing.T) {
	const seconds = 20
	samples := make([]float32, seconds*sampleRate)
	var spans []Span
	for ms := 0; ms < seconds*1000; ms += 400 {
		spans = append(spans, Span{time.Duration(ms) * time.Millisecond, time.Duration(ms+350) * time.Millisecond})
	}
	wins := windows(len(samples), spans)
	// Voice 0 for the first 5 s, then 1, then 0, then 1.
	voice := func(k int) int { return (wins[k].start / (5 * sampleRate)) % 2 }
	for _, tc := range []struct {
		opt  Options
		want int
	}{{Options{}, 2}, {Options{Min: 2, Max: 2}, 2}, {Options{Max: 1}, 1}, {Options{Min: 3}, 3}, {Options{Min: 1, Max: 6}, 2}} {
		labels, err := diarize(context.Background(), samples, spans, tc.opt, fakeEmbedder(voice))
		if err != nil {
			t.Fatal(err)
		}
		if got := Speakers(labels); got != tc.want {
			t.Errorf("%+v: %d speakers, want %d", tc.opt, got, tc.want)
		}
		if labels[0] != 1 {
			t.Errorf("%+v: first word is speaker %d", tc.opt, labels[0])
		}
		if tc.want == 2 {
			// Words well inside each 5 s turn.
			for i, s := range spans {
				sec := s.Start.Seconds()
				if math.Mod(sec, 5) < 1 || math.Mod(sec, 5) > 4 {
					continue
				}
				if want := int(sec/5)%2 + 1; labels[i] != want {
					t.Errorf("%+v: word at %v is speaker %d, want %d", tc.opt, s.Start, labels[i], want)
				}
			}
		}
	}
	if _, err := diarize(context.Background(), samples, spans, Options{Min: 3, Max: 2}, fakeEmbedder(voice)); err == nil {
		t.Error("min > max: no error")
	}
	// No speech: every word is speaker 1.
	labels, err := diarize(context.Background(), samples, nil, Options{}, fakeEmbedder(voice))
	if err != nil || len(labels) != 0 {
		t.Errorf("no words: %v %v", labels, err)
	}
}

// TestClusterSubsample: beyond maxClustered windows, the rest join the
// nearest centroid.
func TestClusterSubsample(t *testing.T) {
	var embs [][]float32
	for i := 0; i < maxClustered*2+7; i++ {
		v := []float32{0, 0, 0}
		v[i%2] = 1
		embs = append(embs, v)
	}
	cl := cluster(embs, Options{})
	for i := range embs {
		if cl[i] != cl[i%2] {
			t.Fatalf("window %d in cluster %d, want %d", i, cl[i], cl[i%2])
		}
	}
	if cl[0] == cl[1] {
		t.Error("the two directions share a cluster")
	}
}
