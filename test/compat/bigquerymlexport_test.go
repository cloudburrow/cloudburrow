//go:build compat

package compat

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
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
