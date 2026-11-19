package bigqueryfront

import (
	"math"
	"strconv"
	"testing"
)

// TestDummyEncoding: DUMMY_ENCODING drops the most frequent category,
// whose weight is 0.0, and still fits (modelencoding.go, #1929).
func TestDummyEncoding(t *testing.T) {
	cols := []mlColumn{{"c", "STRING"}, {"label", "FLOAT"}}
	var rows [][]*string
	for i := 0; i < 30; i++ {
		c := []string{"apple", "banana", "banana", "cranberry", "banana"}[i%5]
		y := map[string]float64{"apple": 1, "banana": 5, "cranberry": 9}[c]
		rows = append(rows, []*string{sp(c), sp(strconv.FormatFloat(y, 'f', -1, 64))})
	}
	for _, strat := range []string{"NORMAL_EQUATION", "BATCH_GRADIENT_DESCENT"} {
		o := defaultMLOptions()
		o.ModelType, o.CategoryEncoding, o.Strategy, o.MaxIterations = "LINEAR_REG", "DUMMY_ENCODING", strat, 200
		m, f := trainModelT(o, cols, rows)
		if f != nil {
			t.Fatal(f)
		}
		ft := m.Features[0]
		if ft.Dropped != 2 || ft.CategoryWeights[1] != 0 {
			t.Fatalf("%s: dropped %d weights %v", strat, ft.Dropped, ft.CategoryWeights)
		}
		if x := encodeFeature(ft, sp("banana")); x[1] != 0 {
			t.Errorf("banana encodes as %v", x)
		}
		if strat == "NORMAL_EQUATION" {
			for c, want := range map[string]float64{"apple": 1, "banana": 5, "cranberry": 9} {
				if got := m.predictRow(map[string]*string{"c": sp(c)}); math.Abs(got-want) > 1e-6 {
					t.Errorf("predict %s: %v, want %v", c, got, want)
				}
			}
		}
	}
	o := defaultMLOptions()
	o.ModelType, o.CategoryEncoding = "LOGISTIC_REG", "DUMMY_ENCODING"
	var lrows [][]*string
	for i := 0; i < 30; i++ {
		lrows = append(lrows, []*string{sp([]string{"a", "b", "c"}[i%3]), sp([]string{"x", "y", "z"}[i%3])})
	}
	m, f := trainModelT(o, []mlColumn{{"c", "STRING"}, {"label", "STRING"}}, lrows)
	if f != nil {
		t.Fatal(f)
	}
	// A three-way tie: the first category is dropped, in every class.
	for k, w := range m.ClassWeights {
		if m.Features[0].Dropped != 1 || w[1] != 0 {
			t.Errorf("class %d weights %v", k, w)
		}
	}
}

// TestTargetEncoding: TARGET_ENCODING replaces a category by its
// training rows' mean label (a binary classifier's positive share).
func TestTargetEncoding(t *testing.T) {
	cols := []mlColumn{{"c", "STRING"}, {"label", "FLOAT"}}
	var rows [][]*string
	for _, r := range [][2]string{{"category_1", "2"}, {"category_1", "3"}, {"category_2", "1"}, {"category_2", "2"}} {
		for i := 0; i < 5; i++ {
			rows = append(rows, []*string{sp(r[0]), sp(r[1])})
		}
	}
	o := defaultMLOptions()
	o.ModelType, o.CategoryEncoding, o.MaxIterations = "BOOSTED_TREE_REGRESSOR", "TARGET_ENCODING", 5
	m, f := trainModelT(o, cols, rows)
	if f != nil {
		t.Fatal(f)
	}
	ft := m.Features[0]
	if !ft.TargetEncoded || ft.LabelEncoded || ft.Targets[0] != 2.5 || ft.Targets[1] != 1.5 || ft.TargetPrior != 2 {
		t.Fatalf("feature %+v", ft)
	}
	if v := targetEncode(ft, nil); v != 2 {
		t.Errorf("NULL encodes as %v", v)
	}
	if v := targetEncode(ft, sp("unseen")); v != 2 {
		t.Errorf("unseen encodes as %v", v)
	}
	// The documented classification example: category_1 is half target_2,
	// category_2 none.
	var crows [][]*string
	for _, r := range [][2]string{{"category_1", "target_1"}, {"category_1", "target_2"}, {"category_2", "target_1"}} {
		for i := 0; i < 4; i++ {
			crows = append(crows, []*string{sp(r[0]), sp(r[1])})
		}
	}
	o.ModelType = "BOOSTED_TREE_CLASSIFIER"
	m, f = trainModelT(o, []mlColumn{{"c", "STRING"}, {"label", "STRING"}}, crows)
	if f != nil {
		t.Fatal(f)
	}
	if ft := m.Features[0]; ft.Targets[0] != 0.5 || ft.Targets[1] != 0 {
		t.Errorf("classifier targets %v", ft.Targets)
	}
	crows = append(crows, []*string{sp("category_2"), sp("target_3")})
	if _, f = trainModelT(o, []mlColumn{{"c", "STRING"}, {"label", "STRING"}}, crows); f == nil || f.code != 501 {
		t.Errorf("multiclass: %v", f)
	}
	for _, c := range []struct{ typ, enc string }{{"BOOSTED_TREE_REGRESSOR", "DUMMY_ENCODING"}, {"LINEAR_REG", "TARGET_ENCODING"}, {"DNN_REGRESSOR", "DUMMY_ENCODING"}} {
		o := defaultMLOptions()
		o.ModelType, o.CategoryEncoding = c.typ, c.enc
		if code, _ := checkModelOptions(o); code != 400 {
			t.Errorf("%s %s: %d", c.typ, c.enc, code)
		}
	}
}
