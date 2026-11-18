//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
)

// TestBigQueryVectorSearch (#1778): VECTOR_SEARCH through the official
// cloud.google.com/go/bigquery client, on the examples of
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/search_functions#vector_search
// (read 2026-11-18), whose distances it checks:
//
//   - the batch form over TABLE base_table and a query table, top_k => 2:
//     dog matches dog (0) and wolf (2.236), cat lion (1.803) and tiger
//     (2.236), with query, base and distance;
//   - a base_table_query with WHERE excludes wolf: dog then snake (3.162);
//   - COSINE: cat lion (0.0061) and tiger (0.0194); dog dog and wolf (0);
//   - the single form, query_value => [1.0, -1.0]: base and distance only;
//   - query_column_to_search named, top_k => -1, options checked;
//   - CREATE and DROP VECTOR INDEX are 501 (no index is trained), and
//     INFORMATION_SCHEMA.VECTOR_INDEXES stays empty.
//
// covers: bigquery.jobs.query
func TestBigQueryVectorSearch(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_vs")
	if err := ds.Create(h.Context(), &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	d := ds.DatasetID
	run := func(sql string) error { return mlRun(h, c, sql) }
	for _, sql := range []string{
		fmt.Sprintf("CREATE TABLE `%s.base_table` (id STRING, description STRING, my_embedding ARRAY<FLOAT64>)", d),
		fmt.Sprintf("INSERT `%s.base_table` (id, description, my_embedding) VALUES "+
			"('dog', \"Man's best friend\", [1.0, 2.0]), ('wolf', 'The ultimate pack hunter', [2.0, 4.0]), "+
			"('snake', 'The slithering serpent', [-2.0, 3.0]), ('lion', 'King of the jungle', [2.0, -2.5]), "+
			"('tiger', 'The striped stalker', [3.0, -2.0]), ('otter', \"The river's playful acrobat\", [-3.0, -1.0]), "+
			"('whale', \"The ocean's gentle giant\", [-5.0, -1.0])", d),
		fmt.Sprintf("CREATE TABLE `%s.query_table` (query_id STRING, embedding ARRAY<FLOAT64>)", d),
		fmt.Sprintf("INSERT `%s.query_table` (query_id, embedding) VALUES ('dog', [1.0, 2.0]), ('cat', [1.0, -1.0])", d),
	} {
		if err := run(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// pairs reads a batch search as "query:base" -> distance.
	pairs := func(sql string) map[string]float64 {
		t.Helper()
		out := map[string]float64{}
		for _, r := range mlQuery(t, h, c, sql) {
			q, _ := r["query"].(map[string]bigquery.Value)
			b, _ := r["base"].(map[string]bigquery.Value)
			dist, _ := r["distance"].(float64)
			if q == nil || b == nil {
				t.Fatalf("%s: row %v has no query or base", sql, r)
			}
			out[fmt.Sprint(q["query_id"])+":"+fmt.Sprint(b["id"])] = dist
		}
		return out
	}
	check := func(what string, got map[string]float64, want map[string]float64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
		for k, w := range want {
			g, ok := got[k]
			if !ok || g < w-0.0001 || g > w+0.0001 {
				t.Errorf("%s: %v, want %v", what, got, want)
				return
			}
		}
	}
	check("batch, top_k 2", pairs(fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', "+
		"(SELECT query_id, embedding FROM `%s.query_table`), 'embedding', top_k => 2)", d, d)),
		map[string]float64{"dog:dog": 0, "dog:wolf": 2.2361, "cat:lion": 1.8028, "cat:tiger": 2.2361})
	check("base_table_query", pairs(fmt.Sprintf("SELECT query, base, distance FROM VECTOR_SEARCH((SELECT * FROM `%s.base_table` WHERE id != 'wolf'), "+
		"'my_embedding', (SELECT query_id, embedding FROM `%s.query_table`), query_column_to_search => 'embedding', top_k => 2, "+
		"options => '{\"use_brute_force\": true}')", d, d)),
		map[string]float64{"dog:dog": 0, "dog:snake": 3.1623, "cat:lion": 1.8028, "cat:tiger": 2.2361})
	check("COSINE", pairs(fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', TABLE `%s.query_table`, 'embedding', "+
		"top_k => 2, distance_type => 'COSINE')", d, d)),
		map[string]float64{"dog:dog": 0, "dog:wolf": 0, "cat:lion": 0.0061, "cat:tiger": 0.0194})
	all := pairs(fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', TABLE `%s.query_table`, "+
		"query_column_to_search => 'embedding', top_k => -1, distance_type => 'DOT_PRODUCT')", d, d))
	if len(all) != 14 {
		t.Errorf("top_k -1: %d rows, want 14: %v", len(all), all)
	}

	// The single form.
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT base.id AS id, base.my_embedding AS e, distance FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', "+
		"query_value => [1.0, -1.0], top_k => 2) ORDER BY distance", d))
	if len(rows) != 2 || rows[0]["id"] != "lion" || !near(rows[0]["distance"], 1.8028, 0.0001) || rows[1]["id"] != "tiger" || !near(rows[1]["distance"], 2.2361, 0.0001) {
		t.Errorf("query_value: %v", rows)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM VECTOR_SEARCH((SELECT id FROM `%s.base_table`), 'my_embedding', query_value => [1.0, 2.0], top_k => 1)", d))
	if len(rows) != 1 || len(rows[0]) != 2 {
		t.Errorf("query_value of a base query: %v", rows)
	} else if b, _ := rows[0]["base"].(map[string]bigquery.Value); len(b) != 1 || b["id"] != "dog" {
		t.Errorf("query_value of a base query: base %v", rows[0])
	}

	for sql, want := range map[string]string{
		fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'description', TABLE `%s.query_table`, 'embedding')", d, d): "autonomous embedding generation",
		fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', query_value => [1.0, 2.0, 3.0])", d):        "dimensions",
		fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', query_value => [1.0, 2.0], "+
			"options => '{\"use_brute_force\": true, \"fraction_lists_to_search\": 0.1}')", d): "fraction_lists_to_search",
		fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', query_value => [1.0, 2.0], "+
			"options => '{\"fraction_lists_to_search\": 1.5}')", d): "fraction_lists_to_search",
		fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', query_value => [1.0, 2.0], distance_type => 'L1')", d): "distance_type",
		fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', query_value => [1.0, -1.0], "+
			"lexical_search_columns => ['description'], lexical_search_query_value => 'striped hunter')", d): "501",
		fmt.Sprintf("CREATE VECTOR INDEX my_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'IVF')", d): "501",
		fmt.Sprintf("DROP VECTOR INDEX my_index ON `%s.base_table`", d):                                             "501",
	} {
		if err := run(sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}
	if rows := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEXES", d)); len(rows) != 0 {
		t.Errorf("VECTOR_INDEXES: %v", rows)
	}
}

// TestBigQueryVectorSearchAutonomousEmbeddings (#1778): VECTOR_SEARCH over
// a STRING column with autonomous embedding generation, its query strings
// embedded at run time on the local embedding model, and the generated
// column's documented writes and renames, through the official Go client:
//
//   - the batch form with a STRING query column, and the single form with
//     a STRING query_value, rank the products page's example as AI.SEARCH;
//   - ALTER TABLE RENAME COLUMN of the embedding column keeps its
//     generation (a new row is embedded, AI.SEARCH still answers), as
//     "you can still drop or rename the generated embedding column";
//   - a load whose schema gives the generated column, and a WRITE_APPEND
//     copy (bq copy -a) from a table that has it, are refused, as the page
//     lists bq load and bq copy -a among the writes it refuses;
//   - with the connection's service account's Vertex AI User role revoked,
//     generation stalls and INFORMATION_SCHEMA.COLUMNS'
//     async_generation_status.blocking_error.message gives the page's
//     message; granted again, the row is embedded and the status cleared.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryVectorSearchAutonomousEmbeddings(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvEmbeddings)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "vsembed")
	const emb = "embeddinggemma-300m-onnx-community"
	run := func(sql string) error { return mlRun(h, c, sql) }
	for _, sql := range []string{
		fmt.Sprintf("CREATE TABLE `%s.products` (name STRING, description STRING, "+
			"description_embedding STRUCT<result ARRAY<FLOAT64>, status STRING> GENERATED ALWAYS AS "+
			"(AI.EMBED(description, connection_id => '%s', endpoint => '%s')) STORED OPTIONS (asynchronous = TRUE))", ds, conn, emb),
		fmt.Sprintf("INSERT INTO `%s.products` (name, description) VALUES "+
			"('Super slingers', 'An exciting board game for the whole family'), "+
			"('Encyclopedia set', 'A collection of informational books'), "+
			"('Lounger chair', 'A comfortable chair for relaxing')", ds),
		fmt.Sprintf("CREATE TABLE `%s.queries` (q STRING) AS SELECT 'A really fun toy' AS q UNION ALL SELECT 'A comfortable seat'", ds),
	} {
		if err := run(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	waitEmbedded := func(col string, want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			rows := mlQuery(t, h, c, fmt.Sprintf("SELECT COUNTIF(%s.result IS NOT NULL) AS n FROM `%s.products`", col, ds))
			if rows[0]["n"] == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("embeddings of %s not generated: %v", col, rows)
			}
			time.Sleep(time.Second)
		}
	}
	waitEmbedded("description_embedding", 3)

	// The batch form with a STRING query column.
	got := map[string]string{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT query.q AS q, base.name AS name, distance FROM VECTOR_SEARCH(TABLE `%s.products`, 'description', "+
		"TABLE `%s.queries`, 'q', top_k => 1)", ds, ds)) {
		got[fmt.Sprint(r["q"])] = fmt.Sprint(r["name"])
	}
	if got["A really fun toy"] != "Super slingers" || got["A comfortable seat"] != "Lounger chair" || len(got) != 2 {
		t.Errorf("VECTOR_SEARCH of STRING queries: %v", got)
	}
	// The single form with a STRING query_value.
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT base.name AS name, distance FROM VECTOR_SEARCH(TABLE `%s.products`, 'description', "+
		"query_value => 'books', top_k => 3) ORDER BY distance", ds))
	if len(rows) != 3 || rows[0]["name"] != "Encyclopedia set" {
		t.Errorf("VECTOR_SEARCH query_value 'books': %v", rows)
	}
	if err := run(fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.products`, 'name', query_value => 'books')", ds)); err == nil ||
		!strings.Contains(err.Error(), "autonomous embedding generation") {
		t.Errorf("VECTOR_SEARCH of a STRING column without generation: %v", err)
	}

	// A load or bq copy -a that gives the generated column is refused.
	schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "description", Type: bigquery.StringFieldType},
		{Name: "description_embedding", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "result", Type: bigquery.FloatFieldType, Repeated: true}, {Name: "status", Type: bigquery.StringFieldType}}}}
	src := bigquery.NewReaderSource(strings.NewReader(`{"name":"Kite","description":"A kite","description_embedding":{"result":[1.0],"status":""}}` + "\n"))
	src.SourceFormat = bigquery.JSON
	src.Schema = schema
	loader := c.Dataset(ds).Table("products").LoaderFrom(src)
	loader.WriteDisposition = bigquery.WriteAppend
	if job, err := loader.Run(h.Context()); err == nil {
		st, werr := job.Wait(h.Context())
		if werr == nil {
			werr = st.Err()
		}
		err = werr
		if err == nil || !strings.Contains(err.Error(), "generated column") {
			t.Errorf("a load that gives the generated column: %v", err)
		}
	} else if !strings.Contains(err.Error(), "generated column") {
		t.Errorf("a load that gives the generated column: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE TABLE `%s.more` AS SELECT * FROM `%s.products` LIMIT 1", ds, ds)); err != nil {
		t.Fatalf("CREATE TABLE more: %v", err)
	}
	cp := c.Dataset(ds).Table("products").CopierFrom(c.Dataset(ds).Table("more"))
	cp.WriteDisposition = bigquery.WriteAppend
	if job, err := cp.Run(h.Context()); err == nil {
		st, werr := job.Wait(h.Context())
		if werr == nil {
			werr = st.Err()
		}
		if werr == nil || !strings.Contains(werr.Error(), "generated column") {
			t.Errorf("bq copy -a into the table: %v", werr)
		}
	} else if !strings.Contains(err.Error(), "generated column") {
		t.Errorf("bq copy -a into the table: %v", err)
	}

	// Renaming the embedding column keeps its generation.
	if err := run(fmt.Sprintf("ALTER TABLE `%s.products` RENAME COLUMN description_embedding TO emb", ds)); err != nil {
		t.Fatalf("RENAME COLUMN of the embedding column: %v", err)
	}
	if err := run(fmt.Sprintf("INSERT INTO `%s.products` (name, description) VALUES ('Kite', 'A kite to fly on windy days')", ds)); err != nil {
		t.Fatalf("INSERT after the rename: %v", err)
	}
	waitEmbedded("emb", 4)
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT column_name FROM `%s`.INFORMATION_SCHEMA.COLUMNS WHERE table_name = 'products' AND is_generated = 'ALWAYS'", ds))
	if len(rows) != 1 || rows[0]["column_name"] != "emb" {
		t.Errorf("INFORMATION_SCHEMA.COLUMNS after the rename: %v", rows)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT base.name AS name FROM AI.SEARCH(TABLE `%s.products`, 'description', 'flying in the wind', top_k => 1)", ds))
	if len(rows) != 1 || rows[0]["name"] != "Kite" {
		t.Errorf("AI.SEARCH after the rename: %v", rows)
	}
	if err := run(fmt.Sprintf("UPDATE `%s.products` SET emb = NULL WHERE TRUE", ds)); err == nil || !strings.Contains(err.Error(), "generated column") {
		t.Errorf("writing the renamed column: %v", err)
	}

	// A blocking error: the connection's service account loses the
	// Vertex AI User role, so generation stalls with the page's message
	// in async_generation_status, and resumes, the status cleared, once
	// the role is granted again.
	cc, project := remoteConnectionClient(t, h)
	parts := strings.Split(conn, ".")
	cn, err := cc.GetConnection(h.Context(), &connectionpb.GetConnectionRequest{Name: "projects/" + project + "/locations/" + parts[1] + "/connections/" + parts[2]})
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	sa := "serviceAccount:" + cn.GetCloudResource().GetServiceAccountId()
	pc := remoteProjectsClient(t, h)
	if err := editProjectMember(h, pc, project, "roles/aiplatform.user", sa, false); err != nil {
		t.Fatalf("revoke roles/aiplatform.user: %v", err)
	}
	if err := run(fmt.Sprintf("INSERT INTO `%s.products` (name, description) VALUES ('Yo-yo', 'A spinning toy on a string')", ds)); err != nil {
		t.Fatalf("INSERT while blocked: %v", err)
	}
	status := fmt.Sprintf("SELECT async_generation_status.blocking_error.message AS m FROM `%s`.INFORMATION_SCHEMA.COLUMNS WHERE table_name = 'products' AND column_name = 'emb'", ds)
	deadline := time.Now().Add(time.Minute)
	for {
		rows = mlQuery(t, h, c, status)
		if len(rows) == 1 && strings.Contains(fmt.Sprint(rows[0]["m"]), "does not have the permission to access resources used by AI.EMBED") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no blocking error after the revoke: %v", rows)
		}
		time.Sleep(time.Second)
	}
	t.Logf("blocking_error: %v", rows[0]["m"])
	if rows := mlQuery(t, h, c, fmt.Sprintf("SELECT emb FROM `%s.products` WHERE name = 'Yo-yo'", ds)); len(rows) != 1 || rows[0]["emb"] != nil {
		t.Errorf("a row embedded while blocked: %v", rows)
	}
	if err := editProjectMember(h, pc, project, "roles/aiplatform.user", sa, true); err != nil {
		t.Fatalf("grant roles/aiplatform.user again: %v", err)
	}
	waitEmbedded("emb", 5)
	if rows := mlQuery(t, h, c, status); len(rows) != 1 || rows[0]["m"] != nil {
		t.Errorf("blocking error not cleared: %v", rows)
	}
}
