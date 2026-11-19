package bigqueryfront

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestXregDetectHistory: ML.DETECT_ANOMALIES of an ARIMA_PLUS_XREG
// model's training data finds the one spike the regressors do not
// explain, and reports the data as given (residual plus regression), not
// the residual.
func TestXregDetectHistory(t *testing.T) {
	cols := []mlColumn{{"d", "DATE"}, {"y", "FLOAT64"}, {"x", "FLOAT64"}}
	var rows [][]*string
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for k := 0; k < 60; k++ {
		x := float64((k*37)%11) - 5
		y := 50 + 3*x + 0.1*float64(k%2)
		if k == 30 {
			y += 40
		}
		d, ys, xs := start.AddDate(0, 0, k).Format("2006-01-02"), strconv.FormatFloat(y, 'g', -1, 64), strconv.FormatFloat(x, 'g', -1, 64)
		rows = append(rows, []*string{&d, &ys, &xs})
	}
	o := defaultMLOptions()
	o.ModelType = "ARIMA_PLUS_XREG"
	o.TS.TimestampCol, o.TS.DataCol = "d", "y"
	applyArimaDefaults(&o)
	tr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	md := bqModel{Type: "ARIMA_PLUS_XREG", Options: o, Trained: tr}
	sql, code, msg := front{}.xregDetect(nil, queryOptions{}, md, mlCall{fn: "DETECT_ANOMALIES", struc: map[string]string{"anomaly_prob_threshold": "0.9999"}})
	if code != 0 {
		t.Fatalf("%d %s", code, msg)
	}
	var anomalies []string
	for _, r := range strings.Split(sql, "), (") {
		if strings.Contains(r, ", true, ") {
			anomalies = append(anomalies, r)
		}
	}
	if len(anomalies) != 1 || !strings.Contains(anomalies[0], "2024-01-31") {
		t.Errorf("anomalies %q; want 2024-01-31 alone", anomalies)
	}
	// The first point's data is y as given.
	if !strings.Contains(sql, "TIMESTAMP '2024-01-01 00:00:00+00', "+sqlF(50+3*(-5))) {
		t.Errorf("history data not y: %.300s", sql)
	}
	o.TS.Decompose = false
	md.Options = o
	if _, code, _ := (front{}).xregDetect(nil, queryOptions{}, md, mlCall{fn: "DETECT_ANOMALIES"}); code != 400 {
		t.Errorf("without decomposition: %d; want 400", code)
	}
	if _, code, _ := (front{}).xregDetect(nil, queryOptions{}, md, mlCall{fn: "DETECT_ANOMALIES", struc: map[string]string{"anomaly_prob_threshold": "1"}}); code != 400 {
		t.Errorf("threshold 1: %d; want 400", code)
	}
}

// TestSeqSplit: data_split_method SEQ's last rows by the split column are
// evaluation data, ties with the threshold too, and NULLs training data.
func TestSeqSplit(t *testing.T) {
	s := func(v string) *string { return &v }
	vals := []*string{s("3"), nil, s("10"), s("2"), s("9.5"), s("10"), s("1"), s("4"), s("5"), s("6")}
	var rows [][]*string
	for _, v := range vals {
		rows = append(rows, []*string{v})
	}
	got, fe := seqSplit(mlColumn{"k", "NUMERIC"}, rows, 0, 0.2)
	if fe != nil {
		t.Fatal(fe)
	}
	for i, v := range vals {
		want := v != nil && (*v == "10")
		if got[i] != want {
			t.Errorf("row %d (%v): eval %v; want %v", i, v, got[i], want)
		}
	}
	got, _ = seqSplit(mlColumn{"k", "NUMERIC"}, rows, 0, 0.3)
	n := 0
	for _, e := range got {
		if e {
			n++
		}
	}
	if n != 3 || !got[4] {
		t.Errorf("0.3: %v", got)
	}
	// STRING sorts by bytes: "9" is after "10".
	got, _ = seqSplit(mlColumn{"k", "STRING"}, rows, 0, 0.1)
	if !got[4] || got[2] {
		t.Errorf("STRING: %v", got)
	}
	if _, fe := seqSplit(mlColumn{"k", "INT64"}, rows, 0, 0.2); fe == nil || fe.code != 400 {
		t.Errorf("INT64: %v", fe)
	}
	if got, _ := seqSplit(mlColumn{"k", "NUMERIC"}, rows, 0, 0); got[2] {
		t.Errorf("fraction 0: %v", got)
	}
}
