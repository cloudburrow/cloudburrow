package bigqueryfront

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// TestTrainXreg (#1766): an ARIMA_PLUS_XREG model of y = 5 + 3·x + 4·[c =
// 'b'] recovers the regression's weights, and its ARIMA is fitted to the
// residuals, which are about 0.
func TestTrainXreg(t *testing.T) {
	cols := []mlColumn{{"d", "DATE"}, {"y", "FLOAT64"}, {"x", "FLOAT64"}, {"c", "STRING"}}
	var rows [][]*string
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for k := 0; k < 40; k++ {
		x := float64((k*37)%11) - 5
		c := "a"
		y := 5 + 3*x
		if k%3 == 0 {
			c, y = "b", y+4
		}
		d, ys, xs := start.AddDate(0, 0, k).Format("2006-01-02"), strconv.FormatFloat(y, 'g', -1, 64), strconv.FormatFloat(x, 'g', -1, 64)
		cc := c
		rows = append(rows, []*string{&d, &ys, &xs, &cc})
	}
	rows[5][2] = nil // a NULL feature is the column's mean
	o := defaultMLOptions()
	o.ModelType = "ARIMA_PLUS_XREG"
	o.TS.TimestampCol, o.TS.DataCol = "d", "y"
	applyArimaDefaults(&o)
	tr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	x := tr.Xreg
	if x == nil || len(x.Features) != 2 || len(tr.Features) != 2 {
		t.Fatalf("%+v", x)
	}
	if w := x.Features[0].Weights[0]; math.Abs(w-3) > 0.3 {
		t.Errorf("x's weight %v; want about 3", w)
	}
	cw := x.Features[1].Weights
	if len(cw) != 2 || math.Abs((cw[1]-cw[0])-4) > 1 {
		t.Errorf("c's weights %v; want b 4 above a", cw)
	}
	if len(x.Hist) != 1 || len(x.Hist[0]) != 40 {
		t.Fatalf("hist %d", len(x.Hist))
	}
	// The data is the residual plus the regression.
	s := tr.Arima.Series[0]
	for _, k := range []int{0, 7, 39} {
		y, _ := strconv.ParseFloat(*rows[k][1], 64)
		if got := s.Data[k] + x.Intercept + sum(x.Hist[0][k]); math.Abs(got-y) > 1e-6 {
			t.Errorf("point %d: %v; want %v", k, got, y)
		}
	}
	vals := []*string{rows[1][2], rows[1][3]}
	if a := x.attr(vals); math.Abs(a[0]-3*func() float64 { v, _ := strconv.ParseFloat(*rows[1][2], 64); return v }()) > 0.6 {
		t.Errorf("attribution %v", a)
	}
	o2 := o
	cols2 := []mlColumn{{"d", "DATE"}, {"y", "FLOAT64"}}
	var rows2 [][]*string
	for _, r := range rows {
		rows2 = append(rows2, r[:2])
	}
	if _, fe := trainModel(o2, cols2, rows2, nil); fe == nil || fe.code != 400 {
		t.Errorf("no feature: %v", fe)
	}
}
