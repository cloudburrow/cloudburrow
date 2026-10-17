//go:build compat

package compat

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

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
