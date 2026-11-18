//go:build compat

package compat

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// aiRows runs sql and fails the test on an error.
func aiRows(t *testing.T, h *Harness, c *bigquery.Client, sql string) []map[string]bigquery.Value {
	t.Helper()
	return mlQuery(t, h, c, sql)
}

// aiConsistent checks one AI.GENERATE_* struct: a value of the function's
// type with an empty status, or a NULL value with a status saying why.
func aiConsistent(t *testing.T, what string, result bigquery.Value, status bigquery.Value, isType func(bigquery.Value) bool) {
	t.Helper()
	s, _ := status.(string)
	switch {
	case s == "" && (result == nil || !isType(result)):
		t.Errorf("%s: empty status but result %#v", what, result)
	case s != "" && result != nil:
		t.Errorf("%s: status %q but result %#v", what, s, result)
	}
	t.Logf("%s (model output recorded, not asserted): result %#v, status %q", what, result, s)
}

// TestBigQueryAIGenerateStructured (#1771): AI.GENERATE_BOOL,
// AI.GENERATE_INT and AI.GENERATE_DOUBLE, AI.GENERATE's output_schema and
// AI.GENERATE_TABLE through the official cloud.google.com/go/bigquery
// client, on the instance's local generation model. Each row is a value
// of the function's type (or the schema's fields) with an empty status,
// or NULL with a status saying why the model's text did not parse (the
// local runtime has no constrained decoding, #1779); a NULL prompt's
// struct is NULL; output_schema replaces result; a bad schema, an
// output_schema of AI.GENERATE_BOOL and an AI.GENERATE_TABLE without one
// are errors; sampling options stay 501. The model's words are recorded,
// not asserted.
//
// covers: bigquery.jobs.query
func TestBigQueryAIGenerateStructured(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "aistruct")
	_, project := remoteConnectionClient(t, h)
	grantProject(t, h, remoteProjectsClient(t, h), project, "roles/aiplatform.user", callerMember(project))

	isBool := func(v bigquery.Value) bool { _, ok := v.(bool); return ok }
	isInt := func(v bigquery.Value) bool { _, ok := v.(int64); return ok }
	isFloat := func(v bigquery.Value) bool { _, ok := v.(float64); return ok }
	for fn, check := range map[string]func(bigquery.Value) bool{"AI.GENERATE_BOOL": isBool, "AI.GENERATE_INT": isInt, "AI.GENERATE_DOUBLE": isFloat} {
		prompt := map[string]string{"AI.GENERATE_BOOL": "Is Paris the capital of France?", "AI.GENERATE_INT": "How many legs does a spider have?",
			"AI.GENERATE_DOUBLE": "What is half of 7?"}[fn]
		rows := aiRows(t, h, c, fmt.Sprintf("SELECT g.result, g.status, g.full_response FROM (SELECT %s(p, endpoint => '%s') AS g "+
			"FROM UNNEST(['%s']) AS p)", fn, localModel, prompt))
		if len(rows) != 1 {
			t.Fatalf("%s: %v", fn, rows)
		}
		aiConsistent(t, fn, rows[0]["result"], rows[0]["status"], check)
		if fr, _ := rows[0]["full_response"].(string); !strings.Contains(fr, "candidates") {
			t.Errorf("%s full_response: %v", fn, rows[0]["full_response"])
		}
		// A NULL prompt's struct is NULL.
		rows = aiRows(t, h, c, fmt.Sprintf("SELECT %s(CAST(NULL AS STRING), endpoint => '%s') AS g", fn, localModel))
		if len(rows) != 1 || rows[0]["g"] != nil {
			t.Errorf("%s of NULL: %v", fn, rows)
		}
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT AI.GENERATE_BOOL('x', endpoint => '%s', output_schema => 'a BOOL')", localModel)); err == nil ||
		!strings.Contains(err.Error(), "Unknown argument output_schema") {
		t.Errorf("output_schema of AI.GENERATE_BOOL: %v", err)
	}

	// AI.GENERATE's output_schema: the schema's fields replace result.
	schema := "name STRING, age INT64 OPTIONS(description = 'age in years'), city STRING"
	rows := aiRows(t, h, c, fmt.Sprintf("SELECT g.name, g.age, g.city, g.status FROM (SELECT AI.GENERATE("+
		"'Extract the person: Maria is 34 years old and lives in Lisbon.', endpoint => '%s', output_schema => '%s') AS g)", localModel, schema))
	if len(rows) != 1 {
		t.Fatalf("output_schema: %v", rows)
	}
	if st, _ := rows[0]["status"].(string); st == "" {
		if _, ok := rows[0]["age"].(int64); !ok || rows[0]["name"] == nil {
			t.Errorf("output_schema with an empty status: %v", rows[0])
		}
	} else if rows[0]["name"] != nil || rows[0]["age"] != nil {
		t.Errorf("output_schema with status %q: %v", st, rows[0])
	}
	t.Logf("output_schema (model output recorded, not asserted): %v", rows[0])
	if err := mlRun(h, c, fmt.Sprintf("SELECT AI.GENERATE('x', endpoint => '%s', output_schema => '%s').result", localModel, schema)); err == nil {
		t.Error("result is still a field with output_schema")
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT AI.GENERATE('x', endpoint => '%s', output_schema => 'a DATE')", localModel)); err == nil ||
		!strings.Contains(err.Error(), "output_schema") {
		t.Errorf("an unsupported schema type: %v", err)
	}

	// AI.GENERATE_TABLE: the input's columns, the schema's, full_response
	// and status.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.gen` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn, localModel)); err != nil {
		t.Fatalf("CREATE MODEL REMOTE: %v", err)
	}
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT * FROM AI.GENERATE_TABLE(MODEL `%s.gen`, (SELECT 1 AS id, "+
		"'Extract the person: Tom is 51 and lives in Oslo.' AS prompt UNION ALL SELECT 2, CAST(NULL AS STRING)), "+
		"STRUCT('%s' AS output_schema)) ORDER BY id", ds, schema))
	if len(rows) != 2 {
		t.Fatalf("AI.GENERATE_TABLE: %v", rows)
	}
	for _, col := range []string{"id", "prompt", "name", "age", "city", "full_response", "status"} {
		if _, ok := rows[0][col]; !ok {
			t.Errorf("AI.GENERATE_TABLE has no %s column: %v", col, rows[0])
		}
	}
	if st, _ := rows[0]["status"].(string); st == "" && rows[0]["name"] == nil {
		t.Errorf("AI.GENERATE_TABLE row with an empty status: %v", rows[0])
	}
	t.Logf("AI.GENERATE_TABLE (model output recorded, not asserted): %v", rows[0])
	if err := mlRun(h, c, fmt.Sprintf("SELECT * FROM AI.GENERATE_TABLE(MODEL `%s.gen`, (SELECT 'x' AS prompt))", ds)); err == nil ||
		!strings.Contains(err.Error(), "output_schema") {
		t.Errorf("AI.GENERATE_TABLE without output_schema: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT * FROM AI.GENERATE_TABLE(MODEL `%s.gen`, (SELECT 'x' AS prompt), STRUCT('a STRING' AS output_schema, 0.5 AS temperature))", ds)); err == nil ||
		!strings.Contains(err.Error(), "temperature") || !strings.Contains(err.Error(), "501") {
		t.Errorf("temperature: %v", err)
	}
}

// TestBigQueryAIManagedFunctions (#1772): AI.IF (in WHERE and the select
// list), AI.SCORE (in ORDER BY) and AI.CLASSIFY (single, output_mode
// 'single' and 'multi') through the official Go client on the local
// generation model; a row the model's text does not answer is NULL; when
// every row fails (the default endpoint, with no alias locally) the query
// fails with BigQuery's error-ratio error; embeddings is 501, as is a
// call in a JOIN's ON; max_error_ratio needs MAXIMIZE_QUALITY.
//
// covers: bigquery.jobs.query
func TestBigQueryAIManagedFunctions(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	_, project := remoteConnectionClient(t, h)
	grantProject(t, h, remoteProjectsClient(t, h), project, "roles/aiplatform.user", callerMember(project))
	ds := mlDataset(t, h, c, map[string]string{
		"reviews": "SELECT 1 AS id, 'The food was cold and the waiter was rude.' AS review UNION ALL " +
			"SELECT 2, 'Wonderful evening, delicious food, friendly staff.' UNION ALL SELECT 3, 'My card was charged twice for one meal.'",
	})

	rows := aiRows(t, h, c, fmt.Sprintf("SELECT id, AI.IF(('Is this review positive? ', review), endpoint => '%s') AS positive "+
		"FROM `%s.reviews` ORDER BY id", localModel, ds))
	if len(rows) != 3 {
		t.Fatalf("AI.IF: %v", rows)
	}
	for _, r := range rows {
		if _, ok := r["positive"].(bool); !ok && r["positive"] != nil {
			t.Errorf("AI.IF is not a BOOL: %v", r)
		}
	}
	t.Logf("AI.IF (model output recorded, not asserted): %v", rows)
	where := aiRows(t, h, c, fmt.Sprintf("SELECT id FROM `%s.reviews` WHERE AI.IF(('Is this review positive? ', review), endpoint => '%s') ORDER BY id", ds, localModel))
	want := 0
	for _, r := range rows {
		if r["positive"] == true {
			want++
		}
	}
	if len(where) != want {
		t.Errorf("AI.IF in WHERE kept %v, the select list said %v", where, rows)
	}

	rows = aiRows(t, h, c, fmt.Sprintf("SELECT id, AI.SCORE(('Rate how positive this review is from 1 to 10: ', review), endpoint => '%s') AS score "+
		"FROM `%s.reviews` ORDER BY score DESC, id", localModel, ds))
	if len(rows) != 3 {
		t.Fatalf("AI.SCORE: %v", rows)
	}
	for _, r := range rows {
		if _, ok := r["score"].(float64); !ok && r["score"] != nil {
			t.Errorf("AI.SCORE is not a FLOAT64: %v", r)
		}
	}
	t.Logf("AI.SCORE (model output recorded, not asserted): %v", rows)

	cats := "['complaint', 'praise', 'billing']"
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT id, AI.CLASSIFY(review, %s, endpoint => '%s') AS c, "+
		"AI.CLASSIFY(review, %s, endpoint => '%s', output_mode => 'multi', optimization_mode => 'MAXIMIZE_QUALITY', max_error_ratio => 1.0) AS m "+
		"FROM `%s.reviews` ORDER BY id", cats, localModel, cats, localModel, ds))
	if len(rows) != 3 {
		t.Fatalf("AI.CLASSIFY: %v", rows)
	}
	for _, r := range rows {
		if s, ok := r["c"].(string); ok && s != "complaint" && s != "praise" && s != "billing" || !ok && r["c"] != nil {
			t.Errorf("AI.CLASSIFY is not one of the categories: %v", r)
		}
		if _, ok := r["m"].([]bigquery.Value); !ok && r["m"] != nil {
			t.Errorf("AI.CLASSIFY multi is not an ARRAY: %v", r)
		}
	}
	t.Logf("AI.CLASSIFY (model output recorded, not asserted): %v", rows)

	for sql, want := range map[string]string{
		// gemini-3.5-flash-lite has no alias locally: every row fails.
		fmt.Sprintf("SELECT AI.IF(review) FROM `%s.reviews`", ds):                                                                               "exceeded their allowed error ratio",
		fmt.Sprintf("SELECT AI.IF(review, endpoint => '%s', embeddings => [0.1, 0.2]) FROM `%s.reviews`", localModel, ds):                       "embeddings",
		fmt.Sprintf("SELECT a.id FROM `%s.reviews` a JOIN `%s.reviews` b ON AI.IF((a.review, b.review), endpoint => '%s')", ds, ds, localModel): "JOIN",
		fmt.Sprintf("SELECT AI.IF(review, endpoint => '%s', max_error_ratio => 0.5) FROM `%s.reviews`", localModel, ds):                         "MINIMIZE_COST",
		fmt.Sprintf("SELECT AI.CLASSIFY(review, [], endpoint => '%s') FROM `%s.reviews`", localModel, ds):                                       "category",
		"SELECT AI.AGG(s, 'summarize') FROM UNNEST(['a']) AS s":                                                                                 "AI.AGG",
	} {
		if err := mlRun(h, c, sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}
}

// TestBigQueryAIEmbed (#1772): AI.EMBED, AI.SIMILARITY and
// AI.GENERATE_EMBEDDING through the official Go client on the local
// embedding model: 768 values (or outputDimensionality's), an empty
// status, a text's similarity to itself 1 and higher to a paraphrase
// than to an unrelated text; model => 'embeddinggemma-300m' is resolved
// by the local endpoint's rule (no alias: its NOT_FOUND is the status);
// AI.GENERATE_EMBEDDING's embedding, statistics and status columns.
//
// covers: bigquery.jobs.query
func TestBigQueryAIEmbed(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvEmbeddings)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "aiemb")
	_, project := remoteConnectionClient(t, h)
	grantProject(t, h, remoteProjectsClient(t, h), project, "roles/aiplatform.user", callerMember(project))
	const emb = "embeddinggemma-300m-onnx-community"

	rows := aiRows(t, h, c, fmt.Sprintf("SELECT e.result, e.status, ARRAY_LENGTH(e.result) AS n FROM (SELECT AI.EMBED('How do I bake bread?', endpoint => '%s') AS e)", emb))
	if len(rows) != 1 || rows[0]["status"] != "" || rows[0]["n"] != int64(768) {
		t.Fatalf("AI.EMBED: %v %v", rows[0]["status"], rows[0]["n"])
	}
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT ARRAY_LENGTH(AI.EMBED(content => 'bread', endpoint => '%s', task_type => 'RETRIEVAL_DOCUMENT', "+
		"title => 'Baking', model_params => JSON '{\"outputDimensionality\": 256}').result) AS n", emb))
	if len(rows) != 1 || rows[0]["n"] != int64(256) {
		t.Errorf("outputDimensionality: %v", rows)
	}
	rows = aiRows(t, h, c, "SELECT AI.EMBED('bread', model => 'embeddinggemma-300m').status AS s")
	if len(rows) != 1 || !strings.Contains(fmt.Sprint(rows[0]["s"]), "NOT_FOUND") {
		t.Errorf("model => embeddinggemma-300m with no alias: %v", rows)
	}
	for sql, want := range map[string]string{
		"SELECT AI.EMBED('bread')": "endpoint or model",
		fmt.Sprintf("SELECT AI.EMBED('bread', endpoint => '%s', title => 'x')", emb):                      "RETRIEVAL_DOCUMENT",
		fmt.Sprintf("SELECT AI.EMBED('bread', endpoint => '%s', task_type => 'NOPE')", emb):               "task_type",
		fmt.Sprintf("SELECT AI.EMBED('bread', endpoint => '%s', model => 'embeddinggemma-300m')", emb):    "not both",
		fmt.Sprintf("SELECT AI.EMBED('bread', endpoint => '%s', model_params => JSON '{\"x\": 1}')", emb): "501",
	} {
		if err := mlRun(h, c, sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}

	rows = aiRows(t, h, c, fmt.Sprintf("SELECT AI.SIMILARITY(content1 => 'How do I bake bread?', content2 => 'How do I bake bread?', endpoint => '%s') AS same, "+
		"AI.SIMILARITY(content1 => 'How do I bake bread?', content2 => 'What is the recipe for a loaf of bread?', endpoint => '%s') AS near, "+
		"AI.SIMILARITY(content1 => 'How do I bake bread?', content2 => 'Interest rates rose by a quarter point.', endpoint => '%s') AS far, "+
		"AI.SIMILARITY(content1 => CAST(NULL AS STRING), content2 => 'x', endpoint => '%s') AS null_sim", emb, emb, emb, emb))
	same, _ := rows[0]["same"].(float64)
	near, _ := rows[0]["near"].(float64)
	far, _ := rows[0]["far"].(float64)
	if len(rows) != 1 || math.Abs(same-1) > 1e-6 || !(near > far) || rows[0]["null_sim"] != nil {
		t.Errorf("AI.SIMILARITY: %v", rows)
	}
	t.Logf("AI.SIMILARITY: same %f, paraphrase %f, unrelated %f", same, near, far)

	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.emb` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn, emb)); err != nil {
		t.Fatalf("CREATE MODEL REMOTE: %v", err)
	}
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT id, embedding, statistics, status FROM AI.GENERATE_EMBEDDING(MODEL `%s.emb`, TABLE `%s.docs`, "+
		"STRUCT('RETRIEVAL_DOCUMENT' AS task_type, 128 AS output_dimensionality)) ORDER BY id", ds, ds))
	if len(rows) != 3 {
		t.Fatalf("AI.GENERATE_EMBEDDING: %v", rows)
	}
	for _, r := range rows {
		vs, _ := r["embedding"].([]bigquery.Value)
		if len(vs) != 128 || r["status"] != "" || !strings.Contains(fmt.Sprint(r["statistics"]), "token_count") {
			t.Errorf("AI.GENERATE_EMBEDDING row %v: %d values, %v, %v", r["id"], len(vs), r["statistics"], r["status"])
		}
	}
}
