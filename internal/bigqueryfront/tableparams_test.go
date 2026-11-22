package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestTableParameterNames (#2120): a table function's table parameters
// are read in its body as tables of no dataset, and a call's TABLE name is
// qualified as any table is.
func TestTableParameterNames(t *testing.T) {
	for q, want := range map[string]string{
		"CREATE TABLE FUNCTION ds.tf(t1 TABLE<year INT64, a ARRAY<STRUCT<x INT64>>>, y INT64) AS (SELECT * FROM t1 WHERE year > y)": "CREATE TABLE FUNCTION ds.tf(t1 TABLE<year INT64, a ARRAY<STRUCT<x INT64>>>, y INT64) AS (SELECT * FROM t1 WHERE year > y)",
		"CREATE TABLE FUNCTION ds.f(t ANY TABLE) AS (SELECT * FROM t JOIN other USING (k))":                                         "CREATE TABLE FUNCTION ds.f(t ANY TABLE) AS (SELECT * FROM t JOIN `dd.other` USING (k))",
		"SELECT * FROM ds.tf(TABLE years, 2020)":                                                 "SELECT * FROM ds.tf(TABLE `dd.years`, 2020)",
		"SELECT * FROM ds.tf(TABLE ds.years, 2020)":                                              "SELECT * FROM ds.tf(TABLE ds.years, 2020)",
		"SELECT * FROM ds.tf((SELECT year FROM years), 2020)":                                    "SELECT * FROM ds.tf((SELECT year FROM `dd.years`), 2020)",
		"CREATE TABLE FUNCTION ds.g(t1 TABLE<year INT64>) AS (SELECT * FROM ds.tf(TABLE t1, 1))": "CREATE TABLE FUNCTION ds.g(t1 TABLE<year INT64>) AS (SELECT * FROM ds.tf(TABLE t1, 1))",
	} {
		if got, _, msg := qualifyTables(q, "dd"); got != want || msg != "" {
			t.Errorf("qualifyTables(%s) = %s, %q; want %s", q, got, msg, want)
		}
	}
	if _, _, msg := qualifyTables("SELECT * FROM ds.tf(TABLE years, 2020)", ""); msg != `Table "years" must be qualified with a dataset (e.g. dataset.table).` {
		t.Errorf("TABLE years with no default dataset: %q", msg)
	}
}

// TestTableParameterArguments (#2120): a table function's TABLE<...>
// parameter is a FIXED_TABLE argument with its tableType, ANY TABLE an
// ANY_TABLE one, and both are written back as they were declared.
func TestTableParameterArguments(t *testing.T) {
	def := routineFromDDL("(t1 TABLE<year INT64, tags ARRAY<STRING>>, y INT64, t2 ANY TABLE) AS (SELECT * FROM t1)", true)
	got, _ := json.Marshal(def["arguments"])
	want := `[{"argumentKind":"FIXED_TABLE","name":"t1","tableType":{"columns":[{"name":"year","type":{"typeKind":"INT64"}},` +
		`{"name":"tags","type":{"arrayElementType":{"typeKind":"STRING"},"typeKind":"ARRAY"}}]}},` +
		`{"argumentKind":"FIXED_TYPE","dataType":{"typeKind":"INT64"},"name":"y"},{"argumentKind":"ANY_TABLE","name":"t2"}]`
	if string(got) != want {
		t.Errorf("arguments = %s\nwant %s", got, want)
	}
	var decls []string
	var raw struct{ Arguments []map[string]any }
	_ = json.Unmarshal([]byte(`{"arguments":`+string(got)+`}`), &raw)
	for _, a := range raw.Arguments {
		decls = append(decls, argumentType(a))
	}
	if g := decls; len(g) != 3 || g[0] != "TABLE<`year` INT64, `tags` ARRAY<STRING>>" || g[1] != "INT64" || g[2] != "ANY TABLE" {
		t.Errorf("argument types = %q", g)
	}
	if !tableArguments(map[string]any{"arguments": []any{map[string]any{"argumentKind": "ANY_TABLE"}}}) ||
		tableArguments(map[string]any{"arguments": []any{map[string]any{"argumentKind": "FIXED_TYPE"}}}) {
		t.Error("tableArguments")
	}
}
