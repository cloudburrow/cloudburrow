package bigqueryfront

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// arimaRows are a daily series of vals from 2024-01-01, as DATE values.
func arimaRows(vals []float64) ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"d", "DATE"}, {"v", "FLOAT"}}
	var rows [][]*string
	day := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, v := range vals {
		ds, vs := day.AddDate(0, 0, i).Format("2006-01-02"), strconv.FormatFloat(v, 'g', -1, 64)
		rows = append(rows, []*string{&ds, &vs})
	}
	return cols, rows
}

func arimaOpts(set ...string) mlOptions {
	o := defaultMLOptions()
	o.ModelType = "ARIMA_PLUS"
	o.TS.TimestampCol, o.TS.DataCol = "d", "v"
	o.Set = append([]string{"model_type", "time_series_timestamp_col", "time_series_data_col"}, set...)
	applyArimaDefaults(&o)
	return o
}

// ar1Series is a deterministic, stationary, wiggly series.
func ar1Series() []float64 {
	var v []float64
	x := 0.0
	for i := 0; i < 60; i++ {
		x = 0.6*x + math.Sin(float64(i)*1.7) + 0.5*math.Cos(float64(i)*0.37)
		v = append(v, 10+x)
	}
	return v
}

// TestTrainArimaAR1: ARIMA(1, 0, 0)'s CSS fit is the least squares
// regression of y_t on y_{t−1} (by hand: φ = cov/var of the pairs, μ =
// c/(1 − φ)), and its forecasts are μ + φ^h (y_n − μ), standard error
// σ √Σ_{j<h} φ^{2j}.
func TestTrainArimaAR1(t *testing.T) {
	y := ar1Series()
	o := arimaOpts("auto_arima", "non_seasonal_order")
	o.TS.AutoArima, o.TS.Order = false, []int{1, 0, 0}
	if c, m := checkArimaOptions(o); c != 0 {
		t.Fatal(m)
	}
	cols, rows := arimaRows(y)
	tr, fe := trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	a := tr.Arima
	if a.Frequency != "DAILY" || len(a.Series) != 1 || len(a.Series[0].Cands) != 1 {
		t.Fatalf("model: %+v", a)
	}
	var mx, my float64
	n := float64(len(y) - 1)
	for i := 1; i < len(y); i++ {
		mx += y[i-1] / n
		my += y[i] / n
	}
	var sxy, sxx float64
	for i := 1; i < len(y); i++ {
		sxy += (y[i-1] - mx) * (y[i] - my)
		sxx += (y[i-1] - mx) * (y[i-1] - mx)
	}
	phi := sxy / sxx
	mu := (my - phi*mx) / (1 - phi)
	var sse float64
	for i := 1; i < len(y); i++ {
		r := y[i] - mu - phi*(y[i-1]-mu)
		sse += r * r
	}
	fit := a.Series[0].Cands[0]
	if math.Abs(fit.AR[0]-phi) > 1e-6 || math.Abs(fit.Mu-mu) > 1e-6 || math.Abs(fit.Sigma2-sse/n) > 1e-9 {
		t.Fatalf("fit %+v; want φ %v μ %v σ² %v", fit, phi, mu, sse/n)
	}
	wantLL := -n / 2 * (math.Log(2*math.Pi*sse/n) + 1)
	if math.Abs(fit.LogLik-wantLL) > 1e-6 || math.Abs(fit.AIC-(-2*wantLL+6)) > 1e-5 {
		t.Errorf("log likelihood %v AIC %v; want %v %v", fit.LogLik, fit.AIC, wantLL, -2*wantLL+6)
	}
	_, fc, se := a.Series[0].arimaPath(fit, a.Cond, 3)
	last := y[len(y)-1]
	var acc float64
	for h := 1; h <= 3; h++ {
		acc += math.Pow(fit.AR[0], float64(2*(h-1)))
		want := fit.Mu + math.Pow(fit.AR[0], float64(h))*(last-fit.Mu)
		if math.Abs(fc[h-1]-want) > 1e-9 || math.Abs(se[h-1]-math.Sqrt(fit.Sigma2*acc)) > 1e-9 {
			t.Errorf("h %d: %v ± %v; want %v ± %v", h, fc[h-1], se[h-1], want, math.Sqrt(fit.Sigma2*acc))
		}
	}
}

// TestTrainArimaDrift: ARIMA(0, 1, 0) with drift is a random walk whose
// drift is the mean difference; it forecasts y_n + h·drift with standard
// error σ √h.
func TestTrainArimaDrift(t *testing.T) {
	y := []float64{1, 3, 4, 7, 8, 11, 12, 15, 17, 18}
	o := arimaOpts("auto_arima", "non_seasonal_order", "include_drift")
	o.TS.AutoArima, o.TS.Order, o.TS.IncludeDrift = false, []int{0, 1, 0}, true
	cols, rows := arimaRows(y)
	tr, fe := trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	fit := s.Cands[0]
	drift := (18.0 - 1) / 9
	var s2 float64
	for i := 1; i < len(y); i++ {
		s2 += (y[i] - y[i-1] - drift) * (y[i] - y[i-1] - drift) / 9
	}
	if !fit.Drift || math.Abs(fit.Mu-drift) > 1e-7 || math.Abs(fit.Sigma2-s2) > 1e-9 {
		t.Fatalf("fit %+v; want drift %v σ² %v", fit, drift, s2)
	}
	fitted, fc, se := s.arimaPath(fit, tr.Arima.Cond, 2)
	if !math.IsNaN(fitted[0]) || math.Abs(fitted[1]-(1+fit.Mu)) > 1e-9 {
		t.Errorf("fitted %v", fitted)
	}
	for h := 1; h <= 2; h++ {
		if math.Abs(fc[h-1]-(18+float64(h)*fit.Mu)) > 1e-9 || math.Abs(se[h-1]-math.Sqrt(s2*float64(h))) > 1e-9 {
			t.Errorf("h %d: %v ± %v", h, fc[h-1], se[h-1])
		}
	}
}

// TestTrainArimaAuto: auto_arima's candidates are every p + q ≤ 2 (six,
// twelve with drift when d is 1), the model the lowest AIC; a trend is
// differenced; a missing day is interpolated.
func TestTrainArimaAuto(t *testing.T) {
	y := ar1Series()
	cols, rows := arimaRows(y)
	tr, fe := trainModelT(arimaOpts(), cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	if s.Cands[0].D != 0 || len(s.Cands) != 6 {
		t.Fatalf("candidates %+v", s.Cands)
	}
	for _, c := range s.Cands[1:] {
		if c.AIC < s.Cands[0].AIC {
			t.Errorf("candidate %+v beats the model %+v", c, s.Cands[0])
		}
	}
	var trend []float64
	for i := 0; i < 40; i++ {
		trend = append(trend, float64(i)*2+math.Sin(float64(i)))
	}
	cols, rows = arimaRows(trend)
	rows = append(rows[:5], rows[6:]...)
	tr, fe = trainModelT(arimaOpts(), cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s = tr.Arima.Series[0]
	if s.Cands[0].D != 1 || len(s.Cands) != 12 || len(s.Times) != 40 || s.Observed[5] || s.Data[5] != (trend[4]+trend[6])/2 {
		t.Fatalf("trend: d %d, %d candidates, %d points, %v", s.Cands[0].D, len(s.Cands), len(s.Times), s.Data[4:7])
	}
}

func TestArimaOptions(t *testing.T) {
	for sql, want := range map[string]int{
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v') AS SELECT 1":                                   0,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', holiday_region='US') AS SELECT 1":              501,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', auto_arima=FALSE) AS SELECT 1":                 400,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', include_drift=TRUE) AS SELECT 1":               400,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', seasonalities=['WEEKLY']) AS SELECT 1":         0,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', seasonalities=['WEEKLY', 'AUTO']) AS SELECT 1": 400,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', forecast_limit_lower_bound=0, " +
			"forecast_limit_upper_bound=10) AS SELECT 1": 0,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', forecast_limit_lower_bound=10, " +
			"forecast_limit_upper_bound=10) AS SELECT 1": 400,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', auto_arima=FALSE, " +
			"non_seasonal_order=(1, 1, 0), include_drift=TRUE) AS SELECT 1": 0,
	} {
		toks, _ := lex(sql)
		d, is, code, msg := parseModelDDL(toks, sql)
		if !is || code != want {
			t.Errorf("%s: %d %s; want %d", sql, code, msg, want)
		}
		if code == 0 && d.opts.TS.Horizon != 1000 {
			t.Errorf("%s: horizon %d", sql, d.opts.TS.Horizon)
		}
	}
}

// weekly is a weekly pattern summing to zero.
var weekly = []float64{3, -1, -2, 0, 1, -4, 3}

// pipelineSeries is 84 days of a slow trend, the weekly pattern, a little
// deterministic noise, a spike of +15 on day 30 and a step of +10 from day
// 55.
func pipelineSeries() []float64 {
	var v []float64
	for t := 0; t < 84; t++ {
		x := 50 + 0.1*float64(t) + weekly[t%7] + 0.2*math.Sin(2.3*float64(t))
		if t == 30 {
			x += 15
		}
		if t >= 55 {
			x += 10
		}
		v = append(v, x)
	}
	return v
}

// TestArimaPipeline: the default pipeline finds the weekly seasonality,
// the spike and the step, takes them out before the ARIMA, and the
// components add up to the data; the forecast continues the latest level
// and the weekly pattern.
func TestArimaPipeline(t *testing.T) {
	y := pipelineSeries()
	cols, rows := arimaRows(y)
	tr, fe := trainModelT(arimaOpts(), cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	if len(s.Seasons) != 1 || s.Seasons[0] != (arimaSeason{"WEEKLY", 7}) {
		t.Fatalf("seasons %v; want WEEKLY", s.Seasons)
	}
	for k := range y {
		if d := s.Seasonal[0][k] - weekly[k%7]; math.Abs(d) > 0.5 {
			t.Errorf("day %d: seasonal %v; want about %v", k, s.Seasonal[0][k], weekly[k%7])
		}
		want := 0.0
		if k == 30 {
			want = 15
		}
		if math.Abs(s.Spikes[k]-want) > 1 || (want == 0 && s.Spikes[k] != 0) {
			t.Errorf("day %d: spike %v; want %v", k, s.Spikes[k], want)
		}
		want = 0
		if k < 55 {
			want = -10
		}
		if math.Abs(s.Steps[k]-want) > 1 {
			t.Errorf("day %d: step %v; want %v", k, s.Steps[k], want)
		}
	}
	// The components add up to the data.
	x := s.adjusted()
	for k := range y {
		if sum := x[k] + s.Seasonal[0][k] + s.Spikes[k] + s.Steps[k]; math.Abs(sum-y[k]) > 1e-9 {
			t.Errorf("day %d: components sum to %v; data %v", k, sum, y[k])
		}
	}
	// The adjusted series is the trend: the step and spike are gone.
	for k := 1; k < len(x); k++ {
		if d := x[k] - x[k-1]; math.Abs(d) > 2 {
			t.Errorf("day %d: adjusted series jumps %v", k, d)
		}
	}
	fc, _, _, _ := s.arimaForecast(arimaOpts().TS, tr.Arima.Cond, 7, 1.96)
	for k, f := range fc {
		day := 84 + k
		want := 60 + 0.1*float64(day) + weekly[day%7]
		if math.Abs(f-want) > 1.5 {
			t.Errorf("forecast %d: %v; want about %v", k, f, want)
		}
	}
}

// TestArimaPipelineOff: with NO_SEASONALITY, clean_spikes_and_dips and
// adjust_step_changes FALSE the series is fitted as it is; an explicit
// seasonality is used without the strength test; forecast limits bound
// the forecast.
func TestArimaPipelineOff(t *testing.T) {
	y := pipelineSeries()
	cols, rows := arimaRows(y)
	o := arimaOpts("seasonalities", "clean_spikes_and_dips", "adjust_step_changes")
	o.TS.Seasonalities, o.TS.CleanSpikes, o.TS.AdjustSteps = "NO_SEASONALITY", false, false
	tr, fe := trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	if len(s.Seasons) != 0 || s.Spikes != nil || s.Steps != nil {
		t.Fatalf("components %v %v %v; want none", s.Seasons, s.Spikes, s.Steps)
	}
	for k, v := range s.adjusted() {
		if v != y[k] {
			t.Fatalf("adjusted %v; want the data", v)
		}
	}
	// A small but exact weekly pattern is found; a given seasonality is
	// fitted without the strength test.
	flat := make([]float64, 28)
	for k := range flat {
		flat[k] = 5 + 0.01*float64(k%7)
	}
	cols, rows = arimaRows(flat)
	tr, fe = trainModelT(arimaOpts(), cols, rows)
	if fe != nil || len(tr.Arima.Series[0].Seasons) != 1 {
		t.Fatalf("a small weekly pattern: %v %v", fe, tr.Arima.Series[0].Seasons)
	}
	o = arimaOpts("seasonalities")
	o.TS.Seasonalities = "WEEKLY"
	cols, rows = arimaRows(ar1Series())
	tr, fe = trainModelT(o, cols, rows)
	if fe != nil || len(tr.Arima.Series[0].Seasons) != 1 {
		t.Fatalf("seasonalities WEEKLY: %v %v", fe, tr.Arima.Series[0].Seasons)
	}
	lo, hi := 59.0, 61.0
	o = arimaOpts("forecast_limit_lower_bound", "forecast_limit_upper_bound")
	o.TS.LimitLower, o.TS.LimitUpper = &lo, &hi
	cols, rows = arimaRows(y)
	tr, fe = trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	fc, _, l, u := tr.Arima.Series[0].arimaForecast(o.TS, tr.Arima.Cond, 14, 1.96)
	for k := range fc {
		if fc[k] < lo || fc[k] > hi || l[k] < lo || u[k] > hi {
			t.Errorf("forecast %d: %v [%v, %v] outside [%v, %v]", k, fc[k], l[k], u[k], lo, hi)
		}
	}
}

// TestStepAndSpikeDetectors: a series of noise alone has neither.
func TestStepAndSpikeDetectors(t *testing.T) {
	var x []float64
	for k := 0; k < 100; k++ {
		x = append(x, math.Sin(float64(k)*1.3)+0.5*math.Cos(float64(k)*0.7))
	}
	if nonZero(stepChanges(x)) || nonZero(spikesAndDips(x)) {
		t.Errorf("steps %v spikes %v in noise", stepChanges(x), spikesAndDips(x))
	}
	x[40] += 20
	if sp := spikesAndDips(x); sp[40] < 15 || nonZero(append(sp[:40:40], sp[41:]...)) {
		t.Errorf("spikes %v; want day 40 only", sp)
	}
}
