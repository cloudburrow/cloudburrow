package bigqueryfront

import (
	"encoding/json"
	"fmt"
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
	// #1409: ADD COLUMN's rounding_mode, and a STRUCT field's.
	toks, _ = lex("ALTER TABLE ds.t ADD COLUMN c NUMERIC(4, 1) OPTIONS (description = 'd', rounding_mode = 'ROUND_HALF_EVEN'), " +
		"ADD COLUMN s ARRAY<STRUCT<x BIGNUMERIC(3) OPTIONS (rounding_mode = 'ROUND_HALF_AWAY_FROM_ZERO')>>")
	if st, unsupported, msg := parseAlter(toks); unsupported != "" || msg != "" || len(st.actions) != 2 ||
		st.actions[0].field["roundingMode"] != "ROUND_HALF_EVEN" || st.actions[0].field["scale"] != "1" || st.actions[0].description == nil ||
		st.actions[1].field["fields"].([]any)[0].(map[string]any)["roundingMode"] != "ROUND_HALF_AWAY_FROM_ZERO" {
		t.Errorf("ADD COLUMN with rounding_mode: %+v, %q, %q", st.actions, unsupported, msg)
	}
	for sql, want := range map[string]string{
		"ALTER TABLE ds.t ALTER COLUMN a SET DATA TYPE ARRAY<INT64>":        "to an ARRAY",
		"ALTER TABLE ds.t SET OPTIONS (enable_fine_grained_mutations=true)": "enable_fine_grained_mutations",
		"ALTER TABLE ds.t ADD COLUMN a INT64 OPTIONS (x = 1) y":             "after the column's type",
	} {
		toks, _ := lex(sql)
		if got := alterUnsupported(toks); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", sql, got, want)
		}
	}
	// #1389: what BigQuery refuses is 400; actions of different ways are
	// carried out a run at a time.
	for sql, want := range map[string]string{
		"ALTER TABLE ds.t RENAME TO other.u":                                "not a path",
		"ALTER TABLE ds.t DROP COLUMN s.x":                                  "top-level column",
		"ALTER TABLE ds.t RENAME COLUMN s.x TO y":                           "top-level column",
		"ALTER TABLE ds.t ALTER COLUMN s.x SET DATA TYPE NUMERIC":           "top-level column",
		"ALTER TABLE ds.t ALTER COLUMN s.x SET OPTIONS (description = 'd')": "top-level column",
		"ALTER TABLE ds.t RENAME COLUMN a TO b, ADD COLUMN c INT64":         "RENAME COLUMN cannot be used with other",
		// #1409: rounding_mode, BigQuery's two modes, on a NUMERIC or BIGNUMERIC.
		"ALTER TABLE ds.t ADD COLUMN c NUMERIC(2) OPTIONS (rounding_mode = 'UP')":                    "Invalid value for rounding_mode",
		"ALTER TABLE ds.t ADD COLUMN c INT64 OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')":            "not a NUMERIC or BIGNUMERIC",
		"ALTER TABLE ds.t ADD COLUMN c STRUCT<x STRING OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')>": "not a NUMERIC or BIGNUMERIC",
	} {
		toks, _ := lex(sql)
		if _, unsupported, msg := parseAlter(toks); unsupported != "" || !strings.Contains(msg, want) {
			t.Errorf("%s: %q %q, want 400 %q", sql, unsupported, msg, want)
		}
	}
	toks, _ = lex("ALTER TABLE ds.t ADD COLUMN a INT64, DROP COLUMN b, ALTER COLUMN c SET DATA TYPE NUMERIC, " +
		"RENAME TO u, SET OPTIONS (description = 'x'), ALTER COLUMN s SET DATA TYPE STRUCT<x NUMERIC>")
	st, unsupported, msg = parseAlter(toks)
	if unsupported != "" || msg != "" {
		t.Fatalf("parseAlter = %q %q", unsupported, msg)
	}
	var ways []string
	for _, g := range alterGroups(st.actions) {
		ways = append(ways, fmt.Sprintf("%s*%d", actionGroup(g[0].kind), len(g)))
	}
	if got := strings.Join(ways, " "); got != "patch*1 remake*2 rename table*1 patch*1 remake*1" {
		t.Errorf("groups = %s", got)
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
	// SET DEFAULT and DROP DEFAULT are carried out; ADD COLUMN ... DEFAULT
	// is refused, as BigQuery refuses it (#1340).
	toks, _ = lex("ALTER TABLE ds.t ALTER COLUMN IF EXISTS a SET DEFAULT CURRENT_TIMESTAMP(), ALTER COLUMN b DROP DEFAULT, " +
		"ALTER COLUMN c SET DEFAULT NULL, ADD COLUMN d INT64")
	if st, unsupported, msg = parseAlter(toks); unsupported != "" || msg != "" || len(st.actions) != 4 ||
		st.actions[0].kind != "set default" || !st.actions[0].ifExists || len(st.actions[0].options[0].val) != 3 ||
		st.actions[1].kind != "drop default" || st.actions[2].kind != "drop default" {
		t.Errorf("parseAlter = %+v %q %q", st, unsupported, msg)
	}
	for sql, want := range map[string]string{
		"ALTER TABLE ds.t ADD COLUMN a INT64 DEFAULT 1":  "Cannot add column a with a default value",
		"ALTER TABLE ds.t ALTER COLUMN a SET DEFAULT":    "Syntax error: SET DEFAULT without an expression",
		"ALTER TABLE ds.t ALTER COLUMN a DROP DEFAULT 1": "Syntax error",
	} {
		toks, _ := lex(sql)
		if _, unsupported, msg := parseAlter(toks); unsupported != "" || !strings.HasPrefix(msg, want) {
			t.Errorf("%s: %q %q, want %q", sql, unsupported, msg, want)
		}
	}
	// SET DATA TYPE and keys are carried out (#1334).
	toks, _ = lex("ALTER TABLE ds.t ALTER COLUMN a SET DATA TYPE NUMERIC(12, 2), RENAME COLUMN b TO c")
	if st, unsupported, msg = parseAlter(toks); unsupported != "" || msg != "" || len(st.actions) != 2 ||
		st.actions[0].kind != "set type" || st.actions[0].field["precision"] != "12" || st.actions[0].field["scale"] != "2" {
		t.Errorf("parseAlter = %+v %q %q", st, unsupported, msg)
	}
	toks, _ = lex("ALTER TABLE ds.t ADD PRIMARY KEY (a, b) NOT ENFORCED, ADD CONSTRAINT IF NOT EXISTS fk FOREIGN KEY (x) " +
		"REFERENCES ds.p (id) NOT ENFORCED, ADD FOREIGN KEY (y) REFERENCES p(id) NOT ENFORCED, DROP PRIMARY KEY IF EXISTS, " +
		"DROP CONSTRAINT IF EXISTS fk, ADD COLUMN z INT64")
	st, unsupported, msg = parseAlter(toks)
	if unsupported != "" || msg != "" || len(st.actions) != 6 {
		t.Fatalf("parseAlter = %+v %q %q", st, unsupported, msg)
	}
	if a := st.actions[0]; a.kind != "add primary key" || strings.Join(a.columns, ",") != "a,b" {
		t.Errorf("primary key = %+v", a)
	}
	if a := st.actions[1]; a.kind != "add foreign key" || a.newName != "fk" || !a.ifExists || strings.Join(a.refPath, ".") != "ds.p" ||
		strings.Join(a.refColumns, ",") != "id" || strings.Join(a.columns, ",") != "x" {
		t.Errorf("foreign key = %+v", a)
	}
	if a := st.actions[2]; a.kind != "add foreign key" || a.newName != "" || strings.Join(a.refPath, ".") != "p" {
		t.Errorf("unnamed foreign key = %+v", a)
	}
	if st.actions[3].kind != "drop primary key" || !st.actions[3].ifExists || st.actions[4].kind != "drop constraint" ||
		st.actions[4].newName != "fk" || !st.actions[4].ifExists {
		t.Errorf("drops = %+v", st.actions[3:5])
	}
	for _, sql := range []string{"ALTER TABLE ds.t ADD PRIMARY KEY (a)", "ALTER TABLE ds.t ADD FOREIGN KEY (a) REFERENCES p (id)",
		"ALTER TABLE ds.t ADD PRIMARY KEY () NOT ENFORCED", "ALTER TABLE ds.t DROP CONSTRAINT",
		"ALTER TABLE ds.t ALTER COLUMN a SET DATA TYPE NUMERIC STRING"} {
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

// TestCoercion (#1334): SET DATA TYPE takes BigQuery's documented
// widenings only.
func TestCoercion(t *testing.T) {
	f := func(typ string, kv ...string) map[string]any {
		m := map[string]any{"type": typ}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		old, next map[string]any
		want      int
	}{
		{f("INTEGER"), f("NUMERIC"), 0},
		{f("INTEGER"), f("BIGNUMERIC"), 0},
		{f("INTEGER"), f("FLOAT"), 0},
		{f("NUMERIC"), f("BIGNUMERIC"), 0},
		{f("NUMERIC"), f("FLOAT"), 0},
		{f("STRING", "maxLength", "10"), f("STRING", "maxLength", "20"), 0},
		{f("STRING", "maxLength", "10"), f("STRING"), 0},
		{f("BYTES", "maxLength", "4"), f("BYTES", "maxLength", "8"), 0},
		{f("NUMERIC", "precision", "10", "scale", "2"), f("NUMERIC", "precision", "12", "scale", "3"), 0},
		{f("NUMERIC", "precision", "10", "scale", "2"), f("NUMERIC"), 0},
		{f("INTEGER"), f("INTEGER"), 0},
		{f("STRING", "maxLength", "10"), f("STRING", "maxLength", "5"), 400},
		{f("STRING"), f("STRING", "maxLength", "5"), 400},
		{f("NUMERIC", "precision", "10", "scale", "2"), f("NUMERIC", "precision", "10", "scale", "3"), 400},
		{f("NUMERIC"), f("NUMERIC", "precision", "38", "scale", "9"), 400},
		{f("FLOAT"), f("NUMERIC"), 400},
		{f("NUMERIC"), f("INTEGER"), 400},
		{f("INTEGER"), f("STRING"), 400},
		{f("BIGNUMERIC"), f("NUMERIC"), 400},
		{f("INTEGER"), f("NUMERIC", "precision", "20"), 501},
	} {
		if got, msg := coercion(c.old, c.next); got != c.want {
			t.Errorf("%v to %v: %d %q, want %d", c.old, c.next, got, msg, c.want)
		}
	}
}

// TestDefaultChange (#1340): a schema update that changes a column's
// default makes the table again; a column added with one is refused.
func TestDefaultChange(t *testing.T) {
	old := []field{{Name: "id", Type: "INTEGER"}, {Name: "s", Type: "STRING", DefaultValueExpression: "'x'"}}
	for _, c := range []struct {
		next    []field
		changed bool
		refused bool
	}{
		{[]field{{Name: "id", Type: "INTEGER"}, {Name: "S", Type: "STRING", DefaultValueExpression: "'x'"}}, false, false},
		{[]field{{Name: "id", Type: "INTEGER"}, {Name: "s", Type: "STRING", DefaultValueExpression: "'y'"}}, true, false},
		{[]field{{Name: "id", Type: "INTEGER", DefaultValueExpression: "1"}, {Name: "s", Type: "STRING"}}, true, false},
		{append(append([]field{}, old...), field{Name: "n", Type: "INTEGER"}), false, false},
		{append(append([]field{}, old...), field{Name: "n", Type: "INTEGER", DefaultValueExpression: "7"}), false, true},
	} {
		changed, refused := defaultChange(old, c.next)
		if changed != c.changed || (refused != "") != c.refused {
			t.Errorf("defaultChange(%+v) = %v %q, want %v %v", c.next, changed, refused, c.changed, c.refused)
		}
	}
}
