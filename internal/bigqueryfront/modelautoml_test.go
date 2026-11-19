package bigqueryfront

import (
	"math"
	"strconv"
	"testing"
)

func TestAutoMLOptions(t *testing.T) {
	const pre = "CREATE MODEL d.m OPTIONS(model_type='AUTOML_"
	for sql, want := range map[string]int{
		pre + "REGRESSOR') AS SELECT 1 AS label":                                                                      0,
		pre + "REGRESSOR', budget_hours=2.5, optimization_objective='MINIMIZE_MAE') AS SELECT 1 AS label":             0,
		pre + "REGRESSOR', budget_hours=0.5) AS SELECT 1 AS label":                                                    400,
		pre + "REGRESSOR', budget_hours=73) AS SELECT 1 AS label":                                                     400,
		pre + "REGRESSOR', optimization_objective='MAXIMIZE_AU_ROC') AS SELECT 1 AS label":                            400,
		pre + "CLASSIFIER', optimization_objective='MAXIMIZE_AU_PRC', input_label_cols=['y']) AS SELECT 1 AS y":       0,
		pre + "CLASSIFIER', optimization_objective=STRUCT('MAXIMIZE_PRECISION_AT_RECALL', 0.3)) AS SELECT 1 AS label": 0,
		pre + "CLASSIFIER', optimization_objective=STRUCT('MAXIMIZE_PRECISION_AT_RECALL', 1.3)) AS SELECT 1 AS label": 400,
		pre + "CLASSIFIER', optimization_objective=STRUCT('MAXIMIZE_AU_ROC', 0.3)) AS SELECT 1 AS label":              400,
		pre + "CLASSIFIER', optimization_objective='MINIMIZE_RMSE') AS SELECT 1 AS label":                             400,
		pre + "CLASSIFIER', data_split_col='s') AS SELECT 1 AS label, 'TRAIN' AS s":                                   0,
		pre + "CLASSIFIER', max_iterations=3) AS SELECT 1 AS label":                                                   400,
	} {
		toks, _ := lex(sql)
		d, is, code, msg := parseModelDDL(toks, sql)
		if !is || code != want {
			t.Errorf("%s: %d %s; want %d", sql, code, msg, want)
			continue
		}
		if code == 0 && d.opts.BudgetHours < 1 {
			t.Errorf("%s: budget_hours %v", sql, d.opts.BudgetHours)
		}
	}
}

func automlData(n int, classify bool) ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"x1", "FLOAT"}, {"x2", "FLOAT"}, {"c", "STRING"}, {"label", "FLOAT"}}
	if classify {
		cols[3].Type = "STRING"
	}
	var rows [][]*string
	for i := 0; i < n; i++ {
		x1 := math.Sin(float64(i) * 0.7)
		x2 := math.Cos(float64(i) * 1.9)
		c := []string{"a", "b", "c"}[i%3]
		y := 3*x1 - 2*x2 + map[string]float64{"a": 0, "b": 1, "c": -1}[c] + 0.05*math.Sin(float64(i)*13.1)
		l := strconv.FormatFloat(y, 'g', -1, 64)
		if classify {
			l = "no"
			if y > 0 {
				l = "yes"
			}
		}
		a, b := strconv.FormatFloat(x1, 'g', -1, 64), strconv.FormatFloat(x2, 'g', -1, 64)
		rows = append(rows, []*string{&a, &b, &c, &l})
	}
	return cols, rows
}

func TestTrainAutoML(t *testing.T) {
	cols, rows := automlData(1200, false)
	o := defaultMLOptions()
	o.ModelType, o.Set = "AUTOML_REGRESSOR", []string{"model_type"}
	if c, m := checkAutoMLOptions(&o); c != 0 {
		t.Fatal(m)
	}
	tr, fe := trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	if tr.automlOpts == nil || tr.automlOpts.AutoML == nil || len(tr.automlOpts.AutoML.Trials) < 4 {
		t.Fatalf("no search: %+v", tr.automlOpts)
	}
	a := tr.automlOpts.AutoML
	if a.Type != "AUTOML_REGRESSOR" || a.BudgetHours != 1 || a.Chosen == "" {
		t.Errorf("AutoML %+v", a)
	}
	for _, f := range tr.Features {
		if f.Name == automlSplitCol {
			t.Errorf("the split flag is a feature")
		}
	}
	// The chosen model fits: its test RMSE is small.
	if v := tr.Eval["mean_squared_error"]; v == nil || *v > 0.5 || tr.EvalRows == 0 {
		t.Errorf("test mean_squared_error %v, %d test rows", v, tr.EvalRows)
	}
	// Under 1,000 rows is 400.
	if _, fe := trainModelT(o, cols, rows[:900]); fe == nil || fe.code != 400 {
		t.Errorf("900 rows: %v; want 400", fe)
	}

	cols, rows = automlData(1200, true)
	o = defaultMLOptions()
	o.ModelType, o.Set = "AUTOML_CLASSIFIER", []string{"model_type"}
	checkAutoMLOptions(&o)
	tr, fe = trainModelT(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	if len(tr.Classes) != 2 || tr.automlOpts.AutoML.Type != "AUTOML_CLASSIFIER" {
		t.Errorf("classes %v", tr.Classes)
	}
	if v := tr.Eval["roc_auc"]; v == nil || *v < 0.9 {
		t.Errorf("test roc_auc %v", v)
	}
	// A multiclass label takes MINIMIZE_LOG_LOSS only.
	o.Objective = "MAXIMIZE_AU_ROC"
	for _, r := range rows[:400] {
		v := "maybe"
		r[3] = &v
	}
	if _, fe := trainModelT(o, cols, rows); fe == nil || fe.code != 400 {
		t.Errorf("multiclass MAXIMIZE_AU_ROC: %v; want 400", fe)
	}
}

func TestAutoMLSplit(t *testing.T) {
	cols := []mlColumn{{"s", "STRING"}, {"label", "FLOAT"}}
	var rows [][]*string
	for _, v := range []string{"TRAIN", "VALIDATE", "TEST", "train"} {
		v, l := v, "1"
		rows = append(rows, []*string{&v, &l})
	}
	part, fe := automlSplit(cols, rows, 0)
	if fe != nil || part[0] != partTrain || part[1] != partValidate || part[2] != partTest || part[3] != partTrain {
		t.Errorf("split %v %v", part, fe)
	}
	bad := "OTHER"
	rows[0][0] = &bad
	if _, fe := automlSplit(cols, rows, 0); fe == nil {
		t.Error("OTHER accepted")
	}
	if _, fe := automlSplit([]mlColumn{{"s", "INT64"}, {"label", "FLOAT"}}, rows, 0); fe == nil {
		t.Error("an INT64 split column accepted")
	}
}

func TestPRCurve(t *testing.T) {
	y := []float64{1, 0, 1, 0}
	p := []float64{0.9, 0.8, 0.7, 0.1}
	if got := areaUnderPR(y, p); math.Abs(got-(0.5+0.5*2.0/3)) > 1e-9 {
		t.Errorf("average precision %v", got)
	}
}
