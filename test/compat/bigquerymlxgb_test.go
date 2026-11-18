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
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLImportedXGBoostModel (#1711): CREATE MODEL … [INPUT(…)
// OUTPUT(…)] OPTIONS (MODEL_TYPE='XGBOOST', MODEL_PATH='gs://…'), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-xgboost
// documents it, through the official Go clients. The models are ones
// XGBoost 3.1.0, the version BigQuery ML runs, trained and saved
// (internal/xgbmodel/testdata/gen.py), and the predictions and importances
// wanted are XGBoost's own (cases.json): a JSON regressor with
// feature_names but no feature_types, refused without INPUT and OUTPUT
// and imported from a folder with them (INPUT matched to feature_names by
// name), its NULL inputs XGBoost's missing values; a UBJ multi:softprob classifier through INPUT and OUTPUT (an
// ARRAY<FLOAT64> of the class probabilities); a model with feature_names
// and feature_types imported without INPUT (output predicted_label), its
// categorical feature read as INT64; a model with no feature_names, refused without an INPUT clause
// and imported with one. ML.FEATURE_IMPORTANCE is XGBoost's get_score; the
// copy is BigQuery's own (deleting the object changes nothing); ML.EVALUATE
// and a missing column are refused.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.models.get, bigquery.models.delete
func TestBigQueryMLImportedXGBoostModel(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	b := h.Project() + "-bucket"
	dir := filepath.Join("..", "..", "internal", "xgbmodel", "testdata")
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
		Test       [][]*float64                    `json:"test"`
		Pred       json.RawMessage                 `json:"pred"`
		Importance map[string][]map[string]float64 `json:"importance"`
	}
	if err := json.Unmarshal(read("cases.json"), &cases); err != nil {
		t.Fatal(err)
	}
	put("xgb/reg/model.json", read("reg.json"))
	put("xgb/reg/notes.txt", []byte("not a model"))
	put("xgb/mc/model.ubj", read("mc.ubj"))
	put("xgb/cat.ubj", read("cat.ubj"))
	put("xgb/noname.json", read("noname.json"))
	// The test rows as a table: id, f_a, f_b, f_c, f_cat.
	var sel []string
	lit := func(v *float64) string {
		if v == nil {
			return "CAST(NULL AS FLOAT64)"
		}
		return strconv.FormatFloat(*v, 'g', -1, 64)
	}
	cats := cases["cat"].Test
	for i, row := range cases["reg"].Test {
		sel = append(sel, fmt.Sprintf("SELECT %d AS id, %s AS f_a, %s AS f_b, %s AS f_c, %d AS f_cat", i, lit(row[0]), lit(row[1]), lit(row[2]), int64(*cats[i][3])))
	}
	name := mlDataset(t, h, c, map[string]string{"rows": strings.Join(sel, " UNION ALL ")})
	near := func(got bigquery.Value, want float64) bool {
		g, ok := got.(float64)
		return ok && math.Abs(g-want) <= 1e-6*math.Max(1, math.Abs(want))
	}

	// reg.json names its features but not their types: INPUT and OUTPUT
	// are required, and INPUT is matched to feature_names by name.
	_, err := c.Query(fmt.Sprintf("CREATE MODEL `%s.reg` OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/reg/*')", name, b)).Read(ctx)
	wantAPIError(t, "feature_names without feature_types", err, http.StatusBadRequest, "feature_names and feature_types")
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.reg` INPUT(f_c FLOAT64, f_a FLOAT64, f_b FLOAT64) OUTPUT(predicted_label FLOAT64) "+
		"OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/reg/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	md, err := c.Dataset(name).Model("reg").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	feats, _ := md.RawFeatureColumns()
	labels, _ := md.RawLabelColumns()
	var fs, ls []string
	for _, f := range feats {
		fs = append(fs, f.Name+" "+string(f.Type.TypeKind))
	}
	for _, f := range labels {
		ls = append(ls, f.Name+" "+string(f.Type.TypeKind))
	}
	if md.Type != "XGBOOST" || !reflect.DeepEqual(fs, []string{"f_a FLOAT64", "f_b FLOAT64", "f_c FLOAT64"}) ||
		!reflect.DeepEqual(ls, []string{"predicted_label FLOAT64"}) {
		t.Errorf("model %s, features %v, labels %v", md.Type, fs, ls)
	}
	if runs := md.RawTrainingRuns(); len(runs) != 1 || runs[0].TrainingOptions == nil ||
		runs[0].TrainingOptions.ModelUri != fmt.Sprintf("gs://%s/xgb/reg/*", b) {
		t.Errorf("training runs %+v", runs)
	}
	if err := bh.Object("xgb/reg/model.json").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	var regWant []float64
	if err := json.Unmarshal(cases["reg"].Pred, &regWant); err != nil {
		t.Fatal(err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, predicted_label, f_cat FROM ML.PREDICT(MODEL `%s.reg`, TABLE `%[1]s.rows`) ORDER BY id", name))
	if len(rows) != len(regWant) {
		t.Fatalf("ML.PREDICT: %d rows", len(rows))
	}
	for i, r := range rows {
		if r["id"] != int64(i) || !near(r["predicted_label"], regWant[i]) || r["f_cat"] == nil {
			t.Errorf("row %d: %v, XGBoost %v", i, r, regWant[i])
		}
	}
	imp := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.FEATURE_IMPORTANCE(MODEL `%s.reg`) ORDER BY feature", name))
	wi := cases["reg"].Importance
	if len(imp) != 3 {
		t.Fatalf("ML.FEATURE_IMPORTANCE: %v", imp)
	}
	for _, r := range imp {
		f, _ := r["feature"].(string)
		g, _ := r["importance_gain"].(float64)
		cv, _ := r["importance_cover"].(float64)
		if r["importance_weight"] != int64(wi["weight"][0][f]) || math.Abs(g-wi["gain"][0][f]) > 1e-3*wi["gain"][0][f] ||
			math.Abs(cv-wi["cover"][0][f]) > 1e-3*wi["cover"][0][f] {
			t.Errorf("importance %v, XGBoost weight %v gain %v cover %v", r, wi["weight"][0][f], wi["gain"][0][f], wi["cover"][0][f])
		}
	}

	// A UBJ multi:softprob model through INPUT and OUTPUT, by jobs.insert.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.mc` INPUT(f_a FLOAT64, f_b FLOAT64, f_c FLOAT64) OUTPUT(probs ARRAY<FLOAT64>) "+
		"OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/mc/*')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL mc: %v", err)
	}
	var mcWant [][]float64
	if err := json.Unmarshal(cases["mc"].Pred, &mcWant); err != nil {
		t.Fatal(err)
	}
	job, err := c.Query(fmt.Sprintf("SELECT id, probs FROM ML.PREDICT(MODEL `%s.mc`, TABLE `%[1]s.rows`) ORDER BY id", name)).Run(ctx)
	if err != nil {
		t.Fatalf("jobs.insert: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("job.Read: %v", err)
	}
	for i := 0; ; i++ {
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			if i != len(mcWant) {
				t.Errorf("mc: %d rows, %v", i, err)
			}
			break
		}
		ps, _ := row[1].([]bigquery.Value)
		ok := len(ps) == 3
		for k := 0; ok && k < 3; k++ {
			ok = near(ps[k], mcWant[i][k])
		}
		if !ok {
			t.Errorf("mc row %d: %v, XGBoost %v", i, row, mcWant[i])
		}
	}

	// A categorical feature, an INT64 input.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.cat` OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/cat.ubj')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL cat: %v", err)
	}
	if cm, err := c.Dataset(name).Model("cat").Metadata(ctx); err != nil {
		t.Errorf("Model.Metadata cat: %v", err)
	} else {
		cf, _ := cm.RawFeatureColumns()
		cl, _ := cm.RawLabelColumns()
		var got []string
		for _, f := range append(cf, cl...) {
			got = append(got, f.Name+" "+string(f.Type.TypeKind))
		}
		if !reflect.DeepEqual(got, []string{"f_a FLOAT64", "f_b FLOAT64", "f_c FLOAT64", "f_cat INT64", "predicted_label FLOAT64"}) {
			t.Errorf("cat's columns %v", got)
		}
	}
	var catWant []float64
	if err := json.Unmarshal(cases["cat"].Pred, &catWant); err != nil {
		t.Fatal(err)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, predicted_label FROM ML.PREDICT(MODEL `%s.cat`, TABLE `%[1]s.rows`) ORDER BY id", name))
	for i, r := range rows {
		if !near(r["predicted_label"], catWant[i]) {
			t.Errorf("cat row %d: %v, XGBoost %v", i, r, catWant[i])
		}
	}

	// No feature_names: INPUT and OUTPUT are required, and name the
	// features in order.
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.nn` OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/noname.json')", name, b)).Read(ctx)
	wantAPIError(t, "no feature_names and no INPUT", err, http.StatusBadRequest, "feature_names and feature_types")
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.nn` INPUT(f_a FLOAT64, f_b FLOAT64, f_c FLOAT64) OUTPUT(y FLOAT64) "+
		"OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/noname.json')", name, b)); err != nil {
		t.Fatalf("CREATE MODEL nn: %v", err)
	}
	var nnWant []float64
	if err := json.Unmarshal(cases["noname"].Pred, &nnWant); err != nil {
		t.Fatal(err)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, y FROM ML.PREDICT(MODEL `%s.nn`, TABLE `%[1]s.rows`) ORDER BY id", name))
	for i, r := range rows {
		if !near(r["y"], nnWant[i]) {
			t.Errorf("nn row %d: %v, XGBoost %v", i, r, nnWant[i])
		}
	}

	// The documented refusals.
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.EVALUATE(MODEL `%s.reg`, TABLE `%[1]s.rows`)", name)).Read(ctx)
	wantAPIError(t, "ML.EVALUATE", err, http.StatusBadRequest, "ML.PREDICT and ML.FEATURE_IMPORTANCE")
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.reg`, (SELECT 1.0 AS f_a, 2.0 AS f_b))", name)).Read(ctx)
	wantAPIError(t, "a missing input column", err, http.StatusBadRequest, "Column f_c is not found in the input data to the PREDICT function")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.two` INPUT(f_a FLOAT64, f_b FLOAT64, f_c FLOAT64) OUTPUT(a FLOAT64, b FLOAT64) "+
		"OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/noname.json')", name, b)).Read(ctx)
	wantAPIError(t, "two outputs", err, http.StatusBadRequest, "single scalar or array output")
	_, err = c.Query(fmt.Sprintf("CREATE MODEL `%s.txt` OPTIONS(model_type='XGBOOST', model_path='gs://%s/xgb/reg/notes.txt')", name, b)).Read(ctx)
	wantHTTPStatus(t, "a file that is not a model", err, http.StatusBadRequest)
	for _, id := range []string{"two", "txt"} {
		if _, err := c.Dataset(name).Model(id).Metadata(ctx); !isNotFound(err) {
			t.Errorf("model %s after its refusal: %v", id, err)
		}
	}
	if err := c.Dataset(name).Model("reg").Delete(ctx); err != nil {
		t.Errorf("Model.Delete: %v", err)
	}
}
