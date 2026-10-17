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
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// BigQuery ML (#1465): CREATE MODEL of LINEAR_REG and binary LOGISTIC_REG,
// trained by the front on the table's rows, ML.PREDICT, ML.EVALUATE,
// ML.WEIGHTS, ML.FEATURE_INFO and ML.TRAINING_INFO, and the models.* API,
// all through the official Go client.

type mlRow struct {
	X1    int64   `bigquery:"x1"`
	X2    float64 `bigquery:"x2"`
	C     string  `bigquery:"c"`
	Y     float64 `bigquery:"y"`
	Label bool    `bigquery:"label"`
}

// mlRows are rows of y = 3 + 2·x1 − x2 + 5·(c = 'b'), exactly, and a label
// that is mostly x1 > 3.
func mlRows() []*mlRow {
	var rows []*mlRow
	for i := 0; i < 40; i++ {
		x1, x2 := int64(i%7), float64(i%5)*1.5
		c := "a"
		y := 3 + 2*float64(x1) - x2
		if i%3 == 0 {
			c = "b"
			y += 5
		}
		rows = append(rows, &mlRow{X1: x1, X2: x2, C: c, Y: y, Label: x1 > 3 || i == 7})
	}
	return rows
}

// mlQuery reads every row of sql as name -> value.
func mlQuery(t *testing.T, h *Harness, c *bigquery.Client, sql string) []map[string]bigquery.Value {
	t.Helper()
	it, err := c.Query(sql).Read(h.Context())
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var out []map[string]bigquery.Value
	for {
		row := map[string]bigquery.Value{}
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out = append(out, row)
	}
}

func mlRun(h *Harness, c *bigquery.Client, sql string) error {
	job, err := c.Query(sql).Run(h.Context())
	if err != nil {
		return err
	}
	st, err := job.Wait(h.Context())
	if err != nil {
		return err
	}
	return st.Err()
}

func near(a bigquery.Value, want, tol float64) bool {
	f, ok := a.(float64)
	return ok && math.Abs(f-want) <= tol
}

// TestBigQueryMLLinearAndLogisticModels trains a LINEAR_REG model by the
// normal equation on rows of an exact linear function, so its weights,
// predictions and evaluation are known to 1e-6, and a LOGISTIC_REG model
// by gradient descent, then reads them through the ML functions and the
// models API.
//
// covers: bigquery.models.get, bigquery.models.list, bigquery.models.patch, bigquery.models.delete, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryMLLinearAndLogisticModels(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	name := "compat_bqml_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(name)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	schema, err := bigquery.InferSchema(mlRow{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := ds.Table("train")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("Table.Create: %v", err)
	}
	if err := tbl.Inserter().Put(ctx, mlRows()); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}

	// The linear model.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lin` OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS "+
		"SELECT x1, x2, c, y FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL linear_reg: %v", err)
	}
	md, err := ds.Model("lin").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if md.Type != "LINEAR_REGRESSION" || len(md.RawTrainingRuns()) != 1 {
		t.Errorf("Model.Metadata: type %q, %d training runs", md.Type, len(md.RawTrainingRuns()))
	}
	feats, err := md.RawFeatureColumns()
	if err != nil || len(feats) != 3 || feats[0].Name != "x1" || feats[2].Type.TypeKind != "STRING" {
		t.Errorf("RawFeatureColumns: %v, %v", feats, err)
	}
	labels, _ := md.RawLabelColumns()
	if len(labels) != 1 || labels[0].Name != "predicted_y" {
		t.Errorf("RawLabelColumns: %v", labels)
	}
	if tr := md.RawTrainingRuns()[0]; tr.TrainingOptions.OptimizationStrategy != "NORMAL_EQUATION" ||
		tr.EvaluationMetrics.RegressionMetrics == nil || math.Abs(tr.EvaluationMetrics.RegressionMetrics.RSquared-1) > 1e-6 {
		t.Errorf("training run: %+v", tr)
	}

	weights := map[string]bigquery.Value{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT processed_input, weight FROM ML.WEIGHTS(MODEL `%s.lin`)", name)) {
		weights[r["processed_input"].(string)] = r["weight"]
	}
	if !near(weights["x1"], 2, 1e-6) || !near(weights["x2"], -1, 1e-6) || weights["c"] != nil {
		t.Errorf("ML.WEIGHTS: %v; want x1 2, x2 -1, c NULL (categorical)", weights)
	}
	cw := mlQuery(t, h, c, fmt.Sprintf("SELECT cw.category, cw.weight FROM ML.WEIGHTS(MODEL `%s.lin`), UNNEST(category_weights) AS cw ORDER BY cw.category", name))
	if len(cw) != 2 || cw[0]["category"] != "a" || cw[1]["category"] != "b" {
		t.Fatalf("ML.WEIGHTS category_weights: %v", cw)
	}
	if d := cw[1]["weight"].(float64) - cw[0]["weight"].(float64); math.Abs(d-5) > 1e-6 {
		t.Errorf("category b - a = %g; want 5", d)
	}

	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT x1, predicted_y FROM ML.PREDICT(MODEL `%s.lin`, "+
		"(SELECT 4 AS x1, 2.0 AS x2, 'b' AS c UNION ALL SELECT 0, 0.0, 'a')) ORDER BY x1", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], 3, 1e-6) || !near(pred[1]["predicted_y"], 14, 1e-6) {
		t.Errorf("ML.PREDICT: %v; want 3 and 14", pred)
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.lin`)", name))
	if len(ev) != 1 || !near(ev[0]["r2_score"], 1, 1e-6) || !near(ev[0]["mean_absolute_error"], 0, 1e-6) {
		t.Errorf("ML.EVALUATE: %v", ev)
	}
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.lin`, "+
		"(SELECT 1 AS x1, 0.0 AS x2, 'a' AS c, 6.0 AS y))", name))
	if len(ev) != 1 || !near(ev[0]["mean_absolute_error"], 1, 1e-6) {
		t.Errorf("ML.EVALUATE with input: %v; want MAE 1 (prediction 5, label 6)", ev)
	}
	fi := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FEATURE_INFO(MODEL `%s.lin`) ORDER BY input", name))
	if len(fi) != 3 || fi[0]["input"] != "c" || fi[0]["category_count"] != int64(2) || !near(fi[1]["min"], 0, 0) || !near(fi[1]["max"], 6, 0) {
		t.Errorf("ML.FEATURE_INFO: %v", fi)
	}
	if ti := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.TRAINING_INFO(MODEL `%s.lin`)", name)); len(ti) != 1 {
		t.Errorf("ML.TRAINING_INFO: %v", ti)
	}

	// The logistic model.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.logit` OPTIONS(model_type='logistic_reg', input_label_cols=['label'], max_iterations=50) AS "+
		"SELECT x1, label FROM `%s.train`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL logistic_reg: %v", err)
	}
	pred = mlQuery(t, h, c, fmt.Sprintf("SELECT x1, predicted_label, predicted_label_probs FROM ML.PREDICT(MODEL `%s.logit`, "+
		"(SELECT 0 AS x1 UNION ALL SELECT 6)) ORDER BY x1", name))
	if len(pred) != 2 || pred[0]["predicted_label"] != false || pred[1]["predicted_label"] != true {
		t.Errorf("ML.PREDICT logistic: %v", pred)
	} else if probs, ok := pred[1]["predicted_label_probs"].([]bigquery.Value); !ok || len(probs) != 2 {
		t.Errorf("predicted_label_probs: %v", pred[1]["predicted_label_probs"])
	}
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT accuracy, roc_auc FROM ML.EVALUATE(MODEL `%s.logit`)", name))
	if len(ev) != 1 || ev[0]["accuracy"].(float64) < 0.9 || ev[0]["roc_auc"].(float64) < 0.9 {
		t.Errorf("ML.EVALUATE logistic: %v", ev)
	}
	if md, err := ds.Model("logit").Metadata(ctx); err != nil || md.Type != "LOGISTIC_REGRESSION" {
		t.Errorf("Model.Metadata logistic: %v, %v", md, err)
	}

	// models.list, models.patch, models.delete.
	var ids []string
	it := ds.Models(ctx)
	for {
		m, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Dataset.Models: %v", err)
		}
		ids = append(ids, m.ModelID)
	}
	if strings.Join(ids, ",") != "lin,logit" {
		t.Errorf("Dataset.Models: %v", ids)
	}
	upd := bigquery.ModelMetadataToUpdate{Description: "linear", Name: "Linear"}
	upd.SetLabel("team", "ml")
	got, err := ds.Model("lin").Update(ctx, upd, "")
	if err != nil || got.Description != "linear" || got.Name != "Linear" || got.Labels["team"] != "ml" {
		t.Errorf("Model.Update: %+v, %v", got, err)
	}
	if again, err := ds.Model("lin").Metadata(ctx); err != nil || again.Description != "linear" || again.Labels["team"] != "ml" {
		t.Errorf("Model.Metadata after Update: %+v, %v", again, err)
	}
	if err := ds.Model("logit").Delete(ctx); err != nil {
		t.Errorf("Model.Delete: %v", err)
	}
	_, err = ds.Model("logit").Metadata(ctx)
	wantHTTPStatus(t, "Model.Metadata of a deleted model", err, http.StatusNotFound)

	// A second CREATE MODEL fails; OR REPLACE and DROP MODEL do not.
	create := fmt.Sprintf("CREATE MODEL `%s.lin` OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x1, y FROM `%s.train`", name, name)
	if err := mlRun(h, c, create); err == nil || !strings.Contains(err.Error(), "Already Exists") {
		t.Errorf("CREATE MODEL of an existing model: %v", err)
	}
	if err := mlRun(h, c, strings.Replace(create, "CREATE MODEL", "CREATE OR REPLACE MODEL", 1)); err != nil {
		t.Errorf("CREATE OR REPLACE MODEL: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("DROP MODEL `%s.lin`", name)); err != nil {
		t.Errorf("DROP MODEL: %v", err)
	}
	_, err = ds.Model("lin").Metadata(ctx)
	wantHTTPStatus(t, "Model.Metadata of a dropped model", err, http.StatusNotFound)
	if err := mlRun(h, c, fmt.Sprintf("DROP MODEL IF EXISTS `%s.lin`", name)); err != nil {
		t.Errorf("DROP MODEL IF EXISTS: %v", err)
	}
	_ = project
}

// TestBigQueryMLRefusals: the model types and clauses CloudBurrow does not
// train are 501 naming them, and nothing is made.
//
// covers: bigquery.jobs.query
func TestBigQueryMLRefusals(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	for _, sql := range []string{
		"CREATE MODEL `%s.km` OPTIONS(model_type='dnn_linear_combined_regressor', input_label_cols=['amount']) AS SELECT id, amount FROM `%s.orders`",
		"CREATE MODEL `%s.km` OPTIONS(model_type='automl_regressor', input_label_cols=['amount']) AS SELECT id, amount FROM `%s.orders`",
		"CREATE MODEL `%s.km` TRANSFORM(ML.HASH_BUCKETIZE(CAST(id AS STRING), 10) AS id, amount) OPTIONS(model_type='linear_reg', input_label_cols=['amount']) AS SELECT id, amount FROM `%s.orders`",
		"CREATE MODEL `%s.km` OPTIONS(model_type='pca', num_principal_components=1, pca_solver='randomized') AS SELECT id, amount FROM `%s.orders`",
		"CREATE MODEL `%s.km` REMOTE WITH CONNECTION `us.conn` OPTIONS(endpoint='gemini-2.0-flash') -- %s",
	} {
		q := fmt.Sprintf(sql, ds.DatasetID, ds.DatasetID)
		_, err := c.Query(q).Read(ctx)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
			t.Errorf("%s: %v; want 501", q, err)
		}
	}
	_, err := ds.Model("km").Metadata(ctx)
	wantHTTPStatus(t, "Model.Metadata after refusals", err, http.StatusNotFound)
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.absent`, (SELECT 1 AS id))", ds.DatasetID)).Read(ctx)
	wantHTTPStatus(t, "ML.PREDICT of a missing model", err, http.StatusNotFound)
}

// TestBigQueryMLRandomSplit: a RANDOM split "is deterministic" and "based
// on the FARM_FINGERPRINT of the data (including the column name and
// schema)" (bigqueryml-syntax-create-glm). CloudBurrow's evaluation rows
// are those whose ABS(FARM_FINGERPRINT) of the row's text, % 100, is under
// data_split_eval_fraction · 100, so ML.EVALUATE equals the error of
// ML.PREDICT over exactly the rows the same SQL picks, and a second model
// of the same rows evaluates the same.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLRandomSplit(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	name := mlDataset(t, h, c, map[string]string{
		"noisy": "SELECT x, 2 * x + MOD(x * 37, 11) - 5 AS y FROM UNNEST(GENERATE_ARRAY(0, 599)) AS x",
	})
	for _, m := range []string{"r1", "r2"} {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='linear_reg', input_label_cols=['y'], "+
			"data_split_method='random', data_split_eval_fraction=0.3) AS SELECT x, y FROM `%s.noisy`", name, m, name)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m, err)
		}
	}
	evalRows := fmt.Sprintf("SELECT x, y FROM `%s.noisy` WHERE MOD(ABS(FARM_FINGERPRINT(CONCAT('x INT64=\"', CAST(x AS STRING), "+
		"'\",y INT64=\"', CAST(y AS STRING), '\"'))), 100) < 30", name)
	n := mlQuery(t, h, c, "SELECT COUNT(*) AS n FROM ("+evalRows+")")
	if len(n) != 1 || n[0]["n"].(int64) < 120 || n[0]["n"].(int64) > 240 {
		t.Fatalf("evaluation rows: %v; want about 30%% of 600", n)
	}
	want := mlQuery(t, h, c, fmt.Sprintf("SELECT AVG(ABS(predicted_y - y)) AS mae FROM ML.PREDICT(MODEL `%s.r1`, (%s))", name, evalRows))
	for _, m := range []string{"r1", "r2"} {
		ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.%s`)", name, m))
		if len(ev) != 1 || len(want) != 1 || !near(ev[0]["mean_absolute_error"], want[0]["mae"].(float64), 1e-9) {
			t.Errorf("ML.EVALUATE(%s): %v; want the error over the FARM_FINGERPRINT rows, %v", m, ev, want)
		}
	}
}
