package bigqueryfront

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// BOOSTED_TREE_REGRESSOR and BOOSTED_TREE_CLASSIFIER models (#1504,
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
// so a tree splits them as it would the raw values), and STRING and BOOL
// features label encoded, BigQuery's default category_encoding_method for
// a boosted tree (LABEL_ENCODING, "This is the default") and its encoding
// for a random forest ("Label encoding transformation is applied to train
// boosted tree and random forest models",
// https://docs.cloud.google.com/bigquery/docs/auto-preprocessing): one
// column, the category's index in the vocabulary sorted alphabetically
// from 1, NULL and a category not seen in training 0 (labelEncode);
// category_encoding_method ONE_HOT_ENCODING one-hot encodes them as the
// linear models do, and TARGET_ENCODING is 501. ML.FEATURE_INFO is the
// same either way (its dimension is for ARRAY<STRUCT> columns only).
// ML.PREDICT is written into the query as SQL of the
// trees, ML.FEATURE_IMPORTANCE gives XGBoost's importance_weight (the
// splits on a feature), importance_gain and importance_cover (their mean
// gain and hessian sum), and ML.EVALUATE, ML.CONFUSION_MATRIX,
// ML.ROC_CURVE, ML.FEATURE_INFO and ML.TRAINING_INFO serve it as they do a
// linear model.
//
// UNVERIFIED against Google, chosen here: the starting score 0.5 (XGBoost
// before 1.0, whose base_score it was; a classifier's margin 0); the mean, not a
// learned default direction, for a NULL; float64 values, where XGBoost
// keeps float32; the loss of an iteration (the mean squared error, or the
// mean log loss). Refused 501 for a boosted tree model: num_parallel_tree,
// subsample or colsample_* other than 1, booster_type DART, and
// tree_method APPROX or HIST.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-boosted-tree
//
// RANDOM_FOREST_REGRESSOR and RANDOM_FOREST_CLASSIFIER (#1504,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-random-forest)
// are XGBoost's random forest: one round of num_parallel_tree (100) trees
// grown as above on the documented sampling (subsample 0.8, colsample_bynode
// 0.8, colsample_bytree and colsample_bylevel 1, max_tree_depth 15), each
// leaf's weight divided among the trees, so the forest predicts their mean.
// UNVERIFIED: the draws come from a fixed seed (1504), so a forest is the
// same each run but not Google's draw.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-random-forest

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

func isRandomForest(typ string) bool {
	return typ == "RANDOM_FOREST_REGRESSOR" || typ == "RANDOM_FOREST_CLASSIFIER"
}

// isTreeModel reports whether typ is a boosted tree or random forest.
func isTreeModel(typ string) bool { return isBoostedTree(typ) || isRandomForest(typ) }

// isClassifierType reports whether typ's label is a class.
func isClassifierType(typ string) bool {
	return typ == "LOGISTIC_REG" || typ == "BOOSTED_TREE_CLASSIFIER" || typ == "RANDOM_FOREST_CLASSIFIER" || typ == "DNN_CLASSIFIER"
}

// checkTreeSampling refuses a boosted tree model's sampling other than
// 1 (its draw would be Google's), and a forest of fewer than two trees.
func checkTreeSampling(o mlOptions) (int, string) {
	if isBoostedTree(o.ModelType) {
		if o.NumParallelTree > 1 {
			return 501, "Not implemented here: num_parallel_tree other than 1 for a boosted tree model (a boosted random forest draws its trees' rows at random). Nothing was run."
		}
		for name, v := range map[string]float64{"subsample": o.Subsample, "colsample_bytree": o.ColsampleTree, "colsample_bylevel": o.ColsampleLevel,
			"colsample_bynode": o.ColsampleNode} {
			if v != 0 && v != 1 {
				return 501, "Not implemented here: " + name + " other than 1.0 for a boosted tree model: it samples rows or columns at random, as Google's " +
					"XGBoost draws them. CloudBurrow builds every boosted tree on every row and column. Nothing was run."
			}
		}
	}
	if isRandomForest(o.ModelType) && o.given("num_parallel_tree") && o.NumParallelTree < 2 {
		return 400, "num_parallel_tree must be at least 2 for a random forest model"
	}
	return 0, ""
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
	if o.NumParallelTree == 0 {
		o.NumParallelTree = 1
	}
	for _, v := range []*float64{&o.Subsample, &o.ColsampleTree, &o.ColsampleLevel, &o.ColsampleNode} {
		if *v == 0 {
			*v = 1
		}
	}
	if isRandomForest(o.ModelType) {
		// The documented random forest defaults: one round (no
		// max_iterations or learn_rate option) of num_parallel_tree 100
		// trees, each of max_tree_depth 15 on a subsample 0.8 of the rows
		// and colsample_bynode 0.8 of the columns at each node.
		o.MaxIterations, o.LearnRate, o.EarlyStop = 1, 1, false
		if !o.given("num_parallel_tree") {
			o.NumParallelTree = 100
		}
		if !o.given("max_tree_depth") {
			o.MaxTreeDepth = 15
		}
		if !o.given("subsample") {
			o.Subsample = 0.8
		}
		if !o.given("colsample_bynode") {
			o.ColsampleNode = 0.8
		}
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
// intercept's 1 first) and Y, and the metrics on EX, EY (or X, Y). K is
// the classes of a multiclass classifier (softmax: a tree a class a
// round, g = p_k − y_k, h = 2p_k(1 − p_k), as XGBoost's multi:softprob
// computes them), else 1.
func (t *mlTrained) trainBoosted(o mlOptions, X [][]float64, Y []float64, EX [][]float64, EY []float64, classifier bool, K int) {
	t.Strategy = "BOOSTED_TREE"
	var rng *rand.Rand
	if isRandomForest(o.ModelType) {
		t.Strategy = "RANDOM_FOREST"
		rng = rand.New(rand.NewSource(1504))
	}
	t.BaseScore = 0.5
	if classifier && K == 1 {
		t.BaseScore = 0
	}
	n := len(X)
	margin := make([][]float64, n)
	for i := range margin {
		margin[i] = make([]float64, K)
		for k := range margin[i] {
			margin[i][k] = t.BaseScore
		}
	}
	loss := func(X [][]float64, Y []float64) float64 {
		var s float64
		for i, x := range X {
			switch {
			case K > 1:
				p := softmaxOf(t.treeMargins(x, K))[int(Y[i])]
				s -= math.Log(math.Max(p, 1e-15))
			case classifier:
				q := sigmoid(t.treeMargins(x, 1)[0])
				q = math.Min(math.Max(q, 1e-15), 1-1e-15)
				s -= Y[i]*math.Log(q) + (1-Y[i])*math.Log(1-q)
			default:
				p := t.treeMargins(x, 1)[0]
				s += (p - Y[i]) * (p - Y[i])
			}
		}
		return s / float64(len(X))
	}
	g, h := make([]float64, n), make([]float64, n)
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	prev := math.Inf(1)
	for round := 0; round < o.MaxIterations; round++ {
		start := time.Now()
		var probs [][]float64
		if K > 1 {
			probs = make([][]float64, n)
			for i := range margin {
				probs[i] = softmaxOf(margin[i])
			}
		}
		for k := 0; k < K; k++ {
			for i := range X {
				switch {
				case K > 1:
					p, y := probs[i][k], 0.0
					if int(Y[i]) == k {
						y = 1
					}
					g[i], h[i] = p-y, math.Max(2*p*(1-p), 1e-16)
				case classifier:
					p := sigmoid(margin[i][0])
					g[i], h[i] = p-Y[i], p*(1-p)
				default:
					g[i], h[i] = margin[i][0]-Y[i], 1
				}
			}
			var grown [][]mlNode
			for pt := 0; pt < max(o.NumParallelTree, 1); pt++ {
				grown = append(grown, growTree(o, X, g, h, all, rng))
			}
			for _, tree := range grown {
				t.Trees = append(t.Trees, tree)
				if K > 1 {
					t.TreeClass = append(t.TreeClass, k)
				}
				for i, x := range X {
					margin[i][k] += treeValue(tree, x)
				}
			}
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
	if K > 1 {
		probs := make([][]float64, len(evX))
		for i, x := range evX {
			probs[i] = softmaxOf(t.treeMargins(x, K))
		}
		t.Eval = multiclassMetrics(evY, probs)
		t.EvalY = evY
		for _, p := range probs {
			t.EvalScore = append(t.EvalScore, float64(argmax(p)))
		}
		return
	}
	pred := make([]float64, len(evX))
	for i, x := range evX {
		pred[i] = t.treeMargins(x, 1)[0]
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

// growTree grows one tree on the rows idx of X for gradients g and
// hessians h, by the exact greedy method. With rng (a random forest) it
// keeps each row with probability subsample, and draws the columns
// colsample_bytree of them for the tree, colsample_bylevel of those for
// each depth, and colsample_bynode of those for each node, each at least
// one, without replacement, as XGBoost does; a leaf's weight is then
// divided among the num_parallel_tree trees of the round.
func growTree(o mlOptions, X [][]float64, g, h []float64, idx []int, rng *rand.Rand) []mlNode {
	var tree []mlNode
	scale := o.LearnRate
	cols := make([]int, 0, len(X[0])-1)
	for f := 1; f < len(X[0]); f++ {
		cols = append(cols, f)
	}
	draw := func(from []int, frac float64) []int {
		if rng == nil || frac >= 1 {
			return from
		}
		n := max(1, int(frac*float64(len(from))))
		s := append([]int(nil), from...)
		rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
		s = s[:n]
		sort.Ints(s)
		return s
	}
	var levels map[int][]int
	if rng != nil {
		scale /= float64(max(o.NumParallelTree, 1))
		var kept []int
		for _, i := range idx {
			if rng.Float64() < o.Subsample {
				kept = append(kept, i)
			}
		}
		idx = kept
		cols = draw(cols, o.ColsampleTree)
		levels = map[int][]int{}
	}
	var grow func(idx []int, depth int) int
	grow = func(idx []int, depth int) int {
		var G, H float64
		for _, i := range idx {
			G, H = G+g[i], H+h[i]
		}
		me := len(tree)
		tree = append(tree, mlNode{Feat: -1, Value: -softL1(G, o.L1) / (H + o.L2) * scale, Cover: H})
		if depth >= o.MaxTreeDepth || len(idx) < 2 {
			return me
		}
		parent := math.Pow(softL1(G, o.L1), 2) / (H + o.L2)
		best, bestF, bestThr := 0.0, -1, 0.0
		use := cols
		if rng != nil {
			lv, ok := levels[depth]
			if !ok {
				lv = draw(cols, o.ColsampleLevel)
				levels[depth] = lv
			}
			use = draw(lv, o.ColsampleNode)
		}
		for _, f := range use {
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
	grow(idx, 0)
	return tree
}

// softmaxOf is the softmax of z.
func softmaxOf(z []float64) []float64 {
	m := math.Inf(-1)
	for _, v := range z {
		m = math.Max(m, v)
	}
	out := make([]float64, len(z))
	var sum float64
	for k, v := range z {
		out[k] = math.Exp(v - m)
		sum += out[k]
	}
	for k := range out {
		out[k] /= sum
	}
	return out
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

// treeMargins are the trees' sums for x of each of K classes (1 for a
// regressor or binary classifier), from the starting score.
func (t mlTrained) treeMargins(x []float64, K int) []float64 {
	z := make([]float64, K)
	for k := range z {
		z[k] = t.BaseScore
	}
	for j, tr := range t.Trees {
		k := 0
		if j < len(t.TreeClass) {
			k = t.TreeClass[j]
		}
		z[k] += treeValue(tr, x)
	}
	return z
}

// treeMargin is a regressor's or binary classifier's trees' sum for x.
func (t mlTrained) treeMargin(x []float64) float64 { return t.treeMargins(x, 1)[0] }

// treesSQL is the SQL of class k's trees' sum (every tree's, for a
// regressor or binary classifier) for the encoded columns enc.
func (t mlTrained) treesSQL(enc []string, k int) string {
	var node func(tree []mlNode, j int) string
	node = func(tree []mlNode, j int) string {
		nd := tree[j]
		if nd.Feat < 0 {
			return sqlF(nd.Value)
		}
		return fmt.Sprintf("IF(%s < %s, %s, %s)", enc[nd.Feat], sqlF(nd.Thr), node(tree, nd.Left), node(tree, nd.Right))
	}
	terms := []string{sqlF(t.BaseScore)}
	for j, tr := range t.Trees {
		if j < len(t.TreeClass) && t.TreeClass[j] != k {
			continue
		}
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
