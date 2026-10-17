//go:build compat

package compat

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// BigQuery ML (#1481): multiclass LOGISTIC_REG, ML.CONFUSION_MATRIX and
// ML.ROC_CURVE, KMEANS and ML.CENTROIDS, PCA and ML.PRINCIPAL_COMPONENTS /
// ML.PRINCIPAL_COMPONENT_INFO, and TRANSFORM clauses, through the official
// Go client. Each model is trained on rows whose answer is known exactly,
// so the checks hold to 1e-6 where the math is closed-form.

// mlDataset makes a dataset of rows for the test, and drops it after.
func mlDataset(t *testing.T, h *Harness, c *bigquery.Client, tables map[string]string) string {
	t.Helper()
	name := "compat_bqml2_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(name)
	if err := ds.Create(h.Context(), &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	for tbl, sql := range tables {
		if err := mlRun(h, c, fmt.Sprintf("CREATE TABLE `%s.%s` AS %s", name, tbl, sql)); err != nil {
			t.Fatalf("CREATE TABLE %s: %v", tbl, err)
		}
	}
	return name
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLMulticlassConfusionAndROC(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	// Three classes, separated on x: a below 10, b 10 to 20, c above.
	name := mlDataset(t, h, c, map[string]string{
		"cls": "SELECT CAST(MOD(i, 3) * 10 + MOD(i, 7) AS FLOAT64) AS x, ['a', 'b', 'c'][OFFSET(MOD(i, 3))] AS label FROM UNNEST(GENERATE_ARRAY(0, 89)) AS i",
		"bin": "SELECT CAST(MOD(i, 10) AS FLOAT64) AS x, MOD(i, 10) >= 5 OR i = 13 AS label FROM UNNEST(GENERATE_ARRAY(0, 59)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.multi` OPTIONS(model_type='logistic_reg', max_iterations=100) AS SELECT x, label FROM `%s.cls`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL multiclass: %v", err)
	}
	md, err := c.Dataset(name).Model("multi").Metadata(ctx)
	if err != nil || md.Type != "LOGISTIC_REGRESSION" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].EvaluationMetrics.MultiClassClassificationMetrics == nil {
		t.Errorf("training run: want multiClassClassificationMetrics")
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_label, predicted_label_probs FROM ML.PREDICT(MODEL `%s.multi`, "+
		"(SELECT 3.0 AS x UNION ALL SELECT 13.0 UNION ALL SELECT 24.0)) ORDER BY x", name))
	if len(pred) != 3 || pred[0]["predicted_label"] != "a" || pred[1]["predicted_label"] != "b" || pred[2]["predicted_label"] != "c" {
		t.Errorf("ML.PREDICT multiclass: %v", pred)
	}
	for _, p := range pred {
		probs, _ := p["predicted_label_probs"].([]bigquery.Value)
		var sum float64
		for _, s := range probs {
			sum += s.(map[string]bigquery.Value)["prob"].(float64)
		}
		if len(probs) != 3 || math.Abs(sum-1) > 1e-9 {
			t.Errorf("predicted_label_probs %v: want 3 summing to 1", probs)
		}
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.multi`)", name))
	if len(ev) != 1 || ev[0]["accuracy"].(float64) < 0.95 {
		t.Errorf("ML.EVALUATE multiclass: %v", ev)
	}
	if _, has := ev[0]["threshold"]; has {
		t.Errorf("ML.EVALUATE multiclass has a threshold column")
	}
	cm := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.CONFUSION_MATRIX(MODEL `%s.multi`) ORDER BY expected_label", name))
	if len(cm) != 3 {
		t.Fatalf("ML.CONFUSION_MATRIX: %v", cm)
	}
	var total, right int64
	for i, row := range cm {
		for j, l := range []string{"a", "b", "c"} {
			n := row[l].(int64)
			total += n
			if i == j {
				right += n
			}
		}
	}
	if total != 90 || math.Abs(float64(right)/90-ev[0]["accuracy"].(float64)) > 1e-9 {
		t.Errorf("ML.CONFUSION_MATRIX: %v; want 90 rows, its diagonal the accuracy %v", cm, ev[0]["accuracy"])
	}

	// The binary model: confusion matrix and ROC curve at given thresholds
	// agree with ML.EVALUATE on the same input.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bin` OPTIONS(model_type='logistic_reg', max_iterations=50) AS SELECT x, label FROM `%s.bin`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL binary: %v", err)
	}
	input := fmt.Sprintf("(SELECT x, label FROM `%s.bin`)", name)
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT recall, accuracy FROM ML.EVALUATE(MODEL `%s.bin`, %s, STRUCT(0.6 AS threshold))", name, input))
	cm = mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.CONFUSION_MATRIX(MODEL `%s.bin`, %s, STRUCT(0.6 AS threshold)) ORDER BY expected_label", name, input))
	if len(cm) != 2 || len(ev) != 1 || cm[0]["expected_label"] != false {
		t.Fatalf("ML.CONFUSION_MATRIX binary: %v", cm)
	}
	tn, fp, fn, tp := cm[0]["false"].(int64), cm[0]["true"].(int64), cm[1]["false"].(int64), cm[1]["true"].(int64)
	if tn+fp+fn+tp != 60 || math.Abs(float64(tp+tn)/60-ev[0]["accuracy"].(float64)) > 1e-9 {
		t.Errorf("confusion %d %d %d %d vs accuracy %v", tn, fp, fn, tp, ev[0]["accuracy"])
	}
	roc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.ROC_CURVE(MODEL `%s.bin`, %s, GENERATE_ARRAY(0.4, 0.6, 0.1)) ORDER BY threshold", name, input))
	if len(roc) != 3 || !near(roc[2]["threshold"], 0.6, 1e-9) || roc[2]["true_positives"] != tp || roc[2]["false_negatives"] != fn ||
		!near(roc[2]["recall"], ev[0]["recall"].(float64), 1e-9) {
		t.Errorf("ML.ROC_CURVE: %v; want its 0.6 row to match the confusion matrix and recall %v", roc, ev[0]["recall"])
	}
	if def := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.ROC_CURVE(MODEL `%s.bin`)", name)); len(def) == 0 {
		t.Errorf("ML.ROC_CURVE with no input gave no rows")
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.ROC_CURVE(MODEL `%s.multi`)", name)).Read(ctx)
	wantHTTPStatus(t, "ML.ROC_CURVE of a multiclass model", err, 400)
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLKMeans(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	// Two clusters, around (1, 1) and (11, 21); the first row of each is
	// the CUSTOM init.
	name := mlDataset(t, h, c, map[string]string{
		"pts": "SELECT CAST(MOD(i, 2) * 10 + MOD(i, 3) AS FLOAT64) AS x, CAST(MOD(i, 2) * 20 + MOD(i, 3) AS FLOAT64) AS y, i < 2 AS init " +
			"FROM UNNEST(GENERATE_ARRAY(0, 29)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.km` OPTIONS(model_type='kmeans', num_clusters=2, kmeans_init_method='CUSTOM', "+
		"kmeans_init_col='init', standardize_features=FALSE) AS SELECT x, y, init FROM `%s.pts`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL kmeans: %v", err)
	}
	md, err := c.Dataset(name).Model("km").Metadata(ctx)
	if err != nil || md.Type != "KMEANS" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	// Each cluster's centroid is its rows' mean: x 0..2 → 1 (and 11), y 1 (and 21).
	cen := mlQuery(t, h, c, fmt.Sprintf("SELECT centroid_id, feature, numerical_value FROM ML.CENTROIDS(MODEL `%s.km`) ORDER BY centroid_id, feature", name))
	want := []float64{1, 1, 11, 21}
	if len(cen) != 4 {
		t.Fatalf("ML.CENTROIDS: %v", cen)
	}
	for i, r := range cen {
		if !near(r["numerical_value"], want[i], 1e-9) {
			t.Errorf("ML.CENTROIDS row %d: %v; want %v", i, r, want[i])
		}
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT centroid_id, nearest_centroids_distance FROM ML.PREDICT(MODEL `%s.km`, (SELECT 4.0 AS x, 5.0 AS y))", name))
	if len(pred) != 1 || pred[0]["centroid_id"] != int64(1) {
		t.Fatalf("ML.PREDICT kmeans: %v", pred)
	}
	nd, _ := pred[0]["nearest_centroids_distance"].([]bigquery.Value)
	if len(nd) != 2 || !near(nd[0].(map[string]bigquery.Value)["DISTANCE"], 5, 1e-9) || !near(nd[1].(map[string]bigquery.Value)["DISTANCE"], math.Hypot(7, 16), 1e-9) {
		t.Errorf("nearest_centroids_distance: %v; want 5 then hypot(7, 16)", nd)
	}
	// Every row is 0, 1 or 2 from its centroid's mean in x and y alike:
	// squared distance 2·(dx²), dx in {−1, 0, 1}, mean 2·(2/3).
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.km`)", name))
	if len(ev) != 1 || !near(ev[0]["mean_squared_distance"], 4.0/3, 1e-9) || ev[0]["davies_bouldin_index"] == nil {
		t.Errorf("ML.EVALUATE kmeans: %v", ev)
	}
	// KMEANS++ with standardization finds the same partition.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.kpp` OPTIONS(model_type='kmeans', num_clusters=2, kmeans_init_method='KMEANS++', "+
		"distance_type='COSINE') AS SELECT x, y FROM `%s.pts`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL kmeans++: %v", err)
	}
	cen = mlQuery(t, h, c, fmt.Sprintf("SELECT feature, numerical_value FROM ML.CENTROIDS(MODEL `%s.kpp`) ORDER BY numerical_value", name))
	if len(cen) != 4 {
		t.Errorf("ML.CENTROIDS kmeans++: %v", cen)
	}
	if ti := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.TRAINING_INFO(MODEL `%s.km`)", name)); len(ti) == 0 {
		t.Errorf("ML.TRAINING_INFO kmeans: no iterations")
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.WEIGHTS(MODEL `%s.km`)", name)).Read(ctx)
	wantHTTPStatus(t, "ML.WEIGHTS of a kmeans model", err, 400)
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLPCA(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	// y = 2x exactly: standardized, x and y are the same column, so the
	// covariance is [[1, 1], [1, 1]], eigenvalues 2 and 0, and the first
	// component (√½, √½).
	name := mlDataset(t, h, c, map[string]string{
		"pts": "SELECT CAST(i AS FLOAT64) AS x, CAST(2 * i AS FLOAT64) AS y FROM UNNEST(GENERATE_ARRAY(1, 9)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.pca` OPTIONS(model_type='pca', num_principal_components=2) AS SELECT x, y FROM `%s.pts`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL pca: %v", err)
	}
	if md, err := c.Dataset(name).Model("pca").Metadata(ctx); err != nil || md.Type != "PCA" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	pc := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PRINCIPAL_COMPONENTS(MODEL `%s.pca`) WHERE principal_component_id = 1 ORDER BY feature", name))
	if len(pc) != 2 || !near(pc[0]["numerical_value"], math.Sqrt(0.5), 1e-9) || !near(pc[1]["numerical_value"], math.Sqrt(0.5), 1e-9) {
		t.Errorf("ML.PRINCIPAL_COMPONENTS: %v", pc)
	}
	info := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PRINCIPAL_COMPONENT_INFO(MODEL `%s.pca`) ORDER BY principal_component_id", name))
	if len(info) != 2 || !near(info[0]["eigenvalue"], 2, 1e-9) || !near(info[0]["explained_variance_ratio"], 1, 1e-9) ||
		!near(info[1]["cumulative_explained_variance_ratio"], 1, 1e-9) {
		t.Errorf("ML.PRINCIPAL_COMPONENT_INFO: %v", info)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.pca`)", name))
	if len(ev) != 1 || !near(ev[0]["total_explained_variance_ratio"], 1, 1e-9) {
		t.Errorf("ML.EVALUATE pca: %v", ev)
	}
	// x = 5 is the mean, so its projection is 0; x = 9 is 4 above it, z = 4/sd
	// (sd of 1..9 is √7.5), and the projection √2·z.
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.pca`, (SELECT 5.0 AS x, 10.0 AS y UNION ALL SELECT 9.0, 18.0), "+
		"STRUCT(TRUE AS keep_original_columns)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["principal_component_1"], 0, 1e-9) || !near(pred[1]["principal_component_1"], math.Sqrt2*4/math.Sqrt(7.5), 1e-9) ||
		pred[1]["x"] != 9.0 {
		t.Errorf("ML.PREDICT pca: %v", pred)
	}
}

// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLTransform(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	// y = 3 + 2·x1 − x2 exactly; the TRANSFORM trains on x2 doubled.
	name := mlDataset(t, h, c, map[string]string{
		"rows": "SELECT MOD(i, 7) AS x1, CAST(MOD(i, 5) AS FLOAT64) * 1.5 AS x2, 3 + 2 * MOD(i, 7) - CAST(MOD(i, 5) AS FLOAT64) * 1.5 AS y, 'n' AS note " +
			"FROM UNNEST(GENERATE_ARRAY(0, 39)) AS i",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.tr` TRANSFORM(* EXCEPT (x2, note), x2 * 2 AS x2d) OPTIONS(model_type='linear_reg', input_label_cols=['y']) "+
		"AS SELECT * FROM `%s.rows`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with TRANSFORM: %v", err)
	}
	w := map[string]bigquery.Value{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT processed_input, weight FROM ML.WEIGHTS(MODEL `%s.tr`)", name)) {
		w[r["processed_input"].(string)] = r["weight"]
	}
	if !near(w["x2d"], -0.5, 1e-6) || !near(w["x1"], 2, 1e-6) {
		t.Errorf("ML.WEIGHTS: %v; want x1 2, x2d -0.5", w)
	}
	// The input needs only the columns the TRANSFORM reads; the rest pass
	// through.
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.tr`, (SELECT 4 AS x1, 2.0 AS x2, 'kept' AS extra))", name))
	if len(pred) != 1 || !near(pred[0]["predicted_y"], 9, 1e-6) || pred[0]["extra"] != "kept" || pred[0]["x2"] != 2.0 {
		t.Errorf("ML.PREDICT with TRANSFORM: %v; want predicted_y 9 and the input columns", pred)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT r2_score FROM ML.EVALUATE(MODEL `%s.tr`, (SELECT * FROM `%s.rows`))", name, name))
	if len(ev) != 1 || !near(ev[0]["r2_score"], 1, 1e-6) {
		t.Errorf("ML.EVALUATE with TRANSFORM: %v", ev)
	}
}
