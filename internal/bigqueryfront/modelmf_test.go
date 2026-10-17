package bigqueryfront

import (
	"math"
	"strconv"
	"testing"
)

func mfRows(rating func(u, i int) float64) ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"user", "INTEGER"}, {"item", "STRING"}, {"rating", "FLOAT"}}
	var rows [][]*string
	for u := 1; u <= 4; u++ {
		for i := 1; i <= 4; i++ {
			us, is, rs := strconv.Itoa(u), "i"+strconv.Itoa(i), strconv.FormatFloat(rating(u, i), 'g', -1, 64)
			rows = append(rows, []*string{&us, &is, &rs})
		}
	}
	return cols, rows
}

func TestTrainMatrixFactorization(t *testing.T) {
	o := defaultMLOptions()
	o.ModelType = "MATRIX_FACTORIZATION"
	applyMFDefaults(&o)
	if o.L2 != 1 || o.UserCol != "user" || o.FeedbackType != "EXPLICIT" {
		t.Fatalf("defaults: %+v", o)
	}
	// Every rating the same: the weights solve to 0, each prediction the
	// mean, exactly.
	cols, rows := mfRows(func(u, i int) float64 { return 3 })
	tr, fe := trainMF(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	if tr.MF.NumFactors != 4 || len(tr.MF.Users) != 4 || tr.MF.Users[0] != "1" || tr.MF.Items[3] != "i4" {
		t.Errorf("model: %+v", tr.MF)
	}
	for u := range tr.MF.Users {
		for i := range tr.MF.Items {
			if p := tr.MF.predict(u, i); math.Abs(p-3) > 1e-12 {
				t.Errorf("predict(%d, %d) = %v; want 3", u, i, p)
			}
		}
	}
	// A rank-1 matrix plus intercepts, lightly regularized: recovered.
	o.L2, o.NumFactors, o.MaxIterations, o.EarlyStop = 1e-6, 2, 200, false
	cols, rows = mfRows(func(u, i int) float64 { return float64(u*i) + float64(u) + 0.5*float64(i) })
	tr, fe = trainMF(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	if mse := *tr.Eval["mean_squared_error"]; mse > 1e-6 {
		t.Errorf("mean_squared_error %v; want ~0", mse)
	}
	if len(tr.Iterations) != 200 || tr.Iterations[0].Index != 199 || tr.Iterations[0].Loss > 1e-6 {
		t.Errorf("iterations: %d, first %+v", len(tr.Iterations), tr.Iterations[0])
	}
	o.FeedbackType = "IMPLICIT"
	if _, fe := trainMF(o, cols, rows); fe == nil || fe.code != 501 {
		t.Errorf("implicit: %v", fe)
	}
	o.FeedbackType = "EXPLICIT"
	if _, fe := trainMF(o, cols[:2], nil); fe == nil || fe.code != 400 {
		t.Errorf("no rating column: %v", fe)
	}
}
