package bigqueryfront

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestArimaSeveralIDColumns: time_series_id_col of two columns trains
// one series per combination of their values (a row with a NULL in
// either left out), sorted column by column, and each ML function's rows
// start with both IDs.
func TestArimaSeveralIDColumns(t *testing.T) {
	cols := []mlColumn{{"d", "DATE"}, {"y", "FLOAT64"}, {"store", "STRING"}, {"dept", "INT64"}}
	var rows [][]*string
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, store := range []string{"b", "a"} {
		for _, dept := range []string{"10", "9"} {
			for k := 0; k < 30; k++ {
				d := start.AddDate(0, 0, k).Format("2006-01-02")
				y := strconv.Itoa(100*len(store) + k%7 + len(dept))
				st, de := store, dept
				rows = append(rows, []*string{&d, &y, &st, &de})
			}
		}
	}
	d, y, st := "2024-01-02", "1", "c"
	rows = append(rows, []*string{&d, &y, &st, nil})
	o := defaultMLOptions()
	o.ModelType = "ARIMA_PLUS"
	o.TS.TimestampCol, o.TS.DataCol = "d", "y"
	o.TS.IDCol, o.TS.IDCols = "store", []string{"store", "dept"}
	applyArimaDefaults(&o)
	tr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	a := tr.Arima
	if len(a.Series) != 4 {
		t.Fatalf("%d series; want 4", len(a.Series))
	}
	var got []string
	for _, s := range a.Series {
		got = append(got, tsIDSQL(a, s))
	}
	want := []string{"'a', 9, ", "'a', 10, ", "'b', 9, ", "'b', 10, "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("series %q; want %q", got, want)
	}
	if decl := tsIDDecl(o.TS, a); decl != "`store` STRING, `dept` INT64, " {
		t.Errorf("decl %q", decl)
	}
	md := bqModel{Type: "ARIMA_PLUS", Options: o, Trained: tr}
	sql, code, msg := front{}.arimaSubquery(nil, queryOptions{}, md, mlCall{fn: "FORECAST", struc: map[string]string{"horizon": "2"}})
	if code != 0 {
		t.Fatalf("%d %s", code, msg)
	}
	if !strings.Contains(sql, "STRUCT<`store` STRING, `dept` INT64, forecast_timestamp") || !strings.Contains(sql, "('b', 10, TIMESTAMP") {
		t.Errorf("forecast %.400s", sql)
	}
	// One column is stored as before.
	o.TS.IDCols = nil
	tr, fe = trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	if len(tr.Arima.Series) != 3 || tr.Arima.IDTypes != nil || *tr.Arima.Series[0].ID != "a" {
		t.Errorf("one column: %d series", len(tr.Arima.Series))
	}
}
