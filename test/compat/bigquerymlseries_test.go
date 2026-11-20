//go:build compat

package compat

import (
	"fmt"
	"math"
	"testing"
)

// TestBigQueryMLXregDetectAnomalies: ML.DETECT_ANOMALIES of an
// ARIMA_PLUS_XREG model through the official cloud.google.com/go/bigquery
// client, of a daily series y = 10 + 2·temp plus a little noise whose
// training data has one spike the regressor does not explain: with no
// input it scores the training data and finds that spike alone; with new
// data (and its features) it finds the one point that departs from
// 10 + 2·temp, answering the documented columns followed by the input's
// other ones.
//
// covers: bigquery.jobs.query
func TestBigQueryMLXregDetectAnomalies(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	gen := func(from, to int) string {
		return fmt.Sprintf("SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 10 + 2 * MOD(i * 37, 11) + 0.2 * MOD(i * 7, 5) + IF(i = 30, 40, 0) AS y, "+
			"CAST(MOD(i * 37, 11) AS FLOAT64) AS temp FROM UNNEST(GENERATE_ARRAY(%d, %d)) AS i", from, to)
	}
	name := mlDataset(t, h, c, map[string]string{"train": gen(0, 59), "future": gen(60, 64)})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.xr` OPTIONS(model_type='ARIMA_PLUS_XREG', time_series_timestamp_col='d', "+
		"time_series_data_col='y') AS SELECT * FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	hist := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.xr`, STRUCT(0.999 AS anomaly_prob_threshold)) ORDER BY d", name))
	if len(hist) != 60 {
		t.Fatalf("ML.DETECT_ANOMALIES of the training data: %d rows; want 60", len(hist))
	}
	for k, r := range hist {
		if (r["is_anomaly"] == true) != (k == 30) {
			t.Errorf("history row %d: %v", k, r)
		}
	}
	if y, _ := hist[30]["y"].(float64); math.Abs(y-(10+2*float64((30*37)%11)+0.2*float64((30*7)%5)+40)) > 1e-9 {
		t.Errorf("history row 30's y %v; want the data as given", hist[30]["y"])
	}
	nw := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.xr`, STRUCT(0.999 AS anomaly_prob_threshold), "+
		"(SELECT d, IF(d = DATE '2024-03-03', y + 30, y) AS y, temp FROM `%s.future`)) ORDER BY d", name, name))
	if len(nw) != 5 {
		t.Fatalf("ML.DETECT_ANOMALIES of new data: %d rows; want 5", len(nw))
	}
	for k, r := range nw {
		for _, col := range []string{"d", "y", "is_anomaly", "lower_bound", "upper_bound", "anomaly_probability", "temp"} {
			if _, ok := r[col]; !ok {
				t.Fatalf("new data row lacks %s: %v", col, r)
			}
		}
		lo, _ := r["lower_bound"].(float64)
		hi, _ := r["upper_bound"].(float64)
		if (r["is_anomaly"] == true) != (k == 2) || !(lo < hi) {
			t.Errorf("new data row %d: %v", k, r)
		}
	}
}

// TestBigQueryMLSeveralSeriesIDs: an ARIMA_PLUS model whose
// time_series_id_col names two columns, as BigQuery documents (STRING or
// ARRAY<STRING>): one series per combination, models.get's
// timeSeriesIdColumns and each series' timeSeriesIds, and ML.FORECAST,
// ML.ARIMA_EVALUATE and ML.DETECT_ANOMALIES of new data answering both ID
// columns first.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLSeveralSeriesIDs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	gen := func(from, to int) string {
		return fmt.Sprintf("SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, store, dept, "+
			"100 * dept + IF(store = 'a', 0, 50) + MOD(i, 7) + 0.1 * MOD(i * 3, 5) AS y "+
			"FROM UNNEST(GENERATE_ARRAY(%d, %d)) AS i, UNNEST(['a', 'b']) AS store, UNNEST([1, 2]) AS dept", from, to)
	}
	name := mlDataset(t, h, c, map[string]string{"train": gen(0, 41), "later": gen(42, 43)})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.m` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', "+
		"time_series_data_col='y', time_series_id_col=['store', 'dept']) AS SELECT * FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	md, err := c.Dataset(name).Model("m").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	runs := md.RawTrainingRuns()
	if len(runs) != 1 || fmt.Sprint(runs[0].TrainingOptions.TimeSeriesIdColumns) != "[store dept]" {
		t.Fatalf("training run options: %+v", runs)
	}
	var ids []string
	for _, r := range runs[0].Results {
		if r.ArimaResult == nil {
			continue
		}
		for _, info := range r.ArimaResult.ArimaModelInfo {
			ids = append(ids, fmt.Sprint(info.TimeSeriesIds))
		}
	}
	if fmt.Sprint(ids) != "[[a 1] [a 2] [b 1] [b 2]]" {
		t.Errorf("timeSeriesIds %v", ids)
	}
	fc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FORECAST(MODEL `%s.m`, STRUCT(2 AS horizon)) ORDER BY store, dept, forecast_timestamp", name))
	if len(fc) != 8 {
		t.Fatalf("ML.FORECAST: %d rows; want 8", len(fc))
	}
	for _, r := range fc {
		base := 100*float64(r["dept"].(int64)) + map[string]float64{"a": 0, "b": 50}[r["store"].(string)]
		if v, _ := r["forecast_value"].(float64); v < base-2 || v > base+9 {
			t.Errorf("forecast %v: want within [%v, %v]", r, base-2, base+9)
		}
	}
	if ev := mlQuery(t, h, c, fmt.Sprintf("SELECT store, dept FROM ML.ARIMA_EVALUATE(MODEL `%s.m`, STRUCT(FALSE AS show_all_candidate_models))", name)); len(ev) != 4 {
		t.Errorf("ML.ARIMA_EVALUATE: %v", ev)
	}
	an := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.m`, STRUCT(0.99 AS anomaly_prob_threshold), "+
		"(SELECT d, store, dept, IF(store = 'b' AND dept = 2 AND d = DATE '2024-02-12', y + 100, y) AS y FROM `%s.later`)) ORDER BY store, dept, d", name, name))
	if len(an) != 8 {
		t.Fatalf("ML.DETECT_ANOMALIES: %d rows; want 8", len(an))
	}
	for k, r := range an {
		if (r["is_anomaly"] == true) != (k == 6) {
			t.Errorf("ML.DETECT_ANOMALIES row %d: %v", k, r)
		}
	}
}

// TestBigQueryMLSeqSplit: data_split_method SEQ, as BigQuery documents
// it: the rows sorted by data_split_col, the last data_split_eval_fraction
// of them are evaluation data, the rest (NULLs among them) training data,
// and the split column is not a feature. Rows k < 80 follow y = 2x and
// the last 20 (by k) y = 2x + 100, so a LINEAR_REG trained on the first
// fits 2x, and its evaluation on the last has a mean absolute error of
// about 100; a split column of type INT64 is 400.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLSeqSplit(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{"t": "SELECT CAST(k AS NUMERIC) AS k, CAST(MOD(k * 37, 23) AS FLOAT64) AS x, " +
		"2 * MOD(k * 37, 23) + IF(k >= 80, 100, 0) AS y FROM UNNEST(GENERATE_ARRAY(0, 99)) AS k"})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lr` OPTIONS(model_type='LINEAR_REG', input_label_cols=['y'], data_split_method='SEQ', "+
		"data_split_col='k', data_split_eval_fraction=0.2) AS SELECT * FROM `%s.t`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	md, err := c.Dataset(name).Model("lr").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.DataSplitMethod != "SEQ" || runs[0].TrainingOptions.DataSplitColumn != "k" {
		t.Errorf("training options: %+v", runs)
	}
	if fc, _ := md.RawFeatureColumns(); len(fc) != 1 || fc[0].Name != "x" {
		t.Errorf("feature columns %+v; want x alone", fc)
	}
	w := mlQuery(t, h, c, fmt.Sprintf("SELECT processed_input, weight FROM ML.WEIGHTS(MODEL `%s.lr`) WHERE processed_input = 'x'", name))
	if len(w) != 1 || math.Abs(w[0]["weight"].(float64)-2) > 0.01 {
		t.Errorf("ML.WEIGHTS: %v; want x's about 2", w)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.lr`)", name))
	if len(ev) != 1 || math.Abs(ev[0]["mean_absolute_error"].(float64)-100) > 1 {
		t.Errorf("ML.EVALUATE: %v; want a mean absolute error of about 100", ev)
	}
	err = mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='LINEAR_REG', input_label_cols=['y'], data_split_method='SEQ', "+
		"data_split_col='k') AS SELECT CAST(k AS INT64) AS k, x, y FROM `%s.t`", name, name))
	wantHTTPStatus(t, "SEQ over an INT64 column", err, 400)
}
