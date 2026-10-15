package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseAlter (#1227): the ALTER TABLE actions the front carries out are
// read with their columns' TableFieldSchemas and options; the rest are named
// for the 501, and a script with an ALTER TABLE is the interpreter's.
func TestParseAlter(t *testing.T) {
	toks, _ := lex("ALTER TABLE IF EXISTS ds.t ADD COLUMN IF NOT EXISTS a ARRAY<STRING> OPTIONS (description = 'd'), " +
		"ADD COLUMN s STRUCT<x INT64 NOT NULL, y ARRAY<NUMERIC(10, 2)> OPTIONS (description = 'why')>, ADD COLUMN c STRING(5), " +
		"SET OPTIONS (description = 'x', labels = [('k', 'v')]), ALTER COLUMN IF EXISTS b DROP NOT NULL, " +
		"ALTER COLUMN b SET OPTIONS (description = NULL)")
	st, unsupported, msg := parseAlter(toks)
	if unsupported != "" || msg != "" || !st.ifExists || strings.Join(st.path, ".") != "ds.t" || len(st.actions) != 6 {
		t.Fatalf("parseAlter = %+v %q %q", st, unsupported, msg)
	}
	got, _ := json.Marshal([]map[string]any{st.actions[0].field, st.actions[1].field, st.actions[2].field})
	want := `[{"mode":"REPEATED","name":"a","type":"STRING"},` +
		`{"fields":[{"mode":"REQUIRED","name":"x","type":"INTEGER"},{"description":"why","mode":"REPEATED","name":"y","precision":"10","scale":"2","type":"NUMERIC"}],"name":"s","type":"RECORD"},` +
		`{"maxLength":"5","name":"c","type":"STRING"}]`
	if string(got) != want {
		t.Errorf("fields =\n%s\nwant\n%s", got, want)
	}
	if !st.actions[0].ifExists || st.actions[0].description == nil || st.actions[3].kind != "options" ||
		len(st.actions[3].options) != 2 || st.actions[4].kind != "drop not null" || !st.actions[4].ifExists ||
		st.actions[5].kind != "column options" {
		t.Errorf("actions = %+v", st.actions)
	}
	for sql, want := range map[string]string{
		"ALTER TABLE ds.t RENAME TO u, SET OPTIONS (description = 'x')": "RENAME TO with other actions",
		"ALTER TABLE ds.t RENAME TO other.u":                            "RENAME TO a name with a dataset",
		"ALTER TABLE ds.t ADD COLUMN a INT64, DROP COLUMN b":            "DROP COLUMN with ADD COLUMN",
		"ALTER TABLE ds.t DROP COLUMN s.x":                              "nested field",
		"ALTER TABLE ds.t RENAME COLUMN s.x TO y":                       "nested field",
		"ALTER TABLE ds.t ALTER COLUMN a SET DATA TYPE NUMERIC":         "SET DATA TYPE",
		"ALTER TABLE ds.t ALTER COLUMN a SET DEFAULT 1":                 "DEFAULT",
		"ALTER TABLE ds.t SET OPTIONS (partition_expiration_days=1)":    "partition_expiration_days",
		"ALTER TABLE ds.t ADD COLUMN a INT64 DEFAULT 1":                 "after the column's type",
		"ALTER TABLE ds.t ADD PRIMARY KEY (a) NOT ENFORCED":             "constraint",
	} {
		toks, _ := lex(sql)
		if got := alterUnsupported(toks); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", sql, got, want)
		}
	}
	// RENAME TO, RENAME COLUMN and DROP COLUMN are carried out (#1322).
	toks, _ = lex("ALTER TABLE ds.t RENAME COLUMN IF EXISTS a TO b, DROP COLUMN IF EXISTS c, DROP COLUMN `d`")
	st, unsupported, msg = parseAlter(toks)
	if unsupported != "" || msg != "" || len(st.actions) != 3 || st.actions[0].kind != "rename column" ||
		!st.actions[0].ifExists || st.actions[0].column != "a" || st.actions[0].newName != "b" ||
		st.actions[1].kind != "drop column" || !st.actions[1].ifExists || st.actions[2].column != "d" {
		t.Errorf("parseAlter = %+v %q %q", st, unsupported, msg)
	}
	toks, _ = lex("ALTER TABLE ds.t RENAME TO u")
	if st, unsupported, msg = parseAlter(toks); unsupported != "" || msg != "" || st.actions[0].kind != "rename table" ||
		st.actions[0].newName != "u" {
		t.Errorf("parseAlter = %+v %q %q", st, unsupported, msg)
	}
	for _, sql := range []string{"ALTER TABLE ds.t RENAME TO", "ALTER TABLE ds.t RENAME COLUMN a b", "ALTER TABLE ds.t DROP COLUMN a b"} {
		toks, _ := lex(sql)
		if _, _, msg := parseAlter(toks); !strings.HasPrefix(msg, "Syntax error") {
			t.Errorf("%s: %q, want a syntax error", sql, msg)
		}
	}
	for _, sql := range []string{"ALTER TABLE ds.t ADD COLUMN a INT64", "SELECT 1; ALTER TABLE ds.t SET OPTIONS (description = 'x')"} {
		if !hasControlFlow(sql) {
			t.Errorf("%s is not carried out by the script interpreter", sql)
		}
	}
	if v := checkStatements("x", nil); v.code != 0 {
		t.Error(v)
	}
}
