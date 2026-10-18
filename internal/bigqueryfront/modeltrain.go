package bigqueryfront

import (
	"fmt"
	"math"
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
// needed no halving; L1 is a proximal (soft-threshold) step; the text
// whose FARM_FINGERPRINT places a row in a RANDOM split (BigQuery says
// only that it is "based on the FARM_FINGERPRINT of the data (including
// the column name and schema)", rowText, fingerprinted by the engine), so the split is
// deterministic, as documented, but not Google's rows; a one-hot column set that is collinear with the intercept
// is solved for the minimum-norm weights. The weights so differ from
// Google's in their last digits, and more where the problem is
// ill-conditioned or gradient descent stops early.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-glm

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
	// CategoryEncoding is category_encoding_method as given ("" when not
	// given: a boosted tree's default, LABEL_ENCODING, and the other
	// models' ONE_HOT_ENCODING).
	CategoryEncoding string `json:"categoryEncodingMethod,omitempty"`
	// KMEANS (modelcluster.go): NumClusters 0 is the documented default,
	// log10 of the training rows.
	NumClusters   int    `json:"numClusters,omitempty"`
	KMeansInit    string `json:"kmeansInitMethod,omitempty"`
	KMeansInitCol string `json:"kmeansInitCol,omitempty"`
	DistanceType  string `json:"distanceType,omitempty"`
	Standardize   bool   `json:"standardizeFeatures"`
	// PCA (modelcluster.go).
	NumPC         int     `json:"numPrincipalComponents,omitempty"`
	PCAVarRatio   float64 `json:"pcaExplainedVarianceRatio,omitempty"`
	ScaleFeatures bool    `json:"scaleFeatures"`
	PCASolver     string  `json:"pcaSolver,omitempty"`
	// BOOSTED_TREE_* (modeltree.go).
	MaxTreeDepth   int     `json:"maxTreeDepth,omitempty"`
	MinSplitLoss   float64 `json:"minSplitLoss,omitempty"`
	MinChildWeight float64 `json:"minTreeChildWeight,omitempty"`
	BoosterType    string  `json:"boosterType,omitempty"`
	// The sampling of RANDOM_FOREST_* (modeltree.go); a boosted tree
	// model's are 1.
	NumParallelTree int     `json:"numParallelTree,omitempty"`
	Subsample       float64 `json:"subsample,omitempty"`
	ColsampleTree   float64 `json:"colsampleBytree,omitempty"`
	ColsampleLevel  float64 `json:"colsampleBylevel,omitempty"`
	ColsampleNode   float64 `json:"colsampleBynode,omitempty"`
	TreeMethod      string  `json:"treeMethod,omitempty"`
	// MATRIX_FACTORIZATION (modelmf.go).
	NumFactors   int    `json:"numFactors,omitempty"`
	FeedbackType string `json:"feedbackType,omitempty"`
	UserCol      string `json:"userCol,omitempty"`
	ItemCol      string `json:"itemCol,omitempty"`
	RatingCol    string `json:"ratingCol,omitempty"`
	// DNN_* (modeldnn.go): HiddenUnits nil is the documented default,
	// one layer sized by the rows; BatchSize 0 min(32, rows).
	HiddenUnits []int   `json:"hiddenUnits,omitempty"`
	Activation  string  `json:"activationFn,omitempty"`
	Dropout     float64 `json:"dropout,omitempty"`
	BatchSize   int     `json:"batchSize,omitempty"`
	Optimizer   string  `json:"optimizer,omitempty"`
	// #1562: class_weights by label, auto_class_weights, and an
	// AUTOENCODER's l1_reg_activation (modeldnn.go).
	ClassWeights     map[string]float64 `json:"classWeights,omitempty"`
	AutoClassWeights bool               `json:"autoClassWeights,omitempty"`
	L1Activation     float64            `json:"l1RegActivation,omitempty"`
	// GlobalExplain is enable_global_explain (modelexplain.go).
	GlobalExplain bool `json:"enableGlobalExplain,omitempty"`
	// ARIMA_PLUS (modelarima.go).
	TS tsOptions `json:"timeSeries"`
	// Set are the options CREATE MODEL gave, lower case.
	Set []string `json:"set,omitempty"`
}

// given reports whether CREATE MODEL gave option name.
func (o mlOptions) given(name string) bool {
	for _, s := range o.Set {
		if s == name {
			return true
		}
	}
	return false
}

// defaultMLOptions are BigQuery's documented defaults.
func defaultMLOptions() mlOptions {
	return mlOptions{Strategy: "AUTO_STRATEGY", MaxIterations: 20, LearnRateStrat: "LINE_SEARCH", LearnRate: 0.1,
		InitLearnRate: 0.1, EarlyStop: true, MinRelProgress: 0.01, SplitMethod: "AUTO_SPLIT", SplitEvalFrac: 0.2,
		KMeansInit: "RANDOM", DistanceType: "EUCLIDEAN", Standardize: true, ScaleFeatures: true, PCASolver: "AUTO"}
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
	// LabelEncoded is a categorical feature of a tree model label encoded
	// (LABEL_ENCODING, labelEncode): one column, the category's index.
	LabelEncoded bool `json:"labelEncoded,omitempty"`
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
	if ft.numeric() || ft.LabelEncoded {
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
	// ClassWeights are a multiclass logistic model's weights, one vector
	// a class (in Classes' order), each laid out as the encoded row:
	// the intercept, then each feature's columns.
	ClassWeights [][]float64 `json:"classWeights,omitempty"`
	// Scaling is how numeric features are encoded: "" standardized,
	// "raw" as they are, "center" less their mean (modelcluster.go).
	Scaling string `json:"scaling,omitempty"`
	// KMEANS: the centroids in the encoded space, their row counts, and
	// the distance type.
	Centroids    [][]float64 `json:"centroids,omitempty"`
	ClusterSizes []int       `json:"clusterSizes,omitempty"`
	Distance     string      `json:"distance,omitempty"`
	// PCA: the components (rows, in the encoded space), their
	// eigenvalues, and the sum of every eigenvalue.
	Components    [][]float64 `json:"components,omitempty"`
	Eigenvalues   []float64   `json:"eigenvalues,omitempty"`
	TotalVariance float64     `json:"totalVariance,omitempty"`
	// EvalY and EvalScore are a classifier's evaluation rows (as Eval
	// was computed on), for ML.CONFUSION_MATRIX and ML.ROC_CURVE with no
	// input: the class index, and the positive class's probability
	// (binary) or the predicted class index (multiclass).
	EvalY     []float64 `json:"evalY,omitempty"`
	EvalScore []float64 `json:"evalScore,omitempty"`
	// Trees are a boosted tree model's trees, BaseScore its starting score
	// (modeltree.go).
	Trees     [][]mlNode `json:"trees,omitempty"`
	TreeClass []int      `json:"treeClass,omitempty"`
	BaseScore float64    `json:"baseScore,omitempty"`
	// DNN is a DNN_* model's network (modeldnn.go).
	DNN *dnnTrained `json:"dnn,omitempty"`
	// MF is a MATRIX_FACTORIZATION model's weights (modelmf.go).
	MF *mfTrained `json:"mf,omitempty"`
	// Global are ML.GLOBAL_EXPLAIN's rows (modelexplain.go).
	Global []mlGlobal `json:"global,omitempty"`
	// Arima is an ARIMA_PLUS model's series and fits (modelarima.go).
	Arima *arimaTrained `json:"arima,omitempty"`
}

// multiclass reports whether t is a multiclass logistic model.
func (t mlTrained) multiclass() bool { return len(t.Classes) > 2 }

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
// value a string or nil as the REST API gives it; fp fingerprints the rows
// of a RANDOM split.
func trainModel(opts mlOptions, cols []mlColumn, rows [][]*string, fp fingerprinter) (mlTrained, *mlFail) {
	switch opts.ModelType {
	case "KMEANS":
		return trainKMeans(opts, cols, rows)
	case "PCA":
		return trainPCA(opts, cols, rows)
	case "MATRIX_FACTORIZATION":
		return trainMF(opts, cols, rows)
	case "ARIMA_PLUS":
		return trainArima(opts, cols, rows)
	case "AUTOENCODER":
		return trainAutoencoder(opts, cols, rows)
	}
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
	logistic := isClassifierType(opts.ModelType)
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
		ft := mlFeature{Name: c.Name, Type: ty}
		ft.LabelEncoded = !ft.numeric() && isTreeModel(opts.ModelType) && opts.CategoryEncoding != "ONE_HOT_ENCODING"
		t.Features = append(t.Features, ft)
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
		if len(t.Classes) < 2 {
			return t, mlInvalid("Classification model requires at least 2 unique labels and the label column had only %d unique label", len(t.Classes))
		}
		if len(t.Classes) > 50 {
			return t, mlInvalid("Logistic regression supports at most 50 unique labels; the label column had %d", len(t.Classes))
		}
		for i, row := range kept {
			for k, c := range t.Classes {
				if *row[li] == c {
					y[i] = float64(k)
				}
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
		split, fe := randomSplit(cols, kept, frac, opts.SplitMethod == "AUTO_SPLIT" && len(kept) > 50000, fp)
		if fe != nil {
			return t, fe
		}
		copy(isEval, split)
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
	if fe := learnFeatures(t.Features, fidx, kept, train); fe != nil {
		return t, fe
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
	if isTreeModel(opts.ModelType) {
		K := 1
		if len(t.Classes) > 2 {
			K = len(t.Classes)
		}
		t.trainBoosted(opts, X, Y, EX, EY, logistic, K)
		t.computeGlobal(opts, evalRows(X, EX))
		return t, nil
	}
	if isDNN(opts.ModelType) {
		K := 1
		if len(t.Classes) > 2 {
			K = len(t.Classes)
		}
		if fe := t.trainDNN(opts, X, Y, EX, EY, logistic, K); fe != nil {
			return t, fe
		}
		return t, nil
	}
	dim := len(X[0])
	multi := logistic && len(t.Classes) > 2
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
	K := len(t.Classes)
	loss := func(w []float64, X [][]float64, Y []float64) float64 {
		if multi {
			return softmaxLoss(w, X, Y, K, opts.L1, opts.L2)
		}
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
		params := dim
		if multi {
			params = K * dim
		}
		var evalLoss func([]float64) float64
		if len(EX) > 0 {
			evalLoss = func(w []float64) float64 { return loss(w, EX, EY) }
		}
		w, t.Iterations = descend(params, opts, func(w []float64) float64 { return loss(w, X, Y) }, evalLoss,
			func(w []float64) []float64 {
				if multi {
					return softmaxGradient(w, X, Y, K, opts.L2)
				}
				return mlGradient(w, X, Y, logistic, opts.L2)
			}, func(j int) bool { return j%dim != 0 })
	}
	// BigQuery lists training iterations newest first.
	sort.Slice(t.Iterations, func(a, b int) bool { return t.Iterations[a].Index > t.Iterations[b].Index })
	if multi {
		for k := 0; k < K; k++ {
			t.ClassWeights = append(t.ClassWeights, append([]float64(nil), w[k*dim:(k+1)*dim]...))
		}
	} else {
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
	}
	// The evaluation metrics.
	evX, evY := EX, EY
	if len(evX) == 0 {
		evX, evY = X, Y
	}
	switch {
	case multi:
		probs := make([][]float64, len(evX))
		for i, x := range evX {
			probs[i] = softmax(t.ClassWeights, x)
		}
		t.Eval = multiclassMetrics(evY, probs)
		t.EvalY = evY
		for _, p := range probs {
			t.EvalScore = append(t.EvalScore, float64(argmax(p)))
		}
	default:
		pred := make([]float64, len(evX))
		for i, x := range evX {
			pred[i] = dot(w, x)
			if logistic {
				pred[i] = sigmoid(pred[i])
			}
		}
		if logistic {
			t.Eval = classificationMetrics(evY, pred, 0.5)
			t.EvalY, t.EvalScore = evY, pred
		} else {
			t.Eval = regressionMetrics(evY, pred)
		}
	}
	t.computeGlobal(opts, evalRows(X, EX))
	return t, nil
}

// evalRows are the evaluation rows EX, or the training rows X with no
// split.
func evalRows(X, EX [][]float64) [][]float64 {
	if len(EX) > 0 {
		return EX
	}
	return X
}

// learnFeatures learns each feature's statistics or categories over the
// rows use of rows, fidx the features' columns.
func learnFeatures(fts []mlFeature, fidx []int, rows [][]*string, use []int) *mlFail {
	for k := range fts {
		ft := &fts[k]
		var vals []float64
		cats := map[string]bool{}
		for _, i := range use {
			v := rows[i][fidx[k]]
			if v == nil {
				ft.NullCount++
				continue
			}
			if ft.numeric() {
				x, err := strconv.ParseFloat(*v, 64)
				if err != nil {
					return mlInvalid("Feature value %q of %s is not numeric", *v, ft.Name)
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
	return nil
}

// descend is batch gradient descent over params parameters from zero,
// with opts' learn rate strategy, early stop and L1 (a proximal step on
// the parameters regularized reports), loss and grad the training loss
// and its smooth part's gradient, evalLoss the evaluation loss or nil.
func descend(params int, opts mlOptions, loss func([]float64) float64, evalLoss func([]float64) float64,
	grad func([]float64) []float64, regularized func(int) bool) ([]float64, []mlIteration) {
	var its []mlIteration
	w := make([]float64, params)
	rate := opts.InitLearnRate
	if opts.LearnRateStrat == "CONSTANT" {
		rate = opts.LearnRate
	}
	prev := loss(w)
	prevEval := prev
	if evalLoss != nil {
		prevEval = evalLoss(w)
	}
	for iter := 0; iter < opts.MaxIterations; iter++ {
		itStart := time.Now()
		g := grad(w)
		step := func(rate float64) []float64 {
			nw := make([]float64, params)
			for j := range w {
				nw[j] = w[j] - rate*g[j]
				if opts.L1 > 0 && regularized(j) {
					nw[j] = softThreshold(nw[j], rate*opts.L1)
				}
			}
			return nw
		}
		nw := step(rate)
		cur := loss(nw)
		if opts.LearnRateStrat != "CONSTANT" {
			halved := false
			for k := 0; k < 40 && cur > prev; k++ {
				rate /= 2
				halved = true
				nw = step(rate)
				cur = loss(nw)
			}
			its = append(its, mlIteration{Index: iter, Loss: cur, LearnRate: rate})
			if !halved {
				rate *= 2
			}
		} else {
			its = append(its, mlIteration{Index: iter, Loss: cur, LearnRate: rate})
		}
		w = nw
		last := &its[len(its)-1]
		cmp, prevCmp := cur, prev
		if evalLoss != nil {
			e := evalLoss(w)
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
	return w, its
}

// softmax is the class probabilities of the encoded row x under the
// class weights cw.
func softmax(cw [][]float64, x []float64) []float64 {
	z := make([]float64, len(cw))
	m := math.Inf(-1)
	for k := range cw {
		z[k] = dot(cw[k], x)
		m = math.Max(m, z[k])
	}
	var sum float64
	for k := range z {
		z[k] = math.Exp(z[k] - m)
		sum += z[k]
	}
	for k := range z {
		z[k] /= sum
	}
	return z
}

// splitClasses is w, K class vectors laid end to end, as one a class.
func splitClasses(w []float64, K int) [][]float64 {
	d := len(w) / K
	out := make([][]float64, K)
	for k := range out {
		out[k] = w[k*d : (k+1)*d]
	}
	return out
}

// softmaxLoss is a multiclass model's training loss: the mean cross
// entropy, plus the regularization of every weight but the intercepts.
func softmaxLoss(w []float64, X [][]float64, Y []float64, K int, l1, l2 float64) float64 {
	cw := splitClasses(w, K)
	var s float64
	for i, x := range X {
		p := softmax(cw, x)[int(Y[i])]
		s -= math.Log(math.Max(p, 1e-15))
	}
	s /= float64(len(X))
	d := len(w) / K
	for j, v := range w {
		if j%d != 0 {
			s += l2*v*v + l1*math.Abs(v)
		}
	}
	return s
}

// softmaxGradient is the gradient of softmaxLoss's smooth part.
func softmaxGradient(w []float64, X [][]float64, Y []float64, K int, l2 float64) []float64 {
	cw := splitClasses(w, K)
	d := len(w) / K
	g := make([]float64, len(w))
	for i, x := range X {
		p := softmax(cw, x)
		for k := 0; k < K; k++ {
			r := p[k]
			if int(Y[i]) == k {
				r--
			}
			for j := range x {
				g[k*d+j] += r * x[j]
			}
		}
	}
	for j := range g {
		g[j] /= float64(len(X))
		if j%d != 0 {
			g[j] += 2 * l2 * w[j]
		}
	}
	return g
}

// multiclassMetrics are a multiclass model's ML.EVALUATE metrics, y the
// class indexes and probs each row's class probabilities, the predicted
// class the most probable (the first on a tie): precision, recall,
// f1_score, log_loss and roc_auc macro-averaged over the classes, each
// class one against the rest, and accuracy the fraction predicted right,
// as BigQuery documents. A class's metric that is undefined (no row
// predicted or labeled it) is left out of the average (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-evaluate
func multiclassMetrics(y []float64, probs [][]float64) map[string]*float64 {
	K := len(probs[0])
	pred := make([]int, len(y))
	right := 0
	for i, p := range probs {
		pred[i] = argmax(p)
		if pred[i] == int(y[i]) {
			right++
		}
	}
	var sums [5]float64
	var counts [5]int
	add := func(m int, v float64) {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			sums[m] += v
			counts[m]++
		}
	}
	for k := 0; k < K; k++ {
		var tp, fp, fn, ll float64
		yk := make([]float64, len(y))
		pk := make([]float64, len(y))
		for i := range y {
			is := int(y[i]) == k
			if is {
				yk[i] = 1
			}
			pk[i] = probs[i][k]
			switch {
			case pred[i] == k && is:
				tp++
			case pred[i] == k:
				fp++
			case is:
				fn++
			}
			q := math.Min(math.Max(pk[i], 1e-15), 1-1e-15)
			ll -= yk[i]*math.Log(q) + (1-yk[i])*math.Log(1-q)
		}
		prec, rec := tp/(tp+fp), tp/(tp+fn)
		add(0, prec)
		add(1, rec)
		add(2, 2*prec*rec/(prec+rec))
		add(3, ll/float64(len(y)))
		add(4, rocAUC(yk, pk))
	}
	avg := func(m int) *float64 {
		if counts[m] == 0 {
			return nil
		}
		return f64(sums[m] / float64(counts[m]))
	}
	return map[string]*float64{"precision": avg(0), "recall": avg(1), "accuracy": f64(float64(right) / float64(len(y))),
		"f1_score": avg(2), "log_loss": avg(3), "roc_auc": avg(4)}
}

// argmax is the index of p's largest value, the first on a tie.
func argmax(p []float64) int {
	best := 0
	for k := range p {
		if p[k] > p[best] {
			best = k
		}
	}
	return best
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

// encodeFeature is a feature value's encoded columns, numbers
// standardized.
func encodeFeature(ft mlFeature, v *string) []float64 { return encodeFeatureMode(ft, v, "") }

// encodeFeatureMode is a feature value's encoded columns, numbers as
// scaling says (mlTrained.Scaling); a NULL number takes the mean.
func encodeFeatureMode(ft mlFeature, v *string, scaling string) []float64 {
	if ft.numeric() {
		x := ft.Mean
		if v != nil {
			x, _ = strconv.ParseFloat(*v, 64)
		}
		switch scaling {
		case "raw":
			return []float64{x}
		case "center":
			return []float64{x - ft.Mean}
		}
		if ft.Stddev == 0 {
			return []float64{0}
		}
		return []float64{(x - ft.Mean) / ft.Stddev}
	}
	if ft.LabelEncoded {
		return []float64{float64(labelEncode(ft, v))}
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

// fingerprinter gives FARM_FINGERPRINT of each text, in order: the
// front's asks the engine (engineFingerprints), whose FARM_FINGERPRINT is
// FarmHash Fingerprint64, as BigQuery's hash functions page documents it.
type fingerprinter func(texts []string) ([]int64, *mlFail)

// rowText is the text a RANDOM split fingerprints for a row: each
// column's name, GoogleSQL type and quoted value (NULL unquoted), as
// `x INT64="3",s STRING=NULL`, so a split depends on the column names and
// schema as BigQuery says its split does.
func rowText(cols []mlColumn, row []*string) string {
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		ty := mlTypeOf(c.Type)
		if ty == "" {
			ty = strings.ToUpper(c.Type)
		}
		b.WriteString(c.Name + " " + ty + "=")
		if row[i] == nil {
			b.WriteString("NULL")
		} else {
			b.WriteString(strconv.Quote(*row[i]))
		}
	}
	return b.String()
}

// randomSplit is which rows a RANDOM split makes evaluation rows: a row
// is one when ABS(FARM_FINGERPRINT(row)) % 100 is below frac, rounded to
// two decimal places ("The service maintains the accuracy of the input
// value to two decimal places"), times 100, so the split is the same each
// run and its size approximate, as BigQuery documents it; count (AUTO_SPLIT
// of more than 50,000 rows, "10,000 rows are used as evaluation data")
// takes the round(frac · rows) rows of the smallest ABS(FARM_FINGERPRINT)
// instead.
func randomSplit(cols []mlColumn, rows [][]*string, frac float64, count bool, fp fingerprinter) ([]bool, *mlFail) {
	eval := make([]bool, len(rows))
	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = rowText(cols, row)
	}
	fps, fe := fp(texts)
	if fe != nil {
		return nil, fe
	}
	if len(fps) != len(rows) {
		return nil, &mlFail{code: 500, msg: fmt.Sprintf("FARM_FINGERPRINT of the split: %d values for %d rows", len(fps), len(rows))}
	}
	key := make([]uint64, len(rows))
	for i, f := range fps {
		key[i] = uint64(f)
		if f < 0 {
			key[i] = uint64(-f) // ABS; -2^63 stays 2^63
		}
	}
	if count {
		order := make([]int, len(rows))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return key[order[a]] < key[order[b]] })
		for _, i := range order[:int(math.Round(frac*float64(len(rows))))] {
			eval[i] = true
		}
		return eval, nil
	}
	pct := uint64(math.Round(frac * 100))
	for i := range rows {
		eval[i] = key[i]%100 < pct
	}
	return eval, nil
}

// labelEncode is BigQuery's label encoding of v
// (https://docs.cloud.google.com/bigquery/docs/auto-preprocessing): "an
// INT64 value in [0, <number of categories>]", the vocabulary "sorted
// alphabetically" from 1, and "NULL values and categories that aren't in
// the vocabulary are encoded to 0".
func labelEncode(ft mlFeature, v *string) int {
	if v == nil {
		return 0
	}
	if i := sort.SearchStrings(ft.Categories, *v); i < len(ft.Categories) && ft.Categories[i] == *v {
		return i + 1
	}
	return 0
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
	if t.DNN != nil {
		z := t.DNN.forward(t.encodeRow(vals)[1:])[0]
		if len(t.Classes) == 2 {
			return sigmoid(z)
		}
		return z
	}
	if len(t.Trees) > 0 {
		z := t.treeMargin(t.encodeRow(vals))
		if len(t.Classes) == 2 {
			return sigmoid(z)
		}
		return z
	}
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

// encodeRow is a row's encoded columns, the intercept's 1 first.
func (t mlTrained) encodeRow(vals map[string]*string) []float64 {
	x := []float64{1}
	for _, ft := range t.Features {
		x = append(x, encodeFeatureMode(ft, vals[strings.ToLower(ft.Name)], t.Scaling)...)
	}
	return x
}

// classProbs is a multiclass model's class probabilities for a row.
func (t mlTrained) classProbs(vals map[string]*string) []float64 {
	if t.DNN != nil {
		return softmaxOf(t.DNN.forward(t.encodeRow(vals)[1:]))
	}
	if len(t.Trees) > 0 {
		return softmaxOf(t.treeMargins(t.encodeRow(vals), len(t.Classes)))
	}
	return softmax(t.ClassWeights, t.encodeRow(vals))
}

// weightVector is a linear or binary model's weights, laid out as the
// encoded row.
func (t mlTrained) weightVector() []float64 {
	w := []float64{t.Intercept}
	for _, ft := range t.Features {
		if ft.numeric() {
			w = append(w, ft.Weight)
		} else {
			w = append(w, ft.CategoryWeights...)
		}
	}
	return w
}
