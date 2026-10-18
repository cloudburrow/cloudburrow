package bigqueryfront

import (
	"math"
	"testing"
)

// TestHolt: Holt's linear method on 1, 3, 4 with α = β = 0.5, by hand:
// ℓ_0 = 1, b_0 = 2; t = 1: forecast 3, error 0, ℓ = 3, b = 2; t = 2:
// forecast 5, error −1, ℓ = 0.5·4 + 0.5·5 = 4.5, b = 0.5·1.5 + 0.5·2 =
// 1.75. One value has slope 0; none is all zeros.
func TestHolt(t *testing.T) {
	l, b, sse := holt([]float64{1, 3, 4}, 0.5, 0.5)
	if l != 4.5 || b != 1.75 || sse != 1 {
		t.Errorf("holt(1, 3, 4): level %v slope %v sse %v; want 4.5, 1.75, 1", l, b, sse)
	}
	if l, b, sse := holt([]float64{7}, 0.5, 0.5); l != 7 || b != 0 || sse != 0 {
		t.Errorf("holt(7): %v %v %v; want 7, 0, 0", l, b, sse)
	}
	if l, b, sse := holt(nil, 0.5, 0.5); l != 0 || b != 0 || sse != 0 {
		t.Errorf("holt(): %v %v %v; want 0, 0, 0", l, b, sse)
	}
}

// TestSeasonETSGrowing: a period-2 component whose amplitude grows by 1 a
// cycle (1, −1, 2, −2, 3, −3) is forecast by each position's level and
// slope: 4, −4, 5, −5, not the last cycle's 3, −3 again. Each position
// is exactly linear, so every α, β fits it with no error and the first
// pair, 0 and 0, is kept.
func TestSeasonETSGrowing(t *testing.T) {
	c := []float64{1, -1, 2, -2, 3, -3}
	e := fitSeasonETS(c, 2)
	if e.Alpha != 0 || e.Beta != 0 {
		t.Errorf("α %v β %v; want 0, 0", e.Alpha, e.Beta)
	}
	for k, want := range []float64{4, -4, 5, -5, 6, -6} {
		if got := e.forecast(len(c), 2, len(c)+k); math.Abs(got-want) > 1e-12 {
			t.Errorf("point %d: %v; want %v", len(c)+k, got, want)
		}
	}
	// A training that ends mid-cycle: position 0's last value is point 4
	// (3), position 1's point 3 (−2); point 5 is one cycle past
	// position 1's, point 6 one past position 0's.
	e = fitSeasonETS(c[:5], 2)
	for k, want := range map[int]float64{5: -3, 6: 4, 7: -4} {
		if got := e.forecast(5, 2, k); math.Abs(got-want) > 1e-12 {
			t.Errorf("mid-cycle point %d: %v; want %v", k, got, want)
		}
	}
}

// TestSeasonETSFlat: a component that repeats exactly is forecast as the
// same cycle, whatever the constants.
func TestSeasonETSFlat(t *testing.T) {
	c := []float64{3, -1, -2, 3, -1, -2, 3, -1, -2}
	e := fitSeasonETS(c, 3)
	for k := 9; k < 18; k++ {
		if got := e.forecast(len(c), 3, k); got != c[k%3] {
			t.Errorf("point %d: %v; want %v", k, got, c[k%3])
		}
	}
}

// TestSeasonETSFit: the fitted constants minimise the one-step squared
// error over every point of the search grid, and a position series whose
// level shifts (0, 0, 0, 10, 10, 10) takes a level constant above 0,
// which follows the shift: its forecast is near 10, not 0.
func TestSeasonETSFit(t *testing.T) {
	c := []float64{0, 5, 0, 5, 0, 5, 10, 5, 10, 5, 10, 5}
	e := fitSeasonETS(c, 2)
	pos := cyclePositions(c, 2)
	sse := func(a, b float64) float64 {
		var s float64
		for _, x := range pos {
			_, _, v := holt(x, a, b)
			s += v
		}
		return s
	}
	best := sse(e.Alpha, e.Beta)
	for i := 0; i <= 100; i++ {
		for j := 0; j <= 100; j++ {
			if s := sse(float64(i)/100, float64(j)/100); s < best-1e-9 {
				t.Errorf("α %v β %v: sse %v below the fit's %v (α %v β %v)", float64(i)/100, float64(j)/100, s, best, e.Alpha, e.Beta)
			}
		}
	}
	if e.Alpha <= 0 {
		t.Errorf("α %v; want above 0 for a level shift", e.Alpha)
	}
	if f := e.forecast(len(c), 2, len(c)); math.Abs(f-10) > 3 {
		t.Errorf("forecast %v; want near 10", f)
	}
	if f := e.forecast(len(c), 2, len(c)+1); f != 5 {
		t.Errorf("constant position's forecast %v; want 5", f)
	}
}

// TestArimaSeasonalETS: an ARIMA_PLUS series whose weekly amplitude grows
// is forecast with its seasonal component's ETS: from one forecast week
// to the next each weekday's seasonal part moves by that position's
// slope, the same each week, and the forecast weeks' amplitude grows
// past the last trained week's (a repeated cycle would hold it).
func TestArimaSeasonalETS(t *testing.T) {
	pattern := []float64{3, -1, -2, 0, 1, -4, 3}
	var y []float64
	for k := 0; k < 56; k++ {
		y = append(y, 10+(1+float64(k)/14)*pattern[k%7])
	}
	cols, rows := arimaRows(y)
	o := arimaOpts("seasonalities", "clean_spikes_and_dips", "adjust_step_changes")
	o.TS.Seasonalities, o.TS.CleanSpikes, o.TS.AdjustSteps = "WEEKLY", false, false
	tr, fe := trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	if len(s.ETS) != 1 || len(s.ETS[0].Level) != 7 {
		t.Fatalf("ETS %+v; want one fit of 7 positions", s.ETS)
	}
	n := len(y)
	amp := func(from int) float64 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for k := from; k < from+7; k++ {
			v := s.seasonalPart(0, k)
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		return hi - lo
	}
	if a, b := amp(n-7), amp(n+7); b <= a+1 {
		t.Errorf("amplitude %v in the last week, %v two weeks on; want it to grow", a, b)
	}
	for k := n; k < n+7; k++ {
		d1 := s.seasonalPart(0, k+7) - s.seasonalPart(0, k)
		d2 := s.seasonalPart(0, k+14) - s.seasonalPart(0, k+7)
		if slope := s.ETS[0].Slope[k%7]; math.Abs(d1-slope) > 1e-9 || math.Abs(d2-slope) > 1e-9 {
			t.Errorf("point %d: weekly steps %v, %v; want the slope %v", k, d1, d2, slope)
		}
		if k == n && s.seasonalPart(0, k) == s.seasonalPart(0, k-7) {
			t.Errorf("point %d repeats the last cycle", k)
		}
	}
	// A model stored before #1600 (no ETS) forecasts the same.
	old := s
	old.ETS = nil
	for k := n; k < n+14; k++ {
		if a, b := old.seasonalPart(0, k), s.seasonalPart(0, k); a != b {
			t.Errorf("point %d: %v without a stored fit; %v with", k, a, b)
		}
	}
}
