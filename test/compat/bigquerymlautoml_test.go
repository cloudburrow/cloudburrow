//go:build compat

package compat

import (
	"fmt"
	"math"
	"net/http"
	"testing"
)

// TestBigQueryMLAutoML (#1776): AUTOML_REGRESSOR and AUTOML_CLASSIFIER
// models, trained locally by a search over CloudBurrow's trainers
// (UNVERIFIED: Google does not document AutoML's), through the official
// cloud.google.com/go/bigquery client, on 1,200 rows of y = 3 x1 − 2 x2 +
// a category's offset: models.get answers modelType AUTOML_REGRESSOR and
// the trainingOptions budgetHours (default 1) and inputLabelColumns (the
// REST TrainingOptions has no optimization objective);
// ML.PREDICT predicts y closely; ML.EVALUATE, ML.FEATURE_INFO,
// ML.TRAINING_INFO and ML.GLOBAL_EXPLAIN answer; ML.WEIGHTS and
// ML.EXPLAIN_PREDICT, which the documented AutoML journey lacks, are 400.
// A classifier of y > 0 with optimization_objective MAXIMIZE_AU_PRC and a
// STRING data_split_col of TRAIN, VALIDATE and TEST predicts the class;
// ML.CONFUSION_MATRIX and ML.ROC_CURVE answer, and ML.EVALUATE's
// roc_auc is high. budget_hours outside [1, 72], an objective of the
// other model type and a split column of other values are 400.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLAutoML(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	const data = "SELECT x1, x2, cat, 3 * x1 - 2 * x2 + CASE cat WHEN 'a' THEN 0 WHEN 'b' THEN 1 ELSE -1 END + 0.05 * SIN(i * 13.1) AS y, " +
		"['TRAIN', 'TRAIN', 'TRAIN', 'TRAIN', 'TRAIN', 'TRAIN', 'TRAIN', 'TRAIN', 'VALIDATE', 'TEST'][OFFSET(MOD(i, 10))] AS part " +
		"FROM (SELECT i, SIN(i * 0.7) AS x1, COS(i * 1.9) AS x2, ['a', 'b', 'c'][OFFSET(MOD(i, 3))] AS cat FROM UNNEST(GENERATE_ARRAY(0, 1199)) AS i)"
	name := mlDataset(t, h, c, map[string]string{"t": data})

	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.reg` OPTIONS(model_type='AUTOML_REGRESSOR', input_label_cols=['y']) AS "+
		"SELECT x1, x2, cat, y FROM `%[1]s.t`", name)); err != nil {
		t.Fatalf("CREATE MODEL AUTOML_REGRESSOR: %v", err)
	}
	md, err := c.Dataset(name).Model("reg").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if md.Type != "AUTOML_REGRESSOR" {
		t.Errorf("modelType %q; want AUTOML_REGRESSOR", md.Type)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions == nil || runs[0].TrainingOptions.BudgetHours != 1 ||
		len(runs[0].TrainingOptions.InputLabelColumns) != 1 || runs[0].TrainingOptions.InputLabelColumns[0] != "y" {
		t.Errorf("trainingOptions not budgetHours 1 and inputLabelColumns [y]")
	}
	pr := mlQuery(t, h, c, fmt.Sprintf("SELECT y, predicted_y FROM ML.PREDICT(MODEL `%s.reg`, (SELECT * FROM `%[1]s.t` WHERE MOD(x1 * 1000, 7) < 1))", name))
	if len(pr) == 0 {
		t.Fatal("ML.PREDICT: no rows")
	}
	var se float64
	for _, r := range pr {
		d := r["y"].(float64) - r["predicted_y"].(float64)
		se += d * d
	}
	if rmse := math.Sqrt(se / float64(len(pr))); rmse > 0.7 {
		t.Errorf("ML.PREDICT rmse %v over %d rows; want under 0.7", rmse, len(pr))
	}
	if ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_squared_error FROM ML.EVALUATE(MODEL `%s.reg`)", name)); len(ev) != 1 || !near(ev[0]["mean_squared_error"], 0, 0.5) {
		t.Errorf("ML.EVALUATE %v", ev)
	}
	if fi := mlQuery(t, h, c, fmt.Sprintf("SELECT input FROM ML.FEATURE_INFO(MODEL `%s.reg`) ORDER BY input", name)); len(fi) != 3 || fi[0]["input"] != "cat" {
		t.Errorf("ML.FEATURE_INFO %v; want cat, x1, x2", fi)
	}
	if ti := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.TRAINING_INFO(MODEL `%s.reg`)", name)); len(ti) == 0 {
		t.Error("ML.TRAINING_INFO: no rows")
	}
	if ge := mlQuery(t, h, c, fmt.Sprintf("SELECT feature, attribution FROM ML.GLOBAL_EXPLAIN(MODEL `%s.reg`)", name)); len(ge) != 3 {
		t.Errorf("ML.GLOBAL_EXPLAIN %v", ge)
	}
	for what, sql := range map[string]string{
		"ML.WEIGHTS":         "SELECT * FROM ML.WEIGHTS(MODEL `%s.reg`)",
		"ML.EXPLAIN_PREDICT": "SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.reg`, (SELECT x1, x2, cat FROM `%[1]s.t`))",
	} {
		_, err := c.Query(fmt.Sprintf(sql, name)).Read(ctx)
		wantHTTPStatus(t, what, err, http.StatusBadRequest)
	}

	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.cls` OPTIONS(model_type='AUTOML_CLASSIFIER', input_label_cols=['pos'], "+
		"optimization_objective='MAXIMIZE_AU_PRC', data_split_col='part', budget_hours=2) AS SELECT x1, x2, cat, y > 0 AS pos, part FROM `%[1]s.t`", name)); err != nil {
		t.Fatalf("CREATE MODEL AUTOML_CLASSIFIER: %v", err)
	}
	md, err = c.Dataset(name).Model("cls").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if runs := md.RawTrainingRuns(); md.Type != "AUTOML_CLASSIFIER" || len(runs) != 1 || runs[0].TrainingOptions == nil ||
		runs[0].TrainingOptions.BudgetHours != 2 ||
		runs[0].TrainingOptions.DataSplitColumn != "part" {
		t.Errorf("classifier modelType %q or trainingOptions not as given", md.Type)
	}
	cp := mlQuery(t, h, c, fmt.Sprintf("SELECT COUNTIF(pos = predicted_pos) / COUNT(*) AS acc FROM ML.PREDICT(MODEL `%s.cls`, "+
		"(SELECT x1, x2, cat, y > 0 AS pos FROM `%[1]s.t`))", name))
	if len(cp) != 1 || !near(cp[0]["acc"], 1, 0.12) {
		t.Errorf("classifier accuracy %v; want about 1", cp)
	}
	if ev := mlQuery(t, h, c, fmt.Sprintf("SELECT roc_auc FROM ML.EVALUATE(MODEL `%s.cls`)", name)); len(ev) != 1 || !near(ev[0]["roc_auc"], 1, 0.1) {
		t.Errorf("classifier ML.EVALUATE %v", ev)
	}
	if cm := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.CONFUSION_MATRIX(MODEL `%s.cls`)", name)); len(cm) != 2 {
		t.Errorf("ML.CONFUSION_MATRIX: %d rows; want 2", len(cm))
	}
	if rc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.ROC_CURVE(MODEL `%s.cls`)", name)); len(rc) == 0 {
		t.Error("ML.ROC_CURVE: no rows")
	}

	for what, opts := range map[string]string{
		"budget_hours 0.5":         "model_type='AUTOML_REGRESSOR', input_label_cols=['y'], budget_hours=0.5",
		"budget_hours 73":          "model_type='AUTOML_REGRESSOR', input_label_cols=['y'], budget_hours=73",
		"a classifier's objective": "model_type='AUTOML_REGRESSOR', input_label_cols=['y'], optimization_objective='MAXIMIZE_AU_ROC'",
		"a value out of [0, 1]":    "model_type='AUTOML_CLASSIFIER', input_label_cols=['cat'], optimization_objective=STRUCT('MAXIMIZE_PRECISION_AT_RECALL', 2.0)",
		"split column values":      "model_type='AUTOML_REGRESSOR', input_label_cols=['y'], data_split_col='cat'",
	} {
		err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(%s) AS SELECT x1, x2, cat, y FROM `%[1]s.t`", name, opts))
		wantHTTPStatus(t, what, err, http.StatusBadRequest)
	}
}
