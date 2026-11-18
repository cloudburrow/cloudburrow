//go:build compat

package compat

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"testing"
)

// TestBigQueryMLWideAndDeep (#1562): DNN_LINEAR_COMBINED_REGRESSOR and
// DNN_LINEAR_COMBINED_CLASSIFIER
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-wnd-models,
// read 2026-11-17), trained by the front through the official Go client.
// "All columns referenced by the query_statement are used as input
// features", so each goes to both the linear and the DNN part, and the
// output is the sum of the two, trained jointly, as in the wide-and-deep
// paper the page links (Cheng et al., 2016).
//
// The regressor is checked against the model computed by hand: one feature
// x of 0 and 2 (standardized ∓1/√2), labels 1 and -2, hidden_units [2],
// TANH, optimizer SGD and learn_rate 0.1 for both parts, one pass of one
// batch: the DNN's Glorot uniform initial weights from CloudBurrow's seed
// (1548), the linear part's 0, then one step down the gradient of the
// batch's mean squared error. The classifier, at the page's default
// optimizers ([STRUCT('dnn', 'ADAM'), STRUCT('linear', 'FTRL')]), gives
// the log loss through ML.PREDICT that ML.EVALUATE computes. Neither takes
// ML.EXPLAIN_PREDICT: BigQuery's explainability table
// (https://cloud.google.com/bigquery/docs/xai-overview) lists neither a DNN
// nor a wide-and-deep model, nor do their pages enable_global_explain.
//
// Measured first: CREATE MODEL of either was 501 ("model_type
// DNN_LINEAR_COMBINED_REGRESSOR"), and ML.EXPLAIN_PREDICT of a DNN 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLWideAndDeep(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"two":  "SELECT 0.0 AS x, 1.0 AS y UNION ALL SELECT 2.0, -2.0",
		"step": "SELECT CAST(x AS FLOAT64) AS x, x >= 5 AS hi FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.wd` OPTIONS(model_type='dnn_linear_combined_regressor', input_label_cols=['y'], "+
		"hidden_units=[2], activation_fn='tanh', optimizer='sgd', learn_rate=0.1, max_iterations=1) AS SELECT x, y FROM `%s.two`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL dnn_linear_combined_regressor: %v", err)
	}
	md, err := c.Dataset(name).Model("wd").Metadata(ctx)
	if err != nil || md.Type != "DNN_LINEAR_COMBINED_REGRESSOR" {
		t.Fatalf("Model.Metadata: %v, %v", md, err)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || len(runs[0].TrainingOptions.HiddenUnits) != 1 ||
		runs[0].TrainingOptions.ActivationFn != "TANH" || runs[0].TrainingOptions.Optimizer != "SGD" {
		t.Errorf("training run: %+v", runs)
	}

	// By hand. DNN: hidden w1, b1 = 0; output v, b2 = 0. Linear: a, c = 0.
	rng := rand.New(rand.NewSource(1548))
	l1, l2 := math.Sqrt(6.0/3), math.Sqrt(6.0/3)
	w1 := []float64{(2*rng.Float64() - 1) * l1, (2*rng.Float64() - 1) * l1}
	v := []float64{(2*rng.Float64() - 1) * l2, (2*rng.Float64() - 1) * l2}
	b1, b2, a, cb := []float64{0, 0}, 0.0, 0.0, 0.0
	enc := func(x float64) float64 { return (x - 1) / math.Sqrt2 }
	net := func(e float64) float64 {
		z := b2 + cb + a*e
		for j := range v {
			z += v[j] * math.Tanh(b1[j]+w1[j]*e)
		}
		return z
	}
	xs, ys := []float64{0, 2}, []float64{1, -2}
	gw, gb, gv, gb2, ga, gc := make([]float64, 2), make([]float64, 2), make([]float64, 2), 0.0, 0.0, 0.0
	for i := range xs {
		e := enc(xs[i])
		dz := 2 * (net(e) - ys[i]) / 2 // the batch's mean
		gb2 += dz
		ga += dz * e
		gc += dz
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
	b2, a, cb = b2-0.1*gb2, a-0.1*ga, cb-0.1*gc
	p0, p2 := net(enc(0)), net(enc(2))
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.wd`, (SELECT 0.0 AS x UNION ALL SELECT 2.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], p0, 1e-9) || !near(pred[1]["predicted_y"], p2, 1e-9) {
		t.Errorf("ML.PREDICT: %v; want %v and %v", pred, p0, p2)
	}
	mse := ((p0-1)*(p0-1) + (p2+2)*(p2+2)) / 2
	ev := mlQuery(t, h, c, fmt.Sprintf("SELECT mean_squared_error FROM ML.EVALUATE(MODEL `%s.wd`)", name))
	if len(ev) != 1 || !near(ev[0]["mean_squared_error"], mse, 1e-9) {
		t.Errorf("ML.EVALUATE: %v; want %v", ev, mse)
	}
	ti := mlQuery(t, h, c, fmt.Sprintf("SELECT iteration, loss FROM ML.TRAINING_INFO(MODEL `%s.wd`)", name))
	if len(ti) != 1 || !near(ti[0]["loss"], mse, 1e-9) {
		t.Errorf("ML.TRAINING_INFO: %v; want one iteration of loss %v", ti, mse)
	}

	// The classifier, at the default optimizers, and a value a part.
	for m, opts := range map[string]string{
		"wc":  "",
		"wc2": ", optimizer=[STRUCT('dnn', 'ADAGRAD'), STRUCT('linear', 'SGD')], learn_rate=[STRUCT('dnn', 0.05), STRUCT('linear', 0.1)], auto_class_weights=TRUE",
	} {
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='dnn_linear_combined_classifier', input_label_cols=['hi'], "+
			"hidden_units=[4], max_iterations=30, early_stop=FALSE%s) AS SELECT x, hi FROM `%[1]s.step`", name, m, opts)); err != nil {
			t.Fatalf("CREATE MODEL %s: %v", m, err)
		}
		cls := mlQuery(t, h, c, fmt.Sprintf("SELECT AVG(-LN(IF(hi, p, 1 - p))) AS ll, COUNTIF(predicted_hi = hi) AS correct FROM ("+
			"SELECT hi, predicted_hi, (SELECT prob FROM UNNEST(predicted_hi_probs) WHERE label) AS p FROM ML.PREDICT(MODEL `%s.%s`, TABLE `%[1]s.step`))", name, m))
		ev := mlQuery(t, h, c, fmt.Sprintf("SELECT log_loss, accuracy FROM ML.EVALUATE(MODEL `%s.%s`)", name, m))
		if len(cls) != 1 || len(ev) != 1 || !near(cls[0]["ll"], ev[0]["log_loss"].(float64), 1e-9) ||
			!near(ev[0]["accuracy"], float64(cls[0]["correct"].(int64))/10, 1e-9) {
			t.Errorf("%s: ML.PREDICT %v, ML.EVALUATE %v", m, cls, ev)
		}
		if md, err := c.Dataset(name).Model(m).Metadata(ctx); err != nil || md.Type != "DNN_LINEAR_COMBINED_CLASSIFIER" {
			t.Errorf("%s Model.Metadata: %v, %v", m, md, err)
		}
	}

	// What BigQuery refuses.
	for what, sql := range map[string]string{
		"ML.EXPLAIN_PREDICT of a wide-and-deep model": "SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.wd`, (SELECT 1.0 AS x))",
		"ML.GLOBAL_EXPLAIN of a wide-and-deep model":  "SELECT * FROM ML.GLOBAL_EXPLAIN(MODEL `%s.wd`)",
		"ML.WEIGHTS of a wide-and-deep model":         "SELECT * FROM ML.WEIGHTS(MODEL `%s.wd`)",
		"enable_global_explain on a DNN": "CREATE MODEL `%s.no` OPTIONS(model_type='dnn_regressor', input_label_cols=['y'], " +
			"enable_global_explain=TRUE) AS SELECT x, y FROM `%[1]s.two`",
		"l1_reg with the default optimizers (ADAM's DNN part)": "CREATE MODEL `%s.no` OPTIONS(model_type='dnn_linear_combined_regressor', " +
			"input_label_cols=['y'], l1_reg=0.1) AS SELECT x, y FROM `%[1]s.two`",
		"a value a part for a DNN": "CREATE MODEL `%s.no` OPTIONS(model_type='dnn_regressor', input_label_cols=['y'], " +
			"optimizer=[STRUCT('dnn', 'SGD')]) AS SELECT x, y FROM `%[1]s.two`",
	} {
		_, err := c.Query(fmt.Sprintf(sql, name)).Read(ctx)
		wantHTTPStatus(t, what, err, http.StatusBadRequest)
	}
}

// TestBigQueryMLHostedModelsRefused (#1562): what stays 501, each with the
// reason measured on CloudBurrow, nothing run and no model left behind.
// AutoML trains on Vertex AI's hosted service, whose model search Google
// does not document (an imported TensorFlow SavedModel converts on the
// CLI's TensorFlow converter since #1775,
// TestBigQueryMLImportedTensorFlowModel; a TensorFlow Lite one runs on
// internal/tflitemodel since #1755, TestBigQueryMLImportedTFLiteModel; an
// XGBoost one runs on internal/xgbmodel since #1711,
// TestBigQueryMLImportedXGBoostModel; an ONNX one in the `-tags onnx`
// build since #1711,
// TestBigQueryMLImportedONNXModel, and the default build's refusal is
// TestBigQueryMLONNXNeedsTheOnnxBuild); holiday_region reads
// Google's holiday calendar
// (bigquery-public-data.ml_datasets.holidays_and_events_for_forecasting),
// which CloudBurrow does not have. A remote model is no longer refused
// (#1760, #1765): on the DEFAULT connection, with no default connection
// and no bigquery.connections.create, it is 403 as Google documents
// (wantDefaultConnectionRefused).
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLHostedModelsRefused(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"two":  "SELECT 0.0 AS x, 1.0 AS y UNION ALL SELECT 2.0, -2.0",
		"walk": "SELECT DATE_ADD(DATE '2024-01-01', INTERVAL n DAY) AS d, CAST(n AS FLOAT64) AS v FROM UNNEST(GENERATE_ARRAY(0, 59)) AS n",
	})
	for what, c2 := range map[string]struct{ sql, want string }{
		"AUTOML_REGRESSOR": {"CREATE MODEL `%s.no` OPTIONS(model_type='automl_regressor', input_label_cols=['y'], budget_hours=1.0) AS SELECT x, y FROM `%[1]s.two`",
			"AutoML"},
		"AUTOML_CLASSIFIER": {"CREATE MODEL `%s.no` OPTIONS(model_type='automl_classifier', input_label_cols=['y']) AS SELECT x, y FROM `%[1]s.two`", "AutoML"},
		"holiday_region": {"CREATE MODEL `%s.no` OPTIONS(model_type='arima_plus', time_series_timestamp_col='d', time_series_data_col='v', " +
			"holiday_region='US') AS SELECT d, v FROM `%[1]s.walk`", "holiday calendar"},
	} {
		_, err := c.Query(fmt.Sprintf(c2.sql, name)).Read(ctx)
		wantAPIError(t, what, err, http.StatusNotImplemented, c2.want)
	}
	// A remote model is no hosted model CloudBurrow lacks: it runs on the
	// local Vertex AI since #1760, on the DEFAULT connection since #1765.
	wantDefaultConnectionRefused(t, h, c, fmt.Sprintf("CREATE MODEL `%s.no` REMOTE WITH CONNECTION DEFAULT OPTIONS(endpoint='gemini-2.0-flash')", name))
	_, err := c.Dataset(name).Model("no").Metadata(ctx)
	wantHTTPStatus(t, "Model.Metadata after the refusals", err, http.StatusNotFound)
}
