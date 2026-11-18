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
	o.Set = append(o.Set, "wals_alpha")
	if _, fe := trainMF(o, cols, rows); fe == nil || fe.code != 400 {
		t.Errorf("wals_alpha of an EXPLICIT model: %v", fe)
	}
	o.Set = o.Set[:len(o.Set)-1]
	if _, fe := trainMF(o, cols[:2], nil); fe == nil || fe.code != 400 {
		t.Errorf("no rating column: %v", fe)
	}
}

// TestTrainImplicitMatrixFactorization (#1766): WALS ranks each user's
// own items above the others', the loss falls, intercepts are 0, and
// wals_alpha defaults to 40.
func TestTrainImplicitMatrixFactorization(t *testing.T) {
	cols := []mlColumn{{"user", "STRING"}, {"item", "STRING"}, {"rating", "INT64"}}
	var rows [][]*string
	for _, r := range [][3]string{{"a", "x", "3"}, {"a", "y", "1"}, {"b", "x", "2"}, {"b", "y", "4"}, {"c", "z", "5"}, {"d", "z", "1"}, {"d", "w", "2"}} {
		u, i, v := r[0], r[1], r[2]
		rows = append(rows, []*string{&u, &i, &v})
	}
	o := defaultMLOptions()
	o.ModelType, o.FeedbackType, o.NumFactors = "MATRIX_FACTORIZATION", "IMPLICIT", 2
	applyMFDefaults(&o)
	if o.WalsAlpha == nil || *o.WalsAlpha != 40 {
		t.Fatalf("wals_alpha default %v", o.WalsAlpha)
	}
	tr, fe := trainMF(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	m := tr.MF
	if !m.Implicit || len(m.Users) != 4 || len(m.Items) != 4 {
		t.Fatalf("%+v", m)
	}
	idx := func(list []string, v string) int {
		for k, s := range list {
			if s == v {
				return k
			}
		}
		t.Fatalf("%s not in %v", v, list)
		return -1
	}
	score := func(u, i string) float64 { return m.predict(idx(m.Users, u), idx(m.Items, i)) }
	for _, c := range [][3]string{{"a", "x", "z"}, {"b", "y", "z"}, {"c", "z", "x"}, {"d", "w", "y"}} {
		if score(c[0], c[1]) <= score(c[0], c[2]) {
			t.Errorf("user %s: %s %.4f is not above %s %.4f", c[0], c[1], score(c[0], c[1]), c[2], score(c[0], c[2]))
		}
	}
	for _, b := range append(append([]float64(nil), m.UserB...), m.ItemB...) {
		if b != 0 {
			t.Errorf("an intercept %v", b)
		}
	}
	n := len(tr.Iterations)
	if n < 2 || tr.Iterations[0].Loss > tr.Iterations[n-1].Loss {
		t.Errorf("the loss did not fall: %+v", tr.Iterations)
	}
	for _, k := range rankingMetricNames {
		if tr.Eval[k] == nil {
			t.Errorf("metric %s missing: %v", k, tr.Eval)
		}
	}
	if v := *tr.Eval["mean_average_precision"]; v < 0.7 {
		t.Errorf("mean_average_precision %v; above chance", v)
	}
	if v := *tr.Eval["normalized_discounted_cumulative_gain"]; v <= 0.8 || v > 1+1e-9 {
		t.Errorf("ndcg %v", v)
	}
	if v := *tr.Eval["average_rank"]; v < 0 || v > 0.34 {
		t.Errorf("average_rank %v", v)
	}
}

// TestRankingMetrics (#1766): hand-computed ranking metrics of a fixed
// model: one user, three items scored 3, 2, 1; the relevant items are the
// first (rating 1) and the third (rating 2).
func TestRankingMetrics(t *testing.T) {
	m := &mfTrained{Users: []string{"u"}, Items: []string{"a", "b", "c"}, UserF: [][]float64{{1}}, ItemF: [][]float64{{3}, {2}, {1}},
		UserB: []float64{0}, ItemB: []float64{0, 0, 0}, Implicit: true}
	got := m.rankingMetrics([]mfObs{{0, 0, 1}, {0, 2, 2}})
	ap := (1.0/1 + 2.0/3) / 2
	dcg := 1/math.Log2(2) + 2/math.Log2(4)
	idcg := 2/math.Log2(2) + 1/math.Log2(3)
	mse := ((3-1)*(3-1) + (1-1)*(1-1)) / 2.0
	rank := (1*0 + 2*1.0) / 3
	for k, want := range map[string]float64{"mean_average_precision": ap, "normalized_discounted_cumulative_gain": dcg / idcg,
		"mean_squared_error": mse, "average_rank": rank} {
		if got[k] == nil || math.Abs(*got[k]-want) > 1e-12 {
			t.Errorf("%s = %v; want %v", k, got[k], want)
		}
	}
	if len(m.rankingMetrics(nil)) != 0 {
		t.Error("metrics of no rows")
	}
}
