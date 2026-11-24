//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"

	tfconvertimage "github.com/cloudburrow/cloudburrow/deploy/tfconvert"
)

// BigQuery ML's EXPORT MODEL and model extract jobs (#2210), through the
// official Go client, as "Export models"
// (https://docs.cloud.google.com/bigquery/docs/exporting-models) documents
// them: a LINEAR_REG model to a TF SavedModel (saved_model.pb, variables/,
// assets/) with EXPORT MODEL, which imports back as a TENSORFLOW model
// that predicts what the original does; a BOOSTED_TREE_CLASSIFIER to an
// XGBoost Booster (model.bst, assets/model_metadata.json, main.py,
// xgboost_predictor-0.1.tar.gz) with Model.ExtractorTo and
// DestinationFormat ML_XGBOOST_BOOSTER, as bq extract -m does; a format
// the model has none of; and a model that is not there.

// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.jobs.get, bigquery.models.get
func TestBigQueryMLExportModel(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH to run the TensorFlow converter image in")
	}
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	name := mlDataset(t, h, c, map[string]string{
		"lin": "SELECT CAST(x AS FLOAT64) AS x, 3.0 * x + 1.0 AS y FROM UNNEST(GENERATE_ARRAY(0, 19)) AS x",
		"cls": "SELECT CAST(MOD(i, 10) AS FLOAT64) AS x, ['red', 'blue'][OFFSET(MOD(i, 2))] AS c, MOD(i, 10) >= 5 AS label FROM UNNEST(GENERATE_ARRAY(0, 59)) AS i",
	})
	objects := func(prefix string) []string {
		var out []string
		for it := bh.Objects(ctx, &storage.Query{Prefix: prefix}); ; {
			o, err := it.Next()
			if err != nil {
				break
			}
			out = append(out, strings.TrimPrefix(o.Name, prefix))
		}
		sort.Strings(out)
		return out
	}
	has := func(got []string, want ...string) bool {
		for _, w := range want {
			if !sortContains(got, w) {
				return false
			}
		}
		return true
	}

	// LINEAR_REG with EXPORT MODEL: a TF SavedModel.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lin` OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM `%s.lin`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL linear_reg: %v", err)
	}
	uri := "gs://" + b + "/export/lin"
	if err := mlRunWithin(h, c, fmt.Sprintf("EXPORT MODEL `%s.lin` OPTIONS(URI = '%s')", name, uri), tensorflowImportDeadline); err != nil {
		t.Fatalf("EXPORT MODEL: %v", err)
	}
	if got := objects("export/lin/"); !has(got, "saved_model.pb", "variables/variables.index") {
		t.Fatalf("the TF SavedModel export wrote %v", got)
	}
	// The export is a SavedModel BigQuery imports: a TENSORFLOW model of
	// it predicts what the original does.
	if err := mlRunWithin(h, c, fmt.Sprintf("CREATE MODEL `%s.relin` OPTIONS(model_type='tensorflow', model_path='%s/*')", name, uri), tensorflowImportDeadline); err != nil {
		t.Fatalf("CREATE MODEL tensorflow from the export: %v", err)
	}
	want := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.lin`, (SELECT 2.5 AS x UNION ALL SELECT 7.0)) ORDER BY x", name))
	got := mlQuery(t, h, c, fmt.Sprintf("SELECT x, predicted_y FROM ML.PREDICT(MODEL `%s.relin`, (SELECT 2.5 AS x UNION ALL SELECT 7.0)) ORDER BY x", name))
	if len(want) != 2 || len(got) != 2 || !near(got[0]["predicted_y"], want[0]["predicted_y"].(float64), 1e-4) ||
		!near(got[1]["predicted_y"], want[1]["predicted_y"].(float64), 1e-4) {
		t.Errorf("the exported model predicts %v; the original %v", got, want)
	}
	// The job is an EXPORT_MODEL query.
	job, err := c.Query(fmt.Sprintf("EXPORT MODEL `%s.lin` OPTIONS(URI = '%s')", name, "gs://"+b+"/export/lin2")).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := job.Wait(ctx)
	if err != nil || st.Err() != nil {
		t.Fatalf("EXPORT MODEL job: %v %v", err, st)
	}
	if qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics); !ok || qs.StatementType != "EXPORT_MODEL" {
		t.Errorf("EXPORT MODEL's statistics: %+v", st.Statistics.Details)
	}

	// BOOSTED_TREE_CLASSIFIER with an extract job: an XGBoost Booster.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bt` OPTIONS(model_type='boosted_tree_classifier', input_label_cols=['label'], max_iterations=5) AS SELECT x, c, label FROM `%s.cls`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL boosted_tree_classifier: %v", err)
	}
	ref := bigquery.NewGCSReference("gs://" + b + "/export/bt")
	ref.DestinationFormat = bigquery.XGBoostBooster
	ej, err := c.Dataset(name).Model("bt").ExtractorTo(ref).Run(ctx)
	if err != nil {
		t.Fatalf("Model.ExtractorTo: %v", err)
	}
	if st, err := ej.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("the model extract job: %v %v", err, st)
	}
	if got := objects("export/bt/"); !has(got, "model.bst", "assets/model_metadata.json", "main.py", "xgboost_predictor-0.1.tar.gz") {
		t.Errorf("the XGBoost Booster export wrote %v", got)
	}

	// A format the model has none of fails the job; a model that is not
	// there is Not found.
	ref = bigquery.NewGCSReference("gs://" + b + "/export/wrong")
	ref.DestinationFormat = bigquery.XGBoostBooster
	ej, err = c.Dataset(name).Model("lin").ExtractorTo(ref).Run(ctx)
	if err == nil {
		st, werr := ej.Wait(ctx)
		err = werr
		if err == nil {
			err = st.Err()
		}
	}
	if err == nil || !strings.Contains(err.Error(), "ML_TF_SAVED_MODEL") {
		t.Errorf("a LINEAR_REG extract to ML_XGBOOST_BOOSTER: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("EXPORT MODEL `%s.nope` OPTIONS(URI = 'gs://%s/export/nope')", name, b)); err == nil ||
		!strings.Contains(err.Error(), "Not found: Model") {
		t.Errorf("EXPORT MODEL of a missing model: %v", err)
	}
}

func sortContains(s []string, v string) bool {
	i := sort.SearchStrings(s, v)
	return i < len(s) && s[i] == v
}

// EXPORT MODEL of a model with a TRANSFORM clause and of a TRANSFORM_ONLY
// model (#2210), as "Export models" documents them: the model's own
// SavedModel at the URI and "an additional preprocessing model [that]
// performs the same logic in the TRANSFORM clause ... under the
// subdirectory transform"; a TRANSFORM_ONLY model's preprocessing
// SavedModel at the URI. Each loads in TensorFlow (the converter image's
// TensorFlow, with no network) and answers what ML.TRANSFORM does, its
// inputs as the page's "Supported data types" table writes them (a DATE
// "%F", NULL 123456789.0e10); the transform's outputs fed to the model's
// SavedModel predict what ML.PREDICT does. An ARRAY output is refused.

// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.jobs.get
func TestBigQueryMLExportModelTransform(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH to run the TensorFlow converter image in")
	}
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	name := mlDataset(t, h, c, map[string]string{
		"tr": "SELECT CAST(i AS FLOAT64) / 3 AS x, ['red', 'green', 'blue'][OFFSET(MOD(i, 3))] AS c, DATE_ADD(DATE '2020-01-15', INTERVAL i * 11 DAY) AS d, " +
			"2 * CAST(i AS FLOAT64) / 3 + IF(MOD(i, 3) = 0, 1.0, 0.0) + EXTRACT(MONTH FROM DATE_ADD(DATE '2020-01-15', INTERVAL i * 11 DAY)) / 10 AS y " +
			"FROM UNNEST(GENERATE_ARRAY(0, 29)) AS i",
	})
	input := "SELECT 1.5 AS x, 'red' AS c, DATE '2020-03-04' AS d UNION ALL SELECT 7.0, 'blue', DATE '2021-07-19' UNION ALL SELECT CAST(NULL AS FLOAT64), 'purple', DATE '2020-12-31'"
	tfIn := map[string][]any{"x": {1.5, 7.0, 123456789.0e10}, "c": {"red", "blue", "purple"}, "d": {"2020-03-04", "2021-07-19", "2020-12-31"}}
	download := func(prefix string) string {
		dir := t.TempDir()
		n := 0
		for it := bh.Objects(ctx, &storage.Query{Prefix: prefix}); ; {
			o, err := it.Next()
			if err != nil {
				break
			}
			rel := strings.TrimPrefix(o.Name, prefix)
			r, err := bh.Object(o.Name).NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n == 0 {
			t.Fatalf("nothing under gs://%s/%s", b, prefix)
		}
		return dir
	}
	// serve runs the SavedModel in dir's serving_default on in, in
	// TensorFlow (the converter image's predict).
	serve := func(dir string, in map[string][]any) map[string][]any {
		t.Helper()
		body, _ := json.Marshal(in)
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "-e", "TF_CPP_MIN_LOG_LEVEL=2",
			"-v", dir+":/model:ro", tfconvertimage.Tag(), "predict")
		cmd.Stdin = bytes.NewReader(body)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("TensorFlow's serving_default: %v: %s", err, stderr.String())
		}
		var out map[string][]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		return out
	}
	same := func(got any, want bigquery.Value) bool {
		switch w := want.(type) {
		case nil:
			return got == "__MISSING__" || got == 123456789.0e10 || got == float64(1234567890000000000)
		case int64:
			return got == float64(w)
		case float64:
			g, ok := got.(float64)
			return ok && math.Abs(g-w) <= 1e-9*math.Max(1, math.Abs(w))
		}
		return fmt.Sprint(got) == fmt.Sprint(want)
	}
	matchTransform := func(model string, out map[string][]any, cols []string) {
		t.Helper()
		want := mlQuery(t, h, c, fmt.Sprintf("SELECT %s FROM ML.TRANSFORM(MODEL `%s.%s`, (%s)) ORDER BY c DESC", strings.Join(cols, ", "), name, model, input))
		// ORDER BY c DESC: red, purple, blue; the TensorFlow rows are red, blue, purple.
		order := []int{0, 2, 1}
		if len(want) != 3 {
			t.Fatalf("ML.TRANSFORM: %v", want)
		}
		for _, col := range cols {
			if len(out[col]) != 3 {
				t.Errorf("%s: the preprocessing SavedModel answers %v (outputs %v)", model, out[col], tfOutputNames(out))
				continue
			}
			for k, i := range order {
				if !same(out[col][i], want[k][col]) {
					t.Errorf("%s.%s row %d: TensorFlow %v, ML.TRANSFORM %v", model, col, i, out[col][i], want[k][col])
				}
			}
		}
	}

	// A linear regression with a TRANSFORM.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lt` TRANSFORM(ML.STANDARD_SCALER(x) OVER () AS xs, ML.BUCKETIZE(x, [2.0, 5.0]) AS xb, "+
		"ML.LABEL_ENCODER(c) OVER () AS ce, EXTRACT(MONTH FROM d) AS mo, y) OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, c, d, y FROM `%s.tr`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL with a TRANSFORM: %v", err)
	}
	if err := mlRunWithin(h, c, fmt.Sprintf("EXPORT MODEL `%s.lt` OPTIONS(URI = 'gs://%s/export/lt')", name, b), tensorflowImportDeadline); err != nil {
		t.Fatalf("EXPORT MODEL with a TRANSFORM: %v", err)
	}
	if got := exportObjects(ctx, bh, "export/lt/"); !hasAll(got, "saved_model.pb", "variables/variables.index", "transform/saved_model.pb", "transform/variables/variables.index") {
		t.Fatalf("the TRANSFORM model's export wrote %v", got)
	}
	pre := serve(download("export/lt/transform/"), tfIn)
	matchTransform("lt", pre, []string{"xs", "xb", "ce", "mo"})
	// The preprocessing outputs into the model's SavedModel: ML.PREDICT's
	// prediction (float32, as the model's export computes), for the rows
	// with no NULL.
	feats := map[string][]any{}
	for _, col := range []string{"xs", "xb", "ce", "mo"} {
		feats[col] = pre[col][:2]
	}
	pred := serve(download("export/lt/"), feats)
	want := mlQuery(t, h, c, fmt.Sprintf("SELECT c, predicted_y FROM ML.PREDICT(MODEL `%s.lt`, (%s)) WHERE x IS NOT NULL ORDER BY c DESC", name, input))
	if len(want) != 2 || len(pred["predicted_y"]) != 2 || !near(pred["predicted_y"][0], want[0]["predicted_y"].(float64), 1e-3) ||
		!near(pred["predicted_y"][1], want[1]["predicted_y"].(float64), 1e-3) {
		t.Errorf("the exported model predicts %v; ML.PREDICT %v", pred["predicted_y"], want)
	}

	// A TRANSFORM_ONLY model: its preprocessing SavedModel at the URI.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.to` TRANSFORM(UPPER(c) AS cu, DATE_DIFF(d, DATE '2020-01-01', DAY) AS dd, "+
		"FORMAT_DATE('%%b %%d', d) AS fd, ML.HASH_BUCKETIZE(c, 5) AS hb, IF(x > 4, 'big', 'small') AS sz, ML.MIN_MAX_SCALER(x) OVER () AS mm, "+
		"DATE_TRUNC(d, MONTH) AS dm) OPTIONS(model_type='transform_only') AS SELECT x, c, d FROM `%s.tr`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL transform_only: %v", err)
	}
	if err := mlRunWithin(h, c, fmt.Sprintf("EXPORT MODEL `%s.to` OPTIONS(URI = 'gs://%s/export/to')", name, b), tensorflowImportDeadline); err != nil {
		t.Fatalf("EXPORT MODEL transform_only: %v", err)
	}
	got := exportObjects(ctx, bh, "export/to/")
	if !hasAll(got, "saved_model.pb", "variables/variables.index") || hasAll(got, "transform/saved_model.pb") {
		t.Fatalf("the TRANSFORM_ONLY export wrote %v", got)
	}
	matchTransform("to", serve(download("export/to/"), tfIn), []string{"cu", "dd", "fd", "hb", "sz", "mm", "dm"})

	// Time zones, named (daylight saving time included) and an offset.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.tz` TRANSFORM("+
		"FORMAT_TIMESTAMP('%%F %%H:%%M %%Ez', TIMESTAMP(d, 'America/New_York'), 'Europe/Paris') AS fz, "+
		"EXTRACT(HOUR FROM TIMESTAMP(DATETIME(d), 'Asia/Kolkata') AT TIME ZONE 'America/Los_Angeles') AS hl, "+
		"STRING(TIMESTAMP(d), 'America/New_York') AS sn, "+
		"DATE(TIMESTAMP_TRUNC(TIMESTAMP(d, '+05:30'), MONTH, 'Australia/Sydney'), 'Australia/Sydney') AS ts) "+
		"OPTIONS(model_type='transform_only') AS SELECT d FROM `%s.tr`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL transform_only with time zones: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("EXPORT MODEL `%s.tz` OPTIONS(URI = 'gs://%s/export/tz')", name, b)); err != nil {
		t.Fatalf("EXPORT MODEL of a TRANSFORM with time zones: %v", err)
	}
	matchTransform("tz", serve(download("export/tz/"), map[string][]any{"d": tfIn["d"]}), []string{"fz", "hl", "sn", "ts"})

	// An ARRAY input is pre-transformed data only: no function the page
	// lists makes a scalar of one.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ar` TRANSFORM(ARRAY_LENGTH(arr) AS al) OPTIONS(model_type='transform_only') AS SELECT [x, x] AS arr FROM `%s.tr`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL transform_only of an ARRAY: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("EXPORT MODEL `%s.ar` OPTIONS(URI = 'gs://%s/export/ar')", name, b)); err == nil || !strings.Contains(err.Error(), "reads an ARRAY input") {
		t.Errorf("EXPORT MODEL of a TRANSFORM output of an ARRAY input: %v", err)
	}

	// An ARRAY output: "not supported as post-transformed data".
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.oh` TRANSFORM(ML.ONE_HOT_ENCODER(c) OVER () AS oh) OPTIONS(model_type='transform_only') AS SELECT c FROM `%s.tr`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL transform_only with ML.ONE_HOT_ENCODER: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("EXPORT MODEL `%s.oh` OPTIONS(URI = 'gs://%s/export/oh')", name, b)); err == nil || !strings.Contains(err.Error(), "post-transformed") {
		t.Errorf("EXPORT MODEL of an ARRAY TRANSFORM output: %v", err)
	}
}

func exportObjects(ctx context.Context, bh *storage.BucketHandle, prefix string) []string {
	var out []string
	for it := bh.Objects(ctx, &storage.Query{Prefix: prefix}); ; {
		o, err := it.Next()
		if err != nil {
			break
		}
		out = append(out, strings.TrimPrefix(o.Name, prefix))
	}
	sort.Strings(out)
	return out
}

func tfOutputNames(m map[string][]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
