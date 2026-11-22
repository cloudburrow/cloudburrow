//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"

	tfconvertimage "github.com/cloudburrow/cloudburrow/deploy/tfconvert"
	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// TestBigQueryMLTensorFlowRuntime (#1775): imported TensorFlow SavedModels
// that TensorFlow Lite's builtin operators cannot take (they need Flex,
// SELECT_TF_OPS), which BigQuery runs with TensorFlow itself, so they
// import and predict here too: on TensorFlow, in the CLI's long-lived
// predictor container (Google's tensorflow/tensorflow:2.20.0 image, on an
// --internal Docker network), with the query's rows in batches. The
// models are made by the test in that image (testdata/tensorflow/
// gen_runtime.py), uploaded to the instance's Cloud Storage and used
// through the official bigquery and storage Go clients, as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tensorflow
// documents (read 2026-11-21):
//   - tf.train.Example inputs ("BigQuery ML automatically determines the
//     feature names and converts the input BigQuery columns"): a model
//     that parses Examples and splits a string feature with
//     tf.strings.split takes the features' columns, text STRING and
//     a ARRAY<FLOAT64>, and predicts TensorFlow's output for 20 rows;
//   - SparseTensor inputs ("You can pass SparseTensors as dense arrays and
//     BigQuery ML automatically converts them into Sparse format"): a
//     coo_sparse input v whose dense values go through
//     tf.strings.as_string takes an ARRAY<FLOAT64> (an empty one too);
//   - ML.EXPLAIN_PREDICT, which the page does not list among the functions
//     a TensorFlow model does not take and names as a resource-intensive
//     one it runs: Integrated Gradients (15 steps by default, from a
//     baseline of zeros) matches TensorFlow's own, as are
//     baseline_prediction_value and prediction_value, with the explain-
//     predict page's columns; integrated_gradients_num_steps and
//     top_k_features are taken, approx_feature_contrib (boosted trees and
//     random forests only) is 400.
//
// It runs in the localai shard, beside the other locally run models.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLTensorFlowRuntime(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH to run the TensorFlow converter image in")
	}
	bctx, cancel := context.WithTimeout(context.Background(), tfconvert.BuildTimeout)
	defer cancel()
	d := &tfconvert.Docker{Image: tfconvertimage.Tag(), Context: tfconvertimage.Tar}
	if err := d.Build(bctx); err != nil {
		t.Fatalf("build %s: %v", d.Image, err)
	}
	if err := d.EnsureNetwork(bctx); err != nil {
		t.Fatal(err)
	}
	gen, err := filepath.Abs(filepath.Join("testdata", "tensorflow"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Chmod(out, 0o777); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(bctx, "docker", "run", "--rm", "--network", tfconvert.Network, "--read-only",
		"--tmpfs", "/tmp", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/tmp", "-e", "TF_CPP_MIN_LOG_LEVEL=3", "-v", gen+":/w:ro", "-v", out+":/out",
		"--entrypoint", "python3", d.Image, "-I", "/w/gen_runtime.py", "/out")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		t.Fatalf("gen_runtime.py: %v\n%s", err, log.String())
	}

	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	models := []string{"exflex", "sparseflex", "mlp"}
	for _, m := range models {
		src := filepath.Join(out, m)
		err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			w := bh.Object("tfrt/" + m + "/" + filepath.ToSlash(rel)).NewWriter(ctx)
			if _, err := w.Write(data); err != nil {
				return err
			}
			return w.Close()
		})
		if err != nil {
			t.Fatalf("upload %s: %v", m, err)
		}
	}
	type rowRef struct {
		Inputs  map[string]any     `json:"inputs"`
		Outputs map[string][]any   `json:"outputs"`
		IG      map[string]float64 `json:"ig"`
	}
	var ref map[string][]rowRef
	raw, err := os.ReadFile(filepath.Join(out, "predictions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	floats := func(v any) string {
		var s []string
		for _, x := range v.([]any) {
			s = append(s, strconv.FormatFloat(x.(float64), 'g', -1, 64))
		}
		return "ARRAY<FLOAT64>[" + strings.Join(s, ", ") + "]"
	}
	var exRows, spRows, mlpRows []string
	for i, r := range ref["exflex"] {
		exRows = append(exRows, fmt.Sprintf("SELECT %d AS id, %q AS text, %s AS a", i, r.Inputs["text"], floats(r.Inputs["a"])))
	}
	for i, r := range ref["sparseflex"] {
		spRows = append(spRows, fmt.Sprintf("SELECT %d AS id, %s AS v", i, floats(r.Inputs["v"])))
	}
	for i, r := range ref["mlp"] {
		mlpRows = append(mlpRows, fmt.Sprintf("SELECT %d AS id, %s AS x", i, floats(r.Inputs["x"])))
	}
	name := mlDataset(t, h, c, map[string]string{"exflex": strings.Join(exRows, " UNION ALL "),
		"sparseflex": strings.Join(spRows, " UNION ALL "), "mlp": strings.Join(mlpRows, " UNION ALL ")})
	for _, m := range models {
		sql := fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfrt/%[2]s/*')", name, m, b)
		if err := mlRunWithin(h, c, sql, tensorflowImportDeadline); err != nil {
			t.Fatalf("CREATE MODEL %s (it needs Flex; BigQuery runs it on TensorFlow): %v", m, err)
		}
		md, err := c.Dataset(name).Model(m).Metadata(h.Context())
		if err != nil {
			t.Fatalf("Model.Metadata %s: %v", m, err)
		}
		if md.Type != "TENSORFLOW" {
			t.Errorf("%s: type %s, want TENSORFLOW", m, md.Type)
		}
	}

	near := func(what string, got bigquery.Value, want float64, tol float64) {
		t.Helper()
		g, ok := got.(float64)
		if !ok {
			if n, isInt := got.(int64); isInt {
				g, ok = float64(n), true
			}
		}
		if !ok || math.Abs(g-want) > tol*math.Max(1, math.Abs(want)) {
			t.Errorf("%s = %v, TensorFlow %v", what, got, want)
		}
	}
	// The first query may start the model's predictor (TensorFlow's load).
	query := func(sql string) []map[string]bigquery.Value {
		t.Helper()
		qctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return mlQueryIn(t, qctx, c, sql)
	}

	// tf.train.Example inputs, on TensorFlow (StringSplitV2).
	got := query(fmt.Sprintf("SELECT id, score, words FROM ML.PREDICT(MODEL `%s.exflex`, TABLE `%[1]s.exflex`) ORDER BY id", name))
	if len(got) != len(ref["exflex"]) {
		t.Fatalf("exflex: %d rows, want %d", len(got), len(ref["exflex"]))
	}
	for i, r := range got {
		want := ref["exflex"][i].Outputs
		near(fmt.Sprintf("exflex row %d score", i), r["score"], want["score"][0].(float64), 1e-5)
		near(fmt.Sprintf("exflex row %d words", i), r["words"], want["words"][0].(float64), 0)
		if _, ok := r["words"].(int64); !ok {
			t.Errorf("exflex row %d words is %T, want INT64", i, r["words"])
		}
	}

	// SparseTensor inputs, on TensorFlow (AsString).
	got = query(fmt.Sprintf("SELECT id, chars, total FROM ML.PREDICT(MODEL `%s.sparseflex`, TABLE `%[1]s.sparseflex`) ORDER BY id", name))
	if len(got) != len(ref["sparseflex"]) {
		t.Fatalf("sparseflex: %d rows, want %d", len(got), len(ref["sparseflex"]))
	}
	for i, r := range got {
		want := ref["sparseflex"][i].Outputs
		near(fmt.Sprintf("sparseflex row %d chars", i), r["chars"], want["chars"][0].(float64), 0)
		near(fmt.Sprintf("sparseflex row %d total", i), r["total"], want["total"][0].(float64), 1e-5)
	}
	// "A NULL, the associated BigQuery ML type, or an ARRAY": a scalar.
	got = query(fmt.Sprintf("SELECT total FROM ML.PREDICT(MODEL `%s.sparseflex`, (SELECT 2.5 AS v))", name))
	if len(got) != 1 {
		t.Fatalf("sparseflex scalar: %v", got)
	}
	near("sparseflex scalar total", got[0]["total"], 2.5, 1e-6)

	// The mlp's STRING output, also on TensorFlow (AsString, ReduceJoin).
	got = query(fmt.Sprintf("SELECT id, probs, text FROM ML.PREDICT(MODEL `%s.mlp`, TABLE `%[1]s.mlp`) ORDER BY id", name))
	for i, r := range got {
		want := ref["mlp"][i].Outputs
		if r["text"] != want["text"][0] {
			t.Errorf("mlp row %d text = %v, TensorFlow %v", i, r["text"], want["text"][0])
		}
		ps, _ := r["probs"].([]bigquery.Value)
		if len(ps) != 3 {
			t.Fatalf("mlp row %d probs = %v", i, r["probs"])
		}
		for j, p := range ps {
			near(fmt.Sprintf("mlp row %d probs[%d]", i, j), p, want["probs"][j].(float64), 1e-5)
		}
	}

	// ML.EXPLAIN_PREDICT: Integrated Gradients on TensorFlow.
	explain := func(m, feature string, opts string) {
		t.Helper()
		rows := query(fmt.Sprintf("SELECT id, top_feature_attributions, baseline_prediction_value, prediction_value, approximation_error "+
			"FROM ML.EXPLAIN_PREDICT(MODEL `%s.%s`, TABLE `%[1]s.%[2]s`%s) ORDER BY id", name, m, opts))
		if len(rows) != len(ref[m]) {
			t.Fatalf("explain %s: %d rows, want %d", m, len(rows), len(ref[m]))
		}
		for i, r := range rows {
			want := ref[m][i].IG
			attrs, _ := r["top_feature_attributions"].([]bigquery.Value)
			if len(attrs) != 1 {
				t.Fatalf("explain %s row %d: attributions %v, want one for %s", m, i, r["top_feature_attributions"], feature)
			}
			a, _ := attrs[0].(map[string]bigquery.Value)
			if a["feature"] != feature {
				t.Errorf("explain %s row %d: feature %v, want %s", m, i, a["feature"], feature)
			}
			near(fmt.Sprintf("explain %s row %d attribution", m, i), a["attribution"], want[feature], 1e-4)
			near(fmt.Sprintf("explain %s row %d baseline_prediction_value", m, i), r["baseline_prediction_value"], want["baseline"], 1e-5)
			near(fmt.Sprintf("explain %s row %d prediction_value", m, i), r["prediction_value"], want["prediction"], 1e-5)
			if e, ok := r["approximation_error"].(float64); !ok || e < 0 {
				t.Errorf("explain %s row %d approximation_error = %v", m, i, r["approximation_error"])
			}
		}
	}
	explain("mlp", "x", "")
	explain("exflex", "a", ", STRUCT(3 AS top_k_features, 15 AS integrated_gradients_num_steps)")
	// The model's own outputs lead an explained row, as in ML.PREDICT.
	got = query(fmt.Sprintf("SELECT probs FROM ML.EXPLAIN_PREDICT(MODEL `%s.mlp`, (SELECT %s AS x))", name, floats(ref["mlp"][0].Inputs["x"])))
	if ps, _ := got[0]["probs"].([]bigquery.Value); len(ps) != 3 {
		t.Errorf("explain mlp probs = %v", got[0]["probs"])
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.mlp`, TABLE `%[1]s.mlp`, STRUCT(TRUE AS approx_feature_contrib))", name)).Read(ctx)
	wantAPIError(t, "approx_feature_contrib", err, http.StatusBadRequest, "approx_feature_contrib")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.mlp`, TABLE `%[1]s.mlp`, STRUCT(0 AS integrated_gradients_num_steps))", name)).Read(ctx)
	wantAPIError(t, "integrated_gradients_num_steps 0", err, http.StatusBadRequest, "integrated_gradients_num_steps")
}
