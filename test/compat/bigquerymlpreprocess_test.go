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

// BigQuery ML preprocessing functions (#1504): the ML functions of no
// model, in a query and in a TRANSFORM clause, through the official Go
// client, against values computed by hand.

// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLPreprocessFunctions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	row := mlQuery(t, h, c, "SELECT ML.BUCKETIZE(2.5, [1.0, 2.0, 3.0]) AS bk, ML.BUCKETIZE(2.5, [1.0, 2.0, 3.0], TRUE) AS bx, "+
		"ML.BUCKETIZE(0.5, [1.0, 2.0, 3.0]) AS low, ML.BUCKETIZE(2.0, [1.0, 2.0, 3.0]) AS edge, ML.BUCKETIZE(CAST(NULL AS FLOAT64), [1.0]) AS n, "+
		"p.a, p.b, p.a_a, p.a_b, p.b_b, q.a_a_b, q.b_b_b, x.f1_f2, x.f1_f3, x.f2_f3, "+
		"ML.DISTANCE([1.0, 2.0], [4.0, 6.0]) AS de, ML.DISTANCE([1.0, 2.0], [4.0, 6.0], 'MANHATTAN') AS dm, "+
		"ML.DISTANCE([1.0, 0.0], [0.0, 1.0], 'COSINE') AS dc, ML.LP_NORM([3.0, -4.0], 2) AS l2, ML.LP_NORM([3.0, -4.0], 1) AS l1, "+
		"ML.NORMALIZER([3.0, -4.0])[OFFSET(1)] AS n2, ML.NORMALIZER([3.0, -4.0], 1)[OFFSET(0)] AS n1 "+
		"FROM (SELECT ML.POLYNOMIAL_EXPAND(STRUCT(2 AS a, 3 AS b)) AS p, ML.POLYNOMIAL_EXPAND(STRUCT(2 AS a, 3 AS b), 3) AS q, "+
		"ML.FEATURE_CROSS(STRUCT('a' AS f1, 'b' AS f2, 'c' AS f3)) AS x)")
	if len(row) != 1 {
		t.Fatalf("rows: %v", row)
	}
	r := row[0]
	for k, want := range map[string]string{"bk": "bin_3", "bx": "bin_2", "low": "bin_1", "edge": "bin_3", "f1_f2": "a_b", "f1_f3": "a_c", "f2_f3": "b_c"} {
		if r[k] != want {
			t.Errorf("%s = %v; want %s", k, r[k], want)
		}
	}
	if r["n"] != nil {
		t.Errorf("ML.BUCKETIZE(NULL) = %v; want NULL", r["n"])
	}
	for k, want := range map[string]float64{"a": 2, "b": 3, "a_a": 4, "a_b": 6, "b_b": 9, "a_a_b": 12, "b_b_b": 27, "de": 5, "dm": 7, "dc": 1, "l2": 5, "l1": 7, "n2": -0.8, "n1": 3.0 / 7} {
		if !near(r[k], want, 1e-9) {
			t.Errorf("%s = %v; want %v", k, r[k], want)
		}
	}
	// The analytic ones over their window.
	sd := math.Sqrt(2.5)
	got := mlQuery(t, h, c, "SELECT v, ML.STANDARD_SCALER(v) OVER () AS s, ML.MIN_MAX_SCALER(v) OVER () AS mm, ML.MAX_ABS_SCALER(v - 6) OVER () AS ma, "+
		"ML.IMPUTER(IF(v = 3, NULL, v), 'mean') OVER () AS im FROM UNNEST([1.0, 2.0, 3.0, 4.0, 5.0]) AS v ORDER BY v")
	if len(got) != 5 {
		t.Fatalf("analytic rows: %v", got)
	}
	for i, g := range got {
		v := float64(i + 1)
		if !near(g["s"], (v-3)/sd, 1e-9) || !near(g["mm"], (v-1)/4, 1e-9) || !near(g["ma"], (v-6)/5, 1e-9) || !near(g["im"], v, 1e-9) {
			t.Errorf("row %v: %v", v, g)
		}
	}
	for _, sql := range []string{
		"SELECT ML.IMPUTER(v, 'median') OVER () FROM UNNEST([1.0, 2.0]) AS v",
	} {
		_, err := c.Query(sql).Read(ctx)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
			t.Errorf("%s: %v; want 501", sql, err)
		}
	}
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLTransformPreprocess(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	// y = 3 + 2·x for x in 0..9; z is 10 from x = 5 up, else 0.
	name := mlDataset(t, h, c, map[string]string{
		"rows": "SELECT CAST(x AS FLOAT64) AS x, 3.0 + 2 * x AS y, IF(x >= 5, 10.0, 0.0) AS z FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ss` TRANSFORM(ML.STANDARD_SCALER(x) OVER () AS xs, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM `%s.rows`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.STANDARD_SCALER: %v", err)
	}
	// The scaler's mean and deviation are those of the training rows,
	// kept: x = 4 predicts 11, and xs's weight is 2·sd(x).
	sd := math.Sqrt(82.5 / 9)
	w := map[string]bigquery.Value{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT processed_input, weight FROM ML.WEIGHTS(MODEL `%s.ss`)", name)) {
		w[r["processed_input"].(string)] = r["weight"]
	}
	if !near(w["xs"], 2*sd, 1e-6) {
		t.Errorf("ML.WEIGHTS xs = %v; want %v", w["xs"], 2*sd)
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT predicted_y FROM ML.PREDICT(MODEL `%s.ss`, (SELECT 4.0 AS x))", name))
	if len(pred) != 1 || !near(pred[0]["predicted_y"], 11, 1e-6) {
		t.Errorf("ML.PREDICT standard-scaled: %v; want 11", pred)
	}
	// ML.MIN_MAX_SCALER caps at 1 past the training maximum: x = 20
	// predicts as x = 9 does, 21.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.mm` TRANSFORM(ML.MIN_MAX_SCALER(x) OVER () AS xm, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM `%s.rows`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.MIN_MAX_SCALER: %v", err)
	}
	pred = mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.mm`, (SELECT 20.0 AS x UNION ALL SELECT 2.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], 7, 1e-6) || !near(pred[1]["predicted_y"], 21, 1e-6) {
		t.Errorf("ML.PREDICT min-max-scaled: %v; want 7 and 21", pred)
	}
	// ML.BUCKETIZE makes a category: z is learned exactly.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bk` TRANSFORM(ML.BUCKETIZE(x, [5.0]) AS b, z) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['z']) AS SELECT x, z FROM `%s.rows`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.BUCKETIZE: %v", err)
	}
	pred = mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_z FROM ML.PREDICT(MODEL `%s.bk`, (SELECT 1.0 AS x UNION ALL SELECT 7.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_z"], 0, 1e-6) || !near(pred[1]["predicted_z"], 10, 1e-6) {
		t.Errorf("ML.PREDICT bucketized: %v; want 0 and 10", pred)
	}
	// A window that is not an ML function stays 501.
	_, err := c.Query(fmt.Sprintf("CREATE MODEL `%s.win` TRANSFORM(AVG(x) OVER () AS a, y) OPTIONS(model_type='linear_reg', input_label_cols=['y']) "+
		"AS SELECT x, y FROM `%s.rows`", name, name)).Read(h.Context())
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
		t.Errorf("TRANSFORM with AVG OVER: %v; want 501", err)
	}
}

// TestBigQueryMLQuantilePreprocess (#1548): ML.QUANTILE_BUCKETIZE,
// ML.ROBUST_SCALER and ML.NGRAMS as BigQuery documents them, on its own
// examples (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-quantile-bucketize,
// .../bigqueryml-syntax-robust-scaler, .../bigqueryml-syntax-ngrams), in a
// query and in a TRANSFORM, whose quantiles are learned over the training
// rows and kept (the vocabulary functions are
// TestBigQueryMLVocabularyPreprocess's, ML.HASH_BUCKETIZE
// TestBigQueryMLHashBucketize's).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLQuantilePreprocess(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	got := mlQuery(t, h, c, "SELECT f, ML.QUANTILE_BUCKETIZE(f, 3) OVER () AS bucket FROM UNNEST([1, 2, 3, 4, 5]) AS f ORDER BY f")
	want := []string{"bin_1", "bin_2", "bin_2", "bin_3", "bin_3"}
	if len(got) != 5 {
		t.Fatalf("ML.QUANTILE_BUCKETIZE rows: %v", got)
	}
	for i, g := range got {
		if g["bucket"] != want[i] {
			t.Errorf("ML.QUANTILE_BUCKETIZE(%v, 3) = %v; want %s", g["f"], g["bucket"], want[i])
		}
	}
	got = mlQuery(t, h, c, "SELECT f, ML.ROBUST_SCALER(f) OVER () AS output FROM UNNEST([NULL, -3, 1, 2, 3, 4, 5]) AS f ORDER BY f")
	wantR := []any{nil, -5.0 / 3, -1.0 / 3, 0.0, 1.0 / 3, 2.0 / 3, 1.0}
	if len(got) != 7 {
		t.Fatalf("ML.ROBUST_SCALER rows: %v", got)
	}
	for i, g := range got {
		if wantR[i] == nil {
			if g["output"] != nil {
				t.Errorf("ML.ROBUST_SCALER(NULL) = %v", g["output"])
			}
			continue
		}
		if !near(g["output"], wantR[i].(float64), 1e-12) {
			t.Errorf("ML.ROBUST_SCALER(%v) = %v; want %v", g["f"], g["output"], wantR[i])
		}
	}
	got = mlQuery(t, h, c, "SELECT ML.NGRAMS(['a', 'b', 'c'], [2,3], '#') AS output, ML.NGRAMS(['a', 'b', 'c'], 1) AS ones, "+
		"ML.NGRAMS(['x', 'y'], 2) AS spaced")
	if len(got) != 1 || fmt.Sprint(got[0]["output"]) != "[a#b a#b#c b#c]" || fmt.Sprint(got[0]["ones"]) != "[a b c]" || fmt.Sprint(got[0]["spaced"]) != "[x y]" {
		t.Errorf("ML.NGRAMS: %v", got)
	} else if s := got[0]["spaced"].([]bigquery.Value); len(s) != 1 {
		t.Errorf("ML.NGRAMS(['x', 'y'], 2) = %q; want one bigram", s)
	}

	// In a TRANSFORM: the training rows' quartiles kept for prediction.
	// x in 0..9: median 4, quartiles 2 and 7 (PERCENTILE_DISC); y = x.
	name := mlDataset(t, h, c, map[string]string{
		"q": "SELECT CAST(x AS FLOAT64) AS x, CAST(x AS FLOAT64) AS y FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.rs` TRANSFORM(ML.ROBUST_SCALER(x) OVER () AS xr, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM `%s.q`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.ROBUST_SCALER: %v", err)
	}
	pred := mlQuery(t, h, c, fmt.Sprintf("SELECT predicted_y FROM ML.PREDICT(MODEL `%s.rs`, (SELECT 100.0 AS x))", name))
	if len(pred) != 1 || !near(pred[0]["predicted_y"], 100, 1e-6) {
		t.Errorf("ML.PREDICT robust-scaled: %v; want 100", pred)
	}
	feat := mlQuery(t, h, c, fmt.Sprintf("SELECT min, max FROM ML.FEATURE_INFO(MODEL `%s.rs`) WHERE input = 'xr'", name))
	if len(feat) != 1 || !near(feat[0]["min"], -4.0/5, 1e-9) || !near(feat[0]["max"], 1, 1e-9) {
		t.Errorf("ML.FEATURE_INFO of xr: %v; want min -0.8 and max 1 ((x - 4) / (7 - 2))", feat)
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.qb` TRANSFORM(ML.QUANTILE_BUCKETIZE(x, 2) OVER () AS b, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM `%s.q`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with ML.QUANTILE_BUCKETIZE: %v", err)
	}
	// Two buckets split at the median 4: bin_1 is x 0..3 (mean 1.5),
	// bin_2 x 4..9 (mean 6.5), whatever the rows predicted.
	pred = mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.qb`, (SELECT -50.0 AS x UNION ALL SELECT 4.0)) ORDER BY x", name))
	if len(pred) != 2 || !near(pred[0]["predicted_y"], 1.5, 1e-6) || !near(pred[1]["predicted_y"], 6.5, 1e-6) {
		t.Errorf("ML.PREDICT quantile-bucketized: %v; want 1.5 and 6.5", pred)
	}
	for _, sql := range []string{
		"SELECT ML.QUANTILE_BUCKETIZE(v, 2, 'bucket_ranges') OVER () FROM UNNEST([1.0, 2.0]) AS v",
	} {
		_, err := c.Query(sql).Read(ctx)
		wantHTTPStatus(t, sql, err, http.StatusNotImplemented)
	}
}
