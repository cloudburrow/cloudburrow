//go:build compat

package compat

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// mlColumns runs sql and answers its columns' names in order and its rows.
func mlColumns(t *testing.T, h *Harness, c *bigquery.Client, sql string) ([]string, [][]bigquery.Value) {
	t.Helper()
	it, err := c.Query(sql).Read(h.Context())
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var rows [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		rows = append(rows, row)
	}
	var names []string
	for _, f := range it.Schema {
		names = append(names, f.Name)
	}
	return names, rows
}

// TestBigQueryMLUnsupervisedDetectAnomalies (#1929): ML.DETECT_ANOMALIES of
// KMEANS, PCA and AUTOENCODER models through the official
// cloud.google.com/go/bigquery client, with STRUCT(contamination) and an
// input (bigqueryml-syntax-detect-anomalies). The training data is two
// tight clusters on the line y = x with one point off it; new rows near a
// cluster are not anomalies, a far one is. The output is is_anomaly, then
// mean_squared_error (AUTOENCODER, PCA) or normalized_distance and
// centroid_id (KMEANS), then the input's columns. contamination outside
// [0, 0.5], none, or no input is 400.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLUnsupervisedDetectAnomalies(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"pts": "SELECT IF(MOD(i, 2) = 0, 0.0, 10.0) + 0.1 * MOD(i, 5) AS f1, IF(MOD(i, 2) = 0, 0.0, 10.0) + 0.1 * MOD(i, 3) AS f2 " +
			"FROM UNNEST(GENERATE_ARRAY(0, 38)) AS i UNION ALL SELECT 9.0, 1.0",
	})
	models := map[string]string{
		"km": "model_type='kmeans', num_clusters=2",
		"pc": "model_type='pca', num_principal_components=1",
		"ae": "model_type='autoencoder', hidden_units=[4, 1, 4], max_iterations=60, learn_rate=0.05",
	}
	for id, opts := range models {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(%s) AS SELECT f1, f2 FROM `%s.pts`", name, id, opts, name)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", id, err)
		}
	}
	input := "(SELECT 0.1 AS f1, 0.1 AS f2, 'near' AS tag UNION ALL SELECT 10.2, 10.1, 'near2' UNION ALL SELECT 30.0, -20.0, 'far')"
	for id := range models {
		cols, rows := mlColumns(t, h, c, fmt.Sprintf("SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.%s`, STRUCT(0.1 AS contamination), %s) ORDER BY tag", name, id, input))
		want := "is_anomaly,mean_squared_error,f1,f2,tag"
		if id == "km" {
			want = "is_anomaly,normalized_distance,centroid_id,f1,f2,tag"
		}
		if strings.Join(cols, ",") != want {
			t.Errorf("%s columns %v; want %s", id, cols, want)
		}
		if len(rows) != 3 {
			t.Fatalf("%s: %d rows; want 3", id, len(rows))
		}
		// ORDER BY tag: far, near, near2.
		if rows[0][0] != true || rows[1][0] != false || id != "ae" && rows[2][0] != false {
			t.Errorf("%s: is_anomaly %v %v %v; want the far row alone", id, rows[0][0], rows[1][0], rows[2][0])
		}
		if m := rows[0][1].(float64); !(m > rows[1][1].(float64)) || math.IsNaN(m) {
			t.Errorf("%s: metric far %v near %v", id, rows[0][1], rows[1][1])
		}
		if id == "km" && (rows[1][2] == rows[2][2] || rows[1][2] == nil) {
			t.Errorf("km centroid_id near %v near2 %v; want different clusters", rows[1][2], rows[2][2])
		}
	}
	// contamination 0 flags nothing of the training rows; 0.5 about half.
	for _, k := range []struct {
		cont     string
		min, max int64
	}{{"0", 0, 0}, {"0.5", 1, 20}} {
		n := mlQuery(t, h, c, fmt.Sprintf("SELECT COUNTIF(is_anomaly) AS n FROM ML.DETECT_ANOMALIES(MODEL `%s.km`, STRUCT(%s AS contamination), TABLE `%s.pts`)", name, k.cont, name))
		if got := n[0]["n"].(int64); got < k.min || got > k.max {
			t.Errorf("contamination %s: %d anomalies of 40 training rows; want %d..%d", k.cont, got, k.min, k.max)
		}
	}
	for what, sql := range map[string]string{
		"contamination 0.6":   "SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.km`, STRUCT(0.6 AS contamination), TABLE `%s.pts`)",
		"no contamination":    "SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.pc`, STRUCT(0.9 AS anomaly_prob_threshold), TABLE `%s.pts`)",
		"no input":            "SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.ae`, STRUCT(0.1 AS contamination))%.0s",
		"a linear_reg model ": "SELECT * FROM ML.DETECT_ANOMALIES(MODEL `%s.lin`, STRUCT(0.1 AS contamination), TABLE `%s.pts`)",
	} {
		if strings.Contains(what, "linear") {
			if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lin` OPTIONS(model_type='linear_reg', input_label_cols=['f2']) AS SELECT f1, f2 FROM `%s.pts`", name, name)); err != nil {
				t.Fatalf("CREATE MODEL lin: %v", err)
			}
		}
		_, err := c.Query(fmt.Sprintf(sql, name, name)).Read(ctx)
		wantHTTPStatus(t, "ML.DETECT_ANOMALIES with "+what, err, http.StatusBadRequest)
	}
}

// TestBigQueryMLCategoryEncodings (#1929): category_encoding_method
// DUMMY_ENCODING for a linear model drops the most frequent category,
// whose ML.WEIGHTS weight is 0.0, and still predicts each category's mean;
// TARGET_ENCODING for a boosted tree replaces a category by its rows' mean
// label (bigqueryml-auto-preprocessing). Each on another model type is
// 400; TARGET_ENCODING of a multiclass classifier, undocumented, is 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLCategoryEncodings(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"fruit": "SELECT c, CASE c WHEN 'apple' THEN 1.0 WHEN 'banana' THEN 5.0 ELSE 9.0 END AS y, IF(c = 'apple', 'x', IF(c = 'banana', 'y', 'z')) AS k " +
			"FROM UNNEST(['apple', 'banana', 'banana', 'cranberry', 'banana']) AS c, UNNEST(GENERATE_ARRAY(1, 6)) AS r",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.dum` OPTIONS(model_type='linear_reg', input_label_cols=['y'], "+
		"category_encoding_method='DUMMY_ENCODING') AS SELECT c, y FROM `%s.fruit`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL DUMMY_ENCODING: %v", err)
	}
	cw := mlQuery(t, h, c, fmt.Sprintf("SELECT cw.category, cw.weight FROM ML.WEIGHTS(MODEL `%s.dum`), UNNEST(category_weights) AS cw ORDER BY cw.category", name))
	if len(cw) != 3 || cw[1]["category"] != "banana" || cw[1]["weight"] != 0.0 {
		t.Errorf("DUMMY_ENCODING ML.WEIGHTS: %v; want banana, the most frequent, 0.0", cw)
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT c, predicted_y FROM ML.PREDICT(MODEL `%s.dum`, (SELECT c FROM UNNEST(['apple', 'banana', 'cranberry']) AS c)) ORDER BY c", name))
	for i, want := range []float64{1, 5, 9} {
		if len(pred) != 3 || !near(pred[i]["predicted_y"], want, 1e-6) {
			t.Errorf("DUMMY_ENCODING ML.PREDICT: %v; want 1, 5, 9", pred)
			break
		}
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.tgt` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y'], "+
		"category_encoding_method='TARGET_ENCODING', max_iterations=30, learn_rate=0.5) AS SELECT c, y FROM `%s.fruit`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL TARGET_ENCODING: %v", err)
	}
	pred = mlQuery(t, h, c, fmt.Sprintf("SELECT c, predicted_y FROM ML.PREDICT(MODEL `%s.tgt`, (SELECT c FROM UNNEST(['apple', 'banana', 'cranberry']) AS c)) ORDER BY c", name))
	if len(pred) != 3 || !(pred[0]["predicted_y"].(float64) < pred[1]["predicted_y"].(float64) && pred[1]["predicted_y"].(float64) < pred[2]["predicted_y"].(float64)) {
		t.Errorf("TARGET_ENCODING ML.PREDICT: %v; want apple < banana < cranberry", pred)
	}
	m, err := c.Dataset(name).Model("tgt").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if runs := m.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.CategoryEncodingMethod != "TARGET_ENCODING" {
		t.Errorf("trainingOptions.categoryEncodingMethod: %+v; want TARGET_ENCODING", runs)
	}
	for _, k := range []struct {
		sql  string
		code int
	}{
		{"CREATE MODEL `%s.no` OPTIONS(model_type='boosted_tree_regressor', input_label_cols=['y'], category_encoding_method='DUMMY_ENCODING') AS SELECT c, y FROM `%s.fruit`", 400},
		{"CREATE MODEL `%s.no` OPTIONS(model_type='linear_reg', input_label_cols=['y'], category_encoding_method='TARGET_ENCODING') AS SELECT c, y FROM `%s.fruit`", 400},
		{"CREATE MODEL `%s.no` OPTIONS(model_type='boosted_tree_classifier', input_label_cols=['k'], category_encoding_method='TARGET_ENCODING') AS SELECT c, k FROM `%s.fruit`", 501},
	} {
		_, err := c.Query(fmt.Sprintf(k.sql, name, name)).Read(ctx)
		wantHTTPStatus(t, k.sql, err, k.code)
	}
}

// TestBigQueryMLMulticlassWeights (#1929): ML.WEIGHTS of a multiclass
// LOGISTIC_REG answers class_label after processed_input, weight and
// category_weights, a row per class and processed_input
// (bigqueryml-syntax-weights), and the class weights give ML.PREDICT's
// probabilities.
//
// covers: bigquery.jobs.query
func TestBigQueryMLMulticlassWeights(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	name := mlDataset(t, h, c, map[string]string{
		"three": "SELECT CAST(MOD(i, 3) * 10 AS FLOAT64) + 0.1 * MOD(i, 7) AS x, IF(MOD(i, 2) = 0, 'u', 'v') AS s, " +
			"['a', 'b', 'c'][OFFSET(MOD(i, 3))] AS label FROM UNNEST(GENERATE_ARRAY(0, 59)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.mc` OPTIONS(model_type='logistic_reg', input_label_cols=['label'], max_iterations=20) "+
		"AS SELECT * FROM `%s.three`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	cols, rows := mlColumns(t, h, c, fmt.Sprintf("SELECT * FROM ML.WEIGHTS(MODEL `%s.mc`, STRUCT(true AS standardize)) ORDER BY class_label, processed_input", name))
	if strings.Join(cols, ",") != "processed_input,weight,category_weights,class_label" {
		t.Errorf("columns %v", cols)
	}
	if len(rows) != 9 {
		t.Fatalf("%d rows; want 3 classes × (x, s, __INTERCEPT__)", len(rows))
	}
	seen := map[string]int{}
	for _, r := range rows {
		seen[r[3].(string)]++
		if r[0] == "s" && (r[1] != nil || len(r[2].([]bigquery.Value)) != 2) {
			t.Errorf("row s: %v; want weight NULL and two category weights", r)
		}
	}
	if seen["a"] != 3 || seen["b"] != 3 || seen["c"] != 3 {
		t.Errorf("rows per class %v", seen)
	}
	// Unstandardized, the weights reproduce ML.PREDICT's probabilities.
	w := map[string]map[string]float64{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT class_label, processed_input, weight, "+
		"(SELECT weight FROM UNNEST(category_weights) WHERE category = 'u') AS wu FROM ML.WEIGHTS(MODEL `%s.mc`)", name)) {
		cl := r["class_label"].(string)
		if w[cl] == nil {
			w[cl] = map[string]float64{}
		}
		if v, ok := r["weight"].(float64); ok {
			w[cl][r["processed_input"].(string)] = v
		}
		if v, ok := r["wu"].(float64); ok {
			w[cl]["u"] = v
		}
	}
	p := mlQuery(t, h, c, fmt.Sprintf("SELECT (SELECT prob FROM UNNEST(predicted_label_probs) WHERE label = 'b') AS pb "+
		"FROM ML.PREDICT(MODEL `%s.mc`, (SELECT 10.0 AS x, 'u' AS s))", name))
	z := map[string]float64{}
	var den float64
	for _, cl := range []string{"a", "b", "c"} {
		z[cl] = w[cl]["__INTERCEPT__"] + w[cl]["x"]*10 + w[cl]["u"]
		den += math.Exp(z[cl])
	}
	if len(p) != 1 || !near(p[0]["pb"], math.Exp(z["b"])/den, 1e-6) {
		t.Errorf("P(b) %v; the weights give %v", p, math.Exp(z["b"])/den)
	}
}

// TestBigQueryMLTransformOnly (#1929): a TRANSFORM_ONLY model
// (bigqueryml-syntax-create-transform) keeps its TRANSFORM's statistics;
// ML.TRANSFORM applies them to new rows, the TRANSFORM's columns first and
// the input's others after, as the ML.TRANSFORM page's example shows; it
// feeds another model's training query and ML.PREDICT's input, as the
// page's second example does; ML.FEATURE_INFO describes its output
// columns, and ML.PREDICT of it, or a TRANSFORM_ONLY model without a
// TRANSFORM, is 400.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLTransformOnly(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"raw": "SELECT CAST(i AS FLOAT64) AS a, 3.0 * i + 1 AS y, 'k' AS keep FROM UNNEST(GENERATE_ARRAY(1, 10)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.tf` TRANSFORM(ML.MAX_ABS_SCALER(a) OVER () AS a, y) "+
		"OPTIONS(model_type='transform_only') AS SELECT * FROM `%s.raw`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL TRANSFORM_ONLY: %v", err)
	}
	m, err := c.Dataset(name).Model("tf").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if m.Type != "TRANSFORM_ONLY" {
		t.Errorf("model type %q", m.Type)
	}
	cols, rows := mlColumns(t, h, c, fmt.Sprintf("SELECT * FROM ML.TRANSFORM(MODEL `%s.tf`, (SELECT 5.0 AS a, 0.0 AS y, 'z' AS other))", name))
	if strings.Join(cols, ",") != "a,y,other" || len(rows) != 1 || !near(rows[0][0], 0.5, 1e-12) || rows[0][2] != "z" {
		t.Errorf("ML.TRANSFORM: %v %v; want a 0.5 (5 / max 10), y, other", cols, rows)
	}
	fi := mlQuery(t, h, c, fmt.Sprintf("SELECT input, max FROM ML.FEATURE_INFO(MODEL `%s.tf`) ORDER BY input", name))
	if len(fi) != 2 || fi[0]["input"] != "a" || !near(fi[0]["max"], 1, 1e-12) {
		t.Errorf("ML.FEATURE_INFO: %v; want a (max 1) and y", fi)
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lin` OPTIONS(model_type='linear_reg', input_label_cols=['y']) "+
		"AS SELECT * FROM ML.TRANSFORM(MODEL `%s.tf`, TABLE `%s.raw`)", name, name, name)); err != nil {
		t.Fatalf("CREATE MODEL on ML.TRANSFORM: %v", err)
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT predicted_y FROM ML.PREDICT(MODEL `%s.lin`, "+
		"(SELECT * FROM ML.TRANSFORM(MODEL `%s.tf`, (SELECT 4.0 AS a, 0.0 AS y, 'k' AS keep))))", name, name))
	if len(pred) != 1 || !near(pred[0]["predicted_y"], 13, 1e-6) {
		t.Errorf("ML.PREDICT of transformed rows: %v; want 13", pred)
	}
	for what, sql := range map[string]string{
		"ML.PREDICT of a TRANSFORM_ONLY model":    "SELECT * FROM ML.PREDICT(MODEL `%s.tf`, TABLE `%s.raw`)",
		"TRANSFORM_ONLY with no TRANSFORM":        "CREATE MODEL `%s.bad` OPTIONS(model_type='transform_only') AS SELECT * FROM `%s.raw`",
		"ML.TRANSFORM of a model of no TRANSFORM": "SELECT * FROM ML.TRANSFORM(MODEL `%s.lin`, TABLE `%s.raw`)",
	} {
		_, err := c.Query(fmt.Sprintf(sql, name, name)).Read(ctx)
		wantHTTPStatus(t, what, err, http.StatusBadRequest)
	}
}
