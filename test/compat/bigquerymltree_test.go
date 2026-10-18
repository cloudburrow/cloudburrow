//go:build compat

package compat

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"
)

// BigQuery ML BOOSTED_TREE_REGRESSOR and BOOSTED_TREE_CLASSIFIER (#1504):
// XGBoost's exact greedy boosting, ML.PREDICT, ML.EVALUATE and
// ML.FEATURE_IMPORTANCE through the official Go client, against values
// computed by hand on a step function.

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLBoostedTree(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"step": "SELECT CAST(x AS FLOAT64) AS x, IF(x >= 5, 10.0, 0.0) AS y, x >= 5 AS hi FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bt` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y']) AS SELECT x, y FROM `%s.step`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL boosted_tree_regressor: %v", err)
	}
	md, err := c.Dataset(name).Model("bt").Metadata(ctx)
	if err != nil || md.Type != "BOOSTED_TREE_REGRESSOR" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.MaxTreeDepth != 6 || runs[0].TrainingOptions.LearnRate != 0.3 ||
		len(runs[0].Results) != 20 {
		t.Errorf("training run: %+v", runs)
	}
	// By hand: each of the 20 rounds splits x at 4.5 alone, and each side's
	// leaf moves its prediction, from 0.5, a quarter of the way to its
	// label (learn_rate 0.3 · 5 rows / (5 + l2_reg 1)).
	a := math.Pow(0.75, 20)
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.bt`, (SELECT 2.0 AS x UNION ALL SELECT 7.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], 0.5*a, 1e-9) || !near(pred[1]["predicted_y"], 10-9.5*a, 1e-9) {
		t.Errorf("ML.PREDICT: %v; want %v and %v", pred, 0.5*a, 10-9.5*a)
	}
	// Each split's gain is 192.99…·0.75^(2r), its cover the 10 rows.
	var sum float64
	for r := 0; r < 20; r++ {
		sum += math.Pow(0.5625, float64(r))
	}
	gain := ((6.25+2256.25)/6 - 2025.0/11) * sum / 20
	fi := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FEATURE_IMPORTANCE(MODEL `%s.bt`)", name))
	if len(fi) != 1 || fi[0]["feature"] != "x" || fi[0]["importance_weight"] != int64(20) || !near(fi[0]["importance_gain"], gain, 1e-9) ||
		!near(fi[0]["importance_cover"], 10, 1e-9) {
		t.Errorf("ML.FEATURE_IMPORTANCE: %v; want x, 20, %v, 10", fi, gain)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.bt`)", name))
	if len(ev) != 1 || !near(ev[0]["mean_absolute_error"], (0.5*a+9.5*a)/2, 1e-9) {
		t.Errorf("ML.EVALUATE: %v", ev)
	}
	// The classifier: a round splits while each side's hessian sum
	// 5p(1 − p) is at least min_tree_child_weight 1, and is one leaf after.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bc` OPTIONS(model_type='boosted_tree_classifier', input_label_cols=['hi'], early_stop=FALSE) "+
		"AS SELECT x, hi FROM `%s.step`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL boosted_tree_classifier: %v", err)
	}
	m0, m1 := 0.0, 0.0
	sig := func(z float64) float64 { return 1 / (1 + math.Exp(-z)) }
	for r := 0; r < 20; r++ {
		p0, p1 := sig(m0), sig(m1)
		g0, h0, g1, h1 := 5*p0, 5*p0*(1-p0), 5*(p1-1), 5*p1*(1-p1)
		if h0 >= 1 && h1 >= 1 {
			m0, m1 = m0-0.3*g0/(h0+1), m1-0.3*g1/(h1+1)
			continue
		}
		d := 0.3 * (g0 + g1) / (h0 + h1 + 1)
		m0, m1 = m0-d, m1-d
	}
	cls := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_hi, (SELECT prob FROM UNNEST(predicted_hi_probs) WHERE label) AS p "+
		"FROM ML.PREDICT(MODEL `%s.bc`, (SELECT 2.0 AS x UNION ALL SELECT 7.0)) ORDER BY x", name))
	if len(cls) != 2 || cls[0]["predicted_hi"] != false || cls[1]["predicted_hi"] != true || !near(cls[0]["p"], sig(m0), 1e-9) || !near(cls[1]["p"], sig(m1), 1e-9) {
		t.Errorf("ML.PREDICT classifier: %v; want probabilities %v and %v", cls, sig(m0), sig(m1))
	}
	cm := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.CONFUSION_MATRIX(MODEL `%s.bc`) ORDER BY expected_label", name))
	if len(cm) != 2 || cm[0]["false"] != int64(5) || cm[1]["true"] != int64(5) {
		t.Errorf("ML.CONFUSION_MATRIX: %v", cm)
	}
	// Multiclass, one round (x 0..2 a, 3..5 b, 6..8 c), by hand: every p is
	// 1/3 and h 4/9; a's tree splits at 2.5, its leaves 0.3·2/(7/3) and
	// −0.3·2/(11/3); b's splits all three apart, b's leaf 0.3·2/(7/3), the
	// others −0.3/(7/3); c's mirrors a's.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bm` OPTIONS(model_type='boosted_tree_classifier', max_iterations=1) "+
		"AS SELECT CAST(x AS FLOAT64) AS x, ['a', 'b', 'c'][OFFSET(DIV(x, 3))] AS label FROM UNNEST(GENERATE_ARRAY(0, 8)) AS x", name)); err != nil {
		t.Fatalf("CREATE MODEL multiclass boosted_tree_classifier: %v", err)
	}
	za, zb, zc := 0.5+0.3*2/(7.0/3), 0.5-0.3/(7.0/3), 0.5-0.3*2/(11.0/3)
	den := math.Exp(za) + math.Exp(zb) + math.Exp(zc)
	mp := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_label, (SELECT prob FROM UNNEST(predicted_label_probs) WHERE label = 'a') AS pa "+
		"FROM ML.PREDICT(MODEL `%s.bm`, (SELECT 1.0 AS x UNION ALL SELECT 4.0 UNION ALL SELECT 7.0)) ORDER BY x", name))
	if len(mp) != 3 || mp[0]["predicted_label"] != "a" || mp[1]["predicted_label"] != "b" || mp[2]["predicted_label"] != "c" ||
		!near(mp[0]["pa"], math.Exp(za)/den, 1e-9) {
		t.Errorf("ML.PREDICT multiclass: %v; want a, b, c and P(a | 1) %v", mp, math.Exp(za)/den)
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.WEIGHTS(MODEL `%s.bt`)", name)).Read(ctx)
	wantHTTPStatus(t, "ML.WEIGHTS of a boosted tree model", err, http.StatusBadRequest)
	for _, sql := range []string{
		"CREATE MODEL `%s.no` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y'], subsample=0.8) AS SELECT x, y FROM `%s.step`",
		"CREATE MODEL `%s.no` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y'], booster_type='dart') AS SELECT x, y FROM `%s.step`",
	} {
		_, err := c.Query(fmt.Sprintf(sql, name, name)).Read(ctx)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
			t.Errorf("%s: %v; want 501", sql, err)
		}
	}
}

// TestBigQueryMLBoostedTreeLabelEncoding: a boosted tree's default
// category_encoding_method is LABEL_ENCODING (bigqueryml-syntax-create-boosted-tree),
// one column of the category's index in the alphabetical vocabulary
// (auto-preprocessing). By hand, for c a, b, c with y 10, 0, 10 (two rows
// each, one round): every p is 0.5, so g is −9.5 for a and c and 0.5 for b,
// and h 1. Label encoded (1, 2, 3), the splits at 1.5 and 2.5 both gain
// 19²/3 + 18²/5 − 37²/7 < 0, so the tree is one leaf, 0.3·37/7; one-hot,
// c_b splits b apart (gain 1/3 + 38²/5 − 37²/7 > 0), its leaves −0.3·1/3
// and 0.3·38/5.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLBoostedTreeLabelEncoding(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"cats": "SELECT c, IF(c = 'b', 0.0, 10.0) AS y FROM UNNEST(['a', 'b', 'c', 'a', 'b', 'c']) AS c",
	})
	for _, m := range []struct{ id, opt string }{{"lbl", ""}, {"hot", ", category_encoding_method='ONE_HOT_ENCODING'"}} {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y'], "+
			"max_iterations=1%s) AS SELECT c, y FROM `%s.cats`", name, m.id, m.opt, name)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m.id, err)
		}
	}
	md, err := c.Dataset(name).Model("lbl").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.CategoryEncodingMethod != "LABEL_ENCODING" {
		t.Errorf("training options: %+v; want categoryEncodingMethod LABEL_ENCODING", runs)
	}
	pred := func(m string) map[string]float64 {
		out := map[string]float64{}
		for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT c, predicted_y FROM ML.PREDICT(MODEL `%s.%s`, "+
			"(SELECT c FROM UNNEST(['a', 'b', 'c', 'z']) AS c))", name, m)) {
			out[r["c"].(string)] = r["predicted_y"].(float64)
		}
		return out
	}
	one := 0.5 + 0.3*37/7
	for k, v := range pred("lbl") {
		if math.Abs(v-one) > 1e-9 {
			t.Errorf("label encoded ML.PREDICT(%s) = %v; want the one leaf %v", k, v, one)
		}
	}
	hot := pred("hot")
	if math.Abs(hot["b"]-(0.5-0.1)) > 1e-9 || math.Abs(hot["a"]-(0.5+0.3*38/5)) > 1e-9 || math.Abs(hot["c"]-hot["a"]) > 1e-9 {
		t.Errorf("one-hot ML.PREDICT: %v; want b %v, a and c %v", hot, 0.4, 0.5+0.3*38/5)
	}
	// ML.FEATURE_INFO is of the input column, whatever the encoding:
	// dimension is for ARRAY<STRUCT> columns only (bigqueryml-syntax-feature).
	for _, m := range []string{"lbl", "hot"} {
		fi := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FEATURE_INFO(MODEL `%s.%s`)", name, m))
		if len(fi) != 1 || fi[0]["input"] != "c" || fi[0]["category_count"] != int64(3) || fi[0]["null_count"] != int64(0) || fi[0]["dimension"] != nil {
			t.Errorf("ML.FEATURE_INFO(%s): %v; want c, 3 categories, no NULLs, dimension NULL", m, fi)
		}
	}
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y'], "+
		"category_encoding_method='TARGET_ENCODING') AS SELECT c, y FROM `%s.cats`", name, name)).Read(ctx)
	wantHTTPStatus(t, "category_encoding_method TARGET_ENCODING", err, http.StatusNotImplemented)
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='linear_reg', input_label_cols=['y'], "+
		"category_encoding_method='LABEL_ENCODING') AS SELECT c, y FROM `%s.cats`", name, name)).Read(ctx)
	wantHTTPStatus(t, "category_encoding_method LABEL_ENCODING of a linear model", err, http.StatusBadRequest)
}
