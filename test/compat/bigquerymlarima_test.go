//go:build compat

package compat

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// BigQuery ML ARIMA_PLUS (#1504): training, ML.FORECAST,
// ML.EXPLAIN_FORECAST, ML.DETECT_ANOMALIES, ML.ARIMA_EVALUATE and
// ML.ARIMA_COEFFICIENTS through the official Go client, against values
// computed by hand for a random walk with drift, and auto.ARIMA's
// candidate search over two series.

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLArimaPlus(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	y := []float64{1, 3, 4, 7, 8, 11, 12, 15, 17, 18}
	name := mlDataset(t, h, c, map[string]string{
		"walk": "SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, v, IF(MOD(i, 2) = 0, 'a', 'b') AS id, v * (1 + MOD(i, 3)) AS w " +
			"FROM UNNEST([1.0, 3, 4, 7, 8, 11, 12, 15, 17, 18]) AS v WITH OFFSET i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.rw` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
		"auto_arima=FALSE, non_seasonal_order=(0, 1, 0), include_drift=TRUE, horizon=5) AS SELECT d, v FROM `%s.walk`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL ARIMA_PLUS: %v", err)
	}
	md, err := c.Dataset(name).Model("rw").Metadata(ctx)
	if err != nil || md.Type != "ARIMA_PLUS" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.TimeSeriesDataColumn != "v" || runs[0].TrainingOptions.Horizon != 5 {
		t.Errorf("training run: %+v", runs)
	}
	// By hand: the drift is the mean difference, 17/9; σ² the differences'
	// mean squared deviation from it; a forecast y_n + h·drift with
	// standard error σ√h.
	drift := 17.0 / 9
	var s2 float64
	for i := 1; i < len(y); i++ {
		s2 += (y[i] - y[i-1] - drift) * (y[i] - y[i-1] - drift) / 9
	}
	z := math.Sqrt2 * math.Erfinv(0.9)
	fc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL `%s.rw`, STRUCT(2 AS horizon, 0.9 AS confidence_level)) ORDER BY forecast_timestamp", name))
	if len(fc) != 2 {
		t.Fatalf("ML.FORECAST: %v", fc)
	}
	for k, r := range fc {
		hh := float64(k + 1)
		f, se := 18+hh*drift, math.Sqrt(s2*hh)
		ts, _ := r["forecast_timestamp"].(time.Time)
		if !ts.Equal(time.Date(2024, 1, 11+k, 0, 0, 0, 0, time.UTC)) || !near(r["forecast_value"], f, 1e-6) || !near(r["standard_error"], se, 1e-6) ||
			!near(r["confidence_level"], 0.9, 1e-12) || !near(r["prediction_interval_lower_bound"], f-z*se, 1e-6) ||
			!near(r["prediction_interval_upper_bound"], f+z*se, 1e-6) {
			t.Errorf("ML.FORECAST row %d: %v; want %v ± %v", k, r, f, se)
		}
	}
	co := mlQuery(t, h, c, fmt.Sprintf("SELECT ARRAY_LENGTH(ar_coefficients) AS p, ARRAY_LENGTH(ma_coefficients) AS q, intercept_or_drift "+
		"FROM ML.ARIMA_COEFFICIENTS(MODEL `%s.rw`)", name))
	if len(co) != 1 || co[0]["p"] != int64(0) || co[0]["q"] != int64(0) || !near(co[0]["intercept_or_drift"], drift, 1e-6) {
		t.Errorf("ML.ARIMA_COEFFICIENTS: %v; want drift %v", co, drift)
	}
	ll := -4.5 * (math.Log(2*math.Pi*s2) + 1)
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.ARIMA_EVALUATE(MODEL `%s.rw`)", name))
	if len(ev) != 1 || ev[0]["non_seasonal_p"] != int64(0) || ev[0]["non_seasonal_d"] != int64(1) || ev[0]["has_drift"] != true ||
		!near(ev[0]["variance"], s2, 1e-6) || !near(ev[0]["log_likelihood"], ll, 1e-5) || !near(ev[0]["AIC"], -2*ll+4, 1e-5) {
		t.Errorf("ML.ARIMA_EVALUATE: %v; want variance %v, log likelihood %v", ev, s2, ll)
	}
	// The training data: each day's one-step fit is the day before plus
	// the drift (the first has none).
	sd := math.Sqrt(s2)
	an := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.rw`, STRUCT(0.9 AS anomaly_prob_threshold)) ORDER BY d", name))
	if len(an) != 10 || an[0]["anomaly_probability"] != nil || !near(an[1]["lower_bound"], 1+drift-z*sd, 1e-6) ||
		!near(an[1]["anomaly_probability"], math.Erf(math.Abs(3-1-drift)/(sd*math.Sqrt2)), 1e-6) {
		t.Errorf("ML.DETECT_ANOMALIES: %v", an)
	}
	// New data: scored against the forecast, a day past the training.
	an = mlQuery(t, h, c, fmt.Sprintf("SELECT d, is_anomaly, anomaly_probability FROM ML.DETECT_ANOMALIES(MODEL `%s.rw`, STRUCT(0.9 AS anomaly_prob_threshold), "+
		"(SELECT DATE '2024-01-11' AS d, %v AS v UNION ALL SELECT DATE '2024-01-12', 100.0 UNION ALL SELECT DATE '2023-12-01', 5.0)) ORDER BY d", name, 18+drift))
	if len(an) != 3 || an[0]["is_anomaly"] != nil || an[1]["is_anomaly"] != false || !near(an[1]["anomaly_probability"], 0, 1e-6) ||
		an[2]["is_anomaly"] != true || !near(an[2]["anomaly_probability"], math.Erf((100-18-2*drift)/(math.Sqrt(2*s2)*math.Sqrt2)), 1e-6) {
		t.Errorf("ML.DETECT_ANOMALIES of new data: %v", an)
	}
	ex := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.rw`, STRUCT(2 AS horizon)) ORDER BY time_series_timestamp", name))
	if len(ex) != 12 || ex[1]["time_series_type"] != "history" || !near(ex[1]["trend"], 1+drift, 1e-6) || !near(ex[1]["residual"], 2-drift, 1e-6) ||
		ex[11]["time_series_type"] != "forecast" || !near(ex[11]["time_series_data"], 18+2*drift, 1e-6) || ex[11]["residual"] != nil ||
		ex[1]["seasonal_period_weekly"] != nil || !near(ex[1]["standard_error"], sd, 1e-6) {
		t.Errorf("ML.EXPLAIN_FORECAST: %v", ex)
	}
	// auto.ARIMA over two series: p + q ≤ 2, the lowest AIC first.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.multi` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='w', "+
		"time_series_id_col='id', data_frequency='DAILY') AS SELECT d, w, id FROM `%s.walk`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL ARIMA_PLUS of two series: %v", err)
	}
	all := mlQuery(t, h, c, fmt.Sprintf("SELECT id, non_seasonal_p + non_seasonal_q AS o, AIC FROM ML.ARIMA_EVALUATE(MODEL `%s.multi`)", name))
	best := mlQuery(t, h, c, fmt.Sprintf("SELECT id, AIC FROM ML.ARIMA_EVALUATE(MODEL `%s.multi`, STRUCT(FALSE AS show_all_candidate_models)) ORDER BY id", name))
	if len(best) != 2 || best[0]["id"] != "a" || best[1]["id"] != "b" {
		t.Fatalf("ML.ARIMA_EVALUATE best: %v", best)
	}
	for _, r := range all {
		k := 0
		if r["id"] == "b" {
			k = 1
		}
		if o, _ := r["o"].(int64); o > 2 || r["AIC"].(float64) < best[k]["AIC"].(float64) {
			t.Errorf("candidate %v beyond the order or better than the model %v", r, best[k])
		}
	}
	if n := len(mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL `%s.multi`)", name))); n != 6 {
		t.Errorf("ML.FORECAST of two series: %d rows; want 3 each", n)
	}
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
		"holiday_region='US') AS SELECT d, v FROM `%s.walk`", name, name)).Read(ctx)
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
		t.Errorf("holiday_region: %v; want 501", err)
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.rw`, (SELECT 1 AS x))", name)).Read(ctx)
	wantHTTPStatus(t, "ML.PREDICT of an ARIMA_PLUS model", err, http.StatusBadRequest)
}

// TestBigQueryMLArimaPlusPipeline (#1518): ARIMA_PLUS's default pipeline
// on 84 days of a slow trend, a weekly pattern, a spike of +15 on day 30
// and a step of +10 from day 55 finds the weekly seasonality, the spike
// and the step; ML.EXPLAIN_FORECAST's components add up to the data, and
// the forecast continues the latest level and the weekly pattern. Explicit
// seasonalities, the stages turned off and the forecast limits are read
// back too.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLArimaPlusPipeline(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	weekly := []float64{3, -1, -2, 0, 1, -4, 3}
	name := mlDataset(t, h, c, map[string]string{
		"s": "SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 50 + 0.1 * i + [3.0, -1, -2, 0, 1, -4, 3][OFFSET(MOD(i, 7))] + 0.2 * SIN(2.3 * i) + " +
			"IF(i = 30, 15, 0) + IF(i >= 55, 10, 0) AS v FROM UNNEST(GENERATE_ARRAY(0, 83)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.m` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v') "+
		"AS SELECT d, v FROM `%s.s`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL ARIMA_PLUS: %v", err)
	}
	md, err := c.Dataset(name).Model("m").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || len(runs[0].Results) != 1 || runs[0].Results[0].ArimaResult == nil ||
		fmt.Sprint(runs[0].Results[0].ArimaResult.SeasonalPeriods) != "[WEEKLY]" {
		t.Errorf("training run's seasonal periods: %+v", runs)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT seasonal_periods, has_spikes_and_dips, has_step_changes FROM ML.ARIMA_EVALUATE(MODEL `%s.m`, "+
		"STRUCT(FALSE AS show_all_candidate_models))", name))
	if len(ev) != 1 || fmt.Sprint(ev[0]["seasonal_periods"]) != "[WEEKLY]" || ev[0]["has_spikes_and_dips"] != true || ev[0]["has_step_changes"] != true {
		t.Errorf("ML.ARIMA_EVALUATE: %v; want WEEKLY with spikes and dips and step changes", ev)
	}
	ex := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.m`, STRUCT(7 AS horizon)) ORDER BY time_series_timestamp", name))
	if len(ex) != 91 {
		t.Fatalf("ML.EXPLAIN_FORECAST: %d rows; want 84 + 7", len(ex))
	}
	f := func(v bigquery.Value) float64 { x, _ := v.(float64); return x }
	for k, r := range ex {
		if r["seasonal_period_weekly"] == nil || r["seasonal_period_yearly"] != nil || r["seasonal_period_monthly"] != nil || r["holiday_effect"] != nil {
			t.Errorf("row %d: seasonal columns %v", k, r)
		}
		if k >= 84 {
			// A forecast: the trend plus the weekly component, the
			// latest level and the weekly pattern.
			want := 60 + 0.1*float64(k) + weekly[k%7]
			if r["time_series_type"] != "forecast" || !near(r["time_series_data"], f(r["trend"])+f(r["seasonal_period_weekly"]), 1e-6) ||
				!near(r["time_series_data"], want, 1.5) || r["spikes_and_dips"] != nil || r["step_changes"] != nil || r["residual"] != nil {
				t.Errorf("forecast row %d: %v; want about %v", k, r, want)
			}
			continue
		}
		spike, step := 0.0, 0.0
		if k == 30 {
			spike = 15
		}
		if k < 55 {
			step = -10
		}
		if r["time_series_type"] != "history" || !near(r["spikes_and_dips"], spike, 1) || !near(r["step_changes"], step, 1) ||
			!near(r["seasonal_period_weekly"], weekly[k%7], 0.5) {
			t.Errorf("history row %d: %v; want spike %v, step %v, weekly %v", k, r, spike, step, weekly[k%7])
		}
		if r["trend"] == nil {
			continue
		}
		sum := f(r["trend"]) + f(r["seasonal_period_weekly"]) + f(r["spikes_and_dips"]) + f(r["step_changes"]) + f(r["residual"])
		if !near(r["time_series_data"], sum, 1e-6) || !near(r["time_series_adjusted_data"], f(r["trend"])+f(r["seasonal_period_weekly"]), 1e-6) {
			t.Errorf("history row %d: components sum to %v; data %v", k, sum, r["time_series_data"])
		}
	}
	// The spike is ML.DETECT_ANOMALIES' most anomalous point; the step,
	// adjusted, is not an anomaly.
	an := mlQuery(t, h, c, fmt.Sprintf("SELECT d, is_anomaly FROM ML.DETECT_ANOMALIES(MODEL `%s.m`, STRUCT(0.9999 AS anomaly_prob_threshold)) "+
		"WHERE d IN ('2024-01-31', '2024-02-24', '2024-02-25') ORDER BY d", name))
	top := mlQuery(t, h, c, fmt.Sprintf("SELECT d FROM ML.DETECT_ANOMALIES(MODEL `%s.m`) ORDER BY anomaly_probability DESC LIMIT 1", name))
	if len(an) != 3 || an[0]["is_anomaly"] != true || an[1]["is_anomaly"] != false || an[2]["is_anomaly"] != false || len(top) != 1 ||
		!top[0]["d"].(time.Time).Equal(time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ML.DETECT_ANOMALIES: %v, most anomalous %v; want the spike of 2024-01-31 alone", an, top)
	}
	// The stages turned off: no components, NO_SEASONALITY.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.off` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
		"seasonalities=['NO_SEASONALITY'], clean_spikes_and_dips=FALSE, adjust_step_changes=FALSE) AS SELECT d, v FROM `%s.s`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with the stages off: %v", err)
	}
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT seasonal_periods, has_spikes_and_dips, has_step_changes FROM ML.ARIMA_EVALUATE(MODEL `%s.off`, "+
		"STRUCT(FALSE AS show_all_candidate_models))", name))
	if len(ev) != 1 || fmt.Sprint(ev[0]["seasonal_periods"]) != "[NO_SEASONALITY]" || ev[0]["has_spikes_and_dips"] != false || ev[0]["has_step_changes"] != false {
		t.Errorf("ML.ARIMA_EVALUATE with the stages off: %v", ev)
	}
	ex = mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.off`, STRUCT(1 AS horizon)) ORDER BY time_series_timestamp LIMIT 2", name))
	if len(ex) != 2 || ex[1]["spikes_and_dips"] != nil || ex[1]["step_changes"] != nil || ex[1]["seasonal_period_weekly"] != nil {
		t.Errorf("ML.EXPLAIN_FORECAST with the stages off: %v", ex)
	}
	// An explicit seasonality, and forecast limits.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lim` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
		"seasonalities=['WEEKLY'], forecast_limit_lower_bound=59, forecast_limit_upper_bound=61) AS SELECT d, v FROM `%s.s`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with forecast limits: %v", err)
	}
	fc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL `%s.lim`, STRUCT(14 AS horizon))", name))
	if len(fc) != 14 {
		t.Fatalf("ML.FORECAST with limits: %v", fc)
	}
	for _, r := range fc {
		if v := f(r["forecast_value"]); v < 59 || v > 61 || f(r["prediction_interval_lower_bound"]) < 59 || f(r["prediction_interval_upper_bound"]) > 61 {
			t.Errorf("ML.FORECAST with limits [59, 61]: %v", r)
		}
	}
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
		"forecast_limit_lower_bound=5, forecast_limit_upper_bound=1) AS SELECT d, v FROM `%s.s`", name, name)).Read(ctx)
	wantHTTPStatus(t, "forecast_limit_lower_bound above the upper bound", err, http.StatusBadRequest)
}

// TestBigQueryMLArimaPlusSTL: ARIMA_PLUS finds seasonality by "Seasonal
// and Trend decomposition using Loess (STL)" (bigqueryml-syntax-create-time-series),
// and ML.EXPLAIN_FORECAST gives its seasonal component. A level plus an
// exact weekly pattern summing to zero is decomposed exactly (each
// cycle-subseries is constant, so is its loess, and the low-pass filter
// of the pattern is 0); a pattern whose amplitude grows is followed as it
// grows, as STL's loess of each cycle-subseries does and a fixed
// per-position mean would not.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLArimaPlusSTL(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	weekly := []float64{3, -1, -2, 0, 1, -4, 3}
	name := mlDataset(t, h, c, map[string]string{
		"flat": "SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 10 + [3.0, -1, -2, 0, 1, -4, 3][OFFSET(MOD(i, 7))] AS v " +
			"FROM UNNEST(GENERATE_ARRAY(0, 27)) AS i",
		"grow": "SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 10 + (1 + i / 14) * [3.0, -1, -2, 0, 1, -4, 3][OFFSET(MOD(i, 7))] AS v " +
			"FROM UNNEST(GENERATE_ARRAY(0, 41)) AS i",
	})
	for _, tbl := range []string{"flat", "grow"} {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
			"seasonalities=['WEEKLY'], clean_spikes_and_dips=FALSE, adjust_step_changes=FALSE) AS SELECT d, v FROM `%s.%s`", name, tbl, name, tbl)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", tbl, err)
		}
	}
	hist := func(m string) []map[string]bigquery.Value {
		return mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.%s`, STRUCT(1 AS horizon)) "+
			"WHERE time_series_type = 'history' ORDER BY time_series_timestamp", name, m))
	}
	ex := hist("flat")
	if len(ex) != 28 {
		t.Fatalf("ML.EXPLAIN_FORECAST flat: %d history rows; want 28", len(ex))
	}
	for k, r := range ex {
		if !near(r["seasonal_period_weekly"], weekly[k%7], 1e-9) {
			t.Errorf("flat day %d: seasonal %v; want %v exactly", k, r["seasonal_period_weekly"], weekly[k%7])
		}
	}
	ex = hist("grow")
	if len(ex) != 42 {
		t.Fatalf("ML.EXPLAIN_FORECAST grow: %d history rows; want 42", len(ex))
	}
	// Monday's +3 is 3·1 in the first week and 3·(1 + 35/14) = 10.5 in the
	// last: the seasonal component grows with it (loess of span 7 over the
	// six Mondays smooths it, so less than the data does; a per-position
	// mean would not grow at all).
	first, last := ex[0]["seasonal_period_weekly"].(float64), ex[35]["seasonal_period_weekly"].(float64)
	if first < 2 || first > 6 || last < 7 || last > 11.5 || last-first < 2 {
		t.Errorf("grow: Monday's seasonal %v in the first week and %v in the last; want it to grow from about 3 toward 10.5", first, last)
	}
}

// TestBigQueryMLArimaPlusETS: BigQuery extrapolates seasonality "by using
// double exponential smoothing (ETS)"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series,
// docs re-read 2026-10-17), so a forecast's seasonal component follows a
// pattern whose amplitude grows, rather than repeating the last trained
// cycle (#1600). Double exponential smoothing's forecast is linear in the
// steps ahead: in ML.EXPLAIN_FORECAST each weekday's seasonal_period_weekly
// moves by the same amount from one forecast week to the next, the
// forecast weeks' amplitude grows past the last trained week's, and
// ML.FORECAST's forecast_value is the trend plus that seasonal part. Its
// constants and how they are fitted are UNVERIFIED (docs re-read
// 2026-10-17: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series),
// so the test checks these properties, not values.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLArimaPlusETS(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	name := mlDataset(t, h, c, map[string]string{
		"grow": "SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 10 + (1 + i / 14) * [3.0, -1, -2, 0, 1, -4, 3][OFFSET(MOD(i, 7))] AS v " +
			"FROM UNNEST(GENERATE_ARRAY(0, 55)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.m` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "+
		"seasonalities=['WEEKLY'], clean_spikes_and_dips=FALSE, adjust_step_changes=FALSE, horizon=21) AS SELECT d, v FROM `%s.grow`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	ex := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.m`, STRUCT(21 AS horizon)) ORDER BY time_series_timestamp", name))
	if len(ex) != 56+21 {
		t.Fatalf("ML.EXPLAIN_FORECAST: %d rows; want 56 + 21", len(ex))
	}
	seas := make([]float64, len(ex))
	for k, r := range ex {
		v, ok := r["seasonal_period_weekly"].(float64)
		if !ok {
			t.Fatalf("row %d: seasonal_period_weekly %v", k, r["seasonal_period_weekly"])
		}
		seas[k] = v
	}
	amp := func(from int) float64 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, v := range seas[from : from+7] {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		return hi - lo
	}
	if last, ahead := amp(49), amp(70); ahead <= last+1 {
		t.Errorf("weekly amplitude %v in the last trained week and %v in the third forecast week; want it to grow, not repeat", last, ahead)
	}
	repeated := true
	for k := 56; k < 63; k++ {
		if !near(seas[k], seas[k-7], 1e-6) {
			repeated = false
		}
		// Linear in the weeks ahead.
		if d1, d2 := seas[k+7]-seas[k], seas[k+14]-seas[k+7]; math.Abs(d1-d2) > 1e-6 {
			t.Errorf("day %d: seasonal moves %v then %v a week; want the same step (double exponential smoothing)", k, d1, d2)
		}
	}
	if repeated {
		t.Errorf("the first forecast week's seasonal %v repeats the last trained week's %v", seas[56:63], seas[49:56])
	}
	fc := mlQuery(t, h, c, fmt.Sprintf("SELECT forecast_value FROM ML.FORECAST(MODEL `%s.m`, STRUCT(21 AS horizon)) ORDER BY forecast_timestamp", name))
	if len(fc) != 21 {
		t.Fatalf("ML.FORECAST: %d rows; want 21", len(fc))
	}
	for k, r := range fc {
		e := ex[56+k]
		tr, _ := e["trend"].(float64)
		if !near(r["forecast_value"], tr+seas[56+k], 1e-6) || !near(e["time_series_data"], tr+seas[56+k], 1e-6) {
			t.Errorf("forecast %d: %v; want the trend %v plus the seasonal %v", k, r["forecast_value"], tr, seas[56+k])
		}
	}
}
