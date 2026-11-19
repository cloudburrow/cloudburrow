package bigqueryfront

import (
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
)

// AutoML models, AUTOML_REGRESSOR and AUTOML_CLASSIFIER (#1776,
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-automl),
// trained locally.
//
// As documented: the options are MODEL_TYPE, BUDGET_HOURS (FLOAT64, 1.0
// to 72.0, default 1.0), OPTIMIZATION_OBJECTIVE (regression MINIMIZE_RMSE,
// the default, MINIMIZE_MAE, MINIMIZE_RMSLE; binary classification
// MAXIMIZE_AU_ROC, the default, MINIMIZE_LOG_LOSS, MAXIMIZE_AU_PRC,
// MAXIMIZE_PRECISION_AT_RECALL and MAXIMIZE_RECALL_AT_PRECISION, the last
// two also as STRUCT(objective, value) with the value in [0, 1];
// multiclass MINIMIZE_LOG_LOSS), INPUT_LABEL_COLS (one column, default
// label; a regressor's numeric, a classifier's of up to 1,000 values),
// DATA_SPLIT_COL (a STRING column of TRAIN, VALIDATE, TEST and UNASSIGNED,
// or a TIMESTAMP column, a chronological split; by default a random split)
// and KMS_KEY_NAME; the input has 1,000 to 200,000,000 rows; feature names
// are at most 125 characters. The functions are ML.PREDICT, ML.EVALUATE,
// ML.CONFUSION_MATRIX (a classifier's), ML.ROC_CURVE (a binary
// classifier's), ML.FEATURE_INFO, ML.TRAINING_INFO and ML.GLOBAL_EXPLAIN
// (https://docs.cloud.google.com/bigquery/docs/e2e-journey); there is no
// ML.WEIGHTS or ML.EXPLAIN_PREDICT.
//
// UNVERIFIED, chosen here (docs re-read 2026-11-18: AutoML "automatically
// performs feature engineering" and "hyperparameter tuning" on Vertex AI,
// whose model search Google does not document):
//   - the search is over CloudBurrow's own trainers: a regressor's
//     LINEAR_REG, BOOSTED_TREE_REGRESSOR, RANDOM_FOREST_REGRESSOR and
//     DNN_REGRESSOR, a classifier's LOGISTIC_REG, BOOSTED_TREE_CLASSIFIER,
//     RANDOM_FOREST_CLASSIFIER and DNN_CLASSIFIER, each with its
//     documented defaults and, for the boosted trees and the network, a
//     second trial with more capacity (max_tree_depth 10 and 50
//     iterations; hidden_units [64, 32]); every trial runs, whatever
//     BUDGET_HOURS (at least one hour, ample for these six), so the same
//     data makes the same model on any machine: a cut at a share of the
//     budget in wall-clock time chose by the machine's speed (main,
//     Landing 153: one trial under -race on macOS); ties keep the earlier
//     trial;
//   - each trial trains on the TRAIN rows and is scored by
//     OPTIMIZATION_OBJECTIVE on the VALIDATE rows; the best is trained
//     again on TRAIN and VALIDATE, and evaluated on TEST, which ML.EVALUATE
//     with no input answers;
//   - the default split is 80/10/10, a row's part by the FNV-1a hash of its
//     text; UNASSIGNED rows are split so; a TIMESTAMP split column orders
//     the rows, the first 80% TRAIN, the next 10% VALIDATE, the last TEST;
//   - the precision-at-recall and recall-at-precision objectives without
//     a value use 0.5; the score is the best precision (recall) at a
//     threshold whose recall (precision) is at least the value;
//   - the model's REST trainingOptions are budgetHours, inputLabelColumns
//     and dataSplitColumn (the REST TrainingOptions has no field for the
//     objective);
//     ML.TRAINING_INFO answers the chosen trial's iterations;
//   - fewer than 1,000 rows, a label of more than 1,000 values and a
//     feature name over 125 characters are 400, with messages chosen here.

var automlObjectives = map[string][]string{
	"regressor":  {"MINIMIZE_RMSE", "MINIMIZE_MAE", "MINIMIZE_RMSLE"},
	"binary":     {"MAXIMIZE_AU_ROC", "MINIMIZE_LOG_LOSS", "MAXIMIZE_AU_PRC", "MAXIMIZE_PRECISION_AT_RECALL", "MAXIMIZE_RECALL_AT_PRECISION"},
	"multiclass": {"MINIMIZE_LOG_LOSS"},
}

const (
	automlMinRows    = 1000
	automlMaxClasses = 1000
	automlMaxName    = 125
)

// automlOptions are an AutoML model's own options.
type automlOptions struct {
	// Type is AUTOML_REGRESSOR or AUTOML_CLASSIFIER.
	Type        string   `json:"type"`
	BudgetHours float64  `json:"budgetHours"`
	Objective   string   `json:"optimizationObjective,omitempty"`
	ObjValue    *float64 `json:"objectiveValue,omitempty"`
	// Chosen is the trial chosen, Trials every trial's model type and
	// score.
	Chosen string        `json:"chosen"`
	Trials []automlTrial `json:"trials"`
}

type automlTrial struct {
	ModelType string  `json:"modelType"`
	Variant   string  `json:"variant,omitempty"`
	Score     float64 `json:"score"`
}

func isAutoML(typ string) bool { return typ == "AUTOML_REGRESSOR" || typ == "AUTOML_CLASSIFIER" }

// parseAutoMLObjective reads OPTIMIZATION_OBJECTIVE: a string, or
// STRUCT('MAXIMIZE_PRECISION_AT_RECALL' | 'MAXIMIZE_RECALL_AT_PRECISION', value).
func parseAutoMLObjective(val []token, o *mlOptions) (int, string) {
	bad := func() (int, string) { return 400, "Invalid value for option optimization_objective" }
	if len(val) == 1 && val[0].kind == tokString {
		s, ok := unquoteString(val[0].text)
		if !ok {
			return bad()
		}
		o.Objective = strings.ToUpper(s)
		return 0, ""
	}
	if len(val) < 5 || !val[0].is("STRUCT") || !val[1].punct("(") || !val[len(val)-1].punct(")") || val[2].kind != tokString || !val[3].punct(",") {
		return bad()
	}
	s, ok := unquoteString(val[2].text)
	if !ok {
		return bad()
	}
	var num string
	for _, t := range val[4 : len(val)-1] {
		num += t.text
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return bad()
	}
	o.Objective = strings.ToUpper(s)
	if o.Objective != "MAXIMIZE_PRECISION_AT_RECALL" && o.Objective != "MAXIMIZE_RECALL_AT_PRECISION" {
		return 400, "optimization_objective " + o.Objective + " takes no value; only MAXIMIZE_PRECISION_AT_RECALL and MAXIMIZE_RECALL_AT_PRECISION do"
	}
	if v < 0 || v > 1 {
		return 400, "The value of optimization_objective " + o.Objective + " must be in [0, 1]"
	}
	o.ObjValue = &v
	return 0, ""
}

// checkAutoMLOptions checks an AutoML model's options as documented.
func checkAutoMLOptions(o *mlOptions) (int, string) {
	if !o.given("budget_hours") {
		o.BudgetHours = 1
	}
	if o.BudgetHours < 1 || o.BudgetHours > 72 {
		return 400, "budget_hours must be between 1.0 and 72.0"
	}
	if o.Objective != "" {
		var ok bool
		kinds := []string{"binary", "multiclass"}
		if o.ModelType == "AUTOML_REGRESSOR" {
			kinds = []string{"regressor"}
		}
		for _, k := range kinds {
			for _, v := range automlObjectives[k] {
				ok = ok || v == o.Objective
			}
		}
		if !ok {
			return 400, "Invalid optimization_objective " + o.Objective + " for model type " + o.ModelType
		}
	}
	if len(o.LabelCols) > 1 {
		return 400, "Only one input label column is supported for model type " + o.ModelType
	}
	return 0, ""
}

// automlCandidates are the trials' options, in order.
func automlCandidates(o mlOptions) []struct {
	opts    mlOptions
	variant string
} {
	types := []string{"LINEAR_REG", "BOOSTED_TREE_REGRESSOR", "RANDOM_FOREST_REGRESSOR", "DNN_REGRESSOR"}
	if o.ModelType == "AUTOML_CLASSIFIER" {
		types = []string{"LOGISTIC_REG", "BOOSTED_TREE_CLASSIFIER", "RANDOM_FOREST_CLASSIFIER", "DNN_CLASSIFIER"}
	}
	var out []struct {
		opts    mlOptions
		variant string
	}
	add := func(typ, variant string, tweak func(*mlOptions)) {
		c := defaultMLOptions()
		c.ModelType, c.LabelCols, c.GlobalExplain = typ, o.LabelCols, true
		c.SplitMethod, c.SplitCol = "CUSTOM", automlSplitCol
		if isTreeModel(typ) {
			applyTreeDefaults(&c)
		}
		if isNetwork(typ) {
			applyDNNDefaults(&c)
		}
		if tweak != nil {
			tweak(&c)
		}
		out = append(out, struct {
			opts    mlOptions
			variant string
		}{c, variant})
	}
	for _, typ := range types {
		add(typ, "", nil)
	}
	add(types[1], "max_tree_depth=10, max_iterations=50", func(c *mlOptions) { c.MaxTreeDepth, c.MaxIterations = 10, 50 })
	add(types[3], "hidden_units=[64, 32]", func(c *mlOptions) { c.HiddenUnits = []int{64, 32} })
	return out
}

// automlSplitCol is the column the trials' CUSTOM split reads: TRUE for
// the rows they are evaluated on.
const automlSplitCol = "_cbml_automl_eval"

// trainAutoML trains an AutoML model: the trials, then the chosen one.
func trainAutoML(opts mlOptions, cols []mlColumn, rows [][]*string, fp fingerprinter) (mlTrained, *mlFail) {
	var t mlTrained
	label := "label"
	if len(opts.LabelCols) == 1 {
		label = opts.LabelCols[0]
	}
	li, si := -1, -1
	for i, c := range cols {
		if strings.EqualFold(c.Name, label) {
			li = i
		}
		if opts.SplitCol != "" && strings.EqualFold(c.Name, opts.SplitCol) {
			si = i
		}
		if len(c.Name) > automlMaxName {
			return t, mlInvalid("Feature column names must be at most %d characters for model type %s: %s", automlMaxName, opts.ModelType, c.Name)
		}
	}
	if li < 0 {
		return t, mlInvalid("Unable to identify the label column '%s' in the input data", label)
	}
	if opts.SplitCol != "" && si < 0 {
		return t, mlInvalid("Unable to find the data split column '%s' in the input data", opts.SplitCol)
	}
	var kept [][]*string
	for _, row := range rows {
		if row[li] != nil {
			kept = append(kept, row)
		}
	}
	if len(kept) < automlMinRows {
		return t, mlInvalid("AutoML models require at least %d rows of input data; the input has %d", automlMinRows, len(kept))
	}
	lt := mlTypeOf(cols[li].Type)
	classes := map[string]bool{}
	for _, row := range kept {
		classes[*row[li]] = true
	}
	if opts.ModelType == "AUTOML_REGRESSOR" {
		if !(mlFeature{Type: lt}).numeric() {
			return t, mlInvalid("Label column %s has an unsupported type %s for model type %s: it must be INT64, NUMERIC, BIGNUMERIC or FLOAT64", cols[li].Name, cols[li].Type, opts.ModelType)
		}
	} else if len(classes) > automlMaxClasses {
		return t, mlInvalid("The label column %s of model type %s can have at most %d unique values, not %d", cols[li].Name, opts.ModelType, automlMaxClasses, len(classes))
	}
	kind := "regressor"
	if opts.ModelType == "AUTOML_CLASSIFIER" {
		kind = "binary"
		if len(classes) > 2 {
			kind = "multiclass"
		}
	}
	objective := opts.Objective
	if objective == "" {
		objective = map[string]string{"regressor": "MINIMIZE_RMSE", "binary": "MAXIMIZE_AU_ROC", "multiclass": "MINIMIZE_LOG_LOSS"}[kind]
	}
	if !containsString(automlObjectives[kind], objective) {
		return t, mlInvalid("optimization_objective %s is not supported for %s; the label has %d classes", objective, opts.ModelType, len(classes))
	}
	part, fe := automlSplit(cols, kept, si)
	if fe != nil {
		return t, fe
	}
	// The trials' columns: without the user's split column, with the
	// evaluation flag.
	var tcols []mlColumn
	for i, c := range cols {
		if i != si {
			tcols = append(tcols, c)
		}
	}
	tcols = append(tcols, mlColumn{automlSplitCol, "BOOL"})
	build := func(keep func(p int) bool, eval func(p int) bool) [][]*string {
		var out [][]*string
		tv, fv := "true", "false"
		for k, row := range kept {
			if !keep(part[k]) {
				continue
			}
			r := make([]*string, 0, len(tcols))
			for i, v := range row {
				if i != si {
					r = append(r, v)
				}
			}
			if eval(part[k]) {
				r = append(r, &tv)
			} else {
				r = append(r, &fv)
			}
			out = append(out, r)
		}
		return out
	}
	trialRows := build(func(p int) bool { return p != partTest }, func(p int) bool { return p == partValidate })
	var valRows [][]*string
	for k, row := range kept {
		if part[k] == partValidate {
			valRows = append(valRows, row)
		}
	}
	if len(valRows) == 0 {
		return t, mlInvalid("No VALIDATE rows after the data split")
	}
	info := &automlOptions{Type: opts.ModelType, BudgetHours: opts.BudgetHours, Objective: opts.Objective, ObjValue: opts.ObjValue}
	best, bestScore := -1, math.Inf(1)
	cands := automlCandidates(opts)
	for k, c := range cands {
		tr, fe := trainModel(c.opts, tcols, trialRows, fp)
		if fe != nil {
			continue // a trainer that refuses this data is not a candidate
		}
		s := automlScore(tr, cols, valRows, li, objective, opts.ObjValue)
		if math.IsNaN(s) {
			continue
		}
		info.Trials = append(info.Trials, automlTrial{ModelType: c.opts.ModelType, Variant: c.variant, Score: automlReport(objective, s)})
		if s < bestScore {
			best, bestScore = k, s
		}
	}
	if best < 0 {
		return t, mlInvalid("AutoML found no model for the input data")
	}
	chosen := cands[best].opts
	info.Chosen = chosen.ModelType
	if v := cands[best].variant; v != "" {
		info.Chosen += " (" + v + ")"
	}
	final, fe := trainModel(chosen, tcols, build(func(int) bool { return true }, func(p int) bool { return p == partTest }), fp)
	if fe != nil {
		return t, fe
	}
	chosen.Set = append([]string{"model_type"}, opts.Set...)
	chosen.SplitCol = opts.SplitCol
	chosen.AutoML = info
	final.automlOpts = &chosen
	return final, nil
}

const (
	partTrain = iota
	partValidate
	partTest
)

// automlSplit assigns each row TRAIN, VALIDATE or TEST.
func automlSplit(cols []mlColumn, rows [][]*string, si int) ([]int, *mlFail) {
	part := make([]int, len(rows))
	random := func(k int) int {
		h := fnv.New64a()
		h.Write([]byte(rowText(cols, rows[k])))
		switch v := h.Sum64() % 10; {
		case v < 8:
			return partTrain
		case v < 9:
			return partValidate
		}
		return partTest
	}
	if si < 0 {
		for k := range rows {
			part[k] = random(k)
		}
		return part, nil
	}
	switch strings.ToUpper(cols[si].Type) {
	case "STRING":
		for k, row := range rows {
			v := ""
			if row[si] != nil {
				v = strings.ToUpper(*row[si])
			}
			switch v {
			case "TRAIN":
				part[k] = partTrain
			case "VALIDATE":
				part[k] = partValidate
			case "TEST":
				part[k] = partTest
			case "UNASSIGNED":
				part[k] = random(k)
			default:
				return nil, mlInvalid("The data split column %s must hold TRAIN, VALIDATE, TEST or UNASSIGNED, not %q", cols[si].Name, v)
			}
		}
	case "TIMESTAMP":
		idx := make([]int, len(rows))
		at := make([]float64, len(rows))
		for k, row := range rows {
			idx[k] = k
			at[k] = math.Inf(1)
			if row[si] != nil {
				if v, err := strconv.ParseFloat(*row[si], 64); err == nil {
					at[k] = v
				}
			}
		}
		sort.SliceStable(idx, func(a, b int) bool { return at[idx[a]] < at[idx[b]] })
		n := len(rows)
		for r, k := range idx {
			switch {
			case r < n*8/10:
				part[k] = partTrain
			case r < n*9/10:
				part[k] = partValidate
			default:
				part[k] = partTest
			}
		}
	default:
		return nil, mlInvalid("The data split column %s must be of type TIMESTAMP or STRING, not %s", cols[si].Name, cols[si].Type)
	}
	return part, nil
}

// automlScore is trial t's objective on the validation rows, lower
// better (a maximized objective negated).
func automlScore(t mlTrained, cols []mlColumn, rows [][]*string, li int, objective string, value *float64) float64 {
	vals := func(row []*string) map[string]*string {
		m := map[string]*string{}
		for i, c := range cols {
			if i != li {
				m[strings.ToLower(c.Name)] = row[i]
			}
		}
		return m
	}
	n := float64(len(rows))
	if len(t.Classes) == 0 {
		var se, ae, sle float64
		for _, row := range rows {
			y, _ := strconv.ParseFloat(*row[li], 64)
			p := t.predictRow(vals(row))
			se += (p - y) * (p - y)
			ae += math.Abs(p - y)
			d := math.Log1p(math.Max(p, 0)) - math.Log1p(math.Max(y, 0))
			sle += d * d
		}
		switch objective {
		case "MINIMIZE_MAE":
			return ae / n
		case "MINIMIZE_RMSLE":
			return math.Sqrt(sle / n)
		}
		return math.Sqrt(se / n)
	}
	if t.multiclass() {
		var ll float64
		for _, row := range rows {
			pr := t.classProbs(vals(row))
			p := 1e-15
			for k, c := range t.Classes {
				if c == *row[li] {
					p = math.Max(pr[k], 1e-15)
				}
			}
			ll -= math.Log(p)
		}
		return ll / n
	}
	y, p := make([]float64, len(rows)), make([]float64, len(rows))
	for k, row := range rows {
		if *row[li] == t.Classes[1] {
			y[k] = 1
		}
		p[k] = t.predictRow(vals(row))
	}
	at := 0.5
	if value != nil {
		at = *value
	}
	switch objective {
	case "MINIMIZE_LOG_LOSS":
		var ll float64
		for k := range y {
			q := math.Min(math.Max(p[k], 1e-15), 1-1e-15)
			ll -= y[k]*math.Log(q) + (1-y[k])*math.Log(1-q)
		}
		return ll / n
	case "MAXIMIZE_AU_PRC":
		return -areaUnderPR(y, p)
	case "MAXIMIZE_PRECISION_AT_RECALL", "MAXIMIZE_RECALL_AT_PRECISION":
		best := 0.0
		for _, pt := range prCurve(y, p) {
			prec, rec := pt[0], pt[1]
			if objective == "MAXIMIZE_PRECISION_AT_RECALL" && rec >= at {
				best = math.Max(best, prec)
			}
			if objective == "MAXIMIZE_RECALL_AT_PRECISION" && prec >= at {
				best = math.Max(best, rec)
			}
		}
		return -best
	}
	return -rocAUC(y, p)
}

// automlReport is a score as the objective states it.
func automlReport(objective string, s float64) float64 {
	if strings.HasPrefix(objective, "MAXIMIZE") {
		return -s
	}
	return s
}

// prCurve is the (precision, recall) at each distinct threshold, highest
// first.
func prCurve(y, p []float64) [][2]float64 {
	idx := make([]int, len(p))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return p[idx[a]] > p[idx[b]] })
	var pos float64
	for _, v := range y {
		pos += v
	}
	var out [][2]float64
	var tp, fp float64
	for k, i := range idx {
		if y[i] == 1 {
			tp++
		} else {
			fp++
		}
		if k+1 < len(idx) && p[idx[k+1]] == p[i] {
			continue
		}
		rec := 0.0
		if pos > 0 {
			rec = tp / pos
		}
		out = append(out, [2]float64{tp / (tp + fp), rec})
	}
	return out
}

// areaUnderPR is the area under the precision-recall curve (average
// precision).
func areaUnderPR(y, p []float64) float64 {
	var area, last float64
	for _, pt := range prCurve(y, p) {
		area += pt[0] * (pt[1] - last)
		last = pt[1]
	}
	return area
}

// automlResource is an AutoML model's REST trainingOptions.
func (md bqModel) automlResource() map[string]any {
	a := md.Options.AutoML
	opts := map[string]any{"budgetHours": a.BudgetHours, "inputLabelColumns": []string{md.Trained.Label}}
	if md.Options.SplitCol != "" {
		opts["dataSplitColumn"] = md.Options.SplitCol
	}
	return opts
}

// automlFunctions are the functions an AutoML model takes.
var automlFunctions = map[string]bool{"PREDICT": true, "EVALUATE": true, "CONFUSION_MATRIX": true, "ROC_CURVE": true,
	"FEATURE_INFO": true, "TRAINING_INFO": true, "GLOBAL_EXPLAIN": true}
