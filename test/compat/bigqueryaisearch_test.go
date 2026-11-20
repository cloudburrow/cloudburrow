//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryAutonomousEmbeddingsAndAISearch (#1778): autonomous
// embedding generation and AI.SEARCH through the official
// cloud.google.com/go/bigquery client (and its generated bigquery/v2
// client for the schema's generatedColumn), on the local embedding model:
//
//   - CREATE TABLE with a column GENERATED ALWAYS AS (AI.EMBED(...))
//     STORED OPTIONS (asynchronous = TRUE) makes the table; rows inserted
//     with only their source get their embedding in the background (768
//     values, status ”), a NULL source's result NULL with BigQuery's
//     status; an UPDATE of a source writes its new embedding;
//   - tables.get's field has generatedColumn GENERATED_ALWAYS with the
//     expression, and INFORMATION_SCHEMA.COLUMNS is_generated ALWAYS;
//   - writing the generated column (INSERT, UPDATE, insertAll) and
//     dropping the source column are refused;
//   - ALTER TABLE ADD COLUMN ... GENERATED fills an existing table's rows;
//   - AI.SEARCH returns base and distance, nearest first, top_k rows, its
//     base query's columns only, each distance_type; a table without
//     generation, another column and HYBRID without a hybrid index are
//     refused, and so are a reranker's num_candidates under top_k and an
//     empty query_value with rerank;
//   - rerank => TRUE adds rerank_score (0 to 1), the rows ordered by it,
//     scored on the local embedding model;
//   - with a hybrid index (CREATE VECTOR INDEX on the embedding column
//     with lexical_search_columns, #1952), mode HYBRID and AUTO run a
//     hybrid search, its distance 1 minus a reciprocal rank fusion score,
//     and mode VECTOR a semantic one.
//
// covers: bigquery.jobs.query, bigquery.tables.get, bigquery.tabledata.insertAll
func TestBigQueryAutonomousEmbeddingsAndAISearch(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvEmbeddings)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "aisearch")
	const emb = "embeddinggemma-300m-onnx-community"
	run := func(sql string) error { return mlRun(h, c, sql) }

	create := fmt.Sprintf("CREATE TABLE `%s.products` (name STRING, description STRING, "+
		"description_embedding STRUCT<result ARRAY<FLOAT64>, status STRING> GENERATED ALWAYS AS "+
		"(AI.EMBED(description, connection_id => '%s', endpoint => '%s')) STORED OPTIONS (asynchronous = TRUE))", ds, conn, emb)
	if err := run(create); err != nil {
		t.Fatalf("CREATE TABLE with a generated column: %v", err)
	}
	if err := run(fmt.Sprintf("INSERT INTO `%s.products` (name, description) VALUES "+
		"('Super slingers', 'An exciting board game for the whole family'), "+
		"('Encyclopedia set', 'A collection of informational books'), "+
		"('Lounger chair', 'A comfortable chair for relaxing'), "+
		"('Mystery', NULL)", ds)); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	// The background job fills every row.
	progress := fmt.Sprintf("SELECT COUNTIF(description_embedding.result IS NOT NULL AND description_embedding.status = '') AS done, "+
		"COUNTIF(description_embedding IS NOT NULL) AS seen, COUNT(*) AS total FROM `%s.products`", ds)
	waitRows := func(what string, want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			rows := mlQuery(t, h, c, progress)
			if rows[0]["seen"] == rows[0]["total"] && rows[0]["done"] == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: embeddings not generated: %v", what, rows)
			}
			time.Sleep(time.Second)
		}
	}
	waitRows("after INSERT", 3)
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT name, ARRAY_LENGTH(description_embedding.result) AS n, description_embedding.status AS status "+
		"FROM `%s.products` ORDER BY name", ds))
	for _, r := range rows {
		if r["name"] == "Mystery" {
			if r["n"] != nil || r["status"] != "NULL value is not supported for embedding generation" {
				t.Errorf("NULL source: %v", r)
			}
			continue
		}
		if r["n"] != int64(768) || r["status"] != "" {
			t.Errorf("row %v", r)
		}
	}

	// tables.get and INFORMATION_SCHEMA.COLUMNS.
	project := c.Project()
	tbl, err := generatedBigQuery(t, h).Tables.Get(project, ds, "products").Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("tables.get: %v", err)
	}
	var gen bool
	for _, fl := range tbl.Schema.Fields {
		if fl.Name == "description_embedding" && fl.GeneratedColumn != nil && fl.GeneratedColumn.GeneratedMode == "GENERATED_ALWAYS" &&
			fl.GeneratedColumn.GeneratedExpressionInfo != nil && fl.GeneratedColumn.GeneratedExpressionInfo.Asynchronous &&
			strings.Contains(fl.GeneratedColumn.GeneratedExpressionInfo.GenerationExpression, "AI.EMBED(description") {
			gen = true
		}
	}
	if !gen {
		t.Errorf("tables.get: no generatedColumn on description_embedding: %+v", tbl.Schema.Fields)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT column_name, is_generated, generation_expression, is_stored FROM `%s`.INFORMATION_SCHEMA.COLUMNS "+
		"WHERE table_name = 'products' AND is_generated = 'ALWAYS'", ds))
	if len(rows) != 1 || rows[0]["column_name"] != "description_embedding" || !strings.Contains(fmt.Sprint(rows[0]["generation_expression"]), "RETRIEVAL_DOCUMENT") {
		t.Errorf("INFORMATION_SCHEMA.COLUMNS: %v", rows)
	}

	// The generated column is BigQuery's to write.
	for sql, want := range map[string]string{
		fmt.Sprintf("INSERT INTO `%s.products` (name, description_embedding) VALUES ('x', NULL)", ds): "generated column",
		fmt.Sprintf("UPDATE `%s.products` SET description_embedding = NULL WHERE TRUE", ds):           "generated column",
		fmt.Sprintf("ALTER TABLE `%s.products` DROP COLUMN description", ds):                          "source",
		fmt.Sprintf("ALTER TABLE `%s.products` ADD COLUMN e2 STRUCT<result ARRAY<FLOAT64>, status STRING> GENERATED ALWAYS AS "+
			"(AI.EMBED(name, connection_id => '%s', endpoint => '%s')) STORED OPTIONS (asynchronous = TRUE)", ds, conn, emb): "at most one",
	} {
		if err := run(sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}
	ins := c.Dataset(ds).Table("products").Inserter()
	if err := ins.Put(h.Context(), []*bigquery.ValuesSaver{{Schema: bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType},
		{Name: "description_embedding", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "status", Type: bigquery.StringFieldType}}}},
		Row: []bigquery.Value{"x", []bigquery.Value{"y"}}}}); err == nil || !strings.Contains(err.Error(), "generated column") {
		t.Errorf("insertAll of the generated column: %v", err)
	}

	// AI.SEARCH.
	search := func(args string) []map[string]bigquery.Value {
		t.Helper()
		return mlQuery(t, h, c, fmt.Sprintf("SELECT base.name AS name, distance FROM AI.SEARCH(TABLE `%s.products`, 'description', %s) ORDER BY distance", ds, args))
	}
	rows = search("'A really fun toy'")
	if len(rows) != 3 || rows[0]["name"] != "Super slingers" {
		t.Errorf("AI.SEARCH: %v", rows)
	}
	t.Logf("AI.SEARCH 'A really fun toy' (EUCLIDEAN): %v", rows)
	for _, d := range []string{"COSINE", "DOT_PRODUCT"} {
		rows = search("'A comfortable seat', top_k => 1, distance_type => '" + d + "'")
		if len(rows) != 1 || rows[0]["name"] != "Lounger chair" {
			t.Errorf("AI.SEARCH %s: %v", d, rows)
		}
	}
	rows = search("'books', top_k => -1, mode => 'AUTO', options => '{\"use_brute_force\": true}'")
	if len(rows) != 3 || rows[0]["name"] != "Encyclopedia set" {
		t.Errorf("AI.SEARCH top_k -1: %v", rows)
	}
	q := c.Query(fmt.Sprintf("SELECT base, distance FROM AI.SEARCH((SELECT name FROM `%s.products` WHERE name != 'Super slingers'), 'description', @q, top_k => 5)", ds))
	q.Parameters = []bigquery.QueryParameter{{Name: "q", Value: "game"}}
	it, err := q.Read(h.Context())
	if err != nil {
		t.Fatalf("AI.SEARCH of a query: %v", err)
	}
	var n int
	for {
		var row map[string]bigquery.Value
		if err := it.Next(&row); err != nil {
			break
		}
		base, _ := row["base"].(map[string]bigquery.Value)
		if len(base) != 1 || base["name"] == "Super slingers" || base["name"] == nil {
			t.Errorf("AI.SEARCH of a query: base %v", row)
		}
		n++
	}
	if n != 2 {
		t.Errorf("AI.SEARCH of a query: %d rows, want 2", n)
	}

	// An UPDATE of the source is embedded again.
	if err := run(fmt.Sprintf("UPDATE `%s.products` SET description = 'A fun toy for children' WHERE name = 'Lounger chair'", ds)); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		rows = search("'A really fun toy', top_k => 1")
		if len(rows) == 1 && rows[0]["name"] == "Lounger chair" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the updated source's embedding: %v", rows)
		}
		time.Sleep(time.Second)
	}

	// ALTER TABLE ADD COLUMN fills an existing table.
	if err := run(fmt.Sprintf("CREATE TABLE `%s.notes` AS SELECT 'note one' AS body UNION ALL SELECT 'note two'", ds)); err != nil {
		t.Fatalf("CREATE TABLE notes: %v", err)
	}
	if err := run(fmt.Sprintf("ALTER TABLE `%s.notes` ADD COLUMN body_embedding STRUCT<result ARRAY<FLOAT64>, status STRING> GENERATED ALWAYS AS "+
		"(AI.EMBED(body, connection_id => '%s', endpoint => '%s')) STORED OPTIONS (asynchronous = TRUE)", ds, conn, emb)); err != nil {
		t.Fatalf("ALTER TABLE ADD COLUMN GENERATED: %v", err)
	}
	deadline = time.Now().Add(time.Minute)
	for {
		rows = mlQuery(t, h, c, fmt.Sprintf("SELECT COUNTIF(ARRAY_LENGTH(body_embedding.result) = 768) AS n FROM `%s.notes`", ds))
		if rows[0]["n"] == int64(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ALTER TABLE ADD COLUMN: %v", rows)
		}
		time.Sleep(time.Second)
	}

	for sql, want := range map[string]string{
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.docs`, 'content', 'bread')", ds):                       "autonomous embedding generation",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'name', 'bread')", ds):                      "source column",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'description', 'x', mode => 'HYBRID')", ds): "hybrid index",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'description', 'x', top_k => 2, rerank => TRUE, "+
			"options => '{\"reranker\": {\"num_candidates\": 1}}')", ds): "num_candidates",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'description', '', rerank => TRUE)", ds):                   "non-empty",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'description', 'x', options => '{\"reranker\": {}}')", ds): "rerank",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'description', 'x', "+
			"options => '{\"use_brute_force\": true, \"fraction_lists_to_search\": 0.5}')", ds): "fraction_lists_to_search",
		fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.products`, 'description', 'x', distance_type => 'MANHATTAN')", ds): "distance_type",
	} {
		if err := run(sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}

	// rerank: the candidates scored and ordered by rerank_score.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT base.name AS name, distance, rerank_score FROM AI.SEARCH(TABLE `%s.products`, 'description', "+
		"'books to learn from', top_k => 2, rerank => TRUE, options => '{\"reranker\": {\"num_candidates\": 3}}') ORDER BY rerank_score DESC", ds))
	if len(rows) != 2 || rows[0]["name"] != "Encyclopedia set" {
		t.Errorf("AI.SEARCH rerank: %v", rows)
	}
	for _, r := range rows {
		if s, ok := r["rerank_score"].(float64); !ok || s < 0 || s > 1 {
			t.Errorf("rerank_score %v, want 0 to 1", r)
		}
	}
	t.Logf("AI.SEARCH rerank: %v", rows)

	// A hybrid index: HYBRID and AUTO fuse the semantic and lexical ranks.
	if err := run(fmt.Sprintf("CREATE VECTOR INDEX hybrid_index ON `%s.products`(description_embedding) "+
		"OPTIONS(index_type = 'IVF', lexical_search_columns = ['description'])", ds)); err != nil {
		t.Fatalf("CREATE VECTOR INDEX with lexical_search_columns: %v", err)
	}
	for _, mode := range []string{"HYBRID", "AUTO"} {
		rows := mlQuery(t, h, c, fmt.Sprintf("SELECT base.name AS name, distance FROM AI.SEARCH(TABLE `%s.products`, 'description', 'board game', "+
			"mode => '%s', top_k => 1)", ds, mode))
		if len(rows) != 1 || rows[0]["name"] != "Super slingers" || !near(rows[0]["distance"], 1-2.0/61, 0.0001) {
			t.Errorf("AI.SEARCH mode %s over a hybrid index: %v, want Super slingers at 1 - (1/61 + 1/61)", mode, rows)
		}
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT base.name AS name, distance FROM AI.SEARCH(TABLE `%s.products`, 'description', 'board game', "+
		"mode => 'VECTOR', top_k => 1)", ds))
	if len(rows) != 1 || rows[0]["name"] != "Super slingers" || near(rows[0]["distance"], 1-2.0/61, 0.0001) {
		t.Errorf("AI.SEARCH mode VECTOR over a hybrid index: %v, want a semantic distance", rows)
	}
	if err := run(fmt.Sprintf("DROP VECTOR INDEX hybrid_index ON `%s.products`", ds)); err != nil {
		t.Errorf("DROP VECTOR INDEX: %v", err)
	}
}
