package bigqueryfront

import (
	"math"
	"testing"
)

// TestSTLSmoothers checks STL's pieces against values computed by hand.
func TestSTLSmoothers(t *testing.T) {
	// The moving average of 3: (1+2+3)/3, (2+3+4)/3, (3+4+5)/3.
	if got := stlMovingAverage([]float64{1, 2, 3, 4, 5}, 3); len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Errorf("moving average: %v", got)
	}
	// A locally linear loess reproduces a line, fitted at every point or
	// at every third and interpolated.
	line := make([]float64, 11)
	for i := range line {
		line[i] = 2*float64(i) + 1
	}
	for _, jump := range []int{1, 3} {
		for i, v := range stlLoess(line, 5, 1, jump, false, nil) {
			if math.Abs(v-line[i]) > 1e-12 {
				t.Errorf("loess of a line, jump %d: point %d = %v; want %v", jump, i, v, line[i])
			}
		}
	}
	// stlest at xs 3 over points 1..5 (h 2), degree 0: tricube weights
	// (1 − (r/2)³)³ of r 2, 1, 0, 1, 2 are 0 (r > 0.999h), 0.669921875,
	// 1, 0.669921875, 0, so the fit of 0, 0, 3, 6, 100 is
	// (6 · 0.669921875 + 3) / (2 · 0.669921875 + 1).
	w := math.Pow(1-1.0/8, 3)
	if v, ok := stlEstimate([]float64{0, 0, 3, 6, 100}, 5, 0, 3, 1, 5, false, nil); !ok || math.Abs(v-(6*w+3)/(2*w+1)) > 1e-12 {
		t.Errorf("stlest: %v, %v; want %v", v, ok, (6*w+3)/(2*w+1))
	}
	// Robustness weights: residuals 0, 1, 2, 3, 10; 6 · median = 12, so
	// bisquare weights 1, (1 − 1/144)², (1 − 4/144)², (1 − 9/144)²,
	// (1 − 100/144)².
	rw := make([]float64, 5)
	stlRobustWeights([]float64{0, 1, 2, 3, 10}, make([]float64, 5), rw)
	for i, r := range []float64{0, 1, 2, 3, 10} {
		want := math.Pow(1-r*r/144, 2)
		if math.Abs(rw[i]-want) > 1e-12 {
			t.Errorf("robust weight of %v: %v; want %v", r, rw[i], want)
		}
	}
}

// TestSTLPeriodic: a level plus an exactly periodic pattern summing to
// zero is decomposed exactly: each cycle-subseries is constant, so its
// loess is; the low-pass filter of the pattern is 0; the seasonal is the
// pattern and the trend the level, plainly and robustly.
func TestSTLPeriodic(t *testing.T) {
	pattern := []float64{3, -1, -2, 0}
	var x []float64
	for i := 0; i < 24; i++ {
		x = append(x, 10+pattern[i%4])
	}
	for _, robust := range []bool{false, true} {
		fit := stl(x, 4, 7, robust)
		for i := range x {
			if math.Abs(fit.Seasonal[i]-pattern[i%4]) > 1e-9 || math.Abs(fit.Trend[i]-10) > 1e-9 {
				t.Fatalf("robust %v, point %d: seasonal %v, trend %v; want %v and 10", robust, i, fit.Seasonal[i], fit.Trend[i], pattern[i%4])
			}
		}
	}
	// With a slope as well, the seasonal is still close to the pattern and
	// the components add up to the data less the remainder.
	for i := range x {
		x[i] += 0.5 * float64(i)
	}
	fit := stl(x, 4, 7, false)
	for i := 4; i < 20; i++ {
		if math.Abs(fit.Seasonal[i]-pattern[i%4]) > 0.2 {
			t.Errorf("sloped, point %d: seasonal %v; want about %v", i, fit.Seasonal[i], pattern[i%4])
		}
	}
}
