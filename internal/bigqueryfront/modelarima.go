package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ARIMA_PLUS time series models (#1504,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series),
// trained by the front: each series (one, or one a time_series_id_col
// value) is put on its data frequency's grid (data_frequency, or
// AUTO_FREQUENCY: the largest documented frequency whose period is at
// most the smallest gap between two timestamps), and a non-seasonal
// ARIMA(p, d, q) is fitted to it, its order given (non_seasonal_order,
// with include_drift) or, with auto_arima (the default), chosen as the
// documented auto.ARIMA does: the candidates are every (p, q) with
// auto_arima_min_order ≤ p + q ≤ auto_arima_max_order (default 0 and 2:
// six candidates), each with and without drift when d is 1, and the one
// of the lowest AIC is the model. ML.FORECAST, ML.EXPLAIN_FORECAST,
// ML.DETECT_ANOMALIES, ML.ARIMA_EVALUATE and ML.ARIMA_COEFFICIENTS serve
// it with their documented columns.
//
// The fit: w is y differenced d times; w_t − μ = Σ φ_i (w_{t−i} − μ) +
// e_t + Σ θ_j e_{t−j}, μ the intercept (d = 0) or the drift (d = 1 with
// drift), else 0; the coefficients minimize the conditional sum of
// squares (CSS) of e from t = m, the largest p searched (so every
// candidate's likelihood is over the same points), by Nelder–Mead from
// φ = θ = 0 and μ the mean of w, refusing a non-stationary φ or a
// non-invertible θ; σ² = SSE / n, log_likelihood = −n/2 (log 2πσ² + 1),
// AIC = −2 log_likelihood + 2 (p + q + [μ] + 1). A forecast is the
// ARMA recursion on w (future e = 0) integrated back to y; its
// standard_error σ √(Σ_{j<h} ψ_j²), ψ the MA(∞) weights of the
// integrated model, and its prediction interval forecast ± z σ_h, z the
// normal quantile of (1 + confidence_level) / 2. ML.DETECT_ANOMALIES'
// anomaly_probability is P(|N(0, σ_h²)| < |y − ŷ|), ŷ the one-step fit
// (history) or the forecast (an input's later rows), and its bounds ŷ ±
// z σ_h for anomaly_prob_threshold.
//
// UNVERIFIED against Google, chosen here: d (with auto_arima) by the
// KPSS level-stationarity test at 5% (critical value 0.463, lag
// ⌊4 (n/100)^¼⌋), differencing again while it rejects, at most twice, as
// R's forecast::ndiffs does; CSS rather than exact maximum likelihood,
// and the Nelder–Mead start and tolerances; the grid of AUTO_FREQUENCY;
// a timestamp off the grid taken to its nearest grid point, duplicates
// averaged, and a missing or NULL point interpolated linearly; the
// intercept reported as μ, the series' mean, not the constant μ (1 − Σφ);
// ML.ARIMA_EVALUATE's candidates in AIC order; the confidence interval
// columns equal to the prediction interval. Before the fit, the series
// is decomposed, cleaned of spikes and dips and adjusted for step changes
// (modelarimadecomp.go, #1518): the ARIMA is fitted to what is left, and
// a forecast adds the seasonal components back; forecast_limit_lower_bound
// and forecast_limit_upper_bound clamp the forecast and its interval
// (UNVERIFIED). Refused 501: holiday_region (Google's holiday calendar),
// hierarchical_time_series_cols, more than one time_series_id_col, and
// ARIMA_PLUS_XREG.

// tsOptions are an ARIMA_PLUS model's options.
type tsOptions struct {
	TimestampCol   string  `json:"timestampCol,omitempty"`
	DataCol        string  `json:"dataCol,omitempty"`
	IDCol          string  `json:"idCol,omitempty"`
	Horizon        int     `json:"horizon,omitempty"`
	AutoArima      bool    `json:"autoArima"`
	MaxOrder       int     `json:"autoArimaMaxOrder,omitempty"`
	MinOrder       int     `json:"autoArimaMinOrder,omitempty"`
	Order          []int   `json:"nonSeasonalOrder,omitempty"`
	Frequency      string  `json:"dataFrequency,omitempty"`
	IncludeDrift   bool    `json:"includeDrift,omitempty"`
	CleanSpikes    bool    `json:"cleanSpikesAndDips"`
	AdjustSteps    bool    `json:"adjustStepChanges"`
	Decompose      bool    `json:"decomposeTimeSeries"`
	Seasonalities  string  `json:"seasonalities,omitempty"`
	SmoothWindow   int     `json:"trendSmoothingWindowSize,omitempty"`
	LengthFraction float64 `json:"timeSeriesLengthFraction,omitempty"`
	MinLength      int     `json:"minTimeSeriesLength,omitempty"`
	MaxLength      int     `json:"maxTimeSeriesLength,omitempty"`
	// LimitLower and LimitUpper are forecast_limit_lower_bound and
	// forecast_limit_upper_bound.
	LimitLower *float64 `json:"forecastLimitLowerBound,omitempty"`
	LimitUpper *float64 `json:"forecastLimitUpperBound,omitempty"`
}

// applyArimaDefaults sets an ARIMA_PLUS model's documented defaults.
func applyArimaDefaults(o *mlOptions) {
	ts := &o.TS
	if !o.given("horizon") {
		ts.Horizon = 1000
	}
	if !o.given("auto_arima") {
		ts.AutoArima = true
	}
	if !o.given("auto_arima_max_order") {
		ts.MaxOrder = 2
	}
	if ts.Frequency == "" {
		ts.Frequency = "AUTO_FREQUENCY"
	}
	for name, p := range map[string]*bool{"clean_spikes_and_dips": &ts.CleanSpikes, "adjust_step_changes": &ts.AdjustSteps, "decompose_time_series": &ts.Decompose} {
		if !o.given(name) {
			*p = true
		}
	}
	if ts.Seasonalities == "" {
		ts.Seasonalities = "AUTO"
	}
	if o.given("time_series_length_fraction") && !o.given("min_time_series_length") {
		ts.MinLength = 20
	}
}

// checkArimaOptions refuses the combinations BigQuery documents as
// invalid.
func checkArimaOptions(o mlOptions) (int, string) {
	ts := o.TS
	switch {
	case ts.TimestampCol == "":
		return 400, "time_series_timestamp_col must be specified for model type ARIMA_PLUS"
	case ts.DataCol == "":
		return 400, "time_series_data_col must be specified for model type ARIMA_PLUS"
	case !ts.AutoArima && len(ts.Order) == 0:
		return 400, "non_seasonal_order must be specified when auto_arima is FALSE"
	case ts.AutoArima && len(ts.Order) > 0:
		return 400, "non_seasonal_order can only be specified when auto_arima is FALSE"
	case ts.AutoArima && o.given("include_drift"):
		return 400, "include_drift can only be specified when auto_arima is FALSE"
	case ts.IncludeDrift && len(ts.Order) == 3 && ts.Order[1] != 1:
		return 400, "include_drift can only be TRUE when the non-seasonal d is 1"
	case !ts.AutoArima && ts.IDCol != "":
		return 400, "Forecasting multiple time series (time_series_id_col) requires auto_arima TRUE"
	case ts.AutoArima && ts.MinOrder >= ts.MaxOrder:
		return 400, "auto_arima_min_order must be less than auto_arima_max_order"
	case o.given("max_time_series_length") && (o.given("time_series_length_fraction") || o.given("min_time_series_length")):
		return 400, "max_time_series_length cannot be used with time_series_length_fraction or min_time_series_length"
	case o.given("min_time_series_length") && !o.given("time_series_length_fraction"):
		return 400, "min_time_series_length requires time_series_length_fraction"
	case ts.LimitLower != nil && ts.LimitUpper != nil && *ts.LimitLower >= *ts.LimitUpper:
		return 400, "forecast_limit_lower_bound must be less than forecast_limit_upper_bound"
	}
	return 0, ""
}

// arimaFreqs are the documented data frequencies, finest first, and each
// one's step: a duration, or months.
var arimaFreqs = []struct {
	name   string
	step   time.Duration
	months int
}{
	{"PER_MINUTE", time.Minute, 0}, {"HOURLY", time.Hour, 0}, {"DAILY", 24 * time.Hour, 0}, {"WEEKLY", 7 * 24 * time.Hour, 0},
	{"MONTHLY", 0, 1}, {"QUARTERLY", 0, 3}, {"YEARLY", 0, 12},
}

// arimaFit is one fitted candidate.
type arimaFit struct {
	P      int       `json:"p"`
	D      int       `json:"d"`
	Q      int       `json:"q"`
	Drift  bool      `json:"drift"`
	HasMu  bool      `json:"hasMu"`
	AR     []float64 `json:"ar"`
	MA     []float64 `json:"ma"`
	Mu     float64   `json:"mu"`
	Sigma2 float64   `json:"sigma2"`
	LogLik float64   `json:"logLik"`
	AIC    float64   `json:"aic"`
}

// arimaSeries is one series and its candidates, the model the first.
type arimaSeries struct {
	ID *string `json:"id,omitempty"`
	// Times are the grid's points (Unix microseconds), Data the values on
	// them, Observed whether a training row gave the point.
	Times    []int64    `json:"times"`
	Data     []float64  `json:"data"`
	Observed []bool     `json:"observed"`
	Start    int        `json:"start"` // the first point the trend is modelled on
	Cands    []arimaFit `json:"cands,omitempty"`
	Error    string     `json:"error,omitempty"`
	// The components the pipeline took out before the ARIMA fit (#1518):
	// the seasonal periods found and each one's component, and the
	// spikes and dips and the step changes (nil when not cleaned or
	// adjusted). A model trained before #1518 has none.
	Seasons  []arimaSeason `json:"seasons,omitempty"`
	Seasonal [][]float64   `json:"seasonal,omitempty"`
	Spikes   []float64     `json:"spikes,omitempty"`
	Steps    []float64     `json:"steps,omitempty"`
}

// adjusted is the series the ARIMA is fitted to: the data less its
// seasonal components, spikes and dips, and step changes.
func (s arimaSeries) adjusted() []float64 {
	x := append([]float64(nil), s.Data...)
	for k := range x {
		x[k] -= s.seasonalAt(k)
		if s.Spikes != nil {
			x[k] -= s.Spikes[k]
		}
		if s.Steps != nil {
			x[k] -= s.Steps[k]
		}
	}
	return x
}

// seasonalPart is the i-th seasonal component at point k, continued past
// the training by its last cycle.
func (s arimaSeries) seasonalPart(i, k int) float64 {
	c, p := s.Seasonal[i], s.Seasons[i].Period
	if k >= len(c) {
		k = len(c) - p + (k-len(c))%p
	}
	return c[k]
}

// seasonalAt is the sum of the seasonal components at point k.
func (s arimaSeries) seasonalAt(k int) float64 {
	var v float64
	for i := range s.Seasonal {
		v += s.seasonalPart(i, k)
	}
	return v
}

// seasonNames are the series' seasonal periods as BigQuery names them.
func (s arimaSeries) seasonNames() []string {
	if len(s.Seasons) == 0 {
		return []string{"NO_SEASONALITY"}
	}
	var out []string
	for _, p := range s.Seasons {
		out = append(out, p.Name)
	}
	return out
}

func nonZero(v []float64) bool {
	for _, x := range v {
		if x != 0 {
			return true
		}
	}
	return false
}

// limit is v within the forecast limits.
func (ts tsOptions) limit(v float64) float64 {
	if ts.LimitLower != nil {
		v = math.Max(v, *ts.LimitLower)
	}
	if ts.LimitUpper != nil {
		v = math.Min(v, *ts.LimitUpper)
	}
	return v
}

// arimaForecast is a fitted series' forecast for h steps: the trend's
// forecast plus the seasonal components, within the forecast limits, and
// its standard errors and prediction intervals at z.
func (s arimaSeries) arimaForecast(ts tsOptions, cond, h int, z float64) (fc, se, lo, hi []float64) {
	_, tr, se := s.arimaPath(s.Cands[0], cond, h)
	n := len(s.Data)
	for k := 0; k < h; k++ {
		v := tr[k] + s.seasonalAt(n+k)
		fc = append(fc, ts.limit(v))
		lo = append(lo, ts.limit(v-z*se[k]))
		hi = append(hi, ts.limit(v+z*se[k]))
	}
	return fc, se, lo, hi
}

// arimaTrained is an ARIMA_PLUS model.
type arimaTrained struct {
	IDType    string        `json:"idType,omitempty"`
	Frequency string        `json:"frequency"`
	Cond      int           `json:"cond"`
	Series    []arimaSeries `json:"series"`
}

// parseTSValue reads a TIMESTAMP (seconds), DATE or DATETIME REST value
// as Unix microseconds.
func parseTSValue(v, typ string) (int64, bool) {
	switch typ {
	case "TIMESTAMP":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return int64(math.Round(f * 1e6)), true
	case "DATE":
		t, err := time.Parse("2006-01-02", v)
		return t.UnixMicro(), err == nil
	case "DATETIME":
		for _, layout := range []string{"2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UnixMicro(), true
			}
		}
	}
	return 0, false
}

// gridStep is the k-th point after start at frequency fq.
func gridPoint(start int64, fq string, k int) int64 {
	for _, f := range arimaFreqs {
		if f.name == fq {
			if f.months > 0 {
				return time.UnixMicro(start).UTC().AddDate(0, f.months*k, 0).UnixMicro()
			}
			return start + int64(k)*f.step.Microseconds()
		}
	}
	return start
}

// gridIndex is the grid point nearest t.
func gridIndex(start int64, fq string, t int64) int {
	for _, f := range arimaFreqs {
		if f.name != fq {
			continue
		}
		if f.months == 0 {
			return int(math.Round(float64(t-start) / float64(f.step.Microseconds())))
		}
		a, b := time.UnixMicro(start).UTC(), time.UnixMicro(t).UTC()
		k := ((b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())) / f.months
		best, bd := k, int64(math.MaxInt64)
		for c := k - 1; c <= k+1; c++ {
			d := gridPoint(start, fq, c) - t
			if d < 0 {
				d = -d
			}
			if d < bd {
				best, bd = c, d
			}
		}
		return best
	}
	return 0
}

// trainArima trains an ARIMA_PLUS model of opts on rows.
func trainArima(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	ts := opts.TS
	ti, di, ii := -1, -1, -1
	for i, c := range cols {
		switch {
		case strings.EqualFold(c.Name, ts.TimestampCol):
			ti = i
		case strings.EqualFold(c.Name, ts.DataCol):
			di = i
		case ts.IDCol != "" && strings.EqualFold(c.Name, ts.IDCol):
			ii = i
		}
	}
	if ti < 0 {
		return t, mlInvalid("Unable to identify the time_series_timestamp_col '%s' in the input data", ts.TimestampCol)
	}
	if di < 0 {
		return t, mlInvalid("Unable to identify the time_series_data_col '%s' in the input data", ts.DataCol)
	}
	if ts.IDCol != "" && ii < 0 {
		return t, mlInvalid("Unable to identify the time_series_id_col '%s' in the input data", ts.IDCol)
	}
	tt := strings.ToUpper(cols[ti].Type)
	if tt != "TIMESTAMP" && tt != "DATE" && tt != "DATETIME" {
		return t, mlInvalid("time_series_timestamp_col %s must be of type TIMESTAMP, DATE or DATETIME, not %s", cols[ti].Name, cols[ti].Type)
	}
	if !(mlFeature{Type: mlTypeOf(cols[di].Type)}).numeric() {
		return t, mlInvalid("time_series_data_col %s must be of type INT64, NUMERIC, BIGNUMERIC or FLOAT64, not %s", cols[di].Name, cols[di].Type)
	}
	a := &arimaTrained{Frequency: ts.Frequency}
	if ii >= 0 {
		a.IDType = mlTypeOf(cols[ii].Type)
		if a.IDType != "STRING" && a.IDType != "INT64" {
			return t, mlInvalid("time_series_id_col %s must be of type STRING or INT64, not %s", cols[ii].Name, cols[ii].Type)
		}
	}
	type obs struct {
		t int64
		v float64
	}
	byID := map[string][]obs{}
	var ids []string
	nullID := false
	for _, row := range rows {
		if row[ti] == nil || row[di] == nil {
			continue
		}
		key := ""
		if ii >= 0 {
			if row[ii] == nil {
				nullID = true
				continue
			}
			key = *row[ii]
		}
		tm, ok := parseTSValue(*row[ti], tt)
		v, err := strconv.ParseFloat(*row[di], 64)
		if !ok || err != nil {
			continue
		}
		if _, seen := byID[key]; !seen {
			ids = append(ids, key)
		}
		byID[key] = append(byID[key], obs{tm, v})
	}
	_ = nullID
	if len(ids) == 0 {
		return t, mlInvalid("Input data doesn't contain any rows")
	}
	if a.IDType != "" {
		sortLabels(ids, a.IDType)
	}
	// The frequency: given, or the largest whose period is at most the
	// smallest gap of any series.
	if a.Frequency == "AUTO_FREQUENCY" {
		gap := int64(math.MaxInt64)
		for _, id := range ids {
			o := byID[id]
			sort.Slice(o, func(x, y int) bool { return o[x].t < o[y].t })
			for k := 1; k < len(o); k++ {
				if d := o[k].t - o[k-1].t; d > 0 && d < gap {
					gap = d
				}
			}
		}
		if gap == math.MaxInt64 {
			return t, mlInvalid("Unable to infer the data frequency: each time series needs at least two distinct timestamps")
		}
		if gap < time.Minute.Microseconds() {
			return t, mlInvalid("The data frequency is finer than PER_MINUTE, the finest BigQuery supports")
		}
		nominal := map[string]time.Duration{"MONTHLY": 28 * 24 * time.Hour, "QUARTERLY": 89 * 24 * time.Hour, "YEARLY": 365 * 24 * time.Hour}
		for _, f := range arimaFreqs {
			p := f.step
			if f.months > 0 {
				p = nominal[f.name]
			}
			if p.Microseconds() <= gap {
				a.Frequency = f.name
			}
		}
	}
	// The candidates' orders.
	type order struct {
		p, q int
	}
	var orders []order
	if ts.AutoArima {
		for s := ts.MinOrder; s <= ts.MaxOrder; s++ {
			for p := s; p >= 0; p-- {
				orders = append(orders, order{p, s - p})
			}
		}
		a.Cond = ts.MaxOrder
	} else {
		orders = []order{{ts.Order[0], ts.Order[2]}}
		a.Cond = ts.Order[0]
	}
	for _, id := range ids {
		o := byID[id]
		sort.SliceStable(o, func(x, y int) bool { return o[x].t < o[y].t })
		s := arimaSeries{}
		if a.IDType != "" {
			v := id
			s.ID = &v
		}
		start := o[0].t
		n := gridIndex(start, a.Frequency, o[len(o)-1].t) + 1
		sum, cnt := make([]float64, n), make([]int, n)
		for _, ob := range o {
			k := gridIndex(start, a.Frequency, ob.t)
			sum[k] += ob.v
			cnt[k]++
		}
		for k := 0; k < n; k++ {
			s.Times = append(s.Times, gridPoint(start, a.Frequency, k))
			s.Observed = append(s.Observed, cnt[k] > 0)
			s.Data = append(s.Data, math.NaN())
			if cnt[k] > 0 {
				s.Data[k] = sum[k] / float64(cnt[k])
			}
		}
		// Linear interpolation of the missing points.
		for k := 0; k < n; k++ {
			if cnt[k] > 0 {
				continue
			}
			l, r := k-1, k+1
			for cnt[r] == 0 {
				r++
			}
			for j := k; j < r; j++ {
				s.Data[j] = s.Data[l] + (s.Data[r]-s.Data[l])*float64(j-l)/float64(r-l)
			}
			k = r
		}
		// The pipeline's stages before the ARIMA (#1518).
		dc := decomposeSeries(s.Data, a.Frequency, ts)
		s.Seasons, s.Seasonal, s.Spikes, s.Steps = dc.Seasons, dc.Seasonal, dc.Spikes, dc.Steps
		// The points the trend is modelled on.
		switch {
		case ts.MaxLength > 0 && n > ts.MaxLength:
			s.Start = n - ts.MaxLength
		case ts.LengthFraction > 0:
			m := max(int(math.Ceil(ts.LengthFraction*float64(n))), ts.MinLength)
			if m < n {
				s.Start = n - m
			}
		}
		y := s.adjusted()[s.Start:]
		d := 0
		if ts.AutoArima {
			d = ndiffs(y)
		} else {
			d = ts.Order[1]
		}
		w := difference(y, d)
		for _, or := range orders {
			drifts := []bool{false}
			switch {
			case d == 1 && ts.AutoArima:
				drifts = []bool{false, true}
			case d == 1:
				drifts = []bool{ts.IncludeDrift}
			}
			for _, dr := range drifts {
				if len(w)-a.Cond < or.p+or.q+2 {
					continue
				}
				s.Cands = append(s.Cands, fitArima(w, or.p, d, or.q, d == 0 || dr, dr, a.Cond))
			}
		}
		if len(s.Cands) == 0 {
			s.Error = fmt.Sprintf("The time series has %d points, too few to fit an ARIMA model", len(y))
		}
		sort.SliceStable(s.Cands, func(x, y int) bool { return s.Cands[x].AIC < s.Cands[y].AIC })
		a.Series = append(a.Series, s)
	}
	t.Arima = a
	t.Strategy = "ARIMA"
	for _, s := range a.Series {
		t.Rows += len(s.Times)
	}
	return t, nil
}

// difference is y differenced d times.
func difference(y []float64, d int) []float64 {
	w := append([]float64(nil), y...)
	for k := 0; k < d; k++ {
		if len(w) == 0 {
			break
		}
		next := make([]float64, len(w)-1)
		for i := range next {
			next[i] = w[i+1] - w[i]
		}
		w = next
	}
	return w
}

// kpss is the KPSS level-stationarity statistic of y.
func kpss(y []float64) float64 {
	n := len(y)
	var mean float64
	for _, v := range y {
		mean += v
	}
	mean /= float64(n)
	e := make([]float64, n)
	var s, num, s0 float64
	for i, v := range y {
		e[i] = v - mean
		s += e[i]
		num += s * s
		s0 += e[i] * e[i]
	}
	lag := int(4 * math.Pow(float64(n)/100, 0.25))
	lrv := s0 / float64(n)
	for l := 1; l <= lag; l++ {
		var c float64
		for i := l; i < n; i++ {
			c += e[i] * e[i-l]
		}
		lrv += 2 * (1 - float64(l)/float64(lag+1)) * c / float64(n)
	}
	if lrv <= 0 {
		return 0
	}
	return num / (float64(n) * float64(n) * lrv)
}

// ndiffs is how many times y is differenced: while the KPSS test rejects
// level stationarity at 5%, at most twice.
func ndiffs(y []float64) int {
	d := 0
	for d < 2 {
		if len(y) < 4 || constant(y) || kpss(y) <= 0.463 {
			break
		}
		y = difference(y, 1)
		d++
	}
	return d
}

func constant(y []float64) bool {
	for _, v := range y {
		if v != y[0] {
			return false
		}
	}
	return true
}

// stationary reports whether x_t = Σ a_i x_{t−i} + e_t is stationary,
// by the Durbin–Levinson step-down: every reflection coefficient |k| < 1.
func stationary(a []float64) bool {
	c := append([]float64(nil), a...)
	for p := len(c); p > 0; p-- {
		k := c[p-1]
		if math.Abs(k) >= 1 {
			return false
		}
		next := make([]float64, p-1)
		for j := 0; j < p-1; j++ {
			next[j] = (c[j] + k*c[p-2-j]) / (1 - k*k)
		}
		c = next
	}
	return true
}

// cssResiduals are the residuals e of the ARMA on w from cond (zero
// before it).
func cssResiduals(w []float64, ar, ma []float64, mu float64, cond int) []float64 {
	e := make([]float64, len(w))
	for t := cond; t < len(w); t++ {
		v := w[t] - mu
		for i, f := range ar {
			v -= f * (w[t-i-1] - mu)
		}
		for j, th := range ma {
			if t-j-1 >= 0 {
				v -= th * e[t-j-1]
			}
		}
		e[t] = v
	}
	return e
}

// fitArima fits ARIMA(p, d, q) to w, y differenced d times.
func fitArima(w []float64, p, d, q int, hasMu, drift bool, cond int) arimaFit {
	var mean float64
	for _, v := range w {
		mean += v
	}
	mean /= float64(len(w))
	unpack := func(x []float64) ([]float64, []float64, float64) {
		mu := 0.0
		if hasMu {
			mu = x[p+q]
		}
		return x[:p], x[p : p+q], mu
	}
	sse := func(x []float64) float64 {
		ar, ma, mu := unpack(x)
		neg := make([]float64, q)
		for j, th := range ma {
			neg[j] = -th
		}
		if !stationary(ar) || !stationary(neg) {
			return math.Inf(1)
		}
		var s float64
		for _, v := range cssResiduals(w, ar, ma, mu, cond)[cond:] {
			s += v * v
		}
		return s
	}
	x0 := make([]float64, p+q)
	if hasMu {
		x0 = append(x0, mean)
	}
	x := nelderMead(sse, x0)
	ar, ma, mu := unpack(x)
	n := float64(len(w) - cond)
	s2 := sse(x) / n
	f := arimaFit{P: p, D: d, Q: q, Drift: drift, HasMu: hasMu, AR: append([]float64{}, ar...), MA: append([]float64{}, ma...), Mu: mu, Sigma2: s2}
	if s2 > 0 {
		f.LogLik = -n / 2 * (math.Log(2*math.Pi*s2) + 1)
	} else {
		f.LogLik = math.Inf(1)
	}
	k := p + q + 1
	if hasMu {
		k++
	}
	f.AIC = -2*f.LogLik + 2*float64(k)
	return f
}

// nelderMead minimizes f from x0, deterministically.
func nelderMead(f func([]float64) float64, x0 []float64) []float64 {
	n := len(x0)
	if n == 0 {
		return x0
	}
	pts := [][]float64{append([]float64(nil), x0...)}
	for i := 0; i < n; i++ {
		x := append([]float64(nil), x0...)
		step := 0.1
		if x[i] != 0 {
			step = 0.1 * math.Max(math.Abs(x[i]), 1e-3)
		}
		x[i] += step
		pts = append(pts, x)
	}
	vals := make([]float64, n+1)
	for i, p := range pts {
		vals[i] = f(p)
	}
	at := func(c []float64, d []float64, t float64) []float64 {
		out := make([]float64, n)
		for j := range out {
			out[j] = c[j] + t*(d[j]-c[j])
		}
		return out
	}
	for iter := 0; iter < 5000*n; iter++ {
		idx := make([]int, n+1)
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return vals[idx[a]] < vals[idx[b]] })
		np, nv := make([][]float64, n+1), make([]float64, n+1)
		for i, k := range idx {
			np[i], nv[i] = pts[k], vals[k]
		}
		pts, vals = np, nv
		if math.Abs(vals[n]-vals[0]) <= 1e-12*(math.Abs(vals[0])+1e-12) {
			var spread float64
			for _, p := range pts[1:] {
				for j := range p {
					spread = math.Max(spread, math.Abs(p[j]-pts[0][j]))
				}
			}
			if spread < 1e-9 {
				break
			}
		}
		c := make([]float64, n)
		for _, p := range pts[:n] {
			for j := range c {
				c[j] += p[j] / float64(n)
			}
		}
		r := at(c, pts[n], -1)
		fr := f(r)
		switch {
		case fr < vals[0]:
			e := at(c, pts[n], -2)
			if fe := f(e); fe < fr {
				pts[n], vals[n] = e, fe
			} else {
				pts[n], vals[n] = r, fr
			}
		case fr < vals[n-1]:
			pts[n], vals[n] = r, fr
		default:
			k := at(c, pts[n], 0.5)
			if fr < vals[n] {
				k = at(c, r, 0.5)
			}
			if fk := f(k); fk < math.Min(fr, vals[n]) {
				pts[n], vals[n] = k, fk
				continue
			}
			for i := 1; i <= n; i++ {
				pts[i] = at(pts[0], pts[i], 0.5)
				vals[i] = f(pts[i])
			}
		}
	}
	best := 0
	for i := range vals {
		if vals[i] < vals[best] {
			best = i
		}
	}
	return pts[best]
}

// diffPoly are c_k of (1 − B)^d = 1 + Σ c_k B^k.
func diffPoly(d int) []float64 {
	c := []float64{1}
	for k := 0; k < d; k++ {
		n := make([]float64, len(c)+1)
		for i, v := range c {
			n[i] += v
			n[i+1] -= v
		}
		c = n
	}
	return c[1:]
}

// arimaPath is a fitted series' one-step fit of y (NaN where it has none),
// its forecasts for h steps, and their standard errors.
func (s arimaSeries) arimaPath(f arimaFit, cond, h int) (fitted, fc, se []float64) {
	y := s.adjusted()[s.Start:]
	w := difference(y, f.D)
	e := cssResiduals(w, f.AR, f.MA, f.Mu, cond)
	c := diffPoly(f.D)
	fitted = make([]float64, len(s.Data))
	for i := range fitted {
		fitted[i] = math.NaN()
	}
	for t := cond; t < len(w); t++ {
		v := w[t] - e[t]
		for k, ck := range c {
			v -= ck * y[t+f.D-k-1]
		}
		fitted[s.Start+t+f.D] = v
	}
	ww := append([]float64(nil), w...)
	yy := append([]float64(nil), y...)
	ee := append([]float64(nil), e...)
	for k := 0; k < h; k++ {
		t := len(ww)
		v := f.Mu
		for i, ph := range f.AR {
			if t-i-1 >= 0 {
				v += ph * (ww[t-i-1] - f.Mu)
			}
		}
		for j, th := range f.MA {
			if t-j-1 >= 0 {
				v += th * ee[t-j-1]
			}
		}
		ww, ee = append(ww, v), append(ee, 0)
		yv := v
		for i, ck := range c {
			yv -= ck * yy[len(yy)-i-1]
		}
		yy = append(yy, yv)
		fc = append(fc, yv)
	}
	// ψ weights of φ*(B) = φ(B)(1 − B)^d.
	astar := make([]float64, len(f.AR)+f.D)
	phi := append([]float64{1}, make([]float64, len(f.AR))...)
	for i, v := range f.AR {
		phi[i+1] = -v
	}
	full := append([]float64{1}, c...)
	for i, a := range phi {
		for j, b := range full {
			if i+j > 0 {
				astar[i+j-1] -= a * b
			}
		}
	}
	psi := []float64{1}
	var acc float64
	for k := 0; k < h; k++ {
		if k > 0 {
			v := 0.0
			if k-1 < len(f.MA) {
				v = f.MA[k-1]
			}
			for i := 1; i <= min(k, len(astar)); i++ {
				v += astar[i-1] * psi[k-i]
			}
			psi = append(psi, v)
		}
		acc += psi[k] * psi[k]
		se = append(se, math.Sqrt(f.Sigma2*acc))
	}
	return fitted, fc, se
}

// normalQuantile is the standard normal's two-sided quantile of level:
// z with P(|Z| < z) = level.
func normalQuantile(level float64) float64 { return math.Sqrt2 * math.Erfinv(level) }

// arimaLevel reads a STRUCT's FLOAT64 field in [0, 1), or def.
func arimaLevel(c mlCall, name string, def float64) (float64, string) {
	v, ok := c.struc[name]
	if !ok {
		return def, ""
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f >= 1 {
		return 0, "Invalid " + name + " " + v + ": it must be in [0, 1)"
	}
	return f, ""
}

// arimaHorizon reads a STRUCT's horizon, default 3, at most the model's.
func arimaHorizon(c mlCall, model int) (int, string) {
	v, ok := c.struc["horizon"]
	if !ok {
		return min(3, model), ""
	}
	h, err := strconv.Atoi(v)
	if err != nil || h < 1 {
		return 0, "Invalid horizon " + v + ": it must be a positive integer"
	}
	if h > model {
		return 0, fmt.Sprintf("Invalid horizon %d: it must be at most the model's horizon, %d", h, model)
	}
	return h, ""
}

func sqlTS(micros int64) string {
	return "TIMESTAMP " + sqlString(time.UnixMicro(micros).UTC().Format("2006-01-02 15:04:05.999999")+"+00")
}

func sqlNaN(v float64) string {
	if math.IsNaN(v) {
		return "NULL"
	}
	return sqlF(v)
}

// arimaSubquery is the subquery that stands for call c of ARIMA_PLUS
// model md.
func (f front) arimaSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	a := md.Trained.Arima
	ts := md.Options.TS
	idCol, idVal := "", func(arimaSeries) string { return "" }
	if a.IDType != "" {
		idCol = sqlIdent(ts.IDCol) + " " + a.IDType + ", "
		idVal = func(s arimaSeries) string { return sqlLabel(*s.ID, a.IDType) + ", " }
	}
	unsupported := func() (string, int, string) {
		return "", 400, "ML." + c.fn + " is not supported for model type ARIMA_PLUS"
	}
	if c.input != "" && c.fn != "DETECT_ANOMALIES" {
		return "", 400, "ML." + c.fn + " of an ARIMA_PLUS model takes no input data"
	}
	switch c.fn {
	case "FORECAST", "EXPLAIN_FORECAST":
		h, why := arimaHorizon(c, ts.Horizon)
		if why != "" {
			return "", 400, why
		}
		level, why := arimaLevel(c, "confidence_level", 0.95)
		if why != "" {
			return "", 400, why
		}
		if c.fn == "EXPLAIN_FORECAST" && !ts.Decompose {
			return "", 400, "ML.EXPLAIN_FORECAST requires a model trained with decompose_time_series TRUE"
		}
		z := normalQuantile(level)
		var rows []string
		for _, s := range a.Series {
			if len(s.Cands) == 0 {
				continue
			}
			fit := s.Cands[0]
			fitted, _, _ := s.arimaPath(fit, a.Cond, 0)
			fc, se, lo, hi := s.arimaForecast(ts, a.Cond, h, z)
			last := len(s.Times) - 1
			// seasonCols are the seasonal_period_* columns at point k:
			// NULL for a period the series does not have.
			seasonCols := func(k int) (string, float64) {
				var out []string
				var sum float64
				for _, name := range []string{"YEARLY", "QUARTERLY", "MONTHLY", "WEEKLY", "DAILY"} {
					v := "NULL"
					for i, p := range s.Seasons {
						if p.Name == name {
							x := s.seasonalPart(i, k)
							v, sum = sqlF(x), sum+x
						}
					}
					out = append(out, v)
				}
				return strings.Join(out, ", "), sum
			}
			if c.fn == "EXPLAIN_FORECAST" {
				trend := smooth(fitted, ts.SmoothWindow)
				sigma := math.Sqrt(fit.Sigma2)
				for k := range s.Times {
					seas, sum := seasonCols(k)
					spikes, steps, other := "NULL", "NULL", 0.0
					if s.Spikes != nil {
						spikes, other = sqlF(s.Spikes[k]), other+s.Spikes[k]
					}
					if s.Steps != nil {
						steps, other = sqlF(s.Steps[k]), other+s.Steps[k]
					}
					// The adjusted data: the data cleaned of spikes and dips,
					// step changes and the residual, the trend plus the
					// seasonal components.
					res, adj := math.NaN(), s.Data[k]-other
					if !math.IsNaN(trend[k]) {
						adj = trend[k] + sum
						res = s.Data[k] - adj - other
					}
					rows = append(rows, fmt.Sprintf("(%s%s, 'history', %s, %s, %s, NULL, NULL, NULL, %s, %s, NULL, %s, %s, %s)",
						idVal(s), sqlTS(s.Times[k]), sqlF(s.Data[k]), sqlF(adj), sqlF(sigma), sqlNaN(trend[k]), seas, spikes, steps, sqlNaN(res)))
				}
			}
			for k := 0; k < h; k++ {
				tm := gridPoint(s.Times[0], a.Frequency, last+k+1)
				if c.fn == "FORECAST" {
					rows = append(rows, fmt.Sprintf("(%s%s, %s, %s, %s, %s, %s, %s, %s)", idVal(s), sqlTS(tm), sqlF(fc[k]), sqlF(se[k]), sqlF(level),
						sqlF(lo[k]), sqlF(hi[k]), sqlF(lo[k]), sqlF(hi[k])))
					continue
				}
				// A forecast is its trend plus its seasonal components; the
				// trend takes up what the forecast limits cut off.
				seas, sum := seasonCols(last + k + 1)
				rows = append(rows, fmt.Sprintf("(%s%s, 'forecast', %s, %s, %s, %s, %s, %s, %s, %s, NULL, NULL, NULL, NULL)",
					idVal(s), sqlTS(tm), sqlF(fc[k]), sqlF(fc[k]), sqlF(se[k]), sqlF(level), sqlF(lo[k]), sqlF(hi[k]), sqlF(fc[k]-sum), seas))
			}
		}
		if c.fn == "FORECAST" {
			return mlRowsSubquery("STRUCT<"+idCol+"forecast_timestamp TIMESTAMP, forecast_value FLOAT64, standard_error FLOAT64, confidence_level FLOAT64, "+
				"prediction_interval_lower_bound FLOAT64, prediction_interval_upper_bound FLOAT64, confidence_interval_lower_bound FLOAT64, "+
				"confidence_interval_upper_bound FLOAT64>", rows), 0, ""
		}
		return mlRowsSubquery("STRUCT<"+idCol+"time_series_timestamp TIMESTAMP, time_series_type STRING, time_series_data FLOAT64, "+
			"time_series_adjusted_data FLOAT64, standard_error FLOAT64, confidence_level FLOAT64, prediction_interval_lower_bound FLOAT64, "+
			"prediction_interval_upper_bound FLOAT64, trend FLOAT64, seasonal_period_yearly FLOAT64, seasonal_period_quarterly FLOAT64, "+
			"seasonal_period_monthly FLOAT64, seasonal_period_weekly FLOAT64, seasonal_period_daily FLOAT64, holiday_effect FLOAT64, "+
			"spikes_and_dips FLOAT64, step_changes FLOAT64, residual FLOAT64>", rows), 0, ""
	case "ARIMA_EVALUATE":
		all := true
		if v, ok := c.struc["show_all_candidate_models"]; ok {
			switch strings.ToUpper(v) {
			case "TRUE":
			case "FALSE":
				all = false
			default:
				return "", 400, "Invalid show_all_candidate_models " + v + ": it must be TRUE or FALSE"
			}
		}
		var rows []string
		for _, s := range a.Series {
			if len(s.Cands) == 0 {
				rows = append(rows, fmt.Sprintf("(%sNULL, NULL, NULL, NULL, NULL, NULL, NULL, ARRAY<STRING>[], NULL, NULL, NULL, %s)", idVal(s), sqlString(s.Error)))
				continue
			}
			var names []string
			for _, n := range s.seasonNames() {
				names = append(names, sqlString(n))
			}
			for k, fit := range s.Cands {
				if k > 0 && !all {
					break
				}
				rows = append(rows, fmt.Sprintf("(%s%d, %d, %d, %t, %s, %s, %s, [%s], FALSE, %t, %t, NULL)", idVal(s), fit.P, fit.D, fit.Q,
					fit.Drift, sqlF(fit.LogLik), sqlF(fit.AIC), sqlF(fit.Sigma2), strings.Join(names, ", "), nonZero(s.Spikes), nonZero(s.Steps)))
			}
		}
		return mlRowsSubquery("STRUCT<"+idCol+"non_seasonal_p INT64, non_seasonal_d INT64, non_seasonal_q INT64, has_drift BOOL, log_likelihood FLOAT64, "+
			"AIC FLOAT64, variance FLOAT64, seasonal_periods ARRAY<STRING>, has_holiday_effect BOOL, has_spikes_and_dips BOOL, has_step_changes BOOL, "+
			"error_message STRING>", rows), 0, ""
	case "ARIMA_COEFFICIENTS":
		var rows []string
		for _, s := range a.Series {
			if len(s.Cands) == 0 {
				continue
			}
			fit := s.Cands[0]
			arr := func(v []float64) string {
				var out []string
				for _, x := range v {
					out = append(out, sqlF(x))
				}
				return "ARRAY<FLOAT64>[" + strings.Join(out, ", ") + "]"
			}
			rows = append(rows, fmt.Sprintf("(%s%s, %s, %s)", idVal(s), arr(fit.AR), arr(fit.MA), sqlF(fit.Mu)))
		}
		return mlRowsSubquery("STRUCT<"+idCol+"ar_coefficients ARRAY<FLOAT64>, ma_coefficients ARRAY<FLOAT64>, intercept_or_drift FLOAT64>", rows), 0, ""
	case "DETECT_ANOMALIES":
		thr, why := arimaLevel(c, "anomaly_prob_threshold", 0.95)
		if why != "" {
			return "", 400, why
		}
		z := normalQuantile(thr)
		tsName, dataName := sqlIdent(ts.TimestampCol), sqlIdent(ts.DataCol)
		cols := "STRUCT<" + idCol + tsName + " TIMESTAMP, " + dataName + " FLOAT64, is_anomaly BOOL, lower_bound FLOAT64, upper_bound FLOAT64, anomaly_probability FLOAT64>"
		score := func(y, yhat, sd float64) string {
			if math.IsNaN(yhat) || sd <= 0 {
				return "NULL, NULL, NULL, NULL"
			}
			p := math.Erf(math.Abs(y-yhat) / (sd * math.Sqrt2))
			return fmt.Sprintf("%t, %s, %s, %s", p > thr, sqlF(yhat-z*sd), sqlF(yhat+z*sd), sqlF(p))
		}
		if c.input == "" {
			if !ts.Decompose {
				return "", 400, "ML.DETECT_ANOMALIES of the training data requires a model trained with decompose_time_series TRUE"
			}
			var rows []string
			for _, s := range a.Series {
				if len(s.Cands) == 0 {
					continue
				}
				fit := s.Cands[0]
				fitted, _, _ := s.arimaPath(fit, a.Cond, 0)
				for k := range s.Times {
					if !s.Observed[k] {
						continue
					}
					// The expected value: the trend's fit, the seasonal
					// components and the step changes; a spike or dip is
					// what stands out from it.
					fitted[k] += s.seasonalAt(k)
					if s.Steps != nil {
						fitted[k] += s.Steps[k]
					}
					rows = append(rows, fmt.Sprintf("(%s%s, %s, %s)", idVal(s), sqlTS(s.Times[k]), sqlF(s.Data[k]), score(s.Data[k], fitted[k], math.Sqrt(fit.Sigma2))))
				}
			}
			return mlRowsSubquery(cols, rows), 0, ""
		}
		// An input: each row later than its series' training, on the grid
		// and within the horizon, scored against the forecast.
		qcols, qrows, code, msg := f.queryAllRows(r, q, c.input)
		if code != 0 {
			return "", code, msg
		}
		ti, di, ii := -1, -1, -1
		for i, col := range qcols {
			switch {
			case strings.EqualFold(col.Name, ts.TimestampCol):
				ti = i
			case strings.EqualFold(col.Name, ts.DataCol):
				di = i
			case ts.IDCol != "" && strings.EqualFold(col.Name, ts.IDCol):
				ii = i
			}
		}
		if ti < 0 || di < 0 || (ts.IDCol != "" && ii < 0) {
			return "", 400, "The input data must have the model's time_series_timestamp_col, time_series_data_col and time_series_id_col"
		}
		tt := strings.ToUpper(qcols[ti].Type)
		type fcs struct{ fc, se []float64 }
		paths := map[string]fcs{}
		series := map[string]arimaSeries{}
		for _, s := range a.Series {
			if len(s.Cands) == 0 {
				continue
			}
			key := ""
			if s.ID != nil {
				key = *s.ID
			}
			fc, se, _, _ := s.arimaForecast(ts, a.Cond, ts.Horizon, 0)
			paths[key], series[key] = fcs{fc, se}, s
		}
		var keys []string
		for _, row := range qrows {
			key := ""
			if ii >= 0 {
				if row[ii] == nil {
					continue
				}
				key = *row[ii]
			}
			s, ok := series[key]
			if !ok || row[ti] == nil || row[di] == nil {
				continue
			}
			tm, ok := parseTSValue(*row[ti], tt)
			y, err := strconv.ParseFloat(*row[di], 64)
			if !ok || err != nil {
				continue
			}
			last := len(s.Times) - 1
			k := gridIndex(s.Times[0], a.Frequency, tm) - last - 1
			if k < 0 || k >= ts.Horizon || gridPoint(s.Times[0], a.Frequency, last+k+1) != tm {
				continue
			}
			p := paths[key]
			keys = append(keys, fmt.Sprintf("(%s%d, %s, %s)", idVal(s), tm, sqlF(y), score(y, p.fc[k], p.se[k])))
		}
		idSel, idJoin := "", ""
		if ii >= 0 {
			idSel = "_cbml_in." + sqlIdent(ts.IDCol) + ", "
			idJoin = " AND _cbml_s._cbml_id = _cbml_in." + sqlIdent(ts.IDCol)
		}
		scoredCols := "STRUCT<"
		if a.IDType != "" {
			scoredCols += "_cbml_id " + a.IDType + ", "
		}
		scoredCols += "_cbml_t INT64, _cbml_y FLOAT64, is_anomaly BOOL, lower_bound FLOAT64, upper_bound FLOAT64, anomaly_probability FLOAT64>"
		if len(keys) == 0 {
			keys = nil
		}
		// The input's other columns follow.
		var rest string
		for i, col := range qcols {
			if i != ti && i != di && i != ii {
				rest += ", _cbml_in." + sqlIdent(col.Name)
			}
		}
		return "(SELECT " + idSel + "CAST(_cbml_in." + tsName + " AS TIMESTAMP) AS " + tsName + ", CAST(_cbml_in." + dataName + " AS FLOAT64) AS " + dataName +
			", _cbml_s.is_anomaly, _cbml_s.lower_bound, _cbml_s.upper_bound, _cbml_s.anomaly_probability" + rest +
			" FROM (" + c.input + ") AS _cbml_in LEFT JOIN " + mlRowsSubquery(scoredCols, keys) + " AS _cbml_s ON _cbml_s._cbml_t = UNIX_MICROS(CAST(_cbml_in." + tsName +
			" AS TIMESTAMP)) AND _cbml_s._cbml_y = CAST(_cbml_in." + dataName + " AS FLOAT64)" + idJoin + ")", 0, ""
	}
	return unsupported()
}

// smooth is v's centered moving average over window points (NaN kept
// out), or v when window is under 2.
func smooth(v []float64, window int) []float64 {
	if window < 2 {
		return v
	}
	out := make([]float64, len(v))
	for i := range v {
		if math.IsNaN(v[i]) {
			out[i] = math.NaN()
			continue
		}
		lo, hi := i-(window-1)/2, i+window/2
		var s float64
		var n int
		for j := max(lo, 0); j <= min(hi, len(v)-1); j++ {
			if !math.IsNaN(v[j]) {
				s += v[j]
				n++
			}
		}
		out[i] = s / float64(n)
	}
	return out
}

// arimaResource is an ARIMA_PLUS model's training options and results.
func (md bqModel) arimaResource() (opts map[string]any, info []map[string]any) {
	ts := md.Options.TS
	opts = map[string]any{"timeSeriesTimestampColumn": ts.TimestampCol, "timeSeriesDataColumn": ts.DataCol, "horizon": strconv.Itoa(ts.Horizon),
		"autoArima": ts.AutoArima, "dataFrequency": md.Trained.Arima.Frequency, "cleanSpikesAndDips": ts.CleanSpikes, "adjustStepChanges": ts.AdjustSteps,
		"decomposeTimeSeries": ts.Decompose, "includeDrift": ts.IncludeDrift}
	if ts.LimitLower != nil {
		opts["forecastLimitLowerBound"] = *ts.LimitLower
	}
	if ts.LimitUpper != nil {
		opts["forecastLimitUpperBound"] = *ts.LimitUpper
	}
	if ts.Seasonalities != "" && ts.Seasonalities != "AUTO" {
		opts["seasonalPeriods"] = splitSeasonalities(ts.Seasonalities)
	}
	if ts.IDCol != "" {
		opts["timeSeriesIdColumn"] = ts.IDCol
	}
	if ts.AutoArima {
		opts["autoArimaMaxOrder"], opts["autoArimaMinOrder"] = strconv.Itoa(ts.MaxOrder), strconv.Itoa(ts.MinOrder)
	} else {
		opts["nonSeasonalOrder"] = map[string]string{"p": strconv.Itoa(ts.Order[0]), "d": strconv.Itoa(ts.Order[1]), "q": strconv.Itoa(ts.Order[2])}
	}
	for _, s := range md.Trained.Arima.Series {
		if len(s.Cands) == 0 {
			continue
		}
		fit := s.Cands[0]
		m := map[string]any{
			"nonSeasonalOrder":    map[string]string{"p": strconv.Itoa(fit.P), "d": strconv.Itoa(fit.D), "q": strconv.Itoa(fit.Q)},
			"arimaCoefficients":   map[string]any{"autoRegressiveCoefficients": fit.AR, "movingAverageCoefficients": fit.MA, "interceptCoefficient": fit.Mu},
			"arimaFittingMetrics": map[string]any{"logLikelihood": fit.LogLik, "aic": fit.AIC, "variance": fit.Sigma2},
			"hasDrift":            fit.Drift, "seasonalPeriods": s.seasonNames(), "hasHolidayEffect": false,
			"hasSpikesAndDips": nonZero(s.Spikes), "hasStepChanges": nonZero(s.Steps),
		}
		if s.ID != nil {
			m["timeSeriesId"] = *s.ID
		}
		info = append(info, m)
	}
	return opts, info
}
