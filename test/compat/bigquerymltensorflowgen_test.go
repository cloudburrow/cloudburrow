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
//     are 400; a VarLenFeature (a SparseTensor, which BigQuery feeds and
//     CloudBurrow cannot) is 501, never a result.
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
	for _, m := range []string{"cf", "example", "old", "dev", "ragged", "sparse"} {
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
		CF      []rowRef `json:"cf"`
		Example []rowRef `json:"example"`
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
	var cfRows, exRows []string
	for i, r := range ref.CF {
		cfRows = append(cfRows, fmt.Sprintf("SELECT %d AS id, %s AS x, %v AS n", i, floats(r.Inputs["x"]), r.Inputs["n"]))
	}
	for i, r := range ref.Example {
		exRows = append(exRows, fmt.Sprintf("SELECT %d AS id, %s AS a, %v AS k, 'kept' AS note", i, floats(r.Inputs["a"]), r.Inputs["k"]))
	}
	name := mlDataset(t, h, c, map[string]string{"cf": strings.Join(cfRows, " UNION ALL "), "ex": strings.Join(exRows, " UNION ALL ")})

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
	refused("sparse", http.StatusNotImplemented, "SparseTensor")
}
