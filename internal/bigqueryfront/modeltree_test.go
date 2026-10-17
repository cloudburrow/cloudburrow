package bigqueryfront

import (
	"math"
	"strconv"
	"testing"
)

// stepRows are x in 0..9 and y 10 from x = 5 up, else 0.
func stepRows() ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"x", "FLOAT"}, {"y", "FLOAT"}}
	var rows [][]*string
	for x := 0; x < 10; x++ {
		xs, ys := strconv.Itoa(x), "0"
		if x >= 5 {
			ys = "10"
		}
		rows = append(rows, []*string{&xs, &ys})
	}
	return cols, rows
}

func TestTrainBoostedTree(t *testing.T) {
	o := defaultMLOptions()
	o.ModelType, o.LabelCols = "BOOSTED_TREE_REGRESSOR", []string{"y"}
	applyTreeDefaults(&o)
	if o.L2 != 1 || o.LearnRate != 0.3 || o.MaxTreeDepth != 6 || o.MinChildWeight != 1 {
		t.Fatalf("defaults: %+v", o)
	}
	cols, rows := stepRows()
	tr, fe := trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	// By hand: each round splits x at 4.5 alone (within a side every
	// gradient is equal, so no split gains), and each side's leaf moves
	// its prediction a quarter of the way to its label (0.3 · 5/(5 + 1)).
	a := math.Pow(0.75, 20)
	if len(tr.Trees) != 20 || tr.Trees[0][0].Feat != 0 || len(tr.Trees[0]) != 3 {
		t.Fatalf("trees: %d, first %+v", len(tr.Trees), tr.Trees[0])
	}
	for x, want := range map[string]float64{"2": 0.5 * a, "7": 10 - 9.5*a} {
		v := x
		if got := tr.predictRow(map[string]*string{"x": &v}); math.Abs(got-want) > 1e-12 {
			t.Errorf("predict(%s) = %v; want %v", x, got, want)
		}
	}
	if *tr.Eval["mean_squared_error"] > 1e-3 {
		t.Errorf("eval: %v", *tr.Eval["mean_squared_error"])
	}
	o.ModelType = "BOOSTED_TREE_CLASSIFIER"
	o.EarlyStop = false
	cols[1].Type = "INTEGER"
	tr, fe = trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	m0, m1 := 0.0, 0.0
	// By hand: a round splits at 4.5 while each side's hessian sum
	// 5p(1 − p) is at least min_tree_child_weight 1, and is one leaf after.
	for r := 0; r < 20; r++ {
		p0, p1 := sigmoid(m0), sigmoid(m1)
		g0, h0, g1, h1 := 5*p0, 5*p0*(1-p0), 5*(p1-1), 5*p1*(1-p1)
		if h0 >= 1 && h1 >= 1 {
			m0 -= 0.3 * g0 / (h0 + 1)
			m1 -= 0.3 * g1 / (h1 + 1)
			continue
		}
		d := 0.3 * (g0 + g1) / (h0 + h1 + 1)
		m0, m1 = m0-d, m1-d
	}
	two, seven := "2", "7"
	if got := tr.predictRow(map[string]*string{"x": &two}); math.Abs(got-sigmoid(m0)) > 1e-12 {
		t.Errorf("classifier predict(2) = %v; want %v", got, sigmoid(m0))
	}
	if got := tr.predictRow(map[string]*string{"x": &seven}); math.Abs(got-sigmoid(m1)) > 1e-12 {
		t.Errorf("classifier predict(7) = %v; want %v", got, sigmoid(m1))
	}
}
