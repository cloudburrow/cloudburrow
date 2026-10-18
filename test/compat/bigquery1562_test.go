//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryTempScriptsUndropAndAlterSchema (#1562): UNDROP SCHEMA and
// ALTER SCHEMA … SET OPTIONS in a script with a TEMP table or function,
// each carried out in its turn as BigQuery runs a script
// (https://cloud.google.com/bigquery/docs/multi-statement-queries), the
// statements after it seeing what it did, through jobs.query and
// jobs.insert; inside a transaction, which takes only queries, DML and DDL
// of TEMP entities (https://cloud.google.com/bigquery/docs/transactions),
// each fails the script, the dataset unchanged; and a dry run of each,
// alone and in a script, answers the documented statementType
// (UNDROP_SCHEMA, ALTER_SCHEMA, SCRIPT;
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics2),
// nothing run.
//
// Measured first: each script with a TEMP table or function was 501 ("in a
// multi-statement query with a TEMP table or function"), the dry run of a
// script with UNDROP SCHEMA 501, and that of ALTER SCHEMA 400 "Statement
// not supported: AlterSchemaStatement".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.get, bigquery.datasets.delete
func TestBigQueryTempScriptsUndropAndAlterSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	base := strings.ReplaceAll(h.Project(), "-", "_")
	read := func(sql string, insert bool) []string {
		t.Helper()
		q := c.Query(sql)
		if !insert {
			it, err := q.Read(ctx)
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			return readRows(t, it, sql)
		}
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("%s: jobs.insert: %v", sql, err)
		}
		it, err := job.Read(ctx)
		if err != nil {
			t.Fatalf("%s: job.Read: %v", sql, err)
		}
		return readRows(t, it, sql)
	}
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	dry := func(sql string) string {
		t.Helper()
		q := c.Query(sql)
		q.DryRun = true
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("dry run of %s: %v", sql, err)
		}
		st := job.LastStatus()
		if st == nil || st.Statistics == nil {
			t.Fatalf("dry run of %s: no statistics", sql)
		}
		qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics)
		if !ok {
			t.Fatalf("dry run of %s: statistics %T", sql, st.Statistics.Details)
		}
		return qs.StatementType
	}
	for _, insert := range []bool{false, true} {
		ds := c.Dataset(fmt.Sprintf("%s_tempundrop_%v", base, insert))
		if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
			t.Fatalf("Dataset.Create: %v", err)
		}
		t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
		tbl := "`" + ds.DatasetID + ".t`"
		run("CREATE TABLE " + tbl + " (n INT64)")
		run("INSERT INTO " + tbl + " VALUES (1), (2)")

		// ALTER SCHEMA beside a TEMP table, read by the next statement.
		// The next statement reads the option from INFORMATION_SCHEMA in
		// its turn (#1565: it read the view as before the script), the
		// view region-qualified, as BigQuery requires of SCHEMATA_OPTIONS
		// (#1595).
		dmd, mdErr := ds.Metadata(ctx)
		if mdErr != nil {
			t.Fatalf("Dataset.Metadata: %v", mdErr)
		}
		got := read("CREATE TEMP TABLE tt AS SELECT 'temp' AS x; "+
			"ALTER SCHEMA `"+ds.DatasetID+"` SET OPTIONS (description = 'beside a temp table'); "+
			"SELECT CONCAT(x, ':', option_value) FROM tt, `region-"+strings.ToLower(dmd.Location)+"`.INFORMATION_SCHEMA.SCHEMATA_OPTIONS "+
			"WHERE schema_name = '"+ds.DatasetID+"' AND option_name = 'description'", insert)
		if !reflect.DeepEqual(got, []string{`temp:"beside a temp table"`}) {
			t.Errorf("insert %v: ALTER SCHEMA beside a TEMP table: %v", insert, got)
		}
		if md, err := ds.Metadata(ctx); err != nil || md.Description != "beside a temp table" {
			t.Errorf("insert %v: description after the TEMP script: %+v, %v", insert, md, err)
		}

		// UNDROP SCHEMA beside a TEMP function, the dataset read after it.
		run("DROP SCHEMA " + ds.DatasetID + " CASCADE")
		got = read("CREATE TEMP FUNCTION twice(x INT64) AS (2 * x); UNDROP SCHEMA "+ds.DatasetID+"; "+
			"SELECT twice(COUNT(*)) FROM "+tbl, insert)
		if !reflect.DeepEqual(got, []string{"4"}) {
			t.Errorf("insert %v: a TEMP function of the rows UNDROP SCHEMA brought back: %v, want [4]", insert, got)
		}

		// Inside a transaction: the script fails, nothing changed.
		err := bqRun(ctx, c, "BEGIN TRANSACTION; ALTER SCHEMA `"+ds.DatasetID+"` SET OPTIONS (description = 'in a transaction'); "+
			"COMMIT TRANSACTION", insert)
		wantAPIError(t, "ALTER SCHEMA in a transaction", err, http.StatusBadRequest, "not supported inside a transaction")
		if md, err := ds.Metadata(ctx); err != nil || md.Description != "beside a temp table" {
			t.Errorf("insert %v: description after ALTER SCHEMA in a transaction: %+v, %v", insert, md, err)
		}
		err = bqRun(ctx, c, "BEGIN TRANSACTION; UNDROP SCHEMA IF NOT EXISTS "+ds.DatasetID+"; COMMIT TRANSACTION", insert)
		wantAPIError(t, "UNDROP SCHEMA in a transaction", err, http.StatusBadRequest, "not supported inside a transaction")
		// An exception handler sees the error, the transaction still open.
		got = read("BEGIN BEGIN TRANSACTION; UNDROP SCHEMA IF NOT EXISTS "+ds.DatasetID+"; COMMIT TRANSACTION; "+
			"EXCEPTION WHEN ERROR THEN ROLLBACK TRANSACTION; SELECT 'caught'; END", insert)
		if !reflect.DeepEqual(got, []string{"caught"}) {
			t.Errorf("insert %v: the handler of UNDROP SCHEMA in a transaction: %v", insert, got)
		}
	}

	// Dry runs: nothing run, the documented statement types.
	ds := c.Dataset(base + "_tempundrop_dry")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Description: "before"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	gone := c.Dataset(base + "_tempundrop_gone")
	if err := gone.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	run("DROP SCHEMA " + gone.DatasetID)
	t.Cleanup(func() { _ = gone.DeleteWithContents(h.Context()) })
	for sql, want := range map[string]string{
		"UNDROP SCHEMA " + gone.DatasetID:                                                               "UNDROP_SCHEMA",
		"ALTER SCHEMA `" + ds.DatasetID + "` SET OPTIONS (description = 'dry')":                         "ALTER_SCHEMA",
		"CREATE TEMP TABLE tt AS SELECT 1 AS x; UNDROP SCHEMA " + gone.DatasetID + "; SELECT * FROM tt": "SCRIPT",
		"ALTER SCHEMA `" + ds.DatasetID + "` SET OPTIONS (description = 'dry'); SELECT 1":               "SCRIPT",
	} {
		if got := dry(sql); got != want {
			t.Errorf("dry run of %s: statementType %q, want %q", sql, got, want)
		}
	}
	if md, err := ds.Metadata(ctx); err != nil || md.Description != "before" {
		t.Errorf("description after the dry runs: %+v, %v", md, err)
	}
	if _, err := gone.Metadata(ctx); err == nil {
		t.Errorf("a dry run of UNDROP SCHEMA undeleted the dataset")
	}
	q := c.Query("ALTER SCHEMA `" + ds.DatasetID + "` SET OPTIONS (storage_billing_model = 'NOPE'); SELECT 1")
	q.DryRun = true
	_, err := q.Run(ctx)
	wantAPIError(t, "dry run of ALTER SCHEMA with an invalid option", err, http.StatusBadRequest, "storage_billing_model")
}

// TestBigQueryMLHashBucketize (#1562): ML.HASH_BUCKETIZE on BigQuery's
// documented example
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-hash-bucketize:
// a, b, c, d in 3 buckets are 0, 1, 1, 2), FarmHash Fingerprint64 read
// unsigned modulo the bucket count, which gives that example; a bucket
// count of 0 hashes only (FARM_FINGERPRINT); NULL stays NULL; and in a
// TRANSFORM, applied the same at prediction.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLHashBucketize(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	got := mlQuery(t, h, c, "SELECT f, ML.HASH_BUCKETIZE(f, 3) AS bucket, ML.HASH_BUCKETIZE(f, 0) = FARM_FINGERPRINT(f) AS hashed "+
		"FROM UNNEST(['a', 'b', 'c', 'd']) AS f ORDER BY f")
	want := map[string]int64{"a": 0, "b": 1, "c": 1, "d": 2}
	if len(got) != 4 {
		t.Fatalf("rows: %v", got)
	}
	for _, g := range got {
		if g["bucket"] != want[g["f"].(string)] || g["hashed"] != true {
			t.Errorf("row %v; want bucket %d and the fingerprint at 0", g, want[g["f"].(string)])
		}
	}
	got = mlQuery(t, h, c, "SELECT ML.HASH_BUCKETIZE(CAST(NULL AS STRING), 3) AS b")
	if len(got) != 1 || got[0]["b"] != nil {
		t.Errorf("of NULL: %v", got)
	}
	if _, err := c.Query("SELECT ML.HASH_BUCKETIZE('a', -1)").Read(h.Context()); err == nil {
		t.Errorf("a negative bucket count: no error")
	}

	// In a TRANSFORM: y = 10 · bucket.
	name := mlDataset(t, h, c, map[string]string{
		"hb": "SELECT f, 10.0 * MOD(n, 3) AS y FROM UNNEST([STRUCT('a' AS f, 0 AS n), ('b', 1), ('c', 1), ('d', 2), ('a', 0), ('d', 2)])",
	})
	if err := bqRun(h.Context(), c, "CREATE MODEL `"+name+".hbm` TRANSFORM(CAST(ML.HASH_BUCKETIZE(f, 3) AS FLOAT64) AS b, y) "+
		"OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT f, y FROM `"+name+".hb`", true); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	got = mlQuery(t, h, c, "SELECT f, predicted_y FROM ML.PREDICT(MODEL `"+name+".hbm`, (SELECT f FROM UNNEST(['a', 'b', 'd']) AS f)) ORDER BY f")
	for _, g := range got {
		w := 10 * float64(want[g["f"].(string)])
		if !near(g["predicted_y"], w, 1e-6) {
			t.Errorf("predicted %v; want %v", g, w)
		}
	}
	if len(got) != 3 {
		t.Errorf("predictions: %v", got)
	}
}

// TestBigQueryMLDNNOptions1562 (#1562): a DNN's optimizer FTRL,
// activation_fn CRELU, class_weights and auto_class_weights, and an
// AUTOENCODER's l1_reg_activation, as BigQuery documents them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-dnn-models,
// .../bigqueryml-syntax-create-autoencoder), trained and served through
// the official client: each model is made and kept, its training options
// read back, and ML.PREDICT of the CRELU network (its hidden layer twice
// its units wide) learns y = 2x. A class with no weight in class_weights
// is 400 ("a weight must be present for every class label"). The
// one-step updates are checked by hand in TestDNNOptionsOneStepByHand.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.models.get
func TestBigQueryMLDNNOptions1562(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"line": "SELECT CAST(x AS FLOAT64) / 10 AS x, CAST(x AS FLOAT64) / 5 AS y FROM UNNEST(GENERATE_ARRAY(-20, 20)) AS x",
		"cls":  "SELECT CAST(x AS FLOAT64) AS x, IF(x >= 7, 'hi', 'lo') AS label FROM UNNEST(GENERATE_ARRAY(0, 9)) AS x",
	})
	mk := func(sql string) error { return mlRun(h, c, fmt.Sprintf(sql, name, name)) }
	if err := mk("CREATE MODEL `%s.crelu` OPTIONS(model_type='dnn_regressor', input_label_cols=['y'], hidden_units=[4], " +
		"activation_fn='crelu', optimizer='adam', learn_rate=0.05, max_iterations=60, early_stop=false) AS SELECT x, y FROM `%s.line`"); err != nil {
		t.Fatalf("CREATE MODEL crelu: %v", err)
	}
	got := mlQuery(t, h, c, "SELECT x, predicted_y FROM ML.PREDICT(MODEL `"+name+".crelu`, (SELECT 1.0 AS x UNION ALL SELECT -1.0)) ORDER BY x")
	for _, g := range got {
		if !near(g["predicted_y"], 2*g["x"].(float64), 0.4) {
			t.Errorf("CRELU predicted %v; want about 2x", g)
		}
	}
	if err := mk("CREATE MODEL `%s.ftrl` OPTIONS(model_type='dnn_regressor', input_label_cols=['y'], hidden_units=[4], " +
		"optimizer='ftrl', learn_rate=0.1, l1_reg=0.001, max_iterations=5) AS SELECT x, y FROM `%s.line`"); err != nil {
		t.Fatalf("CREATE MODEL ftrl: %v", err)
	}
	if err := mk("CREATE MODEL `%s.cw` OPTIONS(model_type='dnn_classifier', input_label_cols=['label'], hidden_units=[4], " +
		"class_weights=[STRUCT('hi', 2.0), STRUCT('lo', 0.5)], max_iterations=5) AS SELECT x, label FROM `%s.cls`"); err != nil {
		t.Fatalf("CREATE MODEL class_weights: %v", err)
	}
	if err := mk("CREATE MODEL `%s.acw` OPTIONS(model_type='dnn_classifier', input_label_cols=['label'], hidden_units=[4], " +
		"auto_class_weights=true, max_iterations=5) AS SELECT x, label FROM `%s.cls`"); err != nil {
		t.Fatalf("CREATE MODEL auto_class_weights: %v", err)
	}
	if err := mk("CREATE MODEL `%s.ae` OPTIONS(model_type='autoencoder', hidden_units=[3, 1, 3], l1_reg_activation=0.01, " +
		"max_iterations=5) AS SELECT x, y FROM `%s.line`"); err != nil {
		t.Fatalf("CREATE MODEL autoencoder l1_reg_activation: %v", err)
	}
	for m, want := range map[string]string{"crelu": "CRELU", "ftrl": "FTRL"} {
		md, err := c.Dataset(name).Model(m).Metadata(ctx)
		if err != nil {
			t.Fatalf("Model.Metadata %s: %v", m, err)
		}
		runs := md.RawTrainingRuns()
		if len(runs) != 1 || runs[0].TrainingOptions.ActivationFn != want && runs[0].TrainingOptions.Optimizer != want {
			t.Errorf("%s training run: %+v", m, runs)
		}
	}
	got = mlQuery(t, h, c, "SELECT COUNT(*) AS n FROM ML.PREDICT(MODEL `"+name+".cw`, (SELECT x FROM `"+name+".cls`))")
	if len(got) != 1 || got[0]["n"] != int64(10) {
		t.Errorf("class-weighted predictions: %v", got)
	}
	err := mk("CREATE MODEL `%s.cw2` OPTIONS(model_type='dnn_classifier', input_label_cols=['label'], " +
		"class_weights=[STRUCT('hi', 2.0)]) AS SELECT x, label FROM `%s.cls`")
	wantAPIError(t, "class_weights without every class", err, http.StatusBadRequest, "weight must be present for every class label")
}
