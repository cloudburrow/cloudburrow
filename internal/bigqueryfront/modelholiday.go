package bigqueryfront

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Custom holidays of ARIMA_PLUS and ARIMA_PLUS_XREG models (#1776,
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series#custom_holiday,
// https://docs.cloud.google.com/bigquery/docs/time-series-forecasting-holidays-tutorial):
//
//	CREATE MODEL m OPTIONS(...) AS (
//	  training_data AS (query_statement),
//	  custom_holiday AS (holiday_statement))
//
// As documented: the holiday query has the columns region, holiday_name
// and primary_date (STRING, STRING, DATE, required) and preholiday_days
// and postholiday_days (INT64, optional, default 1, at least 1); a
// holiday's window [primary_date − preholiday_days, primary_date +
// postholiday_days] is at most 30 days on each side and the same for
// every row of the holiday; at most 50,000 rows and 500 distinct
// holiday_name values, each a valid column name, as it names
// ML.EXPLAIN_FORECAST's holiday_effect_<holiday_name>; only for
// data_frequency DAILY or AUTO_FREQUENCY (the series then being daily);
// no TRANSFORM. ML.HOLIDAY_INFO answers the holidays the model used
// (region, holiday_name, primary_date, preholiday_days,
// postholiday_days); ML.EXPLAIN_FORECAST answers holiday_effect, "the sum
// of the maximum positive individual holiday effect and the minimum
// negative individual holiday effect", and one holiday_effect_<name> per
// holiday after residual (before an XREG model's attribution_*), both
// "NULL if no holiday effect is found", and time_series_adjusted_data
// includes holiday_effect; ML.ARIMA_EVALUATE's has_holiday_effect and the
// model's hasHolidayEffect say whether one was found; the effect is part
// of every forecast (ML.FORECAST, ML.EVALUATE, ML.DETECT_ANOMALIES). A
// holiday_region's built-in holidays are modelholidayregion.go's.
//
// UNVERIFIED, chosen here (docs re-read 2026-11-18: Google documents that
// the algorithm "automatically detects the significance of the holiday
// effect" and "only extracts the holiday effect on the days that the
// algorithm classifies as significant", not how):
//   - the stage runs where the documented pipeline lists it, after spikes
//     and dips and step changes and before the seasonal decomposition; the
//     days of a holiday window are not cleaned as spikes or dips;
//   - on the robustly deseasonalized, step-adjusted, spike-cleaned series,
//     each day's deviation from a baseline, the median of the days within
//     7 (widened by 7 until it has 7) that are in no holiday window; σ is
//     1.4826 MAD of the deviations of the days in no window;
//   - each holiday's effect on each day of its window (its offset from
//     primary_date) is the mean deviation on that day over the holiday's
//     occurrences in the training, kept when significant, |mean| ≥
//     3 σ / √occurrences, else 0; a holiday is found when any day is;
//   - each holiday is estimated on its own, overlaps included, and
//     holiday_effect combines them as documented; the effect of a holiday
//     on a day past the training is that of its offset, for the
//     occurrences the holiday query gives there;
//   - holiday_effect is 0, not NULL, on a day of a series that found some
//     holiday's effect but none that day, and holiday_effect_<name> is 0
//     outside that holiday's windows when it was found (the tutorial
//     filters on holiday_effect != 0); both are NULL when not found;
//   - every row of the holiday query is used whatever its region, which
//     ML.HOLIDAY_INFO answers as given, ordered by region, holiday_name
//     and primary_date; the columns holiday_effect_<name> are in
//     holiday_name order;
//   - the "longer than a year" rule, documented for HOLIDAY_REGION, is not
//     applied to custom holidays; two occurrences of one holiday whose
//     windows overlap, and two holidays with the same window days, are
//     400 (the docs say both are not allowed), with messages chosen here;
//     a NULL preholiday_days or postholiday_days is the default 1.

// customHoliday is one row of a model's holiday query.
type customHoliday struct {
	Region string `json:"region"`
	Name   string `json:"holidayName"`
	Date   string `json:"primaryDate"` // YYYY-MM-DD
	Pre    int    `json:"preholidayDays"`
	Post   int    `json:"postholidayDays"`
}

const (
	holidayMaxRows   = 50000
	holidayMaxNames  = 500
	holidayMaxWindow = 30
	holidayZ         = 3.0
	holidayBaseline  = 7
)

var holidayColumnName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,299}$`)

// parseCustomHolidays reads a holiday query's result as documented.
func parseCustomHolidays(cols []mlColumn, rows [][]*string) ([]customHoliday, *mlFail) {
	idx := map[string]int{}
	for i, c := range cols {
		idx[strings.ToLower(c.Name)] = i
	}
	want := []struct{ name, typ string }{{"region", "STRING"}, {"holiday_name", "STRING"}, {"primary_date", "DATE"},
		{"preholiday_days", "INT64"}, {"postholiday_days", "INT64"}}
	for k, w := range want {
		i, ok := idx[w.name]
		if !ok {
			if k < 3 {
				return nil, mlInvalid("The custom_holiday query must have the column %s", w.name)
			}
			continue
		}
		t := mlTypeOf(cols[i].Type)
		if t == "" {
			t = strings.ToUpper(cols[i].Type)
		}
		if t != w.typ {
			return nil, mlInvalid("The custom_holiday column %s must be of type %s, not %s", w.name, w.typ, cols[i].Type)
		}
	}
	if len(rows) > holidayMaxRows {
		return nil, mlInvalid("The custom_holiday query must return at most %d rows, not %d", holidayMaxRows, len(rows))
	}
	day := func(row []*string, name string) (int, *mlFail) {
		i, ok := idx[name]
		if !ok || row[i] == nil {
			return 1, nil
		}
		v, err := strconv.Atoi(*row[i])
		if err != nil || v < 1 {
			return 0, mlInvalid("%s must be greater than or equal to 1", name)
		}
		if v > holidayMaxWindow {
			return 0, mlInvalid("%s must be less than or equal to %d", name, holidayMaxWindow)
		}
		return v, nil
	}
	var out []customHoliday
	window := map[string][2]int{}
	for _, row := range rows {
		var h customHoliday
		for _, f := range []struct {
			name string
			dst  *string
		}{{"region", &h.Region}, {"holiday_name", &h.Name}, {"primary_date", &h.Date}} {
			v := row[idx[f.name]]
			if v == nil {
				return nil, mlInvalid("The custom_holiday column %s cannot be NULL", f.name)
			}
			*f.dst = *v
		}
		if !holidayColumnName.MatchString(h.Name) {
			return nil, mlInvalid("Invalid holiday_name %q: it must be a valid column name, as it names the column holiday_effect_%s of ML.EXPLAIN_FORECAST", h.Name, h.Name)
		}
		if _, err := time.Parse("2006-01-02", h.Date); err != nil {
			return nil, mlInvalid("Invalid primary_date %s", h.Date)
		}
		var fe *mlFail
		if h.Pre, fe = day(row, "preholiday_days"); fe != nil {
			return nil, fe
		}
		if h.Post, fe = day(row, "postholiday_days"); fe != nil {
			return nil, fe
		}
		if w, ok := window[h.Name]; ok && (w[0] != h.Pre || w[1] != h.Post) {
			return nil, mlInvalid("The holiday window of %s must be the same across the holiday: preholiday_days and postholiday_days differ between its rows", h.Name)
		}
		window[h.Name] = [2]int{h.Pre, h.Post}
		out = append(out, h)
	}
	if len(window) > holidayMaxNames {
		return nil, mlInvalid("The holiday_name column cannot have more than %d distinct values, not %d", holidayMaxNames, len(window))
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Region != y.Region {
			return x.Region < y.Region
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Date < y.Date
	})
	// The documented overlaps: one holiday's occurrences must not overlap,
	// and two holidays must not overlap fully.
	days := map[string][]int{}
	for _, h := range out {
		t, _ := time.Parse("2006-01-02", h.Date)
		d := int(t.Unix() / 86400)
		days[h.Name] = append(days[h.Name], d)
	}
	sets := map[string]string{}
	for _, name := range holidayNames(out) {
		w := window[name]
		ds := append([]int(nil), days[name]...)
		sort.Ints(ds)
		var span []string
		for k, d := range ds {
			if k > 0 && d-w[0] <= ds[k-1]+w[1] {
				return nil, mlInvalid("The occurrences of holiday %s overlap: make sure its holiday windows don't overlap", name)
			}
			span = append(span, strconv.Itoa(d-w[0])+":"+strconv.Itoa(d+w[1]))
		}
		key := strings.Join(span, ",")
		if other, ok := sets[key]; ok {
			return nil, mlInvalid("Holidays %s and %s overlap fully, which isn't allowed", other, name)
		}
		sets[key] = name
	}
	return out, nil
}

// holidayNames are the distinct holiday names of hs, sorted.
func holidayNames(hs []customHoliday) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hs {
		if !seen[h.Name] {
			seen[h.Name] = true
			out = append(out, h.Name)
		}
	}
	sort.Strings(out)
	return out
}

// arimaHoliday is one holiday of a series: its occurrences on the grid
// and its effect on each day of its window.
type arimaHoliday struct {
	Name string `json:"name"`
	Pre  int    `json:"pre"`
	Post int    `json:"post"`
	// Days are the grid indexes of its primary dates (any, before, in or
	// after the training).
	Days []int `json:"days"`
	// Effect is its effect at each offset −Pre … Post; 0 where not
	// significant.
	Effect []float64 `json:"effect"`
}

// found reports whether any day of the holiday had an effect.
func (h arimaHoliday) found() bool { return nonZero(h.Effect) }

// at is the holiday's effect at grid point k.
func (h arimaHoliday) at(k int) float64 {
	for _, d := range h.Days {
		if o := k - d; o >= -h.Pre && o <= h.Post {
			return h.Effect[o+h.Pre]
		}
	}
	return 0
}

// seriesHolidays are hs on the daily grid starting at start (Unix
// microseconds), one per holiday name, in name order, with no effect yet.
//
// On a WEEKLY series (holiday_region, modelholidayregion.go) a holiday is
// the week nearest its primary date, with no days before or after.
func seriesHolidays(hs []customHoliday, start int64, fq string) []arimaHoliday {
	var out []arimaHoliday
	at := map[string]int{}
	for _, name := range holidayNames(hs) {
		at[name] = len(out)
		out = append(out, arimaHoliday{Name: name})
	}
	for _, h := range hs {
		t, _ := time.Parse("2006-01-02", h.Date)
		ah := &out[at[h.Name]]
		ah.Pre, ah.Post = h.Pre, h.Post
		if fq == "WEEKLY" {
			ah.Pre, ah.Post = 0, 0
			ah.Days = append(ah.Days, gridIndex(start, "WEEKLY", t.UnixMicro()))
			continue
		}
		ah.Days = append(ah.Days, gridIndex(start, "DAILY", t.UnixMicro()))
	}
	for i := range out {
		sort.Ints(out[i].Days)
		out[i].Effect = make([]float64, out[i].Pre+out[i].Post+1)
	}
	return out
}

// inHolidayWindow marks the points of n that are in some holiday's window.
func inHolidayWindow(hs []arimaHoliday, n int) []bool {
	in := make([]bool, n)
	for _, h := range hs {
		for _, d := range h.Days {
			for k := max(d-h.Pre, 0); k <= min(d+h.Post, n-1); k++ {
				in[k] = true
			}
		}
	}
	return in
}

// fitHolidays sets each holiday's effect from x, the deseasonalized,
// adjusted and cleaned series, and returns x's combined holiday effect.
func fitHolidays(x []float64, hs []arimaHoliday) []float64 {
	n := len(x)
	in := inHolidayWindow(hs, n)
	r := make([]float64, n)
	var quiet []float64
	for k := range x {
		var near []float64
		for rad := holidayBaseline; len(near) < holidayBaseline && rad <= n+holidayBaseline; rad += holidayBaseline {
			near = near[:0]
			for j := max(k-rad, 0); j <= min(k+rad, n-1); j++ {
				if !in[j] {
					near = append(near, x[j])
				}
			}
		}
		r[k] = x[k] - median(near)
		if !in[k] {
			quiet = append(quiet, r[k])
		}
	}
	sigma := math.Max(robustScale(quiet), 1e3*tolerance(x))
	for i := range hs {
		h := &hs[i]
		for o := -h.Pre; o <= h.Post; o++ {
			var s float64
			m := 0
			for _, d := range h.Days {
				if k := d + o; k >= 0 && k < n {
					s += r[k]
					m++
				}
			}
			h.Effect[o+h.Pre] = 0
			if m == 0 {
				continue
			}
			if mean := s / float64(m); math.Abs(mean) >= holidayZ*sigma/math.Sqrt(float64(m)) {
				h.Effect[o+h.Pre] = mean
			}
		}
	}
	out := make([]float64, n)
	for k := range out {
		out[k] = combineHolidays(hs, k)
	}
	return out
}

// combineHolidays is the documented holiday_effect at grid point k: the
// largest positive individual effect plus the most negative.
func combineHolidays(hs []arimaHoliday, k int) float64 {
	var hi, lo float64
	for _, h := range hs {
		v := h.at(k)
		hi, lo = math.Max(hi, v), math.Min(lo, v)
	}
	return hi + lo
}

// hasHoliday reports whether the series found any holiday's effect.
func (s arimaSeries) hasHoliday() bool {
	for _, h := range s.Holidays {
		if h.found() {
			return true
		}
	}
	return false
}

// holidayAt is the series' holiday_effect at grid point k.
func (s arimaSeries) holidayAt(k int) float64 {
	if !s.hasHoliday() {
		return 0
	}
	return combineHolidays(s.Holidays, k)
}

// holidayCols are ML.EXPLAIN_FORECAST's holiday_effect column value at k
// and its holiday_effect_<name> values, one per name of the model.
func (s arimaSeries) holidayCols(names []string, k int) (string, []string) {
	total := "NULL"
	if s.hasHoliday() {
		total = sqlF(s.holidayAt(k))
	}
	var each []string
	for _, name := range names {
		v := "NULL"
		for _, h := range s.Holidays {
			if h.Name == name && h.found() {
				v = sqlF(h.at(k))
			}
		}
		each = append(each, v)
	}
	return total, each
}

// holidayColumnTypes are the holiday_effect_<name> columns of a model's
// ML.EXPLAIN_FORECAST, each with a leading ", ".
func holidayColumnTypes(names []string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString(", " + sqlIdent("holiday_effect_"+name) + " FLOAT64")
	}
	return b.String()
}

func joinLead(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return ", " + strings.Join(v, ", ")
}

// holidays are the holidays a time series model was trained with: its
// custom holidays, and with holiday_region its built-in ones (#1776).
func (md bqModel) holidays() []customHoliday {
	if md.Trained.Arima != nil && len(md.Trained.Arima.Holidays) > 0 {
		return md.Trained.Arima.Holidays
	}
	return md.Options.TS.Holidays
}

// holidayInfo is ML.HOLIDAY_INFO of a time series model: the holidays it
// was trained with.
func holidayInfo(md bqModel, c mlCall) (string, int, string) {
	if c.input != "" || len(c.struc) > 0 {
		return "", 400, "ML.HOLIDAY_INFO takes only a model"
	}
	var rows []string
	for _, h := range md.holidays() {
		rows = append(rows, fmt.Sprintf("(%s, %s, DATE %s, %d, %d)", sqlString(h.Region), sqlString(h.Name), sqlString(h.Date), h.Pre, h.Post))
	}
	return mlRowsSubquery("STRUCT<region STRING, holiday_name STRING, primary_date DATE, preholiday_days INT64, postholiday_days INT64>", rows), 0, ""
}
