//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLCustomHolidays (#1776): ARIMA_PLUS and ARIMA_PLUS_XREG
// models with custom holidays, CREATE MODEL … AS (training_data AS (…),
// custom_holiday AS (…)), through the official cloud.google.com/go/bigquery
// client, of a daily series with a weekly pattern and a holiday "Fest"
// that adds 20 on its day and 8 the day after, three times in the
// training and once in the forecast, and a holiday "Quiet" that does
// nothing: ML.HOLIDAY_INFO answers the documented columns, the holidays
// as given; ML.EXPLAIN_FORECAST answers holiday_effect and one
// holiday_effect_<holiday_name> per holiday after residual (before an
// XREG model's attribution columns), Fest's about 20 on its days in the
// history and the forecast, Quiet's NULL, and time_series_adjusted_data
// is trend + seasonal + holiday_effect; ML.FORECAST carries the effect on
// the forecast's Fest; ML.EVALUATE of the later data errs less than a
// model without the holidays; ML.ARIMA_EVALUATE's has_holiday_effect and
// the model's hasHolidayEffect are TRUE. A data_frequency other than DAILY
// or AUTO_FREQUENCY, a TRANSFORM, a window over 30 days or one that
// differs between a holiday's rows is 400; holiday_region with custom
// holidays stays 501.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLCustomHolidays(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	const fest = "(i IN (40, 110, 180, 210))"
	gen := func(from, to int) string {
		return fmt.Sprintf("SELECT DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d, 50 + 0.05 * i + 0.7 * SIN(i * 1.3) + "+
			"[3, -1, -2, 0, 1, -4, 3][OFFSET(MOD(i, 7))] + IF(%s, 20, 0) + IF((i - 1) IN (40, 110, 180, 210), 8, 0) AS y, "+
			"CAST(MOD(i * 37, 11) AS FLOAT64) AS temp FROM UNNEST(GENERATE_ARRAY(%d, %d)) AS i", fest, from, to)
	}
	name := mlDataset(t, h, c, map[string]string{"train": gen(0, 199), "future": gen(200, 219),
		"holidays": "SELECT 'XX' AS region, 'Fest' AS holiday_name, DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS primary_date, " +
			"1 AS preholiday_days, 1 AS postholiday_days FROM UNNEST([40, 110, 180, 210]) AS i UNION ALL " +
			"SELECT 'XX', 'Quiet', DATE_ADD(DATE '2024-01-01', INTERVAL i DAY), 2, 1 FROM UNNEST([60, 130, 205]) AS i"})
	date := func(i int) string {
		return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
	}
	create := func(model, opts, train string) error {
		return mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(time_series_timestamp_col='d', time_series_data_col='y', %s) AS ("+
			"training_data AS (%s), custom_holiday AS (SELECT * FROM `%s.holidays`))", name, model, opts, train, name))
	}
	if err := create("hol", "model_type='ARIMA_PLUS', horizon=30", "SELECT d, y FROM `"+name+".train`"); err != nil {
		t.Fatalf("CREATE MODEL with custom_holiday: %v", err)
	}

	info := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.HOLIDAY_INFO(MODEL `%s.hol`) ORDER BY holiday_name, primary_date", name))
	if len(info) != 7 {
		t.Fatalf("ML.HOLIDAY_INFO: %d rows; want 7", len(info))
	}
	if r := info[1]; r["region"] != "XX" || r["holiday_name"] != "Fest" || fmt.Sprint(r["primary_date"]) != date(110) ||
		r["preholiday_days"] != int64(1) || r["postholiday_days"] != int64(1) {
		t.Errorf("ML.HOLIDAY_INFO row %v", r)
	}
	if r := info[4]; r["holiday_name"] != "Quiet" || r["preholiday_days"] != int64(2) {
		t.Errorf("ML.HOLIDAY_INFO row %v", r)
	}

	it, err := c.Query(fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.hol`, STRUCT(20 AS horizon)) ORDER BY time_series_timestamp", name)).Read(ctx)
	if err != nil {
		t.Fatalf("ML.EXPLAIN_FORECAST: %v", err)
	}
	var ex []map[string]bigquery.Value
	for {
		row := map[string]bigquery.Value{}
		if it.Next(&row) != nil {
			break
		}
		ex = append(ex, row)
	}
	var cols []string
	for _, f := range it.Schema {
		cols = append(cols, f.Name)
	}
	if n := len(cols); n < 3 || cols[n-3] != "residual" || cols[n-2] != "holiday_effect_Fest" || cols[n-1] != "holiday_effect_Quiet" {
		t.Errorf("ML.EXPLAIN_FORECAST columns %v", cols)
	}
	if len(ex) != 220 {
		t.Fatalf("ML.EXPLAIN_FORECAST: %d rows; want 200 + 20", len(ex))
	}
	for _, k := range []int{110, 180, 210} {
		r := ex[k]
		if !near(r["holiday_effect_Fest"], 20, 3) || !near(r["holiday_effect"], 20, 3) || r["holiday_effect_Quiet"] != nil {
			t.Errorf("day %d (%v): holiday_effect %v, Fest %v, Quiet %v; want about 20, 20, NULL", k, r["time_series_type"],
				r["holiday_effect"], r["holiday_effect_Fest"], r["holiday_effect_Quiet"])
		}
	}
	if r := ex[100]; r["holiday_effect"] != 0.0 || r["holiday_effect_Fest"] != 0.0 {
		t.Errorf("day 100: holiday_effect %v, Fest %v; want 0", r["holiday_effect"], r["holiday_effect_Fest"])
	}
	for _, k := range []int{110, 150} {
		r := ex[k]
		sum := r["trend"].(float64) + r["seasonal_period_weekly"].(float64) + r["holiday_effect"].(float64)
		if !near(r["time_series_adjusted_data"], sum, 1e-6) {
			t.Errorf("day %d: time_series_adjusted_data %v; want trend + seasonal + holiday_effect = %v", k, r["time_series_adjusted_data"], sum)
		}
	}

	fc := mlQuery(t, h, c, fmt.Sprintf("SELECT forecast_value FROM ML.FORECAST(MODEL `%s.hol`, STRUCT(20 AS horizon)) ORDER BY forecast_timestamp", name))
	if len(fc) != 20 || fc[10]["forecast_value"].(float64)-fc[9]["forecast_value"].(float64) < 10 {
		t.Errorf("ML.FORECAST: day 210 %v, day 209 %v; want the holiday's effect", fc[10], fc[9])
	}

	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.plain` OPTIONS(model_type='ARIMA_PLUS', horizon=30, time_series_timestamp_col='d', "+
		"time_series_data_col='y') AS SELECT d, y FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL without holidays: %v", err)
	}
	mae := func(model string) float64 {
		ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.%s`, (SELECT d, y FROM `%s.future`))", name, model, name))
		if len(ev) != 1 {
			t.Fatalf("ML.EVALUATE %s: %d rows", model, len(ev))
		}
		v, _ := ev[0]["mean_absolute_error"].(float64)
		return v
	}
	if with, without := mae("hol"), mae("plain"); with >= without {
		t.Errorf("ML.EVALUATE mean_absolute_error with the holidays %v, without %v", with, without)
	}

	ae := mlQuery(t, h, c, fmt.Sprintf("SELECT has_holiday_effect FROM ML.ARIMA_EVALUATE(MODEL `%s.hol`, STRUCT(FALSE AS show_all_candidate_models))", name))
	if len(ae) != 1 || ae[0]["has_holiday_effect"] != true {
		t.Errorf("ML.ARIMA_EVALUATE has_holiday_effect %v", ae)
	}
	md, err := c.Dataset(name).Model("hol").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || len(runs[0].Results) != 1 || runs[0].Results[0].ArimaResult == nil ||
		len(runs[0].Results[0].ArimaResult.ArimaModelInfo) != 1 || !runs[0].Results[0].ArimaResult.ArimaModelInfo[0].HasHolidayEffect {
		t.Errorf("models.get: hasHolidayEffect not TRUE")
	}

	// ARIMA_PLUS_XREG with the same holidays: holiday_effect_<name>
	// before attribution_<feature>.
	if err := create("xr", "model_type='ARIMA_PLUS_XREG'", "SELECT d, y, temp FROM `"+name+".train`"); err != nil {
		t.Fatalf("CREATE MODEL ARIMA_PLUS_XREG with custom_holiday: %v", err)
	}
	if n := len(mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.HOLIDAY_INFO(MODEL `%s.xr`)", name))); n != 7 {
		t.Errorf("XREG ML.HOLIDAY_INFO: %d rows; want 7", n)
	}
	it, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.xr`, STRUCT(20 AS horizon), (SELECT d, temp FROM `%s.future`)) "+
		"ORDER BY time_series_timestamp", name, name)).Read(ctx)
	if err != nil {
		t.Fatalf("XREG ML.EXPLAIN_FORECAST: %v", err)
	}
	ex = nil
	for {
		row := map[string]bigquery.Value{}
		if it.Next(&row) != nil {
			break
		}
		ex = append(ex, row)
	}
	cols = nil
	for _, f := range it.Schema {
		cols = append(cols, f.Name)
	}
	if n := len(cols); n < 4 || cols[n-4] != "residual" || cols[n-3] != "holiday_effect_Fest" || cols[n-2] != "holiday_effect_Quiet" || cols[n-1] != "attribution_temp" {
		t.Errorf("XREG ML.EXPLAIN_FORECAST columns %v", cols)
	}
	if len(ex) != 220 || !near(ex[110]["holiday_effect_Fest"], 20, 3) || !near(ex[210]["holiday_effect_Fest"], 20, 3) {
		t.Errorf("XREG ML.EXPLAIN_FORECAST: %d rows, Fest on day 110 %v, on day 210 %v", len(ex), ex[110]["holiday_effect_Fest"], ex[210]["holiday_effect_Fest"])
	}

	// The documented refusals.
	for what, tc := range map[string]struct {
		sql  string
		code int
	}{
		"data_frequency WEEKLY": {fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='y', "+
			"data_frequency='WEEKLY') AS (training_data AS (SELECT d, y FROM `%[1]s.train`), custom_holiday AS (SELECT * FROM `%[1]s.holidays`))", name), http.StatusBadRequest},
		"TRANSFORM": {fmt.Sprintf("CREATE MODEL `%s.no` TRANSFORM(d, y) OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='y') "+
			"AS (training_data AS (SELECT d, y FROM `%[1]s.train`), custom_holiday AS (SELECT * FROM `%[1]s.holidays`))", name), http.StatusBadRequest},
		"window over 30": {fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='y') "+
			"AS (training_data AS (SELECT d, y FROM `%[1]s.train`), custom_holiday AS (SELECT 'XX' AS region, 'Big' AS holiday_name, DATE '2024-03-01' AS primary_date, "+
			"31 AS preholiday_days, 1 AS postholiday_days))", name), http.StatusBadRequest},
		"window differs": {fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='y') "+
			"AS (training_data AS (SELECT d, y FROM `%[1]s.train`), custom_holiday AS (SELECT 'XX' AS region, 'Big' AS holiday_name, DATE '2024-03-01' AS primary_date, "+
			"1 AS preholiday_days, 1 AS postholiday_days UNION ALL SELECT 'XX', 'Big', DATE '2024-06-01', 2, 1))", name), http.StatusBadRequest},
		"holiday_region": {fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='y', "+
			"holiday_region='US') AS (training_data AS (SELECT d, y FROM `%[1]s.train`), custom_holiday AS (SELECT * FROM `%[1]s.holidays`))", name), http.StatusNotImplemented},
	} {
		_, err := c.Query(tc.sql).Read(ctx)
		wantHTTPStatus(t, what, err, tc.code)
	}
}
