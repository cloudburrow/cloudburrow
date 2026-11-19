//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLHolidayRegion (#1776): ARIMA_PLUS and ARIMA_PLUS_XREG
// models with HOLIDAY_REGION, through the official
// cloud.google.com/go/bigquery client, of 600 days (2024-01-01 on) of a
// daily series with a weekly pattern that rises 25 on each US Independence
// Day (July 4): with holiday_region 'US' ML.HOLIDAY_INFO answers US
// holidays, IndependenceDay among them, from the training through the
// horizon; ML.EXPLAIN_FORECAST's holiday_effect_IndependenceDay is about
// 25 on 2024-07-04 and 2025-07-04 and on 2026-07-04 in the forecast;
// ML.ARIMA_EVALUATE's has_holiday_effect and models.get's hasHolidayEffect
// are TRUE, and trainingOptions carries holidayRegions. With ['US', 'GB']
// the holidays of both regions are modelled (their union); a custom
// holiday of region US named IndependenceDay replaces the built-in one
// (its window is the custom one); a series of under a year is not
// holiday-modelled (has_holiday_effect FALSE), as documented; an unknown
// or lower-case region is 400. The calendar is an UNVERIFIED stand-in for
// Google's (python-holidays 0.84, internal/bigqueryfront/modelholidayregion.go).
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLHolidayRegion(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	gen := func(from, to int) string {
		return fmt.Sprintf("SELECT d, 50 + 0.01 * i + 0.7 * SIN(i * 1.3) + [3, -1, -2, 0, 1, -4, 3][OFFSET(MOD(i, 7))] + "+
			"IF(EXTRACT(MONTH FROM d) = 7 AND EXTRACT(DAY FROM d) = 4, 25, 0) AS y, CAST(MOD(i * 37, 11) AS FLOAT64) AS temp "+
			"FROM (SELECT i, DATE_ADD(DATE '2024-01-01', INTERVAL i DAY) AS d FROM UNNEST(GENERATE_ARRAY(%d, %d)) AS i)", from, to)
	}
	name := mlDataset(t, h, c, map[string]string{"train": gen(0, 599), "future": gen(600, 939)})
	create := func(model, opts, as string) error {
		return mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(time_series_timestamp_col='d', time_series_data_col='y', %s) AS %s",
			name, model, opts, as))
	}
	train := "SELECT d, y FROM `" + name + ".train`"
	if err := create("us", "model_type='ARIMA_PLUS', horizon=400, holiday_region='US'", train); err != nil {
		t.Fatalf("CREATE MODEL with holiday_region 'US': %v", err)
	}

	info := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.HOLIDAY_INFO(MODEL `%s.us`) ORDER BY region, holiday_name, primary_date", name))
	july := map[string]bool{}
	for _, r := range info {
		if r["region"] != "US" {
			t.Errorf("ML.HOLIDAY_INFO row of region %v; want US only", r["region"])
		}
		if r["holiday_name"] == "IndependenceDay" {
			july[fmt.Sprint(r["primary_date"])] = true
			if r["preholiday_days"] != int64(1) || r["postholiday_days"] != int64(1) {
				t.Errorf("ML.HOLIDAY_INFO row %v", r)
			}
		}
	}
	if !july["2024-07-04"] || !july["2025-07-04"] || !july["2026-07-04"] || len(info) < 20 {
		t.Errorf("ML.HOLIDAY_INFO: %d rows, IndependenceDay on %v", len(info), july)
	}

	it, err := c.Query(fmt.Sprintf("SELECT * FROM ML.EXPLAIN_FORECAST(MODEL `%s.us`, STRUCT(340 AS horizon)) ORDER BY time_series_timestamp", name)).Read(ctx)
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
	var hasCol bool
	for _, f := range it.Schema {
		hasCol = hasCol || f.Name == "holiday_effect_IndependenceDay"
	}
	if !hasCol || len(ex) != 940 {
		t.Fatalf("ML.EXPLAIN_FORECAST: %d rows, holiday_effect_IndependenceDay column %v", len(ex), hasCol)
	}
	for _, k := range []int{185, 550, 915} { // 2024-07-04, 2025-07-04, 2026-07-04 (forecast)
		if r := ex[k]; !near(r["holiday_effect_IndependenceDay"], 25, 4) || !near(r["holiday_effect"], 25, 4) {
			t.Errorf("day %d (%v %v): holiday_effect %v, IndependenceDay %v; want about 25", k, r["time_series_timestamp"], r["time_series_type"],
				r["holiday_effect"], r["holiday_effect_IndependenceDay"])
		}
	}
	ae := mlQuery(t, h, c, fmt.Sprintf("SELECT has_holiday_effect FROM ML.ARIMA_EVALUATE(MODEL `%s.us`, STRUCT(FALSE AS show_all_candidate_models))", name))
	if len(ae) != 1 || ae[0]["has_holiday_effect"] != true {
		t.Errorf("ML.ARIMA_EVALUATE has_holiday_effect %v", ae)
	}
	md, err := c.Dataset(name).Model("us").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	runs := md.RawTrainingRuns()
	if len(runs) != 1 || runs[0].TrainingOptions == nil || fmt.Sprint(runs[0].TrainingOptions.HolidayRegions) != "[US]" ||
		len(runs[0].Results) != 1 || runs[0].Results[0].ArimaResult == nil || len(runs[0].Results[0].ArimaResult.ArimaModelInfo) != 1 ||
		!runs[0].Results[0].ArimaResult.ArimaModelInfo[0].HasHolidayEffect {
		t.Errorf("models.get: holidayRegions or hasHolidayEffect not as trained")
	}

	// Two regions: their union.
	if err := create("usgb", "model_type='ARIMA_PLUS', holiday_region=['US', 'GB']", train); err != nil {
		t.Fatalf("CREATE MODEL with holiday_region ['US', 'GB']: %v", err)
	}
	regions := map[string]bool{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT DISTINCT region FROM ML.HOLIDAY_INFO(MODEL `%s.usgb`)", name)) {
		regions[fmt.Sprint(r["region"])] = true
	}
	if len(regions) != 2 || !regions["US"] || !regions["GB"] {
		t.Errorf("ML.HOLIDAY_INFO regions %v; want US and GB", regions)
	}

	// A custom holiday of region US replaces its built-in one.
	if err := create("custom", "model_type='ARIMA_PLUS', holiday_region='US'", fmt.Sprintf("(training_data AS (%s), custom_holiday AS ("+
		"SELECT 'US' AS region, 'IndependenceDay' AS holiday_name, DATE_ADD(DATE '2024-07-04', INTERVAL y YEAR) AS primary_date, "+
		"2 AS preholiday_days, 1 AS postholiday_days FROM UNNEST([0, 1, 2]) AS y))", train)); err != nil {
		t.Fatalf("CREATE MODEL with holiday_region and custom_holiday: %v", err)
	}
	ci := mlQuery(t, h, c, fmt.Sprintf("SELECT preholiday_days FROM ML.HOLIDAY_INFO(MODEL `%s.custom`) WHERE holiday_name = 'IndependenceDay'", name))
	if len(ci) != 3 || ci[0]["preholiday_days"] != int64(2) {
		t.Errorf("custom IndependenceDay in ML.HOLIDAY_INFO: %v; want its 3 rows, preholiday_days 2", ci)
	}

	// ARIMA_PLUS_XREG takes holiday_region too.
	if err := create("xr", "model_type='ARIMA_PLUS_XREG', holiday_region='US'", "SELECT d, y, temp FROM `"+name+".train`"); err != nil {
		t.Fatalf("CREATE MODEL ARIMA_PLUS_XREG with holiday_region: %v", err)
	}
	xe := mlQuery(t, h, c, fmt.Sprintf("SELECT holiday_effect_IndependenceDay AS v FROM ML.EXPLAIN_FORECAST(MODEL `%s.xr`, STRUCT(10 AS horizon), "+
		"(SELECT d, temp FROM `%s.future`)) WHERE time_series_timestamp = TIMESTAMP '2025-07-04'", name, name))
	if len(xe) != 1 || !near(xe[0]["v"], 25, 4) {
		t.Errorf("XREG holiday_effect_IndependenceDay on 2025-07-04: %v; want about 25", xe)
	}

	// Under a year: the holiday effect isn't modelled.
	if err := create("short", "model_type='ARIMA_PLUS', holiday_region='US'", "SELECT d, y FROM `"+name+".train` WHERE d < DATE '2024-11-01'"); err != nil {
		t.Fatalf("CREATE MODEL of a short series: %v", err)
	}
	se := mlQuery(t, h, c, fmt.Sprintf("SELECT has_holiday_effect FROM ML.ARIMA_EVALUATE(MODEL `%s.short`, STRUCT(FALSE AS show_all_candidate_models))", name))
	if len(se) != 1 || se[0]["has_holiday_effect"] != false {
		t.Errorf("short series has_holiday_effect %v; want FALSE", se)
	}

	for what, region := range map[string]string{"unknown region": "'XX'", "lower case": "'us'", "not a string": "3"} {
		err := create("no", "model_type='ARIMA_PLUS', holiday_region="+region, train)
		wantHTTPStatus(t, what, err, http.StatusBadRequest)
	}
}
