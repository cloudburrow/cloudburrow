//go:build compat

package compat

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"testing"
)

// BigQuery ML DNN_REGRESSOR, DNN_CLASSIFIER and AUTOENCODER (#1548,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-dnn-models,
// .../bigqueryml-syntax-create-autoencoder), trained by the front, through
// the official Go client.
//
// The regressor is checked against a network computed by hand: one
// feature x of 0 and 2 (standardized ∓1/√2), labels 1 and -2,
// hidden_units [2], TANH, optimizer SGD, learn_rate 0.1, one pass of one
// batch (batch_size is 32 or the rows when fewer): the Glorot uniform
// initial weights CloudBurrow draws from its seed (1548, docs/compatibility.md),
// then one step down the gradient of the batch's mean squared error,
// derived here by hand; ML.PREDICT, ML.EVALUATE and ML.TRAINING_INFO give
// that network's values. The classifier's and the autoencoder's
// ML.PREDICT, ML.RECONSTRUCTION_LOSS (SQL of the network) agree with
// their ML.EVALUATE (computed in Go).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLDNN(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"two":  "SELECT 0.0 AS x, 1.0 AS y UNION ALL SELECT 2.0, -2.0",
		"step": "SELECT CAST(x AS FLOAT64) AS x, x >= 5 AS hi FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
		"pair": "SELECT CAST(x AS FLOAT64) AS a, CAST(2 * x AS FLOAT64) AS b FROM UNNEST(GENERATE_ARRAY(0, 19)) AS x",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.dnn` OPTIONS(model_type='dnn_regressor', input_label_cols=['y'], hidden_units=[2], "+
		"activation_fn='tanh', optimizer='sgd', learn_rate=0.1, max_iterations=1) AS SELECT x, y FROM `%s.two`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL dnn_regressor: %v", err)
	}
	md, err := c.Dataset(name).Model("dnn").Metadata(ctx)
	if err != nil || md.Type != "DNN_REGRESSOR" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || len(runs[0].TrainingOptions.HiddenUnits) != 1 || runs[0].TrainingOptions.HiddenUnits[0] != 2 ||
		runs[0].TrainingOptions.ActivationFn != "TANH" || runs[0].TrainingOptions.Optimizer != "SGD" || runs[0].TrainingOptions.BatchSize != 2 ||
		len(runs[0].Results) != 1 {
		t.Errorf("training run: %+v", runs)
	}

	// By hand. The initial weights: hidden w1, b1 = 0; output v, b2 = 0.
	rng := rand.New(rand.NewSource(1548))
	l1, l2 := math.Sqrt(6.0/3), math.Sqrt(6.0/3)
	w1 := []float64{(2*rng.Float64() - 1) * l1, (2*rng.Float64() - 1) * l1}
	v := []float64{(2*rng.Float64() - 1) * l2, (2*rng.Float64() - 1) * l2}
	b1, b2 := []float64{0, 0}, 0.0
	enc := func(x float64) float64 { return (x - 1) / math.Sqrt2 }
	net := func(e float64) float64 {
		z := b2
		for j := range v {
			z += v[j] * math.Tanh(b1[j]+w1[j]*e)
		}
		return z
	}
	xs, ys := []float64{0, 2}, []float64{1, -2}
	gw, gb, gv, gb2 := make([]float64, 2), make([]float64, 2), make([]float64, 2), 0.0
	for i := range xs {
		e := enc(xs[i])
		dz := 2 * (net(e) - ys[i]) / 2 // the batch's mean
		gb2 += dz
		for j := range v {
			hj := math.Tanh(b1[j] + w1[j]*e)
			gv[j] += dz * hj
			gw[j] += dz * v[j] * (1 - hj*hj) * e
			gb[j] += dz * v[j] * (1 - hj*hj)
		}
	}
	for j := range v {
		w1[j], b1[j], v[j] = w1[j]-0.1*gw[j], b1[j]-0.1*gb[j], v[j]-0.1*gv[j]
	}
	b2 -= 0.1 * gb2
	p0, p2 := net(enc(0)), net(enc(2))
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.dnn`, (SELECT 0.0 AS x UNION ALL SELECT 2.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], p0, 1e-9) || !near(pred[1]["predicted_y"], p2, 1e-9) {
		t.Errorf("ML.PREDICT: %v; want %v and %v", pred, p0, p2)
	}
	mse := ((p0-1)*(p0-1) + (p2+2)*(p2+2)) / 2
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_squared_error FROM ML.EVALUATE(MODEL `%s.dnn`)", name))
	if len(ev) != 1 || !near(ev[0]["mean_squared_error"], mse, 1e-9) {
		t.Errorf("ML.EVALUATE: %v; want %v", ev, mse)
	}
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT mean_absolute_error FROM ML.EVALUATE(MODEL `%s.dnn`, (SELECT 0.0 AS x, 0.0 AS y))", name))
	if len(ev) != 1 || !near(ev[0]["mean_absolute_error"], math.Abs(p0), 1e-9) {
		t.Errorf("ML.EVALUATE of an input: %v; want %v", ev, math.Abs(p0))
	}
	ti := mlQuery(t, h, c, fmt.Sprintf("SELECT iteration, loss FROM ML.TRAINING_INFO(MODEL `%s.dnn`)", name))
	if len(ti) != 1 || ti[0]["iteration"] != int64(0) || !near(ti[0]["loss"], mse, 1e-9) {
		t.Errorf("ML.TRAINING_INFO: %v; want one iteration of loss %v", ti, mse)
	}

	// The classifier: its probabilities, SQL of the network, give the
	// log loss ML.EVALUATE computes in Go.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.dc` OPTIONS(model_type='dnn_classifier', input_label_cols=['hi'], hidden_units=[4, 3], "+
		"learn_rate=0.05, max_iterations=30, early_stop=FALSE) AS SELECT x, hi FROM `%s.step`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL dnn_classifier: %v", err)
	}
	cls := mlQuery(t, h, c, fmt.Sprintf("SELECT AVG(-LN(IF(hi, p, 1 - p))) AS ll, COUNTIF(predicted_hi = hi) AS correct FROM ("+
		"SELECT hi, predicted_hi, (SELECT prob FROM UNNEST(predicted_hi_probs) WHERE label) AS p FROM ML.PREDICT(MODEL `%s.dc`, TABLE `%s.step`))", name, name))
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT log_loss, accuracy FROM ML.EVALUATE(MODEL `%s.dc`)", name))
	if len(cls) != 1 || len(ev) != 1 || !near(cls[0]["ll"], ev[0]["log_loss"].(float64), 1e-9) ||
		!near(ev[0]["accuracy"], float64(cls[0]["correct"].(int64))/10, 1e-9) {
		t.Errorf("classifier: ML.PREDICT %v, ML.EVALUATE %v", cls, ev)
	}

	// The autoencoder: hidden_units [3, 1, 3], the middle its latent
	// space; ML.RECONSTRUCTION_LOSS's rows average to ML.EVALUATE's.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ae` OPTIONS(model_type='autoencoder', hidden_units=[3, 1, 3], activation_fn='tanh', "+
		"learn_rate=0.01, max_iterations=10) AS SELECT a, b FROM `%s.pair`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL autoencoder: %v", err)
	}
	if md, err := c.Dataset(name).Model("ae").Metadata(ctx); err != nil || md.Type != "AUTOENCODER" {
		t.Errorf("autoencoder Model.Metadata: %v, %v", md, err)
	}
	lat := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.ae`, (SELECT 1.0 AS a, 2.0 AS b))", name))
	if len(lat) != 1 || len(lat[0]) != 3 || lat[0]["a"] != 1.0 {
		t.Errorf("autoencoder ML.PREDICT: %v; want latent_col_1, a and b", lat)
	} else if _, ok := lat[0]["latent_col_1"].(float64); !ok {
		t.Errorf("autoencoder ML.PREDICT latent_col_1: %v", lat[0])
	}
	rl := mlQuery(t, h, c, fmt.Sprintf("SELECT AVG(mean_squared_error) AS mse, AVG(mean_absolute_error) AS mae FROM ML.RECONSTRUCTION_LOSS(MODEL `%s.ae`, TABLE `%s.pair`)", name, name))
	ev = mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.ae`)", name))
	if len(rl) != 1 || len(ev) != 1 || len(ev[0]) != 3 || !near(rl[0]["mse"], ev[0]["mean_squared_error"].(float64), 1e-9) ||
		!near(rl[0]["mae"], ev[0]["mean_absolute_error"].(float64), 1e-9) {
		t.Errorf("autoencoder: ML.RECONSTRUCTION_LOSS %v, ML.EVALUATE %v", rl, ev)
	}

	// What stays refused.
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.WEIGHTS(MODEL `%s.dnn`)", name)).Read(ctx)
	wantHTTPStatus(t, "ML.WEIGHTS of a DNN", err, http.StatusBadRequest)
	for what, sql := range map[string]string{
		"ML.EXPLAIN_PREDICT of a DNN":   "SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.dnn`, (SELECT 1.0 AS x))",
		"DNN_LINEAR_COMBINED_REGRESSOR": "CREATE MODEL `%s.no` OPTIONS(model_type='dnn_linear_combined_regressor', input_label_cols=['y']) AS SELECT x, y FROM `%[1]s.two`",
		"an imported ONNX model":        "CREATE MODEL `%s.no` OPTIONS(model_type='onnx', model_path='gs://bucket/model.onnx')",
	} {
		_, err := c.Query(fmt.Sprintf(sql, name)).Read(ctx)
		wantHTTPStatus(t, what, err, http.StatusNotImplemented)
	}
}
