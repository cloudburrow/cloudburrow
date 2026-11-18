package ac3

import (
	"math"
	"math/rand"
	"testing"
)

// forwardMDCT is the encoder's transform of clause 7.2.3.2 on one
// windowed 512-sample block: one 512-point transform, or (short) two
// 256-point transforms interleaved coefficient by coefficient.
func forwardMDCT(x []float64, short bool) [256]float64 {
	var out [256]float64
	tr := func(x []float64, n int, alpha float64) []float64 {
		X := make([]float64, n/2)
		for k := range X {
			s := 0.0
			for i := range n {
				s += x[i] * math.Cos(2*math.Pi/float64(4*n)*float64(2*i+1)*float64(2*k+1)+math.Pi/4*float64(2*k+1)*(1+alpha))
			}
			X[k] = -2 / float64(n) * s
		}
		return X
	}
	if !short {
		copy(out[:], tr(x, 512, 0))
		return out
	}
	a, b := tr(x[:256], 256, -1), tr(x[256:], 256, 1)
	for k := range 128 {
		out[2*k], out[2*k+1] = a[k], b[k]
	}
	return out
}

// TestIMDCTReconstructs: the inverse transforms of clause 6.9.4, long and
// short (block switched) in any order, overlap-add the encoder's
// transforms of clause 7.2.3 of a signal back to the signal. ffmpeg's AC-3
// encoder never switches blocks, so this is what checks the 256-sample
// transforms.
func TestIMDCTReconstructs(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const blocks = 40
	sig := make([]float64, (blocks+1)*256)
	for i := range sig {
		sig[i] = rng.Float64() - 0.5
	}
	var delay [256]float64
	out := make([]float64, blocks*256)
	for b := range blocks {
		short := rng.Intn(2) == 1
		var x [512]float64
		for n := range 512 {
			wi := n
			if n >= 256 {
				wi = 511 - n
			}
			x[n] = sig[b*256+n] * window[wi]
		}
		X := forwardMDCT(x[:], short)
		imdct(&X, short, &delay, out[b*256:(b+1)*256])
	}
	maxd := 0.0
	for i := 256; i < len(out); i++ {
		maxd = math.Max(maxd, math.Abs(out[i]-sig[i]))
	}
	if maxd > 1e-9 {
		t.Fatalf("the reconstruction differs by up to %g", maxd)
	}
}

// TestWindow: the KBD window computed is Table 6.33's to its five places.
func TestWindow(t *testing.T) {
	for i, want := range map[int]float64{0: 0.00014, 9: 0.00187, 100: 0.44030, 128: 0.71150, 130: 0.72877, 200: 0.99558, 255: 1} {
		if math.Abs(window[i]-want) > 0.000005 {
			t.Errorf("w[%d] = %.6f, Table 6.33 %.5f", i, window[i], want)
		}
	}
}
