package bigqueryfront

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// BOOSTED_TREE_REGRESSOR and binary BOOSTED_TREE_CLASSIFIER models (#1504,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-boosted-tree),
// trained by the front with XGBoost's exact greedy algorithm (gbtree, as
// XGBoost documents it: https://xgboost.readthedocs.io/en/stable/tutorials/model.html),
// with BigQuery's documented defaults: max_iterations 20 boosting rounds,
// learn_rate 0.3, max_tree_depth 6, min_split_loss 0, min_tree_child_weight
// 1, l1_reg 0, l2_reg 1.0, early_stop TRUE with min_rel_progress 0.01, and
// subsample, colsample_* and num_parallel_tree 1 (so nothing is drawn at
// random). Each round fits one tree to the loss's gradient g and hessian h
// (squared error: g = p − y, h = 1; log loss: g = p − y, h = p(1 − p)):
// a node's weight is −T(G)/(H + l2_reg), T the l1_reg soft threshold, a
// split's gain T(G_L)²/(H_L + λ) + T(G_R)²/(H_R + λ) − T(G)²/(H + λ), each
// split the best over every feature and every midpoint between two
// consecutive distinct values, kept when its gain exceeds min_split_loss
// and each child's H is at least min_tree_child_weight; a leaf's weight is
// scaled by learn_rate. A row goes left when its value is below the split.
//
// The rows are preprocessed as for the other models (auto-preprocessing):
// numeric features standardized, a NULL taking the mean (a monotone map,
// so a tree splits them as it would the raw values), STRING and BOOL
// features one-hot. ML.PREDICT is written into the query as SQL of the
// trees, ML.FEATURE_IMPORTANCE gives XGBoost's importance_weight (the
// splits on a feature), importance_gain and importance_cover (their mean
// gain and hessian sum), and ML.EVALUATE, ML.CONFUSION_MATRIX,
// ML.ROC_CURVE, ML.FEATURE_INFO and ML.TRAINING_INFO serve it as they do a
// linear model.
//
// UNVERIFIED against Google, chosen here: the starting score 0.5 (XGBoost
// before 1.0, whose base_score it was; a classifier's margin 0); one-hot
// rather than label encoding of a categorical feature; the mean, not a
// learned default direction, for a NULL; float64 values, where XGBoost
// keeps float32; the loss of an iteration (the mean squared error, or the
// mean log loss). Refused 501: num_parallel_tree, subsample or colsample_*
// other than 1, booster_type DART, tree_method APPROX or HIST, and a
// multiclass classifier.

// mlNode is one node of a tree: a split (Feat the encoded column, without
// the intercept, Thr the value, Left and Right the children's indexes) or a
// leaf (Feat -1, Value its weight, learn_rate applied).
type mlNode struct {
	Feat  int     `json:"f"`
	Thr   float64 `json:"t,omitempty"`
	Left  int     `json:"l,omitempty"`
	Right int     `json:"r,omitempty"`
	Value float64 `json:"v,omitempty"`
	Gain  float64 `json:"g,omitempty"`
	Cover float64 `json:"c,omitempty"`
}

func isBoostedTree(typ string) bool {
	return typ == "BOOSTED_TREE_REGRESSOR" || typ == "BOOSTED_TREE_CLASSIFIER"
}

// applyTreeDefaults sets a boosted tree model's documented defaults that
// differ from a linear model's.
func applyTreeDefaults(o *mlOptions) {
	if !o.given("l2_reg") {
		o.L2 = 1
	}
	if !o.given("learn_rate") {
		o.LearnRate = 0.3
	}
	if !o.given("max_tree_depth") {
		o.MaxTreeDepth = 6
	}
	if !o.given("min_tree_child_weight") {
		o.MinChildWeight = 1
	}
	if o.BoosterType == "" {
		o.BoosterType = "GBTREE"
	}
	if o.TreeMethod == "" {
		o.TreeMethod = "AUTO"
	}
}

// softL1 is XGBoost's ThresholdL1.
func softL1(g, alpha float64) float64 {
	switch {
	case g > alpha:
		return g - alpha
	case g < -alpha:
		return g + alpha
	}
	return 0
}

// trainBoosted fits the trees on X (each row's encoded columns, the
// intercept's 1 first) and Y, and the metrics on EX, EY (or X, Y).
func (t *mlTrained) trainBoosted(o mlOptions, X [][]float64, Y []float64, EX [][]float64, EY []float64, classifier bool) {
	t.Strategy = "BOOSTED_TREE"
	if classifier {
		t.BaseScore = 0
	} else {
		t.BaseScore = 0.5
	}
	n := len(X)
	margin := make([]float64, n)
	for i := range margin {
		margin[i] = t.BaseScore
	}
	loss := func(X [][]float64, Y []float64) float64 {
		var s float64
		for i, x := range X {
			p := t.treeMargin(x)
			if classifier {
				q := sigmoid(p)
				q = math.Min(math.Max(q, 1e-15), 1-1e-15)
				s -= Y[i]*math.Log(q) + (1-Y[i])*math.Log(1-q)
			} else {
				s += (p - Y[i]) * (p - Y[i])
			}
		}
		return s / float64(len(X))
	}
	g, h := make([]float64, n), make([]float64, n)
	prev := math.Inf(1)
	for round := 0; round < o.MaxIterations; round++ {
		start := time.Now()
		for i := range X {
			if classifier {
				p := sigmoid(margin[i])
				g[i], h[i] = p-Y[i], p*(1-p)
			} else {
				g[i], h[i] = margin[i]-Y[i], 1
			}
		}
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		var tree []mlNode
		var grow func(idx []int, depth int) int
		grow = func(idx []int, depth int) int {
			var G, H float64
			for _, i := range idx {
				G, H = G+g[i], H+h[i]
			}
			me := len(tree)
			tree = append(tree, mlNode{Feat: -1, Value: -softL1(G, o.L1) / (H + o.L2) * o.LearnRate, Cover: H})
			if depth >= o.MaxTreeDepth || len(idx) < 2 {
				return me
			}
			parent := math.Pow(softL1(G, o.L1), 2) / (H + o.L2)
			best, bestF, bestThr := 0.0, -1, 0.0
			for f := 1; f < len(X[0]); f++ {
				s := append([]int(nil), idx...)
				sort.SliceStable(s, func(a, b int) bool { return X[s[a]][f] < X[s[b]][f] })
				var GL, HL float64
				for k := 0; k < len(s)-1; k++ {
					GL, HL = GL+g[s[k]], HL+h[s[k]]
					v, next := X[s[k]][f], X[s[k+1]][f]
					if v == next || HL < o.MinChildWeight || H-HL < o.MinChildWeight {
						continue
					}
					gain := math.Pow(softL1(GL, o.L1), 2)/(HL+o.L2) + math.Pow(softL1(G-GL, o.L1), 2)/(H-HL+o.L2) - parent
					if gain > best+1e-12 {
						best, bestF, bestThr = gain, f, (v+next)/2
					}
				}
			}
			if bestF < 0 || best <= o.MinSplitLoss || best <= 1e-6 {
				return me
			}
			var left, right []int
			for _, i := range idx {
				if X[i][bestF] < bestThr {
					left = append(left, i)
				} else {
					right = append(right, i)
				}
			}
			l := grow(left, depth+1)
			r := grow(right, depth+1)
			tree[me] = mlNode{Feat: bestF - 1, Thr: bestThr, Left: l, Right: r, Gain: best, Cover: H}
			return me
		}
		grow(all, 0)
		t.Trees = append(t.Trees, tree)
		for i, x := range X {
			margin[i] += treeValue(tree, x)
		}
		it := mlIteration{Index: round, Loss: loss(X, Y), LearnRate: o.LearnRate, DurationMs: time.Since(start).Milliseconds()}
		if len(EX) > 0 {
			e := loss(EX, EY)
			it.EvalLoss = &e
		}
		t.Iterations = append(t.Iterations, it)
		cur := it.Loss
		if it.EvalLoss != nil {
			cur = *it.EvalLoss
		}
		if o.EarlyStop && prev < math.Inf(1) && (prev == 0 || (prev-cur)/prev < o.MinRelProgress) {
			break
		}
		prev = cur
	}
	sort.Slice(t.Iterations, func(a, b int) bool { return t.Iterations[a].Index > t.Iterations[b].Index })
	evX, evY := EX, EY
	if len(evX) == 0 {
		evX, evY = X, Y
	}
	pred := make([]float64, len(evX))
	for i, x := range evX {
		pred[i] = t.treeMargin(x)
		if classifier {
			pred[i] = sigmoid(pred[i])
		}
	}
	if classifier {
		t.Eval = classificationMetrics(evY, pred, 0.5)
		t.EvalY, t.EvalScore = evY, pred
	} else {
		t.Eval = regressionMetrics(evY, pred)
	}
}

// treeValue is a tree's leaf weight for x, the encoded row with the
// intercept's 1 first.
func treeValue(tree []mlNode, x []float64) float64 {
	k := 0
	for tree[k].Feat >= 0 {
		if x[tree[k].Feat+1] < tree[k].Thr {
			k = tree[k].Left
		} else {
			k = tree[k].Right
		}
	}
	return tree[k].Value
}

// treeMargin is the trees' sum for x, from the starting score.
func (t mlTrained) treeMargin(x []float64) float64 {
	z := t.BaseScore
	for _, tr := range t.Trees {
		z += treeValue(tr, x)
	}
	return z
}

// treesSQL is the SQL of the trees' sum for the encoded columns enc.
func (t mlTrained) treesSQL(enc []string) string {
	var node func(tree []mlNode, k int) string
	node = func(tree []mlNode, k int) string {
		nd := tree[k]
		if nd.Feat < 0 {
			return sqlF(nd.Value)
		}
		return fmt.Sprintf("IF(%s < %s, %s, %s)", enc[nd.Feat], sqlF(nd.Thr), node(tree, nd.Left), node(tree, nd.Right))
	}
	terms := []string{sqlF(t.BaseScore)}
	for _, tr := range t.Trees {
		terms = append(terms, node(tr, 0))
	}
	return "(" + strings.Join(terms, " + ") + ")"
}

// featureImportance is ML.FEATURE_IMPORTANCE's rows: each feature's
// splits, and their mean gain and cover.
func (t mlTrained) featureImportance() []string {
	var owner []int
	for k, ft := range t.Features {
		for j := 0; j < ft.dims(); j++ {
			owner = append(owner, k)
		}
	}
	count := make([]int, len(t.Features))
	gain, cover := make([]float64, len(t.Features)), make([]float64, len(t.Features))
	for _, tr := range t.Trees {
		for _, nd := range tr {
			if nd.Feat >= 0 {
				k := owner[nd.Feat]
				count[k]++
				gain[k] += nd.Gain
				cover[k] += nd.Cover
			}
		}
	}
	var rows []string
	for k, ft := range t.Features {
		g, c := 0.0, 0.0
		if count[k] > 0 {
			g, c = gain[k]/float64(count[k]), cover[k]/float64(count[k])
		}
		rows = append(rows, fmt.Sprintf("(%s, %d, %s, %s)", sqlString(ft.Name), count[k], sqlF(g), sqlF(c)))
	}
	return rows
}
