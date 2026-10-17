//go:build compat

package compat

import (
	"fmt"
	"math"
	"testing"
)

// BigQuery ML RANDOM_FOREST_REGRESSOR and RANDOM_FOREST_CLASSIFIER
// (#1504): one round of num_parallel_tree trees, ML.PREDICT,
// ML.EVALUATE and ML.FEATURE_IMPORTANCE through the official Go client,
// against values computed by hand on a step function.

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLRandomForest(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"step": "SELECT CAST(x AS FLOAT64) AS x, IF(x >= 5, 10.0, 0.0) AS y, x >= 5 AS hi FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	// Every row and column: the 10 trees alike, each split at 4.5 with
	// leaves −Σg/(n + l2_reg 1) from 0.5, and their average one tree's.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.rf` OPTIONS(model_type='random_forest_regressor', input_label_cols=['y'], "+
		"num_parallel_tree=10, subsample=1.0, colsample_bynode=1.0) AS SELECT x, y FROM `%s.step`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL random_forest_regressor: %v", err)
	}
	md, err := c.Dataset(name).Model("rf").Metadata(ctx)
	if err != nil || md.Type != "RANDOM_FOREST_REGRESSOR" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.NumParallelTree != 10 || runs[0].TrainingOptions.MaxTreeDepth != 15 {
		t.Errorf("training run: %+v", runs)
	}
	lo, hi := 0.5-2.5/6, 0.5+47.5/6
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.rf`, (SELECT 2.0 AS x UNION ALL SELECT 7.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], lo, 1e-9) || !near(pred[1]["predicted_y"], hi, 1e-9) {
		t.Errorf("ML.PREDICT: %v; want %v and %v", pred, lo, hi)
	}
	fi := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FEATURE_IMPORTANCE(MODEL `%s.rf`)", name))
	if len(fi) != 1 || fi[0]["importance_weight"] != int64(10) || !near(fi[0]["importance_cover"], 10, 1e-9) {
		t.Errorf("ML.FEATURE_IMPORTANCE: %v", fi)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.rf`)", name))
	if len(ev) != 1 || !near(ev[0]["mean_absolute_error"], (math.Abs(lo)+math.Abs(10-hi))/2, 1e-9) {
		t.Errorf("ML.EVALUATE: %v", ev)
	}
	// The documented defaults (100 trees on a 0.8 subsample, 0.8 of the
	// columns at each node), drawn from a fixed seed: the same each time.
	var probs []float64
	for _, m := range []string{"rc1", "rc2"} {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='random_forest_classifier', input_label_cols=['hi']) "+
			"AS SELECT x, hi FROM `%s.step`", name, m, name)); err != nil {
			t.Fatalf("CREATE MODEL random_forest_classifier: %v", err)
		}
		cls := mlQuery(t, h, c, fmt.Sprintf("SELECT predicted_hi, (SELECT prob FROM UNNEST(predicted_hi_probs) WHERE label) AS p "+
			"FROM ML.PREDICT(MODEL `%s.%s`, (SELECT 8.0 AS x))", name, m))
		if len(cls) != 1 || cls[0]["predicted_hi"] != true {
			t.Fatalf("ML.PREDICT classifier: %v", cls)
		}
		probs = append(probs, cls[0]["p"].(float64))
	}
	if probs[0] != probs[1] {
		t.Errorf("two forests of the same data: %v", probs)
	}
	md, err = c.Dataset(name).Model("rc1").Metadata(ctx)
	if runs := md.RawTrainingRuns(); err != nil || len(runs) != 1 || runs[0].TrainingOptions.NumParallelTree != 100 || runs[0].TrainingOptions.Subsample != 0.8 {
		t.Errorf("classifier training run: %+v, %v", runs, err)
	}
}
