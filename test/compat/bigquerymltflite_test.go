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
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLImportedTFLiteModel (#1755): CREATE MODEL … OPTIONS
// (MODEL_TYPE='TENSORFLOW_LITE', MODEL_PATH='gs://…'), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tflite
// documents it, through the official bigquery and storage Go clients. The
// models are ones TensorFlow 2.20's TFLiteConverter wrote
// (internal/tflitemodel/testdata/gen.py) and the predictions wanted are
// LiteRT's own (litert_predictions.json, ai-edge-litert 2.3.0): an MLP
// classifier imported from a folder, its ARRAY<FLOAT64> input and
// softmax output; a model of two inputs (a value and an ARRAY) and three
// outputs; a model of element-wise and reduction operators with an INT32
// input read from INT64. models.get gives the signature's inputs as
// featureColumns and outputs as labelColumns; the copy is BigQuery's own;
// ML.EVALUATE, a missing column and a file that is not a model are
// refused.
//
// covers: bigquery.jobs.query, bigquery.models.get, bigquery.models.delete
func TestBigQueryMLImportedTFLiteModel(t *testing.T) {
	h := New(t)
	// Cloud Storage under the emulators shard's own name for it, as
	// TestBigQueryMLImportedXGBoostModel (#1761), so the Cloud Storage suite
	// stays in the storage shard and this test runs in emulators.
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	dir := filepath.Join("..", "..", "internal", "tflitemodel", "testdata")
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	put := func(name string, data []byte) {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
	}
	var cases map[string]struct {
		Inputs  map[string]json.RawMessage `json:"inputs"`
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(read("litert_predictions.json"), &cases); err != nil {
		t.Fatal(err)
	}
	put("tfl/mlp/model.tflite", read("mlp.tflite"))
	put("tfl/mlp/README.txt", []byte("not a model"))
	put("tfl/multi.tflite", read("multi.tflite"))
	put("tfl/ops.tflite", read("ops.tflite"))
	put("tfl/bad.tflite", []byte("not a model at all"))

	// The rows: id and each model's inputs, as LiteRT was fed them.
	sqlOf := func(raw json.RawMessage, scalar bool) []string {
		var rows [][]float64
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			var vs []string
			for _, v := range r {
				vs = append(vs, fmt.Sprintf("%v", v))
			}
			if scalar {
				out = append(out, vs[0])
			} else {
				out = append(out, "["+strings.Join(vs, ", ")+"]")
			}
		}
		return out
	}
	features := sqlOf(cases["mlp"].Inputs["features"], false)
	ma, mb := sqlOf(cases["multi"].Inputs["a"], true), sqlOf(cases["multi"].Inputs["b"], false)
	ox, on := sqlOf(cases["ops"].Inputs["x"], false), sqlOf(cases["ops"].Inputs["n"], true)
	var sel []string
	for i := range features {
		sel = append(sel, fmt.Sprintf("SELECT %d AS id, ARRAY<FLOAT64>%s AS features, CAST(%s AS FLOAT64) AS a, ARRAY<FLOAT64>%s AS b, "+
			"ARRAY<FLOAT64>%s AS x, CAST(%s AS INT64) AS n, 'kept' AS note", i, features[i], ma[i], mb[i], ox[i], on[i]))
	}
	name := mlDataset(t, h, c, map[string]string{"rows": strings.Join(sel, " UNION ALL ")})
	near := func(got bigquery.Value, want float64) bool {
		g, ok := got.(float64)
		return ok && math.Abs(g-want) <= 1e-5*math.Max(1, math.Abs(want))
	}
	check := func(model, col string, got bigquery.Value, want json.RawMessage, row int) {
		t.Helper()
		var all []any
		if err := json.Unmarshal(want, &all); err != nil {
			t.Fatal(err)
		}
		w := all[row]
		switch wv := w.(type) {
		case []any:
			if len(wv) == 1 {
				if !near(got, wv[0].(float64)) {
					t.Errorf("%s %s row %d: %v, LiteRT %v", model, col, row, got, wv)
				}
				return
			}
			gs, _ := got.([]bigquery.Value)
			ok := len(gs) == len(wv)
			for k := 0; ok && k < len(wv); k++ {
				ok = near(gs[k], wv[k].(float64))
			}
			if !ok {
				t.Errorf("%s %s row %d: %v, LiteRT %v", model, col, row, got, wv)
			}
		case float64:
			if !near(got, wv) {
				t.Errorf("%s %s row %d: %v, LiteRT %v", model, col, row, got, wv)
			}
		}
	}
	columns := func(model string) []string {
		t.Helper()
		md, err := c.Dataset(name).Model(model).Metadata(ctx)
		if err != nil {
			t.Fatalf("Model.Metadata %s: %v", model, err)
		}
		if md.Type != "TENSORFLOW_LITE" {
			t.Errorf("%s: type %s", model, md.Type)
		}
		fs, _ := md.RawFeatureColumns()
		ls, _ := md.RawLabelColumns()
		var got []string
		for _, f := range append(fs, ls...) {
			got = append(got, f.Name+" "+string(f.Type.TypeKind))
		}
		return got
	}

	// The MLP, from a folder.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.mlp` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tfl/mlp/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL mlp: %v", err)
	}
	if got := columns("mlp"); !reflect.DeepEqual(got, []string{"features ARRAY", "probs ARRAY"}) {
		t.Errorf("mlp columns %v", got)
	}
	if err := bh.Object("tfl/mlp/model.tflite").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, probs, note FROM ML.PREDICT(MODEL `%s.mlp`, TABLE `%[1]s.rows`) ORDER BY id", name))
	if len(rows) != len(features) {
		t.Fatalf("mlp: %d rows", len(rows))
	}
	for i, r := range rows {
		if r["note"] != "kept" {
			t.Errorf("mlp row %d passes through %v", i, r)
		}
		check("mlp", "probs", r["probs"], cases["mlp"].Outputs["probs"], i)
	}

	// Two inputs, three outputs.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.multi` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tfl/multi.tflite')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL multi: %v", err)
	}
	if got := columns("multi"); !reflect.DeepEqual(got, []string{"a FLOAT64", "b ARRAY", "logits ARRAY", "pair ARRAY", "score FLOAT64"}) {
		t.Errorf("multi columns %v", got)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, score, logits, pair FROM ML.PREDICT(MODEL `%s.multi`, (SELECT id, a, b FROM `%[1]s.rows`)) ORDER BY id", name))
	for i, r := range rows {
		for _, col := range []string{"score", "logits", "pair"} {
			check("multi", col, r[col], cases["multi"].Outputs[col], i)
		}
	}

	// Element-wise operators and an INT32 input.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ops` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tfl/ops.tflite')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL ops: %v", err)
	}
	if got := columns("ops"); !reflect.DeepEqual(got, []string{"n INT64", "x ARRAY", "q ARRAY", "s FLOAT64", "y ARRAY"}) {
		t.Errorf("ops columns %v", got)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, q, s, y FROM ML.PREDICT(MODEL `%s.ops`, TABLE `%[1]s.rows`) ORDER BY id", name))
	for i, r := range rows {
		for _, col := range []string{"q", "s", "y"} {
			check("ops", col, r[col], cases["ops"].Outputs[col], i)
		}
	}

	// The documented refusals.
	_, err := c.Query(fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.mlp`, TABLE `%[1]s.rows`)", name)).Read(ctx)
	wantAPIError(t, "ML.EVALUATE", err, http.StatusBadRequest, "only use TensorFlow Lite models with the ML.PREDICT function")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.multi`, (SELECT 1.0 AS a))", name)).Read(ctx)
	wantAPIError(t, "a missing column", err, http.StatusBadRequest, "Column b is not found")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tfl/bad.tflite')", name, b)).Read(ctx)
	wantAPIError(t, "not a model", err, http.StatusBadRequest, "Failed to load the TensorFlow Lite model")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/tfl/mlp/README.txt')", name, b)).Read(ctx)
	wantAPIError(t, "not .tflite", err, http.StatusBadRequest, ".tflite")
	if err := c.Dataset(name).Model("mlp").Delete(ctx); err != nil {
		t.Errorf("models.delete: %v", err)
	}
}
