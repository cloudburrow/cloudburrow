//go:build compat

package compat

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// BigQuery ML MATRIX_FACTORIZATION (#1504): explicit feedback trained by
// alternating least squares, ML.RECOMMEND, ML.EVALUATE, ML.WEIGHTS and
// ML.TRAINING_INFO through the official Go client.

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLMatrixFactorization(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	// rating = u·i + u + i/2: rank one plus intercepts, every pair rated.
	name := mlDataset(t, h, c, map[string]string{
		"ratings": "SELECT u AS user, CONCAT('i', CAST(i AS STRING)) AS item, CAST(u * i + u + i / 2 AS FLOAT64) AS rating " +
			"FROM UNNEST(GENERATE_ARRAY(1, 4)) AS u, UNNEST(GENERATE_ARRAY(1, 4)) AS i",
		"flat": "SELECT u AS user, CONCAT('i', CAST(i AS STRING)) AS item, 3.0 AS rating FROM UNNEST(GENERATE_ARRAY(1, 3)) AS u, UNNEST(GENERATE_ARRAY(1, 3)) AS i",
	})
	rating := func(u int64, item string) float64 {
		var i int64
		fmt.Sscanf(item, "i%d", &i)
		return float64(u*i+u) + float64(i)/2
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.mf` OPTIONS(model_type='matrix_factorization', user_col='user', item_col='item', "+
		"rating_col='rating', num_factors=2, l2_reg=0.000001, max_iterations=100, early_stop=FALSE) AS SELECT user, item, rating FROM `%s.ratings`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL matrix_factorization: %v", err)
	}
	md, err := c.Dataset(name).Model("mf").Metadata(ctx)
	if err != nil || md.Type != "MATRIX_FACTORIZATION" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions.NumFactors != 2 || runs[0].TrainingOptions.FeedbackType != "EXPLICIT" ||
		runs[0].EvaluationMetrics.RegressionMetrics == nil {
		t.Errorf("training run: %+v", runs)
	}
	all := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.RECOMMEND(MODEL `%s.mf`)", name))
	if len(all) != 16 {
		t.Fatalf("ML.RECOMMEND: %d rows; want 16", len(all))
	}
	for _, r := range all {
		if !near(r["predicted_rating"], rating(r["user"].(int64), r["item"].(string)), 1e-3) {
			t.Errorf("ML.RECOMMEND %v", r)
		}
	}
	one := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.RECOMMEND(MODEL `%s.mf`, (SELECT 2 AS user)) ORDER BY item", name))
	if len(one) != 4 || one[3]["item"] != "i4" || !near(one[3]["predicted_rating"], 12, 1e-3) {
		t.Errorf("ML.RECOMMEND of a user: %v", one)
	}
	pair := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.RECOMMEND(MODEL `%s.mf`, (SELECT 3 AS user, 'i2' AS item, 'kept' AS note))", name))
	if len(pair) != 1 || pair[0]["note"] != "kept" || !near(pair[0]["predicted_rating"], 10, 1e-3) {
		t.Fatalf("ML.RECOMMEND of a pair: %v", pair)
	}
	// The prediction is the weights' dot product plus both intercepts.
	ws := mlQuery(t, h, c, fmt.Sprintf("SELECT processed_input, feature, intercept, ARRAY(SELECT weight FROM UNNEST(factor_weights) ORDER BY factor) AS w "+
		"FROM ML.WEIGHTS(MODEL `%s.mf`) WHERE (processed_input = 'user' AND feature = '3') OR (processed_input = 'item' AND feature = 'i2') ORDER BY processed_input", name))
	if len(ws) != 2 {
		t.Fatalf("ML.WEIGHTS: %v", ws)
	}
	wi, wu := ws[0]["w"].([]bigquery.Value), ws[1]["w"].([]bigquery.Value)
	sum := ws[0]["intercept"].(float64) + ws[1]["intercept"].(float64)
	for k := range wi {
		sum += wi[k].(float64) * wu[k].(float64)
	}
	if len(wi) != 2 || !near(pair[0]["predicted_rating"], sum, 1e-9) {
		t.Errorf("ML.WEIGHTS %v: dot + intercepts = %v; ML.RECOMMEND %v", ws, sum, pair[0]["predicted_rating"])
	}
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_squared_error, r2_score FROM ML.EVALUATE(MODEL `%s.mf`)", name))
	if len(ev) != 1 || ev[0]["mean_squared_error"].(float64) > 1e-6 || !near(ev[0]["r2_score"], 1, 1e-6) {
		t.Errorf("ML.EVALUATE: %v", ev)
	}
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.mf`, (SELECT 1 AS user, 'i1' AS item, 7.5 AS rating))", name))
	if len(ev) != 1 || !near(ev[0]["mean_absolute_error"], 5, 1e-3) {
		t.Errorf("ML.EVALUATE of an input (rating 2.5 predicted, 7.5 given): %v", ev)
	}
	if ti := mlQuery(t, h, c, fmt.Sprintf("SELECT iteration, loss FROM ML.TRAINING_INFO(MODEL `%s.mf`) ORDER BY iteration", name)); len(ti) != 100 {
		t.Errorf("ML.TRAINING_INFO: %d rows; want 100", len(ti))
	}
	// Every rating 3: the weights solve to 0, so each prediction is 3
	// exactly, at the default l2_reg and num_factors.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.flat` OPTIONS(model_type='matrix_factorization') AS SELECT * FROM `%s.flat`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL flat: %v", err)
	}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT predicted_rating FROM ML.RECOMMEND(MODEL `%s.flat`)", name)) {
		if v := r["predicted_rating"].(float64); math.Abs(v-3) > 1e-12 {
			t.Errorf("flat ML.RECOMMEND: %v; want 3", v)
		}
	}
	for _, sql := range []string{
		"CREATE MODEL `%s.imp` OPTIONS(model_type='matrix_factorization', data_split_method='random') AS SELECT * FROM `%s.flat`",
	} {
		_, err := c.Query(fmt.Sprintf(sql, name, name)).Read(ctx)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
			t.Errorf("%s: %v; want 501", sql, err)
		}
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.mf`, (SELECT 1 AS user, 'i1' AS item))", name)).Read(ctx)
	wantHTTPStatus(t, "ML.PREDICT of a matrix factorization model", err, http.StatusBadRequest)
}

// TestBigQueryMLImplicitMatrixFactorization (#1766): an IMPLICIT matrix
// factorization (WALS, wals_alpha), through the official
// cloud.google.com/go/bigquery client: models.get names feedbackType
// IMPLICIT and walsAlpha 40 (the documented default) and the label column
// predicted_rating_confidence; ML.RECOMMEND answers
// predicted_rating_confidence for every user and item pair, each user's
// own items above the others'; ML.WEIGHTS answers the factors;
// wals_alpha on an EXPLICIT model is 400 and ML.EVALUATE of an IMPLICIT
// one 501.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLImplicitMatrixFactorization(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"views": "SELECT * FROM UNNEST([STRUCT('a' AS user, 'x' AS item, 3 AS rating), ('a', 'y', 1), ('b', 'x', 2), ('b', 'y', 4), " +
			"('c', 'z', 5), ('d', 'z', 1), ('d', 'w', 2)])",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.imp` OPTIONS(model_type='matrix_factorization', feedback_type='implicit', num_factors=2) "+
		"AS SELECT user, item, rating FROM `%s.views`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL implicit: %v", err)
	}
	md, err := c.Dataset(name).Model("imp").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	runs := md.RawTrainingRuns()
	if len(runs) != 1 || runs[0].TrainingOptions.FeedbackType != "IMPLICIT" || runs[0].TrainingOptions.WalsAlpha != 40 {
		t.Errorf("training run: %+v", runs)
	}
	if labels, _ := md.RawLabelColumns(); len(labels) != 1 || labels[0].Name != "predicted_rating_confidence" {
		t.Errorf("label columns: %+v", labels)
	}
	all := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.RECOMMEND(MODEL `%s.imp`)", name))
	if len(all) != 16 {
		t.Fatalf("ML.RECOMMEND: %d rows; want 16", len(all))
	}
	score := map[string]float64{}
	for _, r := range all {
		v, ok := r["predicted_rating_confidence"].(float64)
		if !ok {
			t.Fatalf("ML.RECOMMEND row %v", r)
		}
		score[r["user"].(string)+r["item"].(string)] = v
	}
	for _, p := range [][3]string{{"a", "x", "z"}, {"b", "y", "z"}, {"c", "z", "x"}, {"d", "w", "y"}} {
		if score[p[0]+p[1]] <= score[p[0]+p[2]] {
			t.Errorf("user %s: %s %.4f is not above %s %.4f", p[0], p[1], score[p[0]+p[1]], p[2], score[p[0]+p[2]])
		}
	}
	if w := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.WEIGHTS(MODEL `%s.imp`)", name)); len(w) != 8 {
		t.Errorf("ML.WEIGHTS: %d rows; want 8", len(w))
	}
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='matrix_factorization', wals_alpha=2) AS SELECT user, item, rating FROM `%s.views`", name, name)).Read(ctx)
	wantHTTPStatus(t, "wals_alpha of an EXPLICIT model", err, http.StatusBadRequest)
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.imp`)", name)).Read(ctx)
	wantHTTPStatus(t, "ML.EVALUATE of an IMPLICIT model", err, http.StatusNotImplemented)
}
