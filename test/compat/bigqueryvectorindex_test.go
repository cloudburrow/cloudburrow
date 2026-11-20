//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// TestBigQueryVectorIndex (#1952): CREATE, ALTER and DROP VECTOR INDEX,
// the INFORMATION_SCHEMA vector index views, VECTOR_SEARCH through the
// index and hybrid search, through the official cloud.google.com/go/bigquery
// client (jobs.query, jobs.insert), and statistics.query.vectorSearchStatistics
// through google.golang.org/api/bigquery/v2 (jobs.get), as
// https://docs.cloud.google.com/bigquery/docs/vector-index, the DDL
// reference and the search functions page document them (read 2026-11-19):
//
//   - an index on a table under 10 MB is made but not populated:
//     VECTOR_INDEXES gives TEMPORARILY DISABLED and coverage 0, and a
//     search runs brute force, its job reporting UNUSED with
//     BASE_TABLE_TOO_SMALL; VECTOR_INDEX_COLUMNS and VECTOR_INDEX_OPTIONS
//     give its column and options; the documented refusals (one index per
//     table, options of the other index type, num_lists over 5,000,
//     leaf_node_embedding_count under 500, no index_type, a column that is
//     not ARRAY<FLOAT64>, IF NOT EXISTS with OR REPLACE) and DROP's;
//   - an IVF index on a 12 MB table is ACTIVE with coverage 100; an
//     indexed search reports FULLY_USED, probes only some lists (top_k =>
//     -1 returns one list's rows at fraction_lists_to_search 0.05), gives
//     brute force's results when every list is probed, and keeps a high
//     recall at the default fraction; use_brute_force reports
//     INDEX_SUPPRESSED_BY_FUNCTION_OPTION and is exact; a WHERE on a
//     stored column pre-filters (top_k rows), one on another column
//     post-filters (fewer);
//   - a TREE_AH index with normalization_type L2 and distance_type
//     DOT_PRODUCT is the search's default distance (a row's own distance
//     is -1); ALTER VECTOR INDEX REBUILD fills last_index_alteration_info;
//   - hybrid search, the page's example: lexical_search_columns over
//     description for "striped hunter" ranks tiger and lion, its distance
//     1 minus a reciprocal rank fusion score; a batch hybrid search fails.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryVectorIndex(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc, err := bq.NewService(h.Context(), option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_vi")
	if err := ds.Create(h.Context(), &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	d := ds.DatasetID
	run := func(sql string) error { return mlRun(h, c, sql) }
	must := func(sql string) {
		t.Helper()
		if err := run(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// usage runs sql and reads its job's vectorSearchStatistics.
	usage := func(sql string) (rows []map[string]bigquery.Value, mode, reason string) {
		t.Helper()
		job, err := c.Query(sql).Run(h.Context())
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		it, err := job.Read(h.Context())
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		for {
			row := map[string]bigquery.Value{}
			if err := it.Next(&row); err != nil {
				break
			}
			rows = append(rows, row)
		}
		got, err := svc.Jobs.Get(project, job.ID()).Location(job.Location()).Context(h.Context()).Do()
		if err != nil {
			t.Fatalf("jobs.get %s: %v", job.ID(), err)
		}
		if got.Statistics == nil || got.Statistics.Query == nil || got.Statistics.Query.VectorSearchStatistics == nil {
			t.Fatalf("%s: no vectorSearchStatistics", sql)
		}
		vs := got.Statistics.Query.VectorSearchStatistics
		if len(vs.IndexUnusedReasons) > 0 {
			reason = vs.IndexUnusedReasons[0].Code
		}
		return rows, vs.IndexUsageMode, reason
	}

	// The page's small table: the index is made, not populated.
	must(fmt.Sprintf("CREATE TABLE `%s.base_table` (id STRING, description STRING, my_embedding ARRAY<FLOAT64>)", d))
	must(fmt.Sprintf("INSERT `%s.base_table` (id, description, my_embedding) VALUES "+
		"('dog', \"Man's best friend\", [1.0, 2.0]), ('wolf', 'The ultimate pack hunter', [2.0, 4.0]), "+
		"('snake', 'The slithering serpent', [-2.0, 3.0]), ('lion', 'King of the jungle', [2.0, -2.5]), "+
		"('tiger', 'The striped stalker', [3.0, -2.0]), ('otter', \"The river's playful acrobat\", [-3.0, -1.0]), "+
		"('whale', \"The ocean's gentle giant\", [-5.0, -1.0])", d))
	must(fmt.Sprintf("CREATE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) STORING(id) "+
		"OPTIONS(index_type = 'IVF', distance_type = 'COSINE', ivf_options = '{\"num_lists\": 2}')", d))
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT table_name, index_name, index_status, coverage_percentage, ddl, last_refresh_time, disable_reason "+
		"FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEXES", d))
	if len(rows) != 1 || rows[0]["index_name"] != "small_index" || rows[0]["index_status"] != "TEMPORARILY DISABLED" || rows[0]["coverage_percentage"] != int64(0) ||
		rows[0]["last_refresh_time"] != nil || !strings.Contains(fmt.Sprint(rows[0]["ddl"]), "CREATE VECTOR INDEX `small_index`") ||
		!strings.Contains(fmt.Sprint(rows[0]["ddl"]), "index_type = 'IVF'") {
		t.Errorf("VECTOR_INDEXES of the small table: %v", rows)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT index_column_name, index_field_path FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEX_COLUMNS", d))
	if len(rows) != 1 || rows[0]["index_column_name"] != "my_embedding" || rows[0]["index_field_path"] != "my_embedding" {
		t.Errorf("VECTOR_INDEX_COLUMNS: %v", rows)
	}
	opts := map[string]string{}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT option_name, option_type, option_value FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEX_OPTIONS", d)) {
		opts[fmt.Sprint(r["option_name"])] = fmt.Sprint(r["option_type"]) + " " + fmt.Sprint(r["option_value"])
	}
	if opts["index_type"] != `STRING "IVF"` || opts["distance_type"] != `STRING "COSINE"` || !strings.Contains(opts["ivf_options"], "num_lists") || len(opts) != 3 {
		t.Errorf("VECTOR_INDEX_OPTIONS: %v", opts)
	}
	rows, mode, reason := usage(fmt.Sprintf("SELECT base.id AS id, distance FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', "+
		"query_value => [1.0, -1.0], top_k => 2) ORDER BY distance", d))
	if len(rows) != 2 || rows[0]["id"] != "lion" || !near(rows[0]["distance"], 1.8028, 0.0001) || mode != "UNUSED" || reason != "BASE_TABLE_TOO_SMALL" {
		t.Errorf("a search of the unpopulated index: %v %s %s", rows, mode, reason)
	}
	for sql, want := range map[string]string{
		fmt.Sprintf("CREATE VECTOR INDEX other ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'IVF')", d):                                                                                "only one vector index",
		fmt.Sprintf("CREATE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'IVF')", d):                                                                          "Already Exists",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX IF NOT EXISTS small_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'IVF')", d):                                                 "IF NOT EXISTS",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'TREE_AH', ivf_options = '{}')", d):                                       "ivf_options",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'IVF', ivf_options = '{\"num_lists\": 6000}')", d):                        "num_lists",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'TREE_AH', tree_ah_options = '{\"leaf_node_embedding_count\": 100}')", d): "leaf_node_embedding_count",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) OPTIONS(distance_type = 'COSINE')", d):                                                         "index_type",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX small_index ON `%s.base_table`(description) OPTIONS(index_type = 'IVF')", d):                                                                "ARRAY<FLOAT64>",
		fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX small_index ON `%s.base_table`(my_embedding) STORING(nope) OPTIONS(index_type = 'IVF')", d):                                                 "nope",
		fmt.Sprintf("DROP VECTOR INDEX other ON `%s.base_table`", d):                                                                                                                            "Not found",
	} {
		if err := run(sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}
	must(fmt.Sprintf("CREATE VECTOR INDEX IF NOT EXISTS small_index ON `%s.base_table`(my_embedding) OPTIONS(index_type = 'IVF')", d))

	// Hybrid search, the page's example.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT base.id AS id, distance FROM VECTOR_SEARCH(TABLE `%s.base_table`, \"my_embedding\", query_value => [1.0, -1.0], "+
		"lexical_search_columns => [\"description\"], lexical_search_query_value => \"striped hunter\", top_k => 2) ORDER BY distance", d))
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, fmt.Sprint(r["id"]))
		if f, _ := r["distance"].(float64); f < 0.96 || f > 0.97 {
			t.Errorf("hybrid distance %v, want 1 minus a reciprocal rank fusion score (about 0.967)", r)
		}
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "lion,tiger" {
		t.Errorf("hybrid search: %v, want tiger and lion", rows)
	}
	if err := run(fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', (SELECT [1.0, -1.0] AS my_embedding), "+
		"lexical_search_columns => ['description'], lexical_search_query_value => 'striped')", d)); err == nil || !strings.Contains(err.Error(), "batch") {
		t.Errorf("a batch hybrid search: %v", err)
	}
	if err := run(fmt.Sprintf("SELECT * FROM VECTOR_SEARCH(TABLE `%s.base_table`, 'my_embedding', query_value => [1.0, -1.0], "+
		"lexical_search_columns => ['my_embedding'], lexical_search_query_value => 'striped')", d)); err == nil || !strings.Contains(err.Error(), "STRING column") {
		t.Errorf("a hybrid search of a column that is not STRING: %v", err)
	}
	must(fmt.Sprintf("DROP VECTOR INDEX small_index ON `%s.base_table`", d))
	must(fmt.Sprintf("DROP VECTOR INDEX IF EXISTS small_index ON `%s.base_table`", d))
	if rows := mlQuery(t, h, c, fmt.Sprintf("SELECT index_name FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEXES", d)); len(rows) != 0 {
		t.Errorf("VECTOR_INDEXES after DROP: %v", rows)
	}

	// A 12 MB table of 20 clusters: the index is populated.
	// Its size is mostly a padding column, so brute force stays quick.
	must(fmt.Sprintf("CREATE TABLE `%s.big` AS SELECT id, MOD(id, 20) AS cat, REPEAT('x', 1000) AS pad, "+
		"ARRAY(SELECT MOD(id, 20) * 1.0 + 0.05 * SIN(id * 0.7 + j) FROM UNNEST(GENERATE_ARRAY(1, 8)) AS j ORDER BY j) AS e "+
		"FROM UNNEST(GENERATE_ARRAY(1, 11000)) AS id", d))
	must(fmt.Sprintf("CREATE VECTOR INDEX big_index ON `%s.big`(e) STORING(cat) OPTIONS(index_type = 'IVF', ivf_options = '{\"num_lists\": 20}')", d))
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT index_status, coverage_percentage, unindexed_row_count, last_refresh_time, last_model_build_time, total_logical_bytes "+
		"FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEXES WHERE table_name = 'big'", d))
	if len(rows) != 1 || rows[0]["index_status"] != "ACTIVE" || rows[0]["coverage_percentage"] != int64(100) || rows[0]["unindexed_row_count"] != int64(0) ||
		rows[0]["last_refresh_time"] == nil || rows[0]["last_model_build_time"] == nil {
		t.Fatalf("VECTOR_INDEXES of the 12 MB table: %v", rows)
	}
	queries := fmt.Sprintf("(SELECT id AS qid, e FROM `%s.big` WHERE id IN (1, 2, 3))", d)
	// results reads a batch search as each query's base ids.
	results := func(rows []map[string]bigquery.Value) map[string]map[string]bool {
		out := map[string]map[string]bool{}
		for _, r := range rows {
			q, _ := r["query"].(map[string]bigquery.Value)
			b, _ := r["base"].(map[string]bigquery.Value)
			k := fmt.Sprint(q["qid"])
			if out[k] == nil {
				out[k] = map[string]bool{}
			}
			out[k][fmt.Sprint(b["id"])] = true
		}
		return out
	}
	search := func(options string, topK int) string {
		return fmt.Sprintf("SELECT query, base, distance FROM VECTOR_SEARCH(TABLE `%s.big`, 'e', %s, 'e', top_k => %d, options => '%s')", d, queries, topK, options)
	}
	rows, mode, reason = usage(search(`{"use_brute_force": true}`, 10))
	exact := results(rows)
	if len(rows) != 30 || mode != "UNUSED" || reason != "INDEX_SUPPRESSED_BY_FUNCTION_OPTION" {
		t.Errorf("use_brute_force: %d rows, %s %s", len(rows), mode, reason)
	}
	rows, mode, _ = usage(search(`{}`, 10))
	approx := results(rows)
	if mode != "FULLY_USED" {
		t.Errorf("an indexed search: %s", mode)
	}
	hits, total := 0, 0
	for q, want := range exact {
		for id := range want {
			total++
			if approx[q][id] {
				hits++
			}
		}
	}
	if total == 0 || float64(hits)/float64(total) < 0.9 {
		t.Errorf("recall at the default fraction: %d of %d", hits, total)
	}
	rows, _, _ = usage(search(`{"fraction_lists_to_search": 0.99}`, 10))
	all := results(rows)
	for q, want := range exact {
		for id := range want {
			if !all[q][id] {
				t.Errorf("every list probed: query %s lacks %s (%v, want %v)", q, id, all[q], want)
			}
		}
	}
	// One list probed: top_k => -1 gives that list's rows, not the table's.
	rows, mode, _ = usage(fmt.Sprintf("SELECT base.id AS id FROM VECTOR_SEARCH(TABLE `%s.big`, 'e', (SELECT e FROM `%s.big` WHERE id = 1), 'e', top_k => -1, "+
		"options => '{\"fraction_lists_to_search\": 0.05}')", d, d))
	if len(rows) == 0 || len(rows) >= 11000 || mode != "FULLY_USED" {
		t.Errorf("one list probed, top_k -1: %d rows (%s), want one list's", len(rows), mode)
	}
	t.Logf("one list probed: %d of 11000 rows", len(rows))
	// A stored column pre-filters, another post-filters.
	pre := mlQuery(t, h, c, fmt.Sprintf("SELECT base.id AS id, base.cat AS cat FROM VECTOR_SEARCH((SELECT * FROM `%s.big` WHERE cat = 1), 'e', "+
		"(SELECT e FROM `%s.big` WHERE id = 1), 'e', top_k => 10)", d, d))
	if len(pre) != 10 {
		t.Errorf("a stored column's filter: %d rows, want 10: %v", len(pre), pre)
	}
	postRows := mlQuery(t, h, c, fmt.Sprintf("SELECT base.id AS id FROM VECTOR_SEARCH((SELECT * FROM `%s.big` WHERE id < 100), 'e', "+
		"(SELECT e FROM `%s.big` WHERE id = 1), 'e', top_k => 10)", d, d))
	bruteRows := mlQuery(t, h, c, fmt.Sprintf("SELECT base.id AS id FROM VECTOR_SEARCH((SELECT * FROM `%s.big` WHERE id < 100), 'e', "+
		"(SELECT e FROM `%s.big` WHERE id = 1), 'e', top_k => 10, options => '{\"use_brute_force\": true}')", d, d))
	if len(postRows) >= 10 || len(bruteRows) != 10 {
		t.Errorf("a post-filter: %d rows indexed (want fewer than 10), %d brute force (want 10)", len(postRows), len(bruteRows))
	}

	// TREE_AH, L2-normalized, DOT_PRODUCT by default; REBUILD.
	must(fmt.Sprintf("CREATE OR REPLACE VECTOR INDEX big_index ON `%s.big`(e) OPTIONS(index_type = 'TREE_AH', distance_type = 'DOT_PRODUCT', "+
		"tree_ah_options = '{\"leaf_node_embedding_count\": 500, \"normalization_type\": \"L2\"}')", d))
	rows, mode, _ = usage(fmt.Sprintf("SELECT base.id AS id, distance FROM VECTOR_SEARCH(TABLE `%s.big`, 'e', (SELECT e FROM `%s.big` WHERE id = 1), 'e', top_k => 3) "+
		"ORDER BY distance", d, d))
	if len(rows) != 3 || mode != "FULLY_USED" || !near(rows[0]["distance"], -1, 0.0001) {
		t.Errorf("TREE_AH, L2, DOT_PRODUCT: %v %s", rows, mode)
	}
	must(fmt.Sprintf("ALTER VECTOR INDEX big_index ON `%s.big` REBUILD", d))
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT index_status, last_index_alteration_info.ddl AS ddl, last_index_alteration_info.new_coverage_percentage AS cov "+
		"FROM `%s`.INFORMATION_SCHEMA.VECTOR_INDEXES WHERE table_name = 'big'", d))
	if len(rows) != 1 || rows[0]["index_status"] != "ACTIVE" || !strings.Contains(fmt.Sprint(rows[0]["ddl"]), "REBUILD") || rows[0]["cov"] != int64(100) {
		t.Errorf("after REBUILD: %v", rows)
	}
	if err := run(fmt.Sprintf("ALTER VECTOR INDEX nope ON `%s.big` REBUILD", d)); err == nil || !strings.Contains(err.Error(), "Not found") {
		t.Errorf("REBUILD of another index: %v", err)
	}
	must(fmt.Sprintf("DROP VECTOR INDEX big_index ON `%s.big`", d))
}
