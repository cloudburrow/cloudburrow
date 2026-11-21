package speech

import (
	"context"
	"math"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// TestDenoise (#1799): JFK's sample with white noise added (seeded) is
// denoised by rnnoise.wasm: the result is aligned with the clean speech
// (its frame of delay removed: the correlation peaks at lag 0) and much
// closer to it than the noisy input is, its pauses at least four times
// quieter.
func TestDenoise(t *testing.T) {
	b, err := os.ReadFile("testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	clean, err := Decode(speechconfig.Decoding{Auto: true}, b)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	noisy := make([]float32, len(clean))
	for i, v := range clean {
		noisy[i] = v + float32(r.NormFloat64()*0.03)
	}
	out, err := Denoise(context.Background(), noisy)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(clean) {
		t.Fatalf("%d samples, want %d", len(out), len(clean))
	}
	errEnergy := func(x []float32) float64 {
		var e float64
		for i := range x {
			d := float64(x[i] - clean[i])
			e += d * d
		}
		return e
	}
	if before, after := errEnergy(noisy), errEnergy(out); after >= before {
		t.Errorf("error energy %.1f, noisy %.1f: not reduced", after, before)
	}
	// In the pauses (20 ms windows of near silence) the noise is mostly gone.
	var quietNoisy, quietOut float64
	for at := 0; at+320 <= len(clean); at += 320 {
		var e float64
		for _, v := range clean[at : at+320] {
			e += float64(v) * float64(v)
		}
		if math.Sqrt(e/320) > 0.005 {
			continue
		}
		for i := at; i < at+320; i++ {
			quietNoisy += float64(noisy[i]) * float64(noisy[i])
			quietOut += float64(out[i]) * float64(out[i])
		}
	}
	if quietNoisy == 0 || quietOut > quietNoisy/4 {
		t.Errorf("noise in the pauses: %.2f denoised, %.2f noisy", quietOut, quietNoisy)
	}
	best, bestC := 0, math.Inf(-1)
	for lag := -320; lag <= 320; lag++ {
		var c float64
		for i := 1000; i < len(clean)-1000; i++ {
			c += float64(out[i+lag]) * float64(clean[i])
		}
		if c > bestC {
			best, bestC = lag, c
		}
	}
	if best < -2 || best > 2 {
		t.Errorf("denoised audio is %d samples off the clean speech", best)
	}
	if empty, err := Denoise(context.Background(), nil); err != nil || len(empty) != 0 {
		t.Errorf("no samples: %v %v", empty, err)
	}
}

// TestFilterSNR (#1799): a window whose kept signal is below threshold
// times the removed noise's power is silenced; one above it, or with no
// noise, passes; threshold 0 filters nothing.
func TestFilterSNR(t *testing.T) {
	n := 2 * snrWindow
	orig, clean := make([]float32, n), make([]float32, n)
	for i := range orig {
		clean[i] = 0.5
		noise := float32(0.05) // first window: SNR 100
		if i >= snrWindow {
			noise = 0.25 // second: SNR 4
		}
		orig[i] = clean[i] + noise
	}
	out := FilterSNR(orig, orig, clean, 10)
	if out[0] != orig[0] || out[snrWindow-1] != orig[0] || out[snrWindow] != 0 || out[n-1] != 0 {
		t.Errorf("threshold 10: %v %v %v", out[0], out[snrWindow], out[n-1])
	}
	if out := FilterSNR(orig, orig, clean, 0); out[n-1] != orig[n-1] {
		t.Error("threshold 0 filtered")
	}
	if out := FilterSNR(clean, clean, clean, 1000); out[0] != clean[0] {
		t.Error("a window with no noise was filtered")
	}
}
