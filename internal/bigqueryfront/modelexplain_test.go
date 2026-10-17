package bigqueryfront

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

// condExpect is the tree's value with the columns in S fixed to x's and
// the others averaged by cover: the value function TreeSHAP's Shapley
// values are of.
func condExpect(tree []mlNode, x []float64, S map[int]bool, j int) float64 {
	nd := tree[j]
	if nd.Feat < 0 {
		return nd.Value
	}
	if S[nd.Feat] {
		if x[nd.Feat+1] < nd.Thr {
			return condExpect(tree, x, S, nd.Left)
		}
		return condExpect(tree, x, S, nd.Right)
	}
	return (tree[nd.Left].Cover*condExpect(tree, x, S, nd.Left) + tree[nd.Right].Cover*condExpect(tree, x, S, nd.Right)) / nd.Cover
}

// TestTreeShapExact: TreeSHAP's values are the Shapley values of
// condExpect, by enumeration of every subset, and add up to the
// prediction from the expected value.
func TestTreeShapExact(t *testing.T) {
	cols := []mlColumn{{"a", "FLOAT"}, {"b", "FLOAT"}, {"c", "FLOAT"}, {"y", "FLOAT"}}
	rng := rand.New(rand.NewSource(7))
	var rows [][]*string
	for i := 0; i < 40; i++ {
		a, b, c := rng.Float64(), rng.Float64(), rng.Float64()
		y := 3*a + 5*a*b - 2*c + rng.Float64()*0.1
		vs := []string{strconv.FormatFloat(a, 'g', -1, 64), strconv.FormatFloat(b, 'g', -1, 64), strconv.FormatFloat(c, 'g', -1, 64), strconv.FormatFloat(y, 'g', -1, 64)}
		rows = append(rows, []*string{&vs[0], &vs[1], &vs[2], &vs[3]})
	}
	o := defaultMLOptions()
	o.ModelType, o.LabelCols, o.MaxIterations = "BOOSTED_TREE_REGRESSOR", []string{"y"}, 3
	o.Set = []string{"model_type", "input_label_cols", "max_iterations"}
	applyTreeDefaults(&o)
	tr, fe := trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	fact := func(n int) float64 {
		f := 1.0
		for i := 2; i <= n; i++ {
			f *= float64(i)
		}
		return f
	}
	for r := 0; r < 5; r++ {
		x := []float64{1, rng.NormFloat64(), rng.NormFloat64(), rng.NormFloat64()}
		base, attr := tr.explainScore(x, 0)
		want := make([]float64, 3)
		for _, tree := range tr.Trees {
			for i := 0; i < 3; i++ {
				for mask := 0; mask < 8; mask++ {
					if mask&(1<<i) != 0 {
						continue
					}
					S := map[int]bool{}
					for k := 0; k < 3; k++ {
						if mask&(1<<k) != 0 {
							S[k] = true
						}
					}
					with := map[int]bool{i: true}
					for k := range S {
						with[k] = true
					}
					w := fact(len(S)) * fact(3-len(S)-1) / fact(3)
					want[i] += w * (condExpect(tree, x, with, 0) - condExpect(tree, x, S, 0))
				}
			}
		}
		sum := base
		for i := range attr {
			sum += attr[i]
			if math.Abs(attr[i]-want[i]) > 1e-9 {
				t.Errorf("row %d feature %d: %v; want %v", r, i, attr[i], want[i])
			}
		}
		if math.Abs(sum-tr.treeMargin(x)) > 1e-9 {
			t.Errorf("row %d: baseline + attributions %v; prediction %v", r, sum, tr.treeMargin(x))
		}
	}
}

// TestExplainLinear: a linear model's attributions are its weights times
// the standardized values, from the intercept; a binary model's toward
// the predicted class.
func TestExplainLinear(t *testing.T) {
	tr := mlTrained{Features: []mlFeature{{Name: "a", Type: "FLOAT64"}, {Name: "b", Type: "FLOAT64"}}, Intercept: 0.5}
	tr.Features[0].Weight, tr.Features[1].Weight = 2, -3
	x := []float64{1, 0.25, 1}
	_, value, _, logit, base, attr := tr.explainRow(x, 0.5)
	if base != 0.5 || attr[0] != 0.5 || attr[1] != -3 || value != -2 || logit != -2 {
		t.Fatalf("regression: %v %v %v", base, attr, value)
	}
	tr.Classes = []string{"n", "p"}
	pred, _, prob, logit, base, attr := tr.explainRow(x, 0.5)
	if pred != 0 || math.Abs(prob-(1-sigmoid(-2))) > 1e-12 || logit != 2 || base != -0.5 || attr[0] != -0.5 || attr[1] != 3 {
		t.Fatalf("binary: %v %v %v %v %v", pred, prob, logit, base, attr)
	}
}
