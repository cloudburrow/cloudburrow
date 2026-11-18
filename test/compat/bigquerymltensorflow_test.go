//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLImportedTensorFlowModel (#1775): CREATE MODEL … OPTIONS
// (MODEL_TYPE='TENSORFLOW', MODEL_PATH='gs://…/*'), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tensorflow
// documents it, through the official bigquery and storage Go clients. The
// SavedModels are TensorFlow 2.20's (internal/tflitemodel/testdata/
// gen_savedmodel.py) and the predictions wanted are TensorFlow's own
// serving_default output for them (tensorflow_predictions.json): the
// instance converts the SavedModel on its TensorFlow converter and runs
// it on internal/tflitemodel. models.get answers modelType TENSORFLOW,
// the signature's inputs as featureColumns and outputs as labelColumns;
// the model is frozen at creation (deleting the folder changes nothing);
// a SavedModel with tf.while_loop and tf.cond converts to WHILE and IF
// subgraphs, which run to TensorFlow's output (#1775);
// the functions the page says do not take a TensorFlow model are 400; a
// folder that is not a SavedModel is 400; a graph TensorFlow Lite has no
// builtin for (an SVD) is 501, never a result.
//
// covers: bigquery.jobs.query, bigquery.models.get, bigquery.models.delete
func TestBigQueryMLImportedTensorFlowModel(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	dir := filepath.Join("..", "..", "internal", "tflitemodel", "testdata")
	upload := func(model string) {
		t.Helper()
		src := filepath.Join(dir, "savedmodel", model)
		err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			w := bh.Object("tf/" + model + "/" + filepath.ToSlash(rel)).NewWriter(ctx)
			if _, err := w.Write(data); err != nil {
				return err
			}
			return w.Close()
		})
		if err != nil {
			t.Fatalf("upload %s: %v", model, err)
		}
	}
	upload("mlp")
	upload("loop")
	upload("flex")
	w := bh.Object("tf/notsaved/README.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("not a SavedModel"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	type rowsRef []struct {
		Inputs  map[string][]float64 `json:"inputs"`
		Outputs map[string][]float64 `json:"outputs"`
	}
	var ref struct {
		Rows rowsRef `json:"rows"`
		Loop rowsRef `json:"loop"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tensorflow_predictions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	var sels []string
	for i, r := range ref.Rows {
		var xs []string
		for _, v := range r.Inputs["x"] {
			xs = append(xs, fmt.Sprint(v))
		}
		sels = append(sels, fmt.Sprintf("SELECT %d AS id, ARRAY<FLOAT64>[%s] AS x, %d AS k, 'kept' AS note", i, strings.Join(xs, ", "), int64(r.Inputs["k"][0])))
	}
	var loops []string
	for i, r := range ref.Loop {
		loops = append(loops, fmt.Sprintf("SELECT %d AS id, ARRAY<FLOAT64>[%v, %v] AS x, %v AS `limit`", i, r.Inputs["x"][0], r.Inputs["x"][1], r.Inputs["limit"][0]))
	}
	name := mlDataset(t, h, c, map[string]string{"rows": strings.Join(sels, " UNION ALL "), "loops": strings.Join(loops, " UNION ALL ")})

	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.mlp` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tf/mlp/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL mlp: %v", err)
	}
	md, err := c.Dataset(name).Model("mlp").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if md.Type != "TENSORFLOW" {
		t.Errorf("type %s, want TENSORFLOW", md.Type)
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
	if !reflect.DeepEqual(feats, []string{"k INT64", "x ARRAY"}) || !reflect.DeepEqual(labels, []string{"kk INT64", "probs ARRAY", "score FLOAT64"}) {
		t.Errorf("columns %v %v", feats, labels)
	}
	// Frozen at creation.
	if err := bh.Object("tf/mlp/saved_model.pb").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, score, probs, kk, note FROM ML.PREDICT(MODEL `%s.mlp`, TABLE `%[1]s.rows`) ORDER BY id", name))
	if len(rows) != len(ref.Rows) {
		t.Fatalf("%d rows, want %d", len(rows), len(ref.Rows))
	}
	near := func(col string, i int, got bigquery.Value, want float64) {
		t.Helper()
		var g float64
		switch v := got.(type) {
		case float64:
			g = v
		case int64:
			g = float64(v)
		default:
			t.Errorf("%s row %d: %T %v", col, i, got, got)
			return
		}
		if math.Abs(g-want) > 1e-5 {
			t.Errorf("%s row %d: %v, TensorFlow %v", col, i, g, want)
		}
	}
	for i, r := range rows {
		want := ref.Rows[i].Outputs
		if r["note"] != "kept" {
			t.Errorf("row %d passes through %v", i, r)
		}
		near("score", i, r["score"], want["score"][0])
		near("kk", i, r["kk"], want["kk"][0])
		ps, _ := r["probs"].([]bigquery.Value)
		if len(ps) != len(want["probs"]) {
			t.Fatalf("probs row %d: %v, TensorFlow %v", i, r["probs"], want["probs"])
		}
		for j, p := range ps {
			near("probs", i, p, want["probs"][j])
		}
	}

	// Control flow (#1775): the converter writes WHILE and IF subgraphs,
	// with LESS, GREATER and SELECT_V2, which run as TensorFlow runs them.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.loop` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tf/loop')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL loop: %v", err)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, y, steps, big, pos FROM ML.PREDICT(MODEL `%s.loop`, TABLE `%[1]s.loops`) ORDER BY id", name))
	if len(rows) != len(ref.Loop) {
		t.Fatalf("loop: %d rows, want %d", len(rows), len(ref.Loop))
	}
	for i, r := range rows {
		want := ref.Loop[i].Outputs
		near("steps", i, r["steps"], want["steps"][0])
		near("big", i, r["big"], want["big"][0])
		for _, col := range []string{"y", "pos"} {
			vs, _ := r[col].([]bigquery.Value)
			if len(vs) != len(want[col]) {
				t.Fatalf("loop %s row %d: %v, TensorFlow %v", col, i, r[col], want[col])
			}
			for j, v := range vs {
				near(col, i, v, want[col][j])
			}
		}
	}

	// The documented refusals, and what CloudBurrow cannot run.
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.mlp`, TABLE `%[1]s.rows`)", name)).Read(ctx)
	wantAPIError(t, "ML.EVALUATE", err, http.StatusBadRequest, "not supported for model type TENSORFLOW")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.WEIGHTS(MODEL `%s.mlp`)", name)).Read(ctx)
	wantAPIError(t, "ML.WEIGHTS", err, http.StatusBadRequest, "not supported for model type TENSORFLOW")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.mlp`, (SELECT [1.0, 2.0, 3.0] AS x))", name)).Read(ctx)
	wantAPIError(t, "a missing column", err, http.StatusBadRequest, "Column k is not found")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tf/notsaved/*')", name, b)).Read(ctx)
	wantAPIError(t, "not a SavedModel", err, http.StatusBadRequest, "SavedModel format")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.flex` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/tf/flex/*')", name, b)).Read(ctx)
	wantAPIError(t, "Flex", err, http.StatusNotImplemented, "Svd")
	if _, err := c.Dataset(name).Model("flex").Metadata(ctx); err == nil {
		t.Error("a refused model was kept")
	}
	if err := c.Dataset(name).Model("mlp").Delete(ctx); err != nil {
		t.Errorf("models.delete: %v", err)
	}
}
