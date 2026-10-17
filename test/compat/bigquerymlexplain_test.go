//go:build compat

package compat

import (
	"fmt"
	"math"
	"testing"

	"cloud.google.com/go/bigquery"
)

// BigQuery ML ML.EXPLAIN_PREDICT and ML.GLOBAL_EXPLAIN (#1504) through
// the official Go client: a linear model's exact Shapley values, worked
// by hand, and a tree model's TreeSHAP values adding up to its
// prediction.

// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLExplain(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	name := mlDataset(t, h, c, map[string]string{
		"lin": "SELECT CAST(i AS FLOAT64) AS a, CAST(MOD(i * 7, 10) AS FLOAT64) AS b, 2 * i - 3 * MOD(i * 7, 10) + 1.0 AS y, i >= 5 AS hi " +
			"FROM UNNEST(GENERATE_ARRAY(0, 9)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lr` OPTIONS(model_type='linear_reg', input_label_cols=['y'], enable_global_explain=TRUE) "+
		"AS SELECT a, b, y FROM `%s.lin`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL linear_reg: %v", err)
	}
	// By hand: the exact fit y = 2a − 3b + 1; a feature's attribution is
	// its weight times its distance from the training mean (4.5 each),
	// the baseline the mean prediction, 2·4.5 − 3·4.5 + 1.
	ex := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.lr`, (SELECT 8.0 AS a, 1.0 AS b), STRUCT(1 AS top_k_features))", name))
	if len(ex) != 1 || !near(ex[0]["predicted_y"], 14, 1e-6) || !near(ex[0]["baseline_prediction_value"], -3.5, 1e-6) ||
		!near(ex[0]["prediction_value"], 14, 1e-6) || !near(ex[0]["approximation_error"], 0, 1e-6) || ex[0]["a"] != 8.0 {
		t.Fatalf("ML.EXPLAIN_PREDICT: %v", ex)
	}
	top, _ := ex[0]["top_feature_attributions"].([]bigquery.Value)
	if len(top) != 1 {
		t.Fatalf("top_feature_attributions: %v", ex[0]["top_feature_attributions"])
	}
	if a := top[0].(map[string]bigquery.Value); a["feature"] != "b" || !near(a["attribution"], 10.5, 1e-6) {
		t.Errorf("top attribution: %v; want b 10.5 (−3 · (1 − 4.5)), above a's 7", a)
	}
	// Over the training rows: the mean |2(a − 4.5)| is 5, of b 7.5.
	ge := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GLOBAL_EXPLAIN(MODEL `%s.lr`)", name))
	if len(ge) != 2 || ge[0]["feature"] != "b" || !near(ge[0]["attribution"], 7.5, 1e-6) || !near(ge[1]["attribution"], 5, 1e-6) {
		t.Errorf("ML.GLOBAL_EXPLAIN: %v", ge)
	}
	// A boosted tree classifier: TreeSHAP's attributions toward the
	// predicted class add up to its log-odds.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bc` OPTIONS(model_type='boosted_tree_classifier', input_label_cols=['hi'], enable_global_explain=TRUE) "+
		"AS SELECT a, b, hi FROM `%s.lin`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL boosted_tree_classifier: %v", err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT predicted_hi, probability, baseline_prediction_value AS base, prediction_value AS z, approximation_error AS err, "+
		"(SELECT SUM(attribution) FROM UNNEST(top_feature_attributions)) AS s FROM ML.EXPLAIN_PREDICT(MODEL `%s.bc`, (SELECT 1.0 AS a, 3.0 AS b UNION ALL SELECT 8.0, 3.0)) "+
		"ORDER BY predicted_hi", name))
	if len(rows) != 2 || rows[0]["predicted_hi"] != false || rows[1]["predicted_hi"] != true {
		t.Fatalf("ML.EXPLAIN_PREDICT classifier: %v", rows)
	}
	for _, r := range rows {
		z, base, s := r["z"].(float64), r["base"].(float64), r["s"].(float64)
		if math.Abs(base+s-z) > 1e-9 || !near(r["err"], 0, 1e-9) || !near(r["probability"], 1/(1+math.Exp(-z)), 1e-9) || z <= 0 {
			t.Errorf("row %v: baseline + attributions %v, logit %v", r, base+s, z)
		}
	}
	cl := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GLOBAL_EXPLAIN(MODEL `%s.bc`, STRUCT(TRUE AS class_level_explain))", name))
	if len(cl) != 4 {
		t.Errorf("ML.GLOBAL_EXPLAIN class level: %v", cl)
	}
	_, err := c.Query(fmt.Sprintf("SELECT * FROM ML.GLOBAL_EXPLAIN(MODEL `%s.nope`)", name)).Read(h.Context())
	if err == nil {
		t.Errorf("ML.GLOBAL_EXPLAIN of no model: no error")
	}
}
