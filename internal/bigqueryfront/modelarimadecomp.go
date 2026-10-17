package bigqueryfront

import (
	"math"
	"sort"
)

// The ARIMA_PLUS pipeline's stages before the ARIMA fit (#1518,
// https://cloud.google.com/bigquery/docs/arima-plus-model): seasonal
// decomposition with the seasonal periods detected by data frequency,
// spikes-and-dips cleaning and step-change adjustment. What is left, the
// adjusted, deseasonalized series, is the trend the ARIMA is fitted to; a
// forecast is that trend's forecast plus each seasonal component
// continued by its last cycle. For every point
//
//	y = trend + Σ seasonal_period_* + spikes_and_dips + step_changes + residual
//
// exactly, the trend being the ARIMA's one-step fit of the adjusted series.
//
// Google publishes the stages, not their algorithms. UNVERIFIED, chosen
// here, all deterministic:
//   - seasonal decomposition: MSTL-style iterated classical decomposition
//     (two passes over the periods, shortest first; each period's
//     component the cycle-position means, centred to sum to zero, of the
//     series less the other components and a centred moving average of
//     the period's length); the robust first pass, used to find spikes
//     and steps, takes cycle-position medians;
//   - the candidate periods by data frequency (arimaSeasonCands), each
//     rounded to whole points; a period given in seasonalities is used if
//     the series spans two full cycles of it; with seasonalities AUTO, a
//     candidate the series spans three times is kept when its seasonal
//     strength 1 − Var(R)/Var(S + R) (Wang, Smith and Hyndman 2006) is
//     at least 0.64;
//   - step changes: on the robustly deseasonalized series, a level-shift
//     test at each point t comparing the medians of the 7 points before
//     and after it, less 7 times the median first difference (the
//     slope), in units of the robust noise σ (1.4826 MAD of the first
//     differences / √2); a step where the statistic is at least 6 and the
//     largest within 7 points, placed where, within 7 points, the
//     windows' means differ most; step_changes is the level relative to the
//     last segment, so the forecast continues from the latest level;
//   - spikes and dips: on the deseasonalized, step-adjusted series less
//     its median slope, a point whose robust z-score (its difference from
//     the median of the 7-point window about it, over 1.4826 MAD of those
//     differences) is above 5; the point is replaced by the linear
//     interpolation of its nearest clean neighbours, as BigQuery
//     documents, and spikes_and_dips is the difference.

// arimaSeason is one seasonal period: its name and its length in points.
type arimaSeason struct {
	Name   string `json:"name"`
	Period int    `json:"period"`
}

// arimaSeasonCands are the seasonal periods BigQuery documents
// (seasonalities DAILY, WEEKLY, MONTHLY, QUARTERLY and YEARLY) that a
// series of each data frequency can show, shortest first.
var arimaSeasonCands = map[string][]arimaSeason{
	"PER_MINUTE": {{"DAILY", 1440}, {"WEEKLY", 10080}},
	"HOURLY":     {{"DAILY", 24}, {"WEEKLY", 168}},
	"DAILY":      {{"WEEKLY", 7}, {"MONTHLY", 30}, {"QUARTERLY", 91}, {"YEARLY", 365}},
	"WEEKLY":     {{"YEARLY", 52}},
	"MONTHLY":    {{"QUARTERLY", 3}, {"YEARLY", 12}},
	"QUARTERLY":  {{"YEARLY", 4}},
}

const (
	arimaStepWindow  = 7
	arimaStepZ       = 6.0
	arimaSpikeWindow = 7
	arimaSpikeZ      = 5.0
	arimaStrength    = 0.64
	arimaAutoCycles  = 3
)

// arimaDecomp is a series' components before the ARIMA fit.
type arimaDecomp struct {
	Seasons  []arimaSeason
	Seasonal [][]float64
	Spikes   []float64 // nil when clean_spikes_and_dips is FALSE
	Steps    []float64 // nil when adjust_step_changes is FALSE
}

// decomposeSeries runs the stages on y, a series on its data frequency's
// grid, as opts say.
func decomposeSeries(y []float64, freq string, ts tsOptions) arimaDecomp {
	n := len(y)
	var d arimaDecomp
	var cands []arimaSeason
	auto := ts.Seasonalities == "" || ts.Seasonalities == "AUTO"
	if ts.Seasonalities != "NO_SEASONALITY" {
		want := map[string]bool{}
		for _, s := range splitSeasonalities(ts.Seasonalities) {
			want[s] = true
		}
		for _, c := range arimaSeasonCands[freq] {
			cycles := 2
			if auto {
				cycles = arimaAutoCycles
			}
			if (auto || want[c.Name]) && n >= cycles*c.Period && c.Period >= 2 {
				cands = append(cands, c)
			}
		}
	}
	// The robust first pass: which periods, and the series without them.
	var periods []arimaSeason
	if auto {
		for _, c := range cands {
			try := append(append([]arimaSeason(nil), periods...), c)
			trend, seas := mstl(y, try, true)
			if seasonalStrength(y, trend, seas, len(try)-1) >= arimaStrength {
				periods = try
			}
		}
	} else {
		periods = cands
	}
	_, seas := mstl(y, periods, true)
	des := make([]float64, n)
	for k := range y {
		des[k] = y[k]
		for _, s := range seas {
			des[k] -= s[k]
		}
	}
	adj := append([]float64(nil), y...)
	if ts.AdjustSteps {
		d.Steps = stepChanges(des)
		for k := range adj {
			des[k] -= d.Steps[k]
			adj[k] -= d.Steps[k]
		}
	}
	if ts.CleanSpikes {
		d.Spikes = spikesAndDips(des)
		for k := range adj {
			adj[k] -= d.Spikes[k]
		}
	}
	// The final pass, on the cleaned and adjusted series.
	d.Seasons = periods
	_, d.Seasonal = mstl(adj, periods, false)
	return d
}

// splitSeasonalities are the names in a stored seasonalities option.
func splitSeasonalities(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// movingAverage is x's centred moving average over p points (a 2×p
// average for an even p), its window shrunk at the ends.
func movingAverage(x []float64, p int) []float64 {
	n := len(x)
	out := make([]float64, n)
	h := p / 2
	for i := range x {
		var s, w float64
		for j := i - h; j <= i+h; j++ {
			if j < 0 || j >= n {
				continue
			}
			wt := 1.0
			if p%2 == 0 && (j == i-h || j == i+h) {
				wt = 0.5
			}
			s += wt * x[j]
			w += wt
		}
		out[i] = s / w
	}
	return out
}

// mstl decomposes x into a trend and one seasonal component per period.
func mstl(x []float64, periods []arimaSeason, robust bool) ([]float64, [][]float64) {
	n := len(x)
	seas := make([][]float64, len(periods))
	for i := range seas {
		seas[i] = make([]float64, n)
	}
	rest := func(skip int) []float64 {
		r := append([]float64(nil), x...)
		for i, s := range seas {
			if i == skip {
				continue
			}
			for k := range r {
				r[k] -= s[k]
			}
		}
		return r
	}
	for pass := 0; pass < 2; pass++ {
		for i, p := range periods {
			r := rest(i)
			tr := movingAverage(r, p.Period)
			seas[i] = cycleMeans(r, tr, p.Period, robust)
		}
	}
	maxP := 1
	for _, p := range periods {
		maxP = max(maxP, p.Period)
	}
	trend := x
	if len(periods) > 0 {
		trend = movingAverage(rest(-1), maxP)
	}
	return trend, seas
}

// cycleMeans is the seasonal component of r − tr of period p: each cycle
// position's mean (or median), centred to sum to zero over a cycle.
func cycleMeans(r, tr []float64, p int, robust bool) []float64 {
	pos := make([][]float64, p)
	for k := range r {
		pos[k%p] = append(pos[k%p], r[k]-tr[k])
	}
	val := make([]float64, p)
	var mean float64
	for j, v := range pos {
		if robust {
			val[j] = median(v)
		} else {
			for _, x := range v {
				val[j] += x / float64(len(v))
			}
		}
		mean += val[j] / float64(p)
	}
	out := make([]float64, len(r))
	for k := range out {
		out[k] = val[k%p] - mean
	}
	return out
}

// seasonalStrength is 1 − Var(R)/Var(S_i + R) of x's decomposition.
func seasonalStrength(x, trend []float64, seas [][]float64, i int) float64 {
	res := make([]float64, len(x))
	sr := make([]float64, len(x))
	for k := range x {
		res[k] = x[k] - trend[k]
		for _, s := range seas {
			res[k] -= s[k]
		}
		sr[k] = res[k] + seas[i][k]
	}
	vs := variance(sr)
	if vs <= 0 {
		return 0
	}
	return math.Max(0, 1-variance(res)/vs)
}

func variance(v []float64) float64 {
	var m, s float64
	for _, x := range v {
		m += x / float64(len(v))
	}
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return s / float64(len(v))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// robustScale is 1.4826 MAD of v, or, when that is 0, its mean absolute
// deviation from the median times √(π/2).
func robustScale(v []float64) float64 {
	m := median(v)
	dev := make([]float64, len(v))
	var mean float64
	for i, x := range v {
		dev[i] = math.Abs(x - m)
		mean += dev[i] / float64(len(v))
	}
	if s := 1.4826 * median(dev); s > 0 {
		return s
	}
	return mean * math.Sqrt(math.Pi/2)
}

// tolerance is the size of rounding error in x.
func tolerance(x []float64) float64 {
	var m float64
	for _, v := range x {
		m = math.Max(m, math.Abs(v))
	}
	return 1e-9 * (1 + m)
}

// stepChanges is x's step-change component: at each point the level of
// its segment less the last segment's.
func stepChanges(x []float64) []float64 {
	n, m := len(x), arimaStepWindow
	out := make([]float64, n)
	if n < 2*m {
		return out
	}
	diffs := difference(x, 1)
	slope := median(diffs)
	sigma := robustScale(diffs) / math.Sqrt2
	tol := tolerance(x)
	sigma = math.Max(sigma, 1e3*tol)
	stat := make([]float64, n)
	shift := make([]float64, n)
	for t := m; t <= n-m; t++ {
		shift[t] = median(x[t:t+m]) - median(x[t-m:t]) - float64(m)*slope
		if math.Abs(shift[t]) <= tol {
			continue
		}
		stat[t] = math.Abs(shift[t]) / sigma
	}
	var steps []int
	for t := m; t <= n-m; t++ {
		if stat[t] < arimaStepZ {
			continue
		}
		peak := true
		for u := max(m, t-m); u <= min(n-m, t+m); u++ {
			if stat[u] > stat[t] || (stat[u] == stat[t] && u < t) {
				peak = false
				break
			}
		}
		if !peak {
			continue
		}
		// The medians' statistic is flat about a step; the step is where
		// the windows' means differ most.
		best, bd := t, -1.0
		for u := max(m, t-m); u <= min(n-m, t+m); u++ {
			var l, r float64
			for j := 0; j < m; j++ {
				l += x[u-m+j] / float64(m)
				r += x[u+j] / float64(m)
			}
			if d := math.Abs(r - l - float64(m)*slope); d > bd {
				best, bd = u, d
			}
		}
		if len(steps) == 0 || steps[len(steps)-1] != best {
			steps = append(steps, best)
		}
	}
	for _, t := range steps {
		for k := 0; k < t; k++ {
			out[k] -= shift[t]
		}
	}
	return out
}

// spikesAndDips is x's spikes-and-dips component: at each spike or dip
// its difference from the linear interpolation of its clean neighbours.
func spikesAndDips(x []float64) []float64 {
	n, w := len(x), arimaSpikeWindow
	out := make([]float64, n)
	if n < w {
		return out
	}
	slope := median(difference(x, 1))
	e := make([]float64, n)
	for k := range x {
		e[k] = x[k] - slope*float64(k)
	}
	r := make([]float64, n)
	for k := range e {
		lo := min(max(k-w/2, 0), n-w)
		r[k] = e[k] - median(e[lo:lo+w])
	}
	scale := math.Max(robustScale(r), 1e3*tolerance(x))
	mr := median(r)
	spike := make([]bool, n)
	found := false
	for k := range r {
		if math.Abs(r[k]-mr)/scale > arimaSpikeZ {
			spike[k], found = true, true
		}
	}
	if !found {
		return out
	}
	for k := 0; k < n; k++ {
		if !spike[k] {
			continue
		}
		l, rr := k-1, k+1
		for rr < n && spike[rr] {
			rr++
		}
		for j := k; j < rr; j++ {
			var v float64
			switch {
			case l < 0 && rr >= n:
				v = x[j]
			case l < 0:
				v = x[rr]
			case rr >= n:
				v = x[l]
			default:
				v = x[l] + (x[rr]-x[l])*float64(j-l)/float64(rr-l)
			}
			out[j] = x[j] - v
		}
		k = rr
	}
	return out
}
