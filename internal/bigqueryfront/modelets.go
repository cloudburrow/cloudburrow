package bigqueryfront

// The seasonal components' forecast (#1600). BigQuery's ARIMA_PLUS
// pipeline extrapolates "seasonality by using double exponential
// smoothing (ETS)"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series),
// and gives no more than that. UNVERIFIED, chosen here as the standard
// double exponential smoothing, Holt's linear method, all deterministic:
//   - each seasonal component is smoothed by cycle position: the values
//     at one position of the cycle, one per cycle, are a series of their
//     own, and a point h cycles past that position's last value is
//     forecast as its level plus h times its slope;
//   - Holt's recursions, level ℓ_t = α·x_t + (1 − α)(ℓ_{t−1} + b_{t−1})
//     and slope b_t = β(ℓ_t − ℓ_{t−1}) + (1 − β)b_{t−1}, start from
//     ℓ_0 = x_0 and b_0 = x_1 − x_0 (a position with one value has slope
//     0);
//   - α and β, shared by a component's positions, are fitted by least
//     squares: they minimise the sum, over every position, of the squared
//     one-step errors x_t − (ℓ_{t−1} + b_{t−1}), searched on [0, 1] on a
//     grid of 0.05, α then β ascending, and then of 0.005 about the
//     grid's best; a tie keeps the pair found first.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series

// seasonETS is one seasonal component's fitted double exponential
// smoothing: the constants, and each cycle position's final level and
// slope.
type seasonETS struct {
	Alpha float64   `json:"alpha"`
	Beta  float64   `json:"beta"`
	Level []float64 `json:"level"`
	Slope []float64 `json:"slope"`
}

// holt runs Holt's linear method on x with α and β: its final level and
// slope, and the sum of its squared one-step errors.
func holt(x []float64, alpha, beta float64) (level, slope, sse float64) {
	if len(x) == 0 {
		return 0, 0, 0
	}
	level = x[0]
	if len(x) == 1 {
		return level, 0, 0
	}
	slope = x[1] - x[0]
	for _, v := range x[1:] {
		f := level + slope
		e := v - f
		sse += e * e
		l := alpha*v + (1-alpha)*f
		slope = beta*(l-level) + (1-beta)*slope
		level = l
	}
	return level, slope, sse
}

// cyclePositions splits c, a component of period p, into its cycle
// positions' series: position r holds c[r], c[r+p], c[r+2p], ….
func cyclePositions(c []float64, p int) [][]float64 {
	pos := make([][]float64, p)
	for k, v := range c {
		pos[k%p] = append(pos[k%p], v)
	}
	return pos
}

// fitSeasonETS fits the double exponential smoothing of c, a seasonal
// component of period p.
func fitSeasonETS(c []float64, p int) seasonETS {
	pos := cyclePositions(c, p)
	sse := func(a, b float64) float64 {
		var s float64
		for _, x := range pos {
			_, _, e := holt(x, a, b)
			s += e
		}
		return s
	}
	best, ba, bb := sse(0, 0), 0.0, 0.0
	// The coarse grid, then the fine one about its best.
	for i := 0; i <= 20; i++ {
		for j := 0; j <= 20; j++ {
			a, b := float64(i)*0.05, float64(j)*0.05
			if s := sse(a, b); s < best {
				best, ba, bb = s, a, b
			}
		}
	}
	ca, cb := ba, bb
	for i := -10; i <= 10; i++ {
		for j := -10; j <= 10; j++ {
			a, b := ca+float64(i)*0.005, cb+float64(j)*0.005
			if a < 0 || a > 1 || b < 0 || b > 1 {
				continue
			}
			if s := sse(a, b); s < best {
				best, ba, bb = s, a, b
			}
		}
	}
	e := seasonETS{Alpha: ba, Beta: bb, Level: make([]float64, p), Slope: make([]float64, p)}
	for r, x := range pos {
		e.Level[r], e.Slope[r], _ = holt(x, ba, bb)
	}
	return e
}

// forecast is the component at point k ≥ n, n the training's length:
// its cycle position's level plus its slope times the cycles from that
// position's last value.
func (e seasonETS) forecast(n, p, k int) float64 {
	r := k % p
	if len(e.Level) != p {
		return 0
	}
	if n <= r { // the position has no value
		return 0
	}
	last := r + p*((n-1-r)/p)
	h := (k - last) / p
	return e.Level[r] + float64(h)*e.Slope[r]
}
