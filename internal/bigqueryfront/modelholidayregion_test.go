package bigqueryfront

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHolidayRegionOption(t *testing.T) {
	const opts = "CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS', time_series_timestamp_col='d', time_series_data_col='v', "
	for sql, want := range map[string]int{
		opts + "holiday_region='US') AS SELECT d, v FROM t":               0,
		opts + "holiday_region=['US', 'GB', 'US']) AS SELECT d, v FROM t": 0,
		opts + "holiday_region='GLOBAL') AS SELECT d, v FROM t":           0,
		opts + "holiday_region='us') AS SELECT d, v FROM t":               400,
		opts + "holiday_region='XX') AS SELECT d, v FROM t":               400,
		opts + "holiday_region=[]) AS SELECT d, v FROM t":                 400,
		opts + "holiday_region=3) AS SELECT d, v FROM t":                  400,
		"CREATE MODEL d.m OPTIONS(model_type='ARIMA_PLUS_XREG', time_series_timestamp_col='d', time_series_data_col='v', holiday_region='JP') AS SELECT d, v, x FROM t": 0,
		"CREATE MODEL d.m OPTIONS(model_type='LINEAR_REG', holiday_region='US') AS SELECT 1 AS label":                                                                   400,
	} {
		toks, _ := lex(sql)
		d, is, code, msg := parseModelDDL(toks, sql)
		if !is || code != want {
			t.Errorf("%s: %d %s; want %d", sql, code, msg, want)
			continue
		}
		if strings.Contains(sql, "'GB'") && strings.Join(d.opts.TS.Regions, ",") != "US,GB" {
			t.Errorf("regions %v", d.opts.TS.Regions)
		}
	}
}

func TestHolidayCalendar(t *testing.T) {
	cal := holidayCalendar()
	for r := range holidayRegions {
		if len(cal[r]) == 0 {
			t.Errorf("region %s has no holidays", r)
		}
	}
	has := func(r, name, date string) bool {
		for _, h := range cal[r] {
			if h.Name == name && h.Date == date {
				return true
			}
		}
		return false
	}
	for _, c := range [][3]string{{"US", "ChristmasDay", "2025-12-25"}, {"US", "IndependenceDay", "2024-07-04"}, {"GB", "ChristmasDay", "2025-12-25"},
		{"GLOBAL", "NewYearsDay", "2026-01-01"}, {"US", "ThanksgivingDay", "2025-11-27"}} {
		if !has(c[0], c[1], c[2]) {
			t.Errorf("no %v", c)
		}
	}
	for r, hs := range cal {
		for _, h := range hs {
			if !holidayColumnName.MatchString(h.Name) {
				t.Fatalf("%s: holiday name %q is not a column name", r, h.Name)
			}
		}
	}
}

// TestModelHolidaysFor: the union of the regions' holidays in range, a
// custom holiday of a given region replacing its built-in one, and no
// occurrence overlapping another of its holiday.
func TestModelHolidaysFor(t *testing.T) {
	first := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	last := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC).UnixMicro()
	custom := []customHoliday{{Region: "US", Name: "ChristmasDay", Date: "2024-12-25", Pre: 3, Post: 2}, {Region: "XX", Name: "Fest", Date: "2024-06-01", Pre: 1, Post: 1}}
	hs := modelHolidaysFor([]string{"US", "GB"}, custom, first, last)
	var usXmas, gbXmas, fest int
	for _, h := range hs {
		if h.Date < "2024-01-02" || h.Date > "2025-12-31" {
			t.Errorf("out of range %+v", h)
		}
		switch {
		case h.Name == "ChristmasDay" && h.Region == "US":
			usXmas++
			if h.Pre != 3 || h.Post != 2 {
				t.Errorf("US ChristmasDay %+v; want the custom window", h)
			}
		case h.Name == "ChristmasDay" && h.Region == "GB":
			gbXmas++
			if h.Pre != 3 || h.Post != 2 {
				t.Errorf("GB ChristmasDay %+v; want the holiday's window", h)
			}
		case h.Name == "Fest":
			fest++
		}
	}
	// US: the custom 2024 only (its built-in is replaced); GB's 2024
	// overlaps it and is dropped, its 2025 kept.
	if usXmas != 1 || gbXmas != 1 || fest != 1 {
		t.Errorf("US ChristmasDay %d, GB %d, Fest %d", usXmas, gbXmas, fest)
	}
	if _, fe := parseCustomHolidays([]mlColumn{{"region", "STRING"}, {"holiday_name", "STRING"}, {"primary_date", "DATE"}, {"preholiday_days", "INT64"}, {"postholiday_days", "INT64"}},
		holidayInfoRows(hs)); fe != nil {
		t.Errorf("the model's holidays break the documented rules: %v", fe)
	}
}

func holidayInfoRows(hs []customHoliday) [][]*string {
	var rows [][]string
	for _, h := range hs {
		rows = append(rows, []string{h.Region, h.Name, h.Date, strconv.Itoa(h.Pre), strconv.Itoa(h.Post)})
	}
	return holidayRows(rows...)
}

// TestTrainArimaHolidayRegion: two years of a daily series with +25 on US
// Independence Day; holiday_region US finds it, and its third occurrence
// (in the horizon) is in the forecast. A series of under a year models no
// built-in holiday.
func TestTrainArimaHolidayRegion(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var vals []float64
	for i := 0; i < 600; i++ {
		v := 50 + 0.01*float64(i) + 0.7*math.Sin(float64(i)*1.3) + weekly[i%7]
		if d := start.AddDate(0, 0, i); d.Month() == 7 && d.Day() == 4 {
			v += 25
		}
		vals = append(vals, v)
	}
	cols, rows := arimaRows(vals)
	o := arimaOpts("holiday_region")
	o.TS.Regions = []string{"US"}
	tr, fe := trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	s := tr.Arima.Series[0]
	var july *arimaHoliday
	for i := range s.Holidays {
		if s.Holidays[i].Name == "IndependenceDay" {
			july = &s.Holidays[i]
		}
	}
	if july == nil || !july.found() || math.Abs(july.Effect[1]-25) > 3 {
		t.Fatalf("IndependenceDay %+v", july)
	}
	k := int(time.Date(2024, 7, 4, 0, 0, 0, 0, time.UTC).Sub(start).Hours() / 24)
	if s.holidayAt(k) < 20 {
		t.Errorf("holidayAt(2024-07-04) %v", s.holidayAt(k))
	}
	var in2025 bool
	for _, h := range tr.Arima.Holidays {
		if h.Name == "IndependenceDay" && h.Date == "2026-07-04" && h.Region == "US" {
			in2025 = true
		}
	}
	if !in2025 {
		t.Error("ML.HOLIDAY_INFO lacks 2026-07-04, in the horizon")
	}
	// Under a year: no built-in holidays.
	cols, rows = arimaRows(vals[:300])
	tr, fe = trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	if len(tr.Arima.Series[0].Holidays) != 0 {
		t.Errorf("a 300-day series modelled %d holidays", len(tr.Arima.Series[0].Holidays))
	}
}

// TestHolidayCalendarDigest: the embedded calendar is the one
// third_party/python-holidays/PROVENANCE.md records.
func TestHolidayCalendarDigest(t *testing.T) {
	zr, err := gzip.NewReader(bytes.NewReader(holidayCalendarGz))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != "76dffb88d42ff68d6293f3712cbd0a0a36b80713a9562412d1fd996e3e2e9a57" {
		t.Errorf("holidaycal.tsv.gz decompresses to %s; regenerate it as PROVENANCE.md says, and update both", got)
	}
}
