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
