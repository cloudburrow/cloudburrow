package bigqueryfront

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestHolidayAsClause(t *testing.T) {
	const opts = "OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v'"
	hol := "custom_holiday AS (SELECT 'US' AS region, 'Fest' AS holiday_name, DATE '2024-02-01' AS primary_date)"
	for sql, want := range map[string]int{
		"CREATE MODEL d.m " + opts + ") AS (training_data AS (SELECT d, v FROM t), " + hol + ")":                          0,
		"CREATE MODEL d.m " + opts + ", data_frequency='DAILY') AS (training_data AS (SELECT d, v FROM t), " + hol + ")":  0,
		"CREATE MODEL d.m " + opts + ", data_frequency='WEEKLY') AS (training_data AS (SELECT d, v FROM t), " + hol + ")": 400,
		"CREATE MODEL d.m " + opts + ", holiday_region='US') AS (training_data AS (SELECT d, v FROM t), " + hol + ")":     501,
		"CREATE MODEL d.m " + opts + ") AS (training_data AS (SELECT d, v FROM t))":                                       400,
		"CREATE MODEL d.m " + opts + ") AS (training_data AS (SELECT d, v FROM t), " + hol + ") x":                        400,
		"CREATE MODEL d.m OPTIONS(model_type='LINEAR_REG') AS (training_data AS (SELECT 1 AS label), " + hol + ")":        400,
		"CREATE MODEL d.m TRANSFORM(d, v) " + opts + ") AS (training_data AS (SELECT d, v FROM t), " + hol + ")":          400,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS_XREG', time_series_timestamp_col='d', time_series_data_col='v') AS (" +
			"training_data AS (SELECT d, v, x FROM t), " + hol + ")": 0,
		"CREATE MODEL d.m " + opts + ") AS (SELECT d, v FROM t)": 0,
	} {
		toks, _ := lex(sql)
		d, is, code, msg := parseModelDDL(toks, sql)
		if !is || code != want {
			t.Errorf("%s: %d %s; want %d", sql, code, msg, want)
			continue
		}
		if code == 0 && strings.Contains(sql, "training_data") && (!strings.HasPrefix(d.query, "SELECT d, v") || !strings.HasPrefix(d.holidayQuery, "SELECT 'US'")) {
			t.Errorf("%s: query %q holidays %q", sql, d.query, d.holidayQuery)
		}
	}
}

func holidayRows(rows ...[]string) [][]*string {
	var out [][]*string
	for _, r := range rows {
		var row []*string
		for _, v := range r {
			if v == "NULL" {
				row = append(row, nil)
				continue
			}
			v := v
			row = append(row, &v)
		}
		out = append(out, row)
	}
	return out
}

func TestParseCustomHolidays(t *testing.T) {
	cols := []mlColumn{{"region", "STRING"}, {"holiday_name", "STRING"}, {"primary_date", "DATE"}, {"preholiday_days", "INTEGER"}, {"postholiday_days", "INTEGER"}}
	hs, fe := parseCustomHolidays(cols, holidayRows([]string{"US", "B", "2024-03-01", "2", "NULL"}, []string{"US", "A", "2024-02-01", "1", "1"},
		[]string{"US", "B", "2025-03-01", "2", "1"}))
	if fe != nil {
		t.Fatal(fe)
	}
	if len(hs) != 3 || hs[0].Name != "A" || hs[1].Date != "2024-03-01" || hs[1].Pre != 2 || hs[1].Post != 1 {
		t.Fatalf("%+v", hs)
	}
	for name, rows := range map[string][][]*string{
		"pre 0":          holidayRows([]string{"US", "A", "2024-02-01", "0", "1"}),
		"post 31":        holidayRows([]string{"US", "A", "2024-02-01", "1", "31"}),
		"window differs": holidayRows([]string{"US", "A", "2024-02-01", "1", "1"}, []string{"US", "A", "2025-02-01", "2", "1"}),
		"bad name":       holidayRows([]string{"US", "New Year", "2024-02-01", "1", "1"}),
		"null date":      holidayRows([]string{"US", "A", "NULL", "1", "1"}),
		"self overlap":   holidayRows([]string{"US", "A", "2024-02-01", "1", "1"}, []string{"US", "A", "2024-02-03", "1", "1"}),
		"full overlap":   holidayRows([]string{"US", "A", "2024-02-01", "1", "1"}, []string{"GB", "B", "2024-02-01", "1", "1"}),
		"null region":    holidayRows([]string{"NULL", "A", "2024-02-01", "1", "1"}),
	} {
		if _, fe := parseCustomHolidays(cols, rows); fe == nil || fe.code != 400 {
			t.Errorf("%s: %v; want 400", name, fe)
		}
	}
	if _, fe := parseCustomHolidays(cols, holidayRows([]string{"US", "A", "2024-02-01", "1", "1"}, []string{"US", "B", "2024-02-02", "1", "1"})); fe != nil {
		t.Errorf("partial overlap: %v", fe)
	}
	if _, fe := parseCustomHolidays(cols[:2], nil); fe == nil {
		t.Error("no primary_date column accepted")
	}
	if _, fe := parseCustomHolidays([]mlColumn{{"region", "STRING"}, {"holiday_name", "STRING"}, {"primary_date", "STRING"}}, nil); fe == nil {
		t.Error("STRING primary_date accepted")
	}
}

// TestTrainArimaCustomHoliday: a daily series with a +20 effect on a
// holiday's day and +8 on the day after, three times; the effect is found
// on those days, not elsewhere, is not cleaned as a spike, and is in the
// forecast on the holiday's fourth occurrence.
func TestTrainArimaCustomHoliday(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var vals []float64
	dates := []int{40, 110, 180, 250}
	for i := 0; i < 230; i++ {
		v := 50 + 0.05*float64(i) + 0.7*math.Sin(float64(i)*1.3) + weekly[i%7]
		for _, d := range dates {
			switch i - d {
			case 0:
				v += 20
			case 1:
				v += 8
			}
		}
		vals = append(vals, v)
	}
	cols, rows := arimaRows(vals)
	o := arimaOpts()
	for _, d := range dates {
		o.TS.Holidays = append(o.TS.Holidays, customHoliday{Region: "XX", Name: "Fest", Date: start.AddDate(0, 0, d).Format("2006-01-02"), Pre: 1, Post: 1})
	}
	tr, fe := trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	if len(s.Holidays) != 1 || !s.hasHoliday() {
		t.Fatalf("holidays %+v", s.Holidays)
	}
	h := s.Holidays[0]
	if math.Abs(h.Effect[1]-20) > 2 || math.Abs(h.Effect[2]-8) > 2 || h.Effect[0] != 0 {
		t.Errorf("effect %v; want about [0 20 8]", h.Effect)
	}
	if nonZero(s.Spikes) {
		for k, v := range s.Spikes {
			if v != 0 && h.at(k) != 0 {
				t.Errorf("holiday day %d cleaned as a spike %v", k, v)
			}
		}
	}
	if got := s.holidayAt(110); math.Abs(got-h.Effect[1]) > 1e-9 {
		t.Errorf("holidayAt(110) %v", got)
	}
	if s.holidayAt(100) != 0 {
		t.Errorf("holidayAt(100) %v", s.holidayAt(100))
	}
	// The forecast of day 250 (h = 21) carries the effect; day 245 not.
	fc, _, _, _ := s.arimaForecast(o.TS, tr.Arima.Cond, 30, 1.96)
	n := len(s.Data)
	if d := fc[250-n] - fc[249-n]; d < 10 {
		t.Errorf("forecast at the holiday %v, the day before %v", fc[250-n], fc[249-n])
	}
	// A series not daily is refused.
	o2 := arimaOpts()
	o2.TS.Holidays = o.TS.Holidays
	var wk []float64
	for i := 0; i < 40; i++ {
		wk = append(wk, float64(i))
	}
	wcols, wrows := arimaRows(wk)
	for i, r := range wrows {
		d := start.AddDate(0, 0, 7*i).Format("2006-01-02")
		r[0] = &d
	}
	if _, fe := trainModelT(o2, wcols, wrows); fe == nil || fe.code != 400 {
		t.Errorf("weekly series: %v; want 400", fe)
	}
}

func TestCombineHolidays(t *testing.T) {
	hs := []arimaHoliday{{Name: "A", Days: []int{5}, Effect: []float64{0, 4, 0}, Pre: 1, Post: 1}, {Name: "B", Days: []int{5}, Effect: []float64{3}},
		{Name: "C", Days: []int{5}, Effect: []float64{-2}}, {Name: "D", Days: []int{5}, Effect: []float64{-1}}}
	if got := combineHolidays(hs, 5); got != 4-2 {
		t.Errorf("combined %v; want 2 (the largest positive plus the most negative)", got)
	}
	if got := combineHolidays(hs, 7); got != 0 {
		t.Errorf("off the windows %v", got)
	}
}
