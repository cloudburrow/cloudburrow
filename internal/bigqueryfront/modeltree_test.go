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
	// Multiclass, one round: x 0..2 a, 3..5 b, 6..8 c. By hand, every
	// p is 1/3 and h 2·(1/3)(2/3) = 4/9: a's tree splits at 2.5 (gain
	// 12/7 + 12/11), its leaves 0.3·2/(7/3) and −0.3·2/(11/3); b's splits
	// a, b and c apart, b's leaf 0.3·2/(7/3) and the others −0.3·1/(7/3).
	mcols := []mlColumn{{"x", "FLOAT"}, {"label", "STRING"}}
	var mrows [][]*string
	for x := 0; x < 9; x++ {
		xs, ls := strconv.Itoa(x), []string{"a", "b", "c"}[x/3]
		mrows = append(mrows, []*string{&xs, &ls})
	}
	o.LabelCols, o.MaxIterations = nil, 1
	tr, fe = trainModel(o, mcols, mrows)
	if fe != nil {
		t.Fatal(fe)
	}
	if len(tr.Trees) != 3 || len(tr.TreeClass) != 3 {
		t.Fatalf("multiclass trees: %d", len(tr.Trees))
	}
	one := "1"
	want := softmaxOf([]float64{0.5 + 0.3*2/(7.0/3), 0.5 - 0.3/(7.0/3), 0.5 - 0.3*2/(11.0/3)})
	got := tr.classProbs(map[string]*string{"x": &one})
	for k := range want {
		if math.Abs(got[k]-want[k]) > 1e-12 {
			t.Errorf("multiclass probs(1) = %v; want %v", got, want)
		}
	}
}

// TestTrainRandomForest: with every row and column a forest's trees are
// alike, and their average is one tree's weights unscaled (by hand on the
// step: 0.5 − 2.5/6 and 0.5 + 47.5/6); with the documented sampling the
// forest is the same each run.
func TestTrainRandomForest(t *testing.T) {
	o := defaultMLOptions()
	o.ModelType, o.LabelCols = "RANDOM_FOREST_REGRESSOR", []string{"y"}
	o.Set = []string{"model_type", "input_label_cols", "subsample", "colsample_bynode", "num_parallel_tree"}
	o.Subsample, o.ColsampleNode, o.NumParallelTree = 1, 1, 10
	applyTreeDefaults(&o)
	if c, m := checkTreeSampling(o); c != 0 || o.MaxTreeDepth != 15 || o.MaxIterations != 1 {
		t.Fatalf("options %+v: %s", o, m)
	}
	cols, rows := stepRows()
	tr, fe := trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	lo, hi := tr.treeMargin([]float64{1, -1}), tr.treeMargin([]float64{1, 1})
	if len(tr.Trees) != 10 || math.Abs(lo-(0.5-2.5/6)) > 1e-12 || math.Abs(hi-(0.5+47.5/6)) > 1e-12 {
		t.Fatalf("%d trees, %v %v", len(tr.Trees), lo, hi)
	}
	d := defaultMLOptions()
	d.ModelType, d.LabelCols, d.Set = "RANDOM_FOREST_REGRESSOR", []string{"y"}, []string{"model_type", "input_label_cols"}
	applyTreeDefaults(&d)
	if d.NumParallelTree != 100 || d.Subsample != 0.8 || d.ColsampleNode != 0.8 {
		t.Fatalf("defaults %+v", d)
	}
	a, _ := trainModel(d, cols, rows)
	b, _ := trainModel(d, cols, rows)
	if len(a.Trees) != 100 || a.treeMargin([]float64{1, 0.3}) != b.treeMargin([]float64{1, 0.3}) {
		t.Fatalf("forest: %d trees, %v vs %v", len(a.Trees), a.treeMargin([]float64{1, 0.3}), b.treeMargin([]float64{1, 0.3}))
	}
}

// TestBoostedTreeLabelEncoding: a boosted tree's STRING feature is label
// encoded by default, as BigQuery documents (auto-preprocessing): one
// column, the vocabulary sorted alphabetically from 1, NULL and an unseen
// category 0.
func TestBoostedTreeLabelEncoding(t *testing.T) {
	cols := []mlColumn{{"c", "STRING"}, {"y", "FLOAT"}}
	var rows [][]*string
	for _, r := range [][2]string{{"b", "0"}, {"a", "0"}, {"c", "10"}, {"b", "0"}, {"a", "0"}, {"c", "10"}} {
		rows = append(rows, []*string{sp(r[0]), sp(r[1])})
	}
	o := defaultMLOptions()
	o.ModelType, o.LabelCols, o.MaxIterations = "BOOSTED_TREE_REGRESSOR", []string{"y"}, 1
	applyTreeDefaults(&o)
	tr, fe := trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	ft := tr.Features[0]
	if !ft.LabelEncoded || ft.dims() != 1 {
		t.Fatalf("feature: %+v; want label encoded, one column", ft)
	}
	for v, want := range map[string]float64{"a": 1, "b": 2, "c": 3, "z": 0} {
		if got := encodeFeature(ft, sp(v)); len(got) != 1 || got[0] != want {
			t.Errorf("encode(%q) = %v; want [%v]", v, got, want)
		}
	}
	if got := encodeFeature(ft, nil); got[0] != 0 {
		t.Errorf("encode(NULL) = %v; want [0]", got)
	}
	// By hand: c (3) apart from a and b (1, 2): the one split is at 2.5.
	if root := tr.Trees[0][0]; root.Feat != 0 || root.Thr != 2.5 || len(tr.Trees[0]) != 3 {
		t.Fatalf("tree: %+v; want a split of column 0 at 2.5", tr.Trees[0])
	}
	a, z := tr.predictRow(map[string]*string{"c": sp("a")}), tr.predictRow(map[string]*string{"c": sp("z")})
	if n := tr.predictRow(map[string]*string{"c": nil}); a != z || a != n {
		t.Errorf("predict a %v, unseen %v, NULL %v; want the same leaf (0 encodes below 2.5)", a, z, n)
	}
	sql := tr.encodedSQL(func(s string) string { return s })
	if len(sql) != 1 || sql[0] != "(CASE CAST(c AS STRING) WHEN 'a' THEN 1.0 WHEN 'b' THEN 2.0 WHEN 'c' THEN 3.0 ELSE 0.0 END)" {
		t.Errorf("encodedSQL: %q", sql)
	}
	// ONE_HOT_ENCODING, given, keeps a column per category.
	o.CategoryEncoding = "ONE_HOT_ENCODING"
	if tr, fe = trainModel(o, cols, rows); fe != nil || tr.Features[0].LabelEncoded || tr.Features[0].dims() != 3 {
		t.Errorf("one-hot: %+v, %v", tr.Features, fe)
	}
}
