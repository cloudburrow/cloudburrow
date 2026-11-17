//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"testing"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel/onnxgen"
)

// EnvONNX is set when the instance under test is a `-tags onnx` build
// (make build-onnx) run with ONNXRUNTIME_LIB, whose BigQuery imports ONNX
// models (#1711). Without it the default build's refusal is tested.
const EnvONNX = "CLOUDBURROW_TEST_ONNX"

// TestBigQueryMLImportedONNXModel (#1711): CREATE MODEL … OPTIONS
// (MODEL_TYPE='ONNX', MODEL_PATH='gs://…'), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-onnx
// documents it, through the official Go clients: the model is read from
// the instance's Cloud Storage (a folder's one .onnx file, by wildcard),
// kept with the model (deleting the object changes nothing), reported by
// models.get as type ONNX with its inputs as featureColumns and outputs as
// labelColumns, and ML.PREDICT runs it on ONNX Runtime: each input column
// read by the model input of its name (a FLOAT64 for a [N, 1] tensor, an
// ARRAY<FLOAT64> for [N, 2]), each output a column (FLOAT64, INT64,
// ARRAY<FLOAT64>), the input's columns passed through. The model is
// onnxgen's hand-built linear model (internal/onnxmodel/onnxgen), so
// every prediction is known exactly: y = 2a + 3b + v[0] - v[1] + 1,
// label = y > 5, scores = [y, -y]. The documented refusals: a sequence
// output in the page's words, ML functions other than ML.PREDICT ("Imported
// models work only with the ML.PREDICT function"), a path that is not a
// .onnx file or names none.
//
// Not passing yet (#1711): the BigQuery front runs in the cluster without
// ONNX Runtime and refuses the CREATE MODEL 501; this is the acceptance test
// for the runner that will serve it. CI never runs it (ci.yml's never=).
func TestBigQueryMLImportedONNXModel(t *testing.T) {
	if os.Getenv(EnvONNX) == "" {
		t.Skipf("%s is not set: the instance must be a -tags onnx build run with ONNXRUNTIME_LIB", EnvONNX)
	}
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
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
	put("models/linear/model.onnx", onnxgen.Linear())
	put("models/linear/README.txt", []byte("not a model"))
	put("models/sequence.onnx", onnxgen.SequenceOutput())
	name := mlDataset(t, h, c, map[string]string{
		"feats": "SELECT 'r1' AS id, 1.0 AS a, 1.0 AS b, [0.0, 0.0] AS v UNION ALL SELECT 'r2', 0.0, 0.5, [2.0, 1.0] " +
			"UNION ALL SELECT 'r3', 1.0, 1.0, [0.0, 0.0]",
	})

	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.lin` OPTIONS(model_type='ONNX', model_path='gs://%s/models/linear/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	md, err := c.Dataset(name).Model("lin").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	feats, _ := md.RawFeatureColumns()
	labels, _ := md.RawLabelColumns()
	var fs, ls []string
	for _, f := range feats {
		fs = append(fs, f.Name+" "+string(f.Type.TypeKind)+arrayOf(f.Type))
	}
	for _, f := range labels {
		ls = append(ls, f.Name+" "+string(f.Type.TypeKind)+arrayOf(f.Type))
	}
	if md.Type != "ONNX" || !reflect.DeepEqual(fs, []string{"a FLOAT64", "b FLOAT64", "v ARRAY<FLOAT64>"}) ||
		!reflect.DeepEqual(ls, []string{"y FLOAT64", "label INT64", "scores ARRAY<FLOAT64>"}) {
		t.Errorf("model %s, features %v, labels %v", md.Type, fs, ls)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions == nil ||
		runs[0].TrainingOptions.ModelUri != fmt.Sprintf("gs://%s/models/linear/*", b) {
		t.Errorf("training runs %+v", runs)
	}

	// BigQuery keeps its copy of the model.
	if err := bh.Object("models/linear/model.onnx").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.lin`, TABLE `%[1]s.feats`) ORDER BY id", name))
	want := []struct {
		id     string
		y      float64
		label  int64
		scores []bigquery.Value
	}{{"r1", 6, 1, []bigquery.Value{6.0, -6.0}}, {"r2", 3.5, 0, []bigquery.Value{3.5, -3.5}}, {"r3", 6, 1, []bigquery.Value{6.0, -6.0}}}
	if len(rows) != len(want) {
		t.Fatalf("ML.PREDICT: %d rows %v", len(rows), rows)
	}
	for i, w := range want {
		r := rows[i]
		var keys []string
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if r["id"] != w.id || r["y"] != w.y || r["label"] != w.label || !reflect.DeepEqual(r["scores"], w.scores) ||
			!reflect.DeepEqual(keys, []string{"a", "b", "id", "label", "scores", "v", "y"}) {
			t.Errorf("row %d: %v, want %+v", i, r, w)
		}
	}
	// A query's columns, INT64 converted to the FLOAT input, through
	// jobs.insert.
	q := c.Query(fmt.Sprintf("SELECT y, label, note FROM ML.PREDICT(MODEL `%s.lin`, (SELECT 2 AS a, 0 AS b, [1.0, 1.0] AS v, 'x' AS note))", name))
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("jobs.insert: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("job.Read: %v", err)
	}
	var got []bigquery.Value
	if err := it.Next(&got); err != nil || !reflect.DeepEqual(got, []bigquery.Value{5.0, int64(0), "x"}) {
		t.Errorf("ML.PREDICT of a query: %v %v", got, err)
	}

	// The documented refusals.
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.seq` OPTIONS(model_type='ONNX', model_path='gs://%s/models/sequence.onnx')", name, b)).Read(ctx)
	wantAPIError(t, "a sequence output", err, http.StatusBadRequest, "ONNX model output 'y' has unsupported ONNX type: ONNX_TYPE_SEQUENCE.")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.lin`, TABLE `%[1]s.feats`)", name)).Read(ctx)
	wantAPIError(t, "ML.EVALUATE", err, http.StatusBadRequest, "only with the ML.PREDICT function")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.lin`, (SELECT 1.0 AS a, [0.0, 0.0] AS v))", name)).Read(ctx)
	wantAPIError(t, "a missing input column", err, http.StatusBadRequest, "Column b is not found in the input data to the PREDICT function")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.lin`, (SELECT 1.0 AS a, 'one' AS b, [0.0, 0.0] AS v))", name)).Read(ctx)
	wantHTTPStatus(t, "a STRING for a FLOAT input", err, http.StatusBadRequest)
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.gone` OPTIONS(model_type='ONNX', model_path='gs://%s/models/linear/*')", name, b)).Read(ctx)
	wantHTTPStatus(t, "a folder with no .onnx file", err, http.StatusNotFound)
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.txt` OPTIONS(model_type='ONNX', model_path='gs://%s/models/linear/README.txt')", name, b)).Read(ctx)
	wantAPIError(t, "a file that is not .onnx", err, http.StatusBadRequest, ".onnx file")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.q` OPTIONS(model_type='ONNX', model_path='gs://%s/models/sequence.onnx') AS SELECT 1 AS a", name, b)).Read(ctx)
	wantHTTPStatus(t, "an ONNX model with a query", err, http.StatusBadRequest)
	for _, id := range []string{"seq", "gone", "txt", "q"} {
		if _, err := c.Dataset(name).Model(id).Metadata(ctx); !isNotFound(err) {
			t.Errorf("model %s after its refusal: %v", id, err)
		}
	}
	if err := c.Dataset(name).Model("lin").Delete(ctx); err != nil {
		t.Errorf("Model.Delete: %v", err)
	}
}

// TestBigQueryMLONNXNeedsTheOnnxBuild (#1711): the default build, without
// ONNX Runtime, refuses an imported ONNX model 501, naming the build that
// runs one, and keeps no model.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLONNXNeedsTheOnnxBuild(t *testing.T) {
	if os.Getenv(EnvONNX) != "" {
		t.Skipf("%s is set: the instance runs ONNX models (TestBigQueryMLImportedONNXModel)", EnvONNX)
	}
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, nil)
	_, err := c.Query(fmt.Sprintf("CREATE MODEL `%s.no` OPTIONS(model_type='onnx', model_path='gs://bucket/model.onnx')", name)).Read(ctx)
	wantAPIError(t, "an imported ONNX model", err, http.StatusNotImplemented, "-tags onnx")
	_, err = c.Dataset(name).Model("no").Metadata(ctx)
	wantHTTPStatus(t, "Model.Metadata after the refusal", err, http.StatusNotFound)
}

// arrayOf is " <elem>" wrapped as ARRAY's element, or "".
func arrayOf(t *bigquery.StandardSQLDataType) string {
	if t == nil || t.ArrayElementType == nil {
		return ""
	}
	return "<" + string(t.ArrayElementType.TypeKind) + ">"
}
