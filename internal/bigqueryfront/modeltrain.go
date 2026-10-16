package bigqueryfront

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BigQuery ML training (#1465): LINEAR_REG and binary LOGISTIC_REG models
// trained by the front on the rows of their training query, with
// BigQuery's documented options and defaults
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-glm):
//
//   - preprocessing (https://cloud.google.com/bigquery/docs/auto-preprocessing):
//     a numeric feature (INT64, FLOAT64, NUMERIC, BIGNUMERIC) is
//     standardized, (x - mean) / stddev, a NULL taking the mean; a STRING
//     or BOOL feature is one-hot encoded, one column a category seen in
//     training, NULL a category of its own, and a category not seen in
//     training no column;
//   - optimize_strategy AUTO_STRATEGY: NORMAL_EQUATION for a linear model
//     with no l1_reg and at most 10,000 one-hot and numeric columns, else
//     BATCH_GRADIENT_DESCENT (a logistic model always);
//   - batch gradient descent: max_iterations 20, learn_rate_strategy
//     LINE_SEARCH from ls_init_learn_rate 0.1 (CONSTANT: learn_rate 0.1),
//     early_stop TRUE with min_rel_progress 0.01, l1_reg 0 and l2_reg 0;
//   - data_split_method AUTO_SPLIT: no split under 500 rows, a RANDOM 20%
//     to 50,000 rows, 10,000 rows over that; RANDOM, CUSTOM and NO_SPLIT.
//
// What is not documented, and so chosen here (UNVERIFIED against Google;
// docs/compatibility.md): the loss is the mean squared error (linear) or
// the mean log loss (logistic) plus l2_reg·‖w‖² and l1_reg·‖w‖₁, the
// intercept unregularized; the stddev is the sample one; the line search
// halves the rate until the loss falls and doubles it after a step that
// needed no halving; L1 is a proximal (soft-threshold) step; a RANDOM
// split is drawn with a fixed seed, so it is the same each run but not
// Google's rows; a one-hot column set that is collinear with the intercept
// is solved for the minimum-norm weights. The weights so differ from
// Google's in their last digits, and more where the problem is
// ill-conditioned or gradient descent stops early.

// mlOptions are a model's training options, as CREATE MODEL gives them.
type mlOptions struct {
	ModelType      string   `json:"modelType"`
	LabelCols      []string `json:"inputLabelCols,omitempty"`
	Strategy       string   `json:"optimizeStrategy"`
	L1             float64  `json:"l1Reg"`
	L2             float64  `json:"l2Reg"`
	MaxIterations  int      `json:"maxIterations"`
	LearnRateStrat string   `json:"learnRateStrategy"`
	LearnRate      float64  `json:"learnRate"`
	InitLearnRate  float64  `json:"lsInitLearnRate"`
	EarlyStop      bool     `json:"earlyStop"`
	MinRelProgress float64  `json:"minRelProgress"`
	SplitMethod    string   `json:"dataSplitMethod"`
	SplitEvalFrac  float64  `json:"dataSplitEvalFraction"`
	SplitCol       string   `json:"dataSplitCol,omitempty"`
}

// defaultMLOptions are BigQuery's documented defaults.
func defaultMLOptions() mlOptions {
	return mlOptions{Strategy: "AUTO_STRATEGY", MaxIterations: 20, LearnRateStrat: "LINE_SEARCH", LearnRate: 0.1,
		InitLearnRate: 0.1, EarlyStop: true, MinRelProgress: 0.01, SplitMethod: "AUTO_SPLIT", SplitEvalFrac: 0.2}
}

// mlFeature is one input column of a model and what training learned of
// it.
type mlFeature struct {
	Name string `json:"name"`
	// Type is the column's BigQuery type: INT64, FLOAT64, NUMERIC,
	// BIGNUMERIC, STRING or BOOL.
	Type string `json:"type"`
	// Numeric features: their statistics over the training rows.
	Mean   float64 `json:"mean,omitempty"`
	Stddev float64 `json:"stddev,omitempty"`
	Min    float64 `json:"min,omitempty"`
	Max    float64 `json:"max,omitempty"`
	Median float64 `json:"median,omitempty"`
	// Categories are a categorical feature's values seen in training,
	// sorted; NullSeen whether NULL was one.
	Categories []string `json:"categories,omitempty"`
	NullSeen   bool     `json:"nullSeen,omitempty"`
	NullCount  int64    `json:"nullCount"`
	// Weight is a numeric feature's weight on its standardized value;
	// CategoryWeights a categorical one's, by category, the NULL
	// category's last when NullSeen.
	Weight          float64   `json:"weight"`
	CategoryWeights []float64 `json:"categoryWeights,omitempty"`
}

func (ft mlFeature) numeric() bool {
	switch ft.Type {
	case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC":
		return true
	}
	return false
}

// dims is how many encoded columns the feature has.
func (ft mlFeature) dims() int {
	if ft.numeric() {
		return 1
	}
	n := len(ft.Categories)
	if ft.NullSeen {
		n++
	}
	return n
}

// mlIteration is one iteration of training, as ML.TRAINING_INFO and the
// model's trainingRuns give it.
type mlIteration struct {
	Index      int      `json:"index"`
	Loss       float64  `json:"loss"`
	EvalLoss   *float64 `json:"evalLoss,omitempty"`
	LearnRate  float64  `json:"learnRate"`
	DurationMs int64    `json:"durationMs"`
}

// mlTrained is what training made.
type mlTrained struct {
	Features  []mlFeature `json:"features"`
	Intercept float64     `json:"intercept"`
	// Label is the label column, LabelType its BigQuery type; Classes a
	// logistic model's two labels as strings, the negative first.
	Label      string        `json:"label"`
	LabelType  string        `json:"labelType"`
	Classes    []string      `json:"classes,omitempty"`
	Iterations []mlIteration `json:"iterations"`
	// Eval are the evaluation metrics on the evaluation rows, or the
	// training rows with no split (ML.EVALUATE with no input).
	Eval     map[string]*float64 `json:"eval"`
	Strategy string              `json:"strategy"`
	Rows     int                 `json:"rows"`
	EvalRows int                 `json:"evalRows"`
}

// mlColumn is a column of a query's result: its name and BigQuery type.
type mlColumn struct{ Name, Type string }

// mlTypeOf maps a REST schema type to the model's type name, or "".
func mlTypeOf(t string) string {
	switch strings.ToUpper(t) {
	case "INTEGER", "INT64":
		return "INT64"
	case "FLOAT", "FLOAT64":
		return "FLOAT64"
	case "NUMERIC":
		return "NUMERIC"
	case "BIGNUMERIC":
		return "BIGNUMERIC"
	case "STRING":
		return "STRING"
	case "BOOLEAN", "BOOL":
		return "BOOL"
	}
	return ""
}

// mlFail is a training failure: the status (400, 501) and BigQuery's
// message.
type mlFail struct {
	code int
	msg  string
}

func (e *mlFail) Error() string { return e.msg }

func mlInvalid(format string, a ...any) *mlFail {
	return &mlFail{code: 400, msg: fmt.Sprintf(format, a...)}
}

func mlUnimplemented(format string, a ...any) *mlFail {
	return &mlFail{code: 501, msg: "Not implemented here: " + fmt.Sprintf(format, a...)}
}

// trainModel trains a model of opts on rows, whose columns are cols, each
// value a string or nil as the REST API gives it.
func trainModel(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	label := "label"
	if len(opts.LabelCols) > 1 {
		return t, mlInvalid("Only one input label column is supported for model type %s", opts.ModelType)
	}
	if len(opts.LabelCols) == 1 {
		label = opts.LabelCols[0]
	}
	li := -1
	si := -1
	for i, c := range cols {
		if strings.EqualFold(c.Name, label) {
			li = i
		}
		if opts.SplitCol != "" && strings.EqualFold(c.Name, opts.SplitCol) {
			si = i
		}
	}
	if li < 0 {
		return t, mlInvalid("Unable to identify the label column '%s' in the input data", label)
	}
	if opts.SplitCol != "" && si < 0 {
		return t, mlInvalid("Unable to find the data split column '%s' in the input data", opts.SplitCol)
	}
	t.Label, t.LabelType = cols[li].Name, mlTypeOf(cols[li].Type)
	logistic := opts.ModelType == "LOGISTIC_REG"
	if t.LabelType == "" || (!logistic && !(mlFeature{Type: t.LabelType}).numeric()) || (logistic && t.LabelType != "STRING" && t.LabelType != "BOOL" && t.LabelType != "INT64") {
		return t, mlInvalid("Label column %s has an unsupported type %s for model type %s", cols[li].Name, cols[li].Type, opts.ModelType)
	}
	var fidx []int
	for i, c := range cols {
		if i == li || i == si {
			continue
		}
		ty := mlTypeOf(c.Type)
		if ty == "" {
			return t, mlUnimplemented("a feature column (%s) of type %s: CloudBurrow trains on INT64, FLOAT64, NUMERIC, BIGNUMERIC, "+
				"STRING and BOOL features only", c.Name, c.Type)
		}
		t.Features = append(t.Features, mlFeature{Name: c.Name, Type: ty})
		fidx = append(fidx, i)
	}
	if len(t.Features) == 0 {
		return t, mlInvalid("No feature columns found in the input data; at least one column other than the label is required")
	}
	// Rows with a NULL label are dropped, as BigQuery drops them.
	var kept [][]*string
	for _, row := range rows {
		if row[li] != nil {
			kept = append(kept, row)
		}
	}
	if len(kept) == 0 {
		return t, mlInvalid("Input data doesn't contain any rows")
	}
	// The labels.
	y := make([]float64, len(kept))
	if logistic {
		seen := map[string]bool{}
		for _, row := range kept {
			seen[*row[li]] = true
		}
		for c := range seen {
			t.Classes = append(t.Classes, c)
		}
		sortLabels(t.Classes, t.LabelType)
		if len(t.Classes) != 2 {
			if len(t.Classes) < 2 {
				return t, mlInvalid("Classification model requires at least 2 unique labels and the label column had only %d unique label", len(t.Classes))
			}
			return t, mlUnimplemented("a multiclass LOGISTIC_REG model (%d labels): CloudBurrow trains binary logistic regression only", len(t.Classes))
		}
		for i, row := range kept {
			if *row[li] == t.Classes[1] {
				y[i] = 1
			}
		}
	} else {
		for i, row := range kept {
			v, err := strconv.ParseFloat(*row[li], 64)
			if err != nil {
				return t, mlInvalid("Label value %q is not numeric", *row[li])
			}
			y[i] = v
		}
	}
	// The split.
	isEval := make([]bool, len(kept))
	method := opts.SplitMethod
	frac := opts.SplitEvalFrac
	if method == "AUTO_SPLIT" {
		switch n := len(kept); {
		case n < 500:
			method = "NO_SPLIT"
		case n <= 50000:
			method, frac = "RANDOM", 0.2
		default:
			method, frac = "RANDOM", 10000/float64(n)
		}
	}
	switch method {
	case "RANDOM":
		rng := rand.New(rand.NewSource(1465))
		perm := rng.Perm(len(kept))
		n := int(math.Round(frac * float64(len(kept))))
		for _, p := range perm[:n] {
			isEval[p] = true
		}
	case "CUSTOM":
		if si < 0 {
			return t, mlInvalid("data_split_col must be specified when data_split_method is CUSTOM")
		}
		for i, row := range kept {
			if row[si] != nil && strings.EqualFold(*row[si], "true") {
				isEval[i] = true
			}
		}
	}
	var train, eval []int
	for i := range kept {
		if isEval[i] {
			eval = append(eval, i)
		} else {
			train = append(train, i)
		}
	}
	if len(train) == 0 {
		return t, mlInvalid("No training data after the data split")
	}
	t.Rows, t.EvalRows = len(train), len(eval)
	// Feature statistics over the training rows.
	for k := range t.Features {
		ft := &t.Features[k]
		var vals []float64
		cats := map[string]bool{}
		for _, i := range train {
			v := kept[i][fidx[k]]
			if v == nil {
				ft.NullCount++
				continue
			}
			if ft.numeric() {
				x, err := strconv.ParseFloat(*v, 64)
				if err != nil {
					return t, mlInvalid("Feature value %q of %s is not numeric", *v, ft.Name)
				}
				vals = append(vals, x)
			} else {
				cats[*v] = true
			}
		}
		if ft.numeric() {
			ft.Mean, ft.Stddev, ft.Min, ft.Max, ft.Median = stats(vals)
		} else {
			for c := range cats {
				ft.Categories = append(ft.Categories, c)
			}
			sort.Strings(ft.Categories)
			ft.NullSeen = ft.NullCount > 0
		}
	}
	encode := func(i int) []float64 {
		x := []float64{1}
		for k, ft := range t.Features {
			v := kept[i][fidx[k]]
			x = append(x, encodeFeature(ft, v)...)
		}
		return x
	}
	X := make([][]float64, len(train))
	Y := make([]float64, len(train))
	for j, i := range train {
		X[j], Y[j] = encode(i), y[i]
	}
	var EX [][]float64
	var EY []float64
	for _, i := range eval {
		EX, EY = append(EX, encode(i)), append(EY, y[i])
	}
	dim := len(X[0])
	strategy := opts.Strategy
	if strategy == "AUTO_STRATEGY" {
		strategy = "BATCH_GRADIENT_DESCENT"
		if !logistic && opts.L1 == 0 && dim-1 <= 10000 {
			strategy = "NORMAL_EQUATION"
		}
	}
	if strategy == "NORMAL_EQUATION" && (logistic || opts.L1 != 0) {
		return t, mlInvalid("NORMAL_EQUATION is not supported with l1_reg or for model type %s", opts.ModelType)
	}
	t.Strategy = strategy
	loss := func(w []float64, X [][]float64, Y []float64) float64 {
		return mlLoss(w, X, Y, logistic, opts.L1, opts.L2)
	}
	var w []float64
	start := time.Now()
	if strategy == "NORMAL_EQUATION" {
		w = normalEquation(X, Y, opts.L2)
		it := mlIteration{Index: 0, Loss: loss(w, X, Y), LearnRate: 0, DurationMs: time.Since(start).Milliseconds()}
		if len(EX) > 0 {
			e := loss(w, EX, EY)
			it.EvalLoss = &e
		}
		t.Iterations = []mlIteration{it}
	} else {
		w = make([]float64, dim)
		rate := opts.InitLearnRate
		if opts.LearnRateStrat == "CONSTANT" {
			rate = opts.LearnRate
		}
		prev := loss(w, X, Y)
		prevEval := prev
		if len(EX) > 0 {
			prevEval = loss(w, EX, EY)
		}
		for iter := 0; iter < opts.MaxIterations; iter++ {
			itStart := time.Now()
			g := mlGradient(w, X, Y, logistic, opts.L2)
			step := func(rate float64) []float64 {
				nw := make([]float64, dim)
				for j := range w {
					nw[j] = w[j] - rate*g[j]
					if j > 0 && opts.L1 > 0 {
						nw[j] = softThreshold(nw[j], rate*opts.L1)
					}
				}
				return nw
			}
			nw := step(rate)
			cur := loss(nw, X, Y)
			if opts.LearnRateStrat != "CONSTANT" {
				halved := false
				for k := 0; k < 40 && cur > prev; k++ {
					rate /= 2
					halved = true
					nw = step(rate)
					cur = loss(nw, X, Y)
				}
				t.Iterations = append(t.Iterations, mlIteration{Index: iter, Loss: cur, LearnRate: rate})
				if !halved {
					rate *= 2
				}
			} else {
				t.Iterations = append(t.Iterations, mlIteration{Index: iter, Loss: cur, LearnRate: rate})
			}
			w = nw
			last := &t.Iterations[len(t.Iterations)-1]
			cmp, prevCmp := cur, prev
			if len(EX) > 0 {
				e := loss(w, EX, EY)
				last.EvalLoss = &e
				cmp, prevCmp = e, prevEval
				prevEval = e
			}
			last.DurationMs = time.Since(itStart).Milliseconds()
			prev = cur
			if opts.EarlyStop && iter > 0 && prevCmp > 0 && (prevCmp-cmp)/prevCmp < opts.MinRelProgress {
				break
			}
		}
	}
	// BigQuery lists training iterations newest first.
	sort.Slice(t.Iterations, func(a, b int) bool { return t.Iterations[a].Index > t.Iterations[b].Index })
	t.Intercept = w[0]
	j := 1
	for k := range t.Features {
		ft := &t.Features[k]
		if ft.numeric() {
			ft.Weight = w[j]
			j++
			continue
		}
		ft.CategoryWeights = append([]float64(nil), w[j:j+ft.dims()]...)
		j += ft.dims()
	}
	// The evaluation metrics.
	evX, evY := EX, EY
	if len(evX) == 0 {
		evX, evY = X, Y
	}
	pred := make([]float64, len(evX))
	for i, x := range evX {
		pred[i] = dot(w, x)
		if logistic {
			pred[i] = sigmoid(pred[i])
		}
	}
	if logistic {
		t.Eval = classificationMetrics(evY, pred, 0.5)
	} else {
		t.Eval = regressionMetrics(evY, pred)
	}
	return t, nil
}

// sortLabels sorts a classification model's labels: numbers as numbers,
// false before true, strings as strings.
func sortLabels(l []string, typ string) {
	sort.Slice(l, func(a, b int) bool {
		if typ == "INT64" {
			x, _ := strconv.ParseInt(l[a], 10, 64)
			y, _ := strconv.ParseInt(l[b], 10, 64)
			return x < y
		}
		return l[a] < l[b]
	})
}

// encodeFeature is a feature value's encoded columns.
func encodeFeature(ft mlFeature, v *string) []float64 {
	if ft.numeric() {
		if v == nil || ft.Stddev == 0 {
			return []float64{0}
		}
		x, _ := strconv.ParseFloat(*v, 64)
		return []float64{(x - ft.Mean) / ft.Stddev}
	}
	out := make([]float64, ft.dims())
	if v == nil {
		if ft.NullSeen {
			out[len(out)-1] = 1
		}
		return out
	}
	if i := sort.SearchStrings(ft.Categories, *v); i < len(ft.Categories) && ft.Categories[i] == *v {
		out[i] = 1
	}
	return out
}

// stats are vals' mean, sample stddev, min, max and median.
func stats(vals []float64) (mean, sd, lo, hi, med float64) {
	if len(vals) == 0 {
		return
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	lo, hi = s[0], s[len(s)-1]
	if n := len(s); n%2 == 1 {
		med = s[n/2]
	} else {
		med = (s[n/2-1] + s[n/2]) / 2
	}
	for _, v := range s {
		mean += v
	}
	mean /= float64(len(s))
	if len(s) > 1 {
		for _, v := range s {
			sd += (v - mean) * (v - mean)
		}
		sd = math.Sqrt(sd / float64(len(s)-1))
	}
	return
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

func softThreshold(v, t float64) float64 {
	switch {
	case v > t:
		return v - t
	case v < -t:
		return v + t
	}
	return 0
}

// mlLoss is the training loss of w: the mean squared error or mean log
// loss, plus the regularization of every weight but the intercept.
func mlLoss(w []float64, X [][]float64, Y []float64, logistic bool, l1, l2 float64) float64 {
	var s float64
	for i, x := range X {
		z := dot(w, x)
		if logistic {
			p := math.Min(math.Max(sigmoid(z), 1e-15), 1-1e-15)
			s -= Y[i]*math.Log(p) + (1-Y[i])*math.Log(1-p)
		} else {
			s += (z - Y[i]) * (z - Y[i])
		}
	}
	s /= float64(len(X))
	for j := 1; j < len(w); j++ {
		s += l2*w[j]*w[j] + l1*math.Abs(w[j])
	}
	return s
}

// mlGradient is the gradient of mlLoss's smooth part.
func mlGradient(w []float64, X [][]float64, Y []float64, logistic bool, l2 float64) []float64 {
	g := make([]float64, len(w))
	for i, x := range X {
		z := dot(w, x)
		var r float64
		if logistic {
			r = sigmoid(z) - Y[i]
		} else {
			r = 2 * (z - Y[i])
		}
		for j := range x {
			g[j] += r * x[j]
		}
	}
	for j := range g {
		g[j] /= float64(len(X))
		if j > 0 {
			g[j] += 2 * l2 * w[j]
		}
	}
	return g
}

// normalEquation solves (XᵀX/n + l2·D) w = XᵀY/n, D the identity but for
// the intercept, for the minimum-norm w (a symmetric eigendecomposition,
// so collinear one-hot columns have a solution).
func normalEquation(X [][]float64, Y []float64, l2 float64) []float64 {
	d := len(X[0])
	n := float64(len(X))
	A := make([][]float64, d)
	for i := range A {
		A[i] = make([]float64, d)
	}
	b := make([]float64, d)
	for k, x := range X {
		for i := 0; i < d; i++ {
			if x[i] == 0 {
				continue
			}
			b[i] += x[i] * Y[k] / n
			for j := 0; j < d; j++ {
				A[i][j] += x[i] * x[j] / n
			}
		}
	}
	for i := 1; i < d; i++ {
		A[i][i] += l2
	}
	vals, vecs := jacobiEigen(A)
	maxv := 0.0
	for _, v := range vals {
		maxv = math.Max(maxv, math.Abs(v))
	}
	w := make([]float64, d)
	for k, lam := range vals {
		if math.Abs(lam) <= 1e-10*maxv {
			continue
		}
		var c float64
		for i := 0; i < d; i++ {
			c += vecs[i][k] * b[i]
		}
		c /= lam
		for i := 0; i < d; i++ {
			w[i] += c * vecs[i][k]
		}
	}
	return w
}

// jacobiEigen is the eigendecomposition of the symmetric matrix a: its
// eigenvalues and, in columns, its eigenvectors.
func jacobiEigen(a [][]float64) ([]float64, [][]float64) {
	n := len(a)
	A := make([][]float64, n)
	V := make([][]float64, n)
	for i := range a {
		A[i] = append([]float64(nil), a[i]...)
		V[i] = make([]float64, n)
		V[i][i] = 1
	}
	for sweep := 0; sweep < 100; sweep++ {
		off := 0.0
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				off += A[i][j] * A[i][j]
			}
		}
		if off < 1e-30 {
			break
		}
		for p := 0; p < n; p++ {
			for q := p + 1; q < n; q++ {
				if math.Abs(A[p][q]) < 1e-300 {
					continue
				}
				theta := (A[q][q] - A[p][p]) / (2 * A[p][q])
				t := 1 / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				if theta < 0 {
					t = -t
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < n; k++ {
					akp, akq := A[k][p], A[k][q]
					A[k][p], A[k][q] = c*akp-s*akq, s*akp+c*akq
				}
				for k := 0; k < n; k++ {
					apk, aqk := A[p][k], A[q][k]
					A[p][k], A[q][k] = c*apk-s*aqk, s*apk+c*aqk
				}
				for k := 0; k < n; k++ {
					vkp, vkq := V[k][p], V[k][q]
					V[k][p], V[k][q] = c*vkp-s*vkq, s*vkp+c*vkq
				}
			}
		}
	}
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = A[i][i]
	}
	return vals, V
}

func f64(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// regressionMetrics are ML.EVALUATE's columns for a linear model
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-evaluate):
// mean_absolute_error, mean_squared_error, mean_squared_log_error (NULL
// when a label or prediction is -1 or less), median_absolute_error (the
// exact median; BigQuery's is approximate), r2_score and
// explained_variance.
func regressionMetrics(y, p []float64) map[string]*float64 {
	n := float64(len(y))
	var mae, mse, msle, my, mr float64
	logOK := true
	abs := make([]float64, len(y))
	for i := range y {
		d := y[i] - p[i]
		abs[i] = math.Abs(d)
		mae += abs[i]
		mse += d * d
		if y[i] <= -1 || p[i] <= -1 {
			logOK = false
		} else {
			l := math.Log1p(y[i]) - math.Log1p(p[i])
			msle += l * l
		}
		my += y[i]
		mr += d
	}
	mae, mse, msle, my, mr = mae/n, mse/n, msle/n, my/n, mr/n
	var ssTot, varRes float64
	for i := range y {
		ssTot += (y[i] - my) * (y[i] - my)
		d := y[i] - p[i] - mr
		varRes += d * d
	}
	_, _, _, _, med := stats(abs)
	m := map[string]*float64{
		"mean_absolute_error":   f64(mae),
		"mean_squared_error":    f64(mse),
		"median_absolute_error": f64(med),
		"r2_score":              f64(1 - mse*n/ssTot),
		"explained_variance":    f64(1 - varRes/ssTot),
	}
	if logOK {
		m["mean_squared_log_error"] = f64(msle)
	} else {
		m["mean_squared_log_error"] = nil
	}
	return m
}

// regressionMetricNames are their columns, in BigQuery's order.
var regressionMetricNames = []string{"mean_absolute_error", "mean_squared_error", "mean_squared_log_error",
	"median_absolute_error", "r2_score", "explained_variance"}

// classificationMetricNames are a binary logistic model's ML.EVALUATE
// columns, in BigQuery's order.
var classificationMetricNames = []string{"precision", "recall", "accuracy", "f1_score", "log_loss", "roc_auc"}

// classificationMetrics are a binary model's metrics at threshold, y 0 or
// 1 and p the positive class's probability. roc_auc is the exact area
// (ties counted half); BigQuery's is computed over bucketed thresholds.
func classificationMetrics(y, p []float64, threshold float64) map[string]*float64 {
	var tp, fp, tn, fn, ll float64
	for i := range y {
		pred := p[i] >= threshold
		switch {
		case pred && y[i] == 1:
			tp++
		case pred:
			fp++
		case y[i] == 1:
			fn++
		default:
			tn++
		}
		q := math.Min(math.Max(p[i], 1e-15), 1-1e-15)
		ll -= y[i]*math.Log(q) + (1-y[i])*math.Log(1-q)
	}
	n := float64(len(y))
	prec, rec := tp/(tp+fp), tp/(tp+fn)
	m := map[string]*float64{
		"precision": f64(prec),
		"recall":    f64(rec),
		"accuracy":  f64((tp + tn) / n),
		"f1_score":  f64(2 * prec * rec / (prec + rec)),
		"log_loss":  f64(ll / n),
		"roc_auc":   f64(rocAUC(y, p)),
	}
	return m
}

func rocAUC(y, p []float64) float64 {
	type sc struct{ p, y float64 }
	s := make([]sc, len(y))
	for i := range y {
		s[i] = sc{p[i], y[i]}
	}
	sort.Slice(s, func(a, b int) bool { return s[a].p < s[b].p })
	var pos, neg, rankSum float64
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j].p == s[i].p {
			j++
		}
		avg := float64(i+j+1) / 2 // 1-based average rank of the tie
		for k := i; k < j; k++ {
			if s[k].y == 1 {
				rankSum += avg
				pos++
			} else {
				neg++
			}
		}
		i = j
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	return (rankSum - pos*(pos+1)/2) / (pos * neg)
}

// predictRow is a trained model's prediction for one row's feature
// values, by name (a missing name is NULL): the linear value, or the
// positive class's probability.
func (t mlTrained) predictRow(vals map[string]*string) float64 {
	z := t.Intercept
	for _, ft := range t.Features {
		x := encodeFeature(ft, vals[strings.ToLower(ft.Name)])
		if ft.numeric() {
			z += ft.Weight * x[0]
			continue
		}
		for i, v := range x {
			z += ft.CategoryWeights[i] * v
		}
	}
	if len(t.Classes) == 2 {
		return sigmoid(z)
	}
	return z
}
