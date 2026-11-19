//go:build compat

package compat

import (
	"fmt"
	"math"
	"net/http"
	"testing"
)

// TestBigQueryMLArimaPlusXreg (#1766): an ARIMA_PLUS_XREG model through
// the official cloud.google.com/go/bigquery client, of a daily series
// y = 10 + 2·temp + 5·[promo] whose regressors explain it: models.get names
// the type, the features and l2Regularization; ML.FORECAST of the next
// five days' features forecasts each within 1 of 10 + 2·temp + 5·[promo];
// ML.EVALUATE of the next days' data answers the documented aggregate
// errors (a small MAE) and, with perform_aggregation FALSE, each row's;
// ML.EXPLAIN_FORECAST answers the history and the forecast with
// attribution_temp, about 2·temp; ML.ARIMA_COEFFICIENTS names each
// processed_input and its weight; ML.FORECAST with no input is 400.
// ARIMA_PLUS's ML.EVALUATE of later data
// answers the same aggregate errors.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLArimaPlusXreg(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	gen := func(from, to int) string {
		return fmt.Sprintf("SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 10 + 2 * MOD(i * 37, 11) + IF(MOD(i, 3) = 0, 5, 0) AS y, "+
			"CAST(MOD(i * 37, 11) AS FLOAT64) AS temp, MOD(i, 3) = 0 AS promo FROM UNNEST(GENERATE_ARRAY(%d, %d)) AS i", from, to)
	}
	name := mlDataset(t, h, c, map[string]string{"train": gen(0, 59), "future": gen(60, 64)})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.xr` OPTIONS(model_type='ARIMA_PLUS_XREG', time_series_timestamp_col='d', "+
		"time_series_data_col='y') AS SELECT * FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	md, err := c.Dataset(name).Model("xr").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if md.Type != "ARIMA_PLUS_XREG" {
		t.Errorf("type %q", md.Type)
	}
	if fc, _ := md.RawFeatureColumns(); len(fc) != 2 || fc[0].Name != "temp" || fc[1].Name != "promo" {
		t.Errorf("feature columns %+v", fc)
	}
	want := func(i int) float64 {
		v := 10 + 2*float64((i*37)%11)
		if i%3 == 0 {
			v += 5
		}
		return v
	}
	fc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL `%s.xr`, STRUCT(5 AS horizon), (SELECT d, temp, promo FROM `%s.future`)) "+
		"ORDER BY forecast_timestamp", name, name))
	if len(fc) != 5 {
		t.Fatalf("ML.FORECAST: %d rows; want 5", len(fc))
	}
	for k, r := range fc {
		v, _ := r["forecast_value"].(float64)
		lo, _ := r["prediction_interval_lower_bound"].(float64)
		hi, _ := r["prediction_interval_upper_bound"].(float64)
		if math.Abs(v-want(60+k)) > 1 || lo > v || hi < v {
			t.Errorf("forecast %d: %v [%v, %v]; want about %v", k, v, lo, hi, want(60+k))
		}
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.xr`, (SELECT * FROM `%s.future`))", name, name))
	if len(ev) != 1 {
		t.Fatalf("ML.EVALUATE: %d rows", len(ev))
	}
	for _, k := range []string{"mean_absolute_error", "mean_squared_error", "root_mean_squared_error", "mean_absolute_percentage_error",
		"symmetric_mean_absolute_percentage_error", "mean_absolute_scaled_error"} {
		if _, ok := ev[0][k].(float64); !ok {
			t.Errorf("ML.EVALUATE %s = %v", k, ev[0][k])
		}
	}
	if mae, _ := ev[0]["mean_absolute_error"].(float64); mae > 1 {
		t.Errorf("mean_absolute_error %v", mae)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.xr`, (SELECT * FROM `%s.future`), STRUCT(FALSE AS perform_aggregation))", name, name))
	if len(rows) != 5 {
		t.Fatalf("ML.EVALUATE per row: %d rows", len(rows))
	}
	for _, k := range []string{"d", "y", "forecasted_y", "lower_bound", "upper_bound", "absolute_error", "absolute_percentage_error"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("ML.EVALUATE per row lacks %s: %v", k, rows[0])
		}
	}
	ex := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.xr`, STRUCT(5 AS horizon), (SELECT d, temp, promo FROM `%s.future`)) "+
		"ORDER BY time_series_timestamp", name, name))
	if len(ex) != 65 {
		t.Fatalf("ML.EXPLAIN_FORECAST: %d rows; want 60 + 5", len(ex))
	}
	for _, k := range []int{3, 62} {
		temp := float64((k * 37) % 11)
		a, _ := ex[k]["attribution_temp"].(float64)
		if math.Abs(a-2*temp) > 0.5 {
			t.Errorf("row %d: attribution_temp %v; want about %v", k, a, 2*temp)
		}
		if ty := ex[k]["time_series_type"]; (k < 60) != (ty == "history") {
			t.Errorf("row %d: type %v", k, ty)
		}
	}
	co := mlQuery(t, h, c, fmt.Sprintf("SELECT processed_input, weight FROM ML.ARIMA_COEFFICIENTS(MODEL `%s.xr`) ORDER BY processed_input", name))
	got := map[string]any{}
	for _, r := range co {
		got[fmt.Sprint(r["processed_input"])] = r["weight"]
	}
	if w, _ := got["temp"].(float64); math.Abs(w-2) > 0.2 {
		t.Errorf("temp's weight %v; want about 2: %v", got["temp"], got)
	}
	if _, ok := got["__INTERCEPT__"]; !ok || len(got) != 3 {
		t.Errorf("processed inputs %v", got)
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL `%s.xr`)", name)).Read(ctx)
	wantHTTPStatus(t, "ML.FORECAST of an XREG model without input", err, http.StatusBadRequest)
	// ML.DETECT_ANOMALIES is TestBigQueryMLXregDetectAnomalies'.

	// ARIMA_PLUS's ML.EVALUATE of later data.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ar` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', "+
		"time_series_data_col='y') AS SELECT d, y FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL ARIMA_PLUS: %v", err)
	}
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.ar`, (SELECT d, y FROM `%s.future`))", name, name))
	if len(ev) != 1 {
		t.Fatalf("ARIMA_PLUS ML.EVALUATE: %d rows", len(ev))
	}
	if _, ok := ev[0]["root_mean_squared_error"].(float64); !ok {
		t.Errorf("ARIMA_PLUS ML.EVALUATE: %v", ev[0])
	}
}
