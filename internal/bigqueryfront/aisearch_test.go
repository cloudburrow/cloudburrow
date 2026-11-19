package bigqueryfront

import (
	"strings"
	"testing"
)

func TestParseGeneratedColumns(t *testing.T) {
	const col = "e STRUCT<result ARRAY<FLOAT64>, status STRING> GENERATED ALWAYS AS (AI.EMBED(d, model => 'embeddinggemma-300m')) STORED OPTIONS (asynchronous = TRUE)"
	st, ok, code, msg := parseGeneratedColumns("CREATE TABLE ds.t (n STRING, d STRING, " + col + ")")
	if !ok || code != 0 || st.alter || len(st.cols) != 1 || st.cols[0].column != "e" || st.cols[0].expr != "AI.EMBED(d, model => 'embeddinggemma-300m')" {
		t.Fatalf("CREATE: %+v %v %d %s", st, ok, code, msg)
	}
	if st.text != "CREATE TABLE ds.t (n STRING, d STRING, e STRUCT<result ARRAY<FLOAT64>, status STRING> )" || st.columns["d"] != "STRING" {
		t.Errorf("CREATE text %q, columns %v", st.text, st.columns)
	}
	st, ok, code, _ = parseGeneratedColumns("ALTER TABLE ds.t ADD COLUMN IF NOT EXISTS " + col)
	if !ok || code != 0 || !st.alter || st.cols[0].column != "e" || strings.Contains(st.text, "GENERATED") {
		t.Errorf("ALTER: %+v %d", st, code)
	}
	for sql, want := range map[string]int{
		"SELECT 1": 0,
		"CREATE TABLE ds.t (" + strings.Replace(col, "STORED ", "", 1) + ")":                                               400,
		"CREATE TABLE ds.t (e INT64 GENERATED ALWAYS AS (AI.EMBED(d, model => 'm')) STORED OPTIONS (asynchronous = TRUE))": 400,
		"CREATE TABLE ds.t (d STRING, " + col + ", " + strings.Replace(col, "e STRUCT", "f STRUCT", 1) + ")":               400,
		"CREATE TABLE ds.t (d STRING, " + col + "); SELECT 1":                                                              501,
	} {
		_, _, code, _ := parseGeneratedColumns(sql)
		if code != want {
			t.Errorf("%s: %d, want %d", sql, code, want)
		}
	}
}

func TestFindAISearch(t *testing.T) {
	calls, code, msg := findAISearch("SELECT base.n, distance FROM AI.SEARCH(TABLE ds.t, 'd', 'toy', top_k => 3, distance_type => 'COSINE')")
	if code != 0 || len(calls) != 1 || calls[0].column != "d" || calls[0].value != "'toy'" || calls[0].named["top_k"] != "3" ||
		strings.Join(calls[0].table, ".") != "ds.t" {
		t.Fatalf("%+v %d %s", calls, code, msg)
	}
	calls, code, _ = findAISearch("SELECT * FROM AI.SEARCH((SELECT n FROM ds.t WHERE n > 'a'), 'd', @q)")
	if code != 0 || len(calls) != 1 || calls[0].query != "SELECT n FROM ds.t WHERE n > 'a'" || calls[0].query[calls[0].fromAt:calls[0].fromAt+4] != "FROM" {
		t.Fatalf("query form: %+v %d", calls, code)
	}
	for sql, want := range map[string]int{
		"SELECT * FROM AI.SEARCH(TABLE ds.t, 'd')":                                    400,
		"SELECT * FROM AI.SEARCH(TABLE ds.t, d, 'x')":                                 400,
		"SELECT * FROM AI.SEARCH(TABLE ds.t, 'd', 'x', nope => 1)":                    400,
		"SELECT * FROM AI.SEARCH((SELECT n FROM ds.t JOIN ds.u USING (k)), 'd', 'x')": 400,
	} {
		if _, code, _ := findAISearch(sql); code != want {
			t.Errorf("%s: %d, want %d", sql, code, want)
		}
	}
}

func TestRestFieldType(t *testing.T) {
	fl := map[string]any{"name": "e", "type": "RECORD", "fields": []any{
		map[string]any{"name": "result", "type": "FLOAT", "mode": "REPEATED"}, map[string]any{"name": "status", "type": "STRING"}}}
	if got := restFieldType(fl); got != embeddingColumnType {
		t.Errorf("%s, want %s", got, embeddingColumnType)
	}
}

func TestFindVectorSearch(t *testing.T) {
	calls, code, msg := findVectorSearch("SELECT * FROM VECTOR_SEARCH(TABLE ds.base, 'e', (SELECT id, q FROM ds.qt), 'q', top_k => 2, distance_type => 'COSINE')")
	if code != 0 || len(calls) != 1 || calls[0].column != "e" || calls[0].querySQL != "SELECT id, q FROM ds.qt" ||
		calls[0].named["query_column_to_search"] != "'q'" || calls[0].named["top_k"] != "2" || strings.Join(calls[0].table, ".") != "ds.base" {
		t.Fatalf("%+v %d %s", calls, code, msg)
	}
	calls, code, _ = findVectorSearch("SELECT * FROM VECTOR_SEARCH((SELECT id FROM ds.base WHERE id > 1), 'e', query_value => [1.0, 2.0], top_k => 1)")
	if code != 0 || len(calls) != 1 || calls[0].query != "SELECT id FROM ds.base WHERE id > 1" || calls[0].named["query_value"] != "[1.0, 2.0]" {
		t.Fatalf("single: %+v %d", calls, code)
	}
	calls, code, _ = findVectorSearch("SELECT * FROM VECTOR_SEARCH(TABLE ds.base, 'e', TABLE ds.qt)")
	if code != 0 || strings.Join(calls[0].queryTable, ".") != "ds.qt" {
		t.Fatalf("TABLE query: %+v %d", calls, code)
	}
	for _, sql := range []string{
		"SELECT * FROM VECTOR_SEARCH(TABLE ds.base, 'e')",
		"SELECT * FROM VECTOR_SEARCH(TABLE ds.base, 'e', TABLE ds.qt, query_value => [1.0])",
		"SELECT * FROM VECTOR_SEARCH(TABLE ds.base, 'e', TABLE ds.qt, bogus => 1)",
		"SELECT * FROM VECTOR_SEARCH((SELECT * FROM ds.a JOIN ds.b USING (id)), 'e', TABLE ds.qt)",
		"SELECT * FROM VECTOR_SEARCH(TABLE ds.base, e, TABLE ds.qt)",
	} {
		if _, code, _ := findVectorSearch(sql); code != 400 {
			t.Errorf("%s: %d, want 400", sql, code)
		}
	}
	if got := searchDistance("DOT_PRODUCT", "a", "v"); got != "(-(SELECT SUM(x * v[OFFSET(i)]) FROM UNNEST(a) AS x WITH OFFSET i))" {
		t.Errorf("DOT_PRODUCT: %s", got)
	}
	for _, o := range []string{`'{"use_brute_force": true, "fraction_lists_to_search": 0.5}'`, `'{"fraction_lists_to_search": 1}'`, `'{"reranker": "x"}'`, `'[]'`} {
		if checkSearchOptions(o, false, "VECTOR_SEARCH") == "" {
			t.Errorf("options %s accepted", o)
		}
	}
	if m := checkSearchOptions(`JSON '{"fraction_lists_to_search": 0.2}'`, false, "VECTOR_SEARCH"); m != "" {
		t.Errorf("fraction 0.2: %s", m)
	}
}

func TestVectorIndexStatement(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE VECTOR INDEX i ON ds.t(e) OPTIONS(index_type = 'IVF')":  true,
		"CREATE OR REPLACE VECTOR INDEX IF NOT EXISTS i ON ds.t(e)":     true,
		"DROP VECTOR INDEX i ON ds.t":                                   true,
		"ALTER VECTOR INDEX i ON ds.t SET OPTIONS (x = 1)":              true,
		"SELECT 'CREATE VECTOR INDEX' AS s":                             false,
		"SELECT * FROM ds.INFORMATION_SCHEMA.VECTOR_INDEXES":            false,
		"CREATE TABLE ds.t (x INT64); CREATE VECTOR INDEX i ON ds.t(e)": true,
	} {
		if got := vectorIndexStatement(sql) != ""; got != want {
			t.Errorf("%s: %v, want %v", sql, got, want)
		}
	}
}
