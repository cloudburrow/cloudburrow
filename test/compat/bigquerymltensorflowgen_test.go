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
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"

	tfconvertimage "github.com/cloudburrow/cloudburrow/deploy/tfconvert"
	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// TestBigQueryMLTensorFlowImportGenerated (#1775): imported TensorFlow
// SavedModels, made by the test itself in CloudBurrow's pinned TensorFlow
// converter image (deploy/tfconvert: TensorFlow 2.20.0 from PyPI, run
// --network none; testdata/tensorflow/gen_import.py), uploaded to the
// instance's Cloud Storage and imported with CREATE MODEL …
// OPTIONS(MODEL_TYPE='TENSORFLOW', MODEL_PATH='gs://…/*') through the
// official bigquery and storage Go clients, as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tensorflow
// documents (read 2026-11-20):
//   - a model of tf.while_loop with a tf.cond in its body and a tf.cond
//     after it (TFLite WHILE and IF subgraphs) predicts TensorFlow's own
//     serving_default output, row by row, its INT64 and BOOL outputs
//     typed as the page's table says;
//   - a tf.train.Example signature ("BigQuery ML automatically determines
//     the feature names and converts the input BigQuery columns") takes
//     the features as columns of their names and predicts TensorFlow's
//     output for the same Examples (FLOAT64 score, INT64 k2);
//   - "Models trained using a version of GraphDef earlier than version 20
//     aren't supported", "Models trained using an unreleased version of
//     TensorFlow aren't supported" and "RaggedTensors aren't supported"
//     are 400;
//   - SparseTensor inputs ("You can pass SparseTensors as dense arrays and
//     BigQuery ML automatically converts them into Sparse format"; the
//     type table's "A NULL, the associated BigQuery ML type, or an ARRAY
//     of the associated BigQuery ML type"): a model written with
//     tf.compat.v1's sparse_placeholder takes ARRAY columns, a value or a
//     NULL, and predicts TensorFlow's output for the row's values at
//     [0, 0] .. [0, n-1] through tf.sparse.reduce_sum,
//     sparse_dense_matmul, to_dense and embedding_lookup_sparse; a
//     SparseTensor of rank 3 is 501;
//   - VarLenFeature tf.train.Example features are ARRAY columns too, an
//     empty ARRAY a feature with no values;
//   - a jax2tf-style XlaCallModule (a core TensorFlow op) whose StableHLO
//     is a stablehlo.case runs as STABLEHLO_CASE, the last branch for an
//     index out of range;
//   - ML.EXPLAIN_PREDICT of an imported TensorFlow model is 501: Google's
//     troubleshooting page runs it, and no page says how it attributes.
//
// It runs in the localai shard, beside the other locally run models.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLTensorFlowImportGenerated(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker on PATH to run the TensorFlow converter image in")
	}

	// Build (or find) the image the instance's converter runs, then make
	// the SavedModels in it with no network.
	bctx, cancel := context.WithTimeout(context.Background(), tfconvert.BuildTimeout)
	defer cancel()
	img := tfconvertimage.Tag()
	d := &tfconvert.Docker{Image: img, Context: tfconvertimage.Tar}
	if err := d.Build(bctx); err != nil {
		t.Fatalf("build %s: %v", img, err)
	}
	gen, err := filepath.Abs(filepath.Join("testdata", "tensorflow"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Chmod(out, 0o777); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(bctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/tmp",
		"-e", "HOME=/tmp", "-e", "TF_CPP_MIN_LOG_LEVEL=3", "-v", gen+":/w:ro", "-v", out+":/out",
		"--entrypoint", "python3", img, "-I", "/w/gen_import.py", "/out")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		t.Fatalf("gen_import.py: %v\n%s", err, log.String())
	}

	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	for _, m := range []string{"cf", "example", "old", "dev", "ragged", "sparse", "sparsein", "sparse3", "hlocase"} {
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
			w := bh.Object("tfgen/" + m + "/" + filepath.ToSlash(rel)).NewWriter(ctx)
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
		Inputs  map[string]any       `json:"inputs"`
		Outputs map[string][]float64 `json:"outputs"`
	}
	var ref struct {
		CF       []rowRef `json:"cf"`
		Example  []rowRef `json:"example"`
		Sparse   []rowRef `json:"sparse"`
		SparseIn []rowRef `json:"sparsein"`
		HloCase  []rowRef `json:"hlocase"`
	}
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
			s = append(s, fmt.Sprint(x))
		}
		return "ARRAY<FLOAT64>[" + strings.Join(s, ", ") + "]"
	}
	ints := func(v any) string {
		var s []string
		for _, x := range v.([]any) {
			s = append(s, fmt.Sprint(x))
		}
		return "ARRAY<INT64>[" + strings.Join(s, ", ") + "]"
	}
	var cfRows, exRows, spRows, siRows, hcRows []string
	for i, r := range ref.Sparse {
		spRows = append(spRows, fmt.Sprintf("SELECT %d AS id, %s AS v, %s AS ids, %s AS a", i, floats(r.Inputs["v"]), ints(r.Inputs["ids"]), floats(r.Inputs["a"])))
	}
	for i, r := range ref.SparseIn {
		siRows = append(siRows, fmt.Sprintf("SELECT %d AS id, %s AS v, %s AS ids, %s AS x", i, floats(r.Inputs["v"]), ints(r.Inputs["ids"]), floats(r.Inputs["x"])))
	}
	for i, r := range ref.HloCase {
		hcRows = append(hcRows, fmt.Sprintf("SELECT %d AS id, %v AS i, %s AS x", i, r.Inputs["i"], floats(r.Inputs["x"])))
	}
	for i, r := range ref.CF {
		cfRows = append(cfRows, fmt.Sprintf("SELECT %d AS id, %s AS x, %v AS n", i, floats(r.Inputs["x"]), r.Inputs["n"]))
	}
	for i, r := range ref.Example {
		exRows = append(exRows, fmt.Sprintf("SELECT %d AS id, %s AS a, %v AS k, 'kept' AS note", i, floats(r.Inputs["a"]), r.Inputs["k"]))
	}
	name := mlDataset(t, h, c, map[string]string{"cf": strings.Join(cfRows, " UNION ALL "), "ex": strings.Join(exRows, " UNION ALL "),
		"sp": strings.Join(spRows, " UNION ALL "), "si": strings.Join(siRows, " UNION ALL "), "hc": strings.Join(hcRows, " UNION ALL ")})

	// The first TENSORFLOW import may still build the instance's converter
	// image: tensorflowImportDeadline.
	if err := mlRunWithin(h, c, fmt.Sprintf("CREATE MODEL `%s.cf` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfgen/cf/*')", name, b), tensorflowImportDeadline); err != nil {
		t.Fatalf("CREATE MODEL cf: %v", err)
	}
	ctx = h.Context()
	columns := func(model string) ([]string, []string) {
		t.Helper()
		md, err := c.Dataset(name).Model(model).Metadata(ctx)
		if err != nil {
			t.Fatalf("Model.Metadata %s: %v", model, err)
		}
		if md.Type != "TENSORFLOW" {
			t.Errorf("%s: type %s, want TENSORFLOW", model, md.Type)
		}
		fs, _ := md.RawFeatureColumns()
		ls, _ := md.RawLabelColumns()
		var feats, labels []string
		for _, f := range fs {
			feats = append(feats, f.Name+" "+string(f.Type.TypeKind))
		}
		for _, f := range ls {
			labels = append(labels, f.Name+" "+string(f.Type.TypeKind))
		}
		sort.Strings(feats)
		sort.Strings(labels)
		return feats, labels
	}
	if f, l := columns("cf"); !reflect.DeepEqual(f, []string{"n INT64", "x ARRAY"}) ||
		!reflect.DeepEqual(l, []string{"big BOOL", "halvings INT64", "total FLOAT64", "y ARRAY"}) {
		t.Errorf("cf columns %v %v", f, l)
	}
	num := func(col string, i int, got bigquery.Value, want float64) {
		t.Helper()
		var g float64
		switch v := got.(type) {
		case float64:
			g = v
		case int64:
			g = float64(v)
		case bool:
			if v {
				g = 1
			}
		default:
			t.Errorf("%s row %d: %T %v", col, i, got, got)
			return
		}
		if math.Abs(g-want) > 1e-4*math.Max(1, math.Abs(want)) {
			t.Errorf("%s row %d: %v, TensorFlow %v", col, i, g, want)
		}
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, y, total, halvings, big FROM ML.PREDICT(MODEL `%s.cf`, TABLE `%[1]s.cf`) ORDER BY id", name))
	if len(rows) != len(ref.CF) {
		t.Fatalf("cf: %d rows, want %d", len(rows), len(ref.CF))
	}
	for i, r := range rows {
		want := ref.CF[i].Outputs
		if _, ok := r["big"].(bool); !ok {
			t.Errorf("big row %d is %T, want BOOL", i, r["big"])
		}
		if _, ok := r["halvings"].(int64); !ok {
			t.Errorf("halvings row %d is %T, want INT64", i, r["halvings"])
		}
		num("total", i, r["total"], want["total"][0])
		num("halvings", i, r["halvings"], want["halvings"][0])
		num("big", i, r["big"], want["big"][0])
		ys, _ := r["y"].([]bigquery.Value)
		if len(ys) != len(want["y"]) {
			t.Fatalf("y row %d: %v, TensorFlow %v", i, r["y"], want["y"])
		}
		for j, y := range ys {
			num("y", i, y, want["y"][j])
		}
	}

	// tf.train.Example inputs: the features are the model's columns.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.example` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfgen/example/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL example: %v", err)
	}
	if f, l := columns("example"); !reflect.DeepEqual(f, []string{"a ARRAY", "k INT64"}) || !reflect.DeepEqual(l, []string{"k2 INT64", "score FLOAT64"}) {
		t.Errorf("example columns %v %v", f, l)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, score, k2, note FROM ML.PREDICT(MODEL `%s.example`, TABLE `%[1]s.ex`) ORDER BY id", name))
	if len(rows) != len(ref.Example) {
		t.Fatalf("example: %d rows, want %d", len(rows), len(ref.Example))
	}
	for i, r := range rows {
		want := ref.Example[i].Outputs
		if r["note"] != "kept" {
			t.Errorf("example row %d passes through %v", i, r)
		}
		num("k2", i, r["k2"], want["k2"][0])
		// score is [None, 1]: one value a row, a FLOAT64, as the mlp
		// model's in TestBigQueryMLImportedTensorFlowModel.
		num("score", i, r["score"], want["score"][0])
	}

	// The page's limits.
	refused := func(model string, code int, text string) {
		t.Helper()
		_, err := c.Query(fmt.Sprintf("CREATE MODEL `%s.%s` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfgen/%[2]s/*')", name, model, b)).Read(ctx)
		wantAPIError(t, model, err, code, text)
		if _, err := c.Dataset(name).Model(model).Metadata(ctx); err == nil {
			t.Errorf("a refused model %s was kept", model)
		}
	}
	refused("old", http.StatusBadRequest, "earlier than version 20 aren't supported")
	refused("dev", http.StatusBadRequest, "unreleased version of TensorFlow aren't supported")
	refused("ragged", http.StatusBadRequest, "RaggedTensors aren't supported")
	refused("sparse3", http.StatusNotImplemented, "SparseTensor")

	// arrays checks an ARRAY output against TensorFlow's values.
	arrays := func(col string, i int, got bigquery.Value, want []float64) {
		t.Helper()
		vs, _ := got.([]bigquery.Value)
		if len(vs) != len(want) {
			t.Errorf("%s row %d: %v, TensorFlow %v", col, i, got, want)
			return
		}
		for j, v := range vs {
			num(col, i, v, want[j])
		}
	}

	// VarLenFeatures: ARRAY columns of the features' names.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.sparse` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfgen/sparse/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL sparse: %v", err)
	}
	if f, l := columns("sparse"); !reflect.DeepEqual(f, []string{"a ARRAY", "ids ARRAY", "v ARRAY"}) || !reflect.DeepEqual(l, []string{"emb ARRAY", "total FLOAT64"}) {
		t.Errorf("sparse columns %v %v", f, l)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, total, emb FROM ML.PREDICT(MODEL `%s.sparse`, TABLE `%[1]s.sp`) ORDER BY id", name))
	if len(rows) != len(ref.Sparse) {
		t.Fatalf("sparse: %d rows, want %d", len(rows), len(ref.Sparse))
	}
	for i, r := range rows {
		num("total", i, r["total"], ref.Sparse[i].Outputs["total"][0])
		arrays("emb", i, r["emb"], ref.Sparse[i].Outputs["emb"])
	}

	// SparseTensor inputs.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.sparsein` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfgen/sparsein/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL sparsein: %v", err)
	}
	if f, l := columns("sparsein"); !reflect.DeepEqual(f, []string{"ids ARRAY", "v ARRAY", "x ARRAY"}) ||
		!reflect.DeepEqual(l, []string{"dense ARRAY", "emb ARRAY", "proj ARRAY", "total FLOAT64"}) {
		t.Errorf("sparsein columns %v %v", f, l)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, total, proj, dense, emb FROM ML.PREDICT(MODEL `%s.sparsein`, TABLE `%[1]s.si`) ORDER BY id", name))
	if len(rows) != len(ref.SparseIn) {
		t.Fatalf("sparsein: %d rows, want %d", len(rows), len(ref.SparseIn))
	}
	for i, r := range rows {
		want := ref.SparseIn[i].Outputs
		num("total", i, r["total"], want["total"][0])
		arrays("proj", i, r["proj"], want["proj"])
		arrays("dense", i, r["dense"], want["dense"])
		arrays("emb", i, r["emb"], want["emb"])
	}
	// "A NULL, the associated BigQuery ML type, or an ARRAY": a NULL v is
	// no values, a scalar v one, and ids a scalar too.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT total, dense FROM ML.PREDICT(MODEL `%s.sparsein`, "+
		"(SELECT CAST(NULL AS FLOAT64) AS v, 3 AS ids, [1.5, 2.0] AS x UNION ALL SELECT 2.5, 3, [1.5, 2.0])) ORDER BY total", name))
	if len(rows) != 2 {
		t.Fatalf("sparsein scalars: %d rows", len(rows))
	}
	num("total", 0, rows[0]["total"], 3.5)
	arrays("dense", 0, rows[0]["dense"], []float64{0, 0, 0, 0})
	num("total", 1, rows[1]["total"], 6)
	arrays("dense", 1, rows[1]["dense"], []float64{2.5, 0, 0, 0})
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.sparsein`, (SELECT [1.0, 2, 3, 4, 5] AS v, [1] AS ids, [0.0, 0] AS x))", name)).Read(ctx)
	wantAPIError(t, "five values for [batch, 4]", err, http.StatusBadRequest, "at most 4")

	// StableHLO: an XlaCallModule's stablehlo.case.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.hlocase` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tfgen/hlocase/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL hlocase: %v", err)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, y FROM ML.PREDICT(MODEL `%s.hlocase`, TABLE `%[1]s.hc`) ORDER BY id", name))
	if len(rows) != len(ref.HloCase) {
		t.Fatalf("hlocase: %d rows, want %d", len(rows), len(ref.HloCase))
	}
	for i, r := range rows {
		arrays("y", i, r["y"], ref.HloCase[i].Outputs["y"])
	}

	// ML.EXPLAIN_PREDICT: Google runs it (its troubleshooting page) but no
	// page says how it attributes an imported TensorFlow model.
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EXPLAIN_PREDICT(MODEL `%s.cf`, TABLE `%[1]s.cf`)", name)).Read(ctx)
	wantAPIError(t, "ML.EXPLAIN_PREDICT", err, http.StatusNotImplemented, "EXPLAIN_PREDICT")
}
