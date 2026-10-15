package bigqueryfront

import (
	"strings"
	"testing"
)

// TestExecuteImmediateOfAConstant (#1037): SQL given by a constant
// expression (CONCAT or || of literals and known variables) is carried
// out; one that reads a value the script computes stays 501.
func TestExecuteImmediateOfAConstant(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"EXECUTE IMMEDIATE CONCAT('SELECT ', '1 AS a')", "SELECT 1 AS a"},
		{"EXECUTE IMMEDIATE 'SELECT ' || '2'", "SELECT 2"},
		{"DECLARE t STRING DEFAULT 'ds.t'; EXECUTE IMMEDIATE ('SELECT * FROM ' || t)", "DECLARE t STRING DEFAULT 'ds.t'; SELECT * FROM ds.t"},
		{"DECLARE q STRING DEFAULT CONCAT('SELECT ', \"3\"); EXECUTE IMMEDIATE q", "DECLARE q STRING DEFAULT CONCAT('SELECT ', \"3\"); SELECT 3"},
		// INTO (#1037).
		{"DECLARE a INT64; EXECUTE IMMEDIATE 'SELECT @y' INTO a USING 1 AS y", "DECLARE a INT64; SET a = IF((SELECT COUNT(*) FROM (SELECT (1))) > 1, " +
			"ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row'), (SELECT (1)))"},
		// INTO several variables, one SET per variable (#1037), and a guard
		// that runs at once when a later statement gives the answer.
		{"DECLARE a, b INT64; EXECUTE IMMEDIATE 'SELECT 1, 2' INTO a, b", "DECLARE a, b INT64; " +
			"SET a = IF((SELECT COUNT(*) FROM (SELECT 1, 2)) > 1, ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row'), " +
			"(SELECT AS STRUCT * FROM (SELECT 1, 2))[OFFSET(0)]); " +
			"SET b = IF((SELECT COUNT(*) FROM (SELECT 1, 2)) > 1, ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row'), " +
			"(SELECT AS STRUCT * FROM (SELECT 1, 2))[OFFSET(1)])"},
		{"DECLARE a INT64; EXECUTE IMMEDIATE 'SELECT 1' INTO a; SELECT 2", "DECLARE a INT64; " +
			"CREATE TEMP TABLE _cloudburrow_into_1 AS SELECT IF((SELECT COUNT(*) FROM (SELECT 1)) > 1, " +
			"ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row'), 0) AS ok; " +
			"SET a = IF((SELECT COUNT(*) FROM (SELECT 1)) > 1, ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row'), " +
			"(SELECT 1)); SELECT 2"},
	} {
		got, changed, code, msg := expandExecuteImmediate(c.sql, nil)
		if !changed || code != 0 || got != c.want {
			t.Errorf("%s: %q %v %d %s", c.sql, got, changed, code, msg)
		}
	}
	for _, sql := range []string{
		"EXECUTE IMMEDIATE CONCAT('SELECT ', CAST(1 AS STRING))",
		"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT ' || x",
		"EXECUTE IMMEDIATE 'INSERT ds.t VALUES (1)' INTO a",
		"EXECUTE IMMEDIATE 'SELECT 1' INTO a + 1",
		"DECLARE a INT64; EXECUTE IMMEDIATE 'SELECT 1, 2' INTO a, `select`",
		// The numbers of variables and columns differ (#1037).
		"DECLARE a, b INT64; EXECUTE IMMEDIATE 'SELECT 1, 2, 3' INTO a, b",
		"DECLARE a, b INT64; EXECUTE IMMEDIATE 'SELECT 1' INTO a, b",
	} {
		if _, _, code, _ := expandExecuteImmediate(sql, nil); code != 501 {
			t.Errorf("%s: %d, want 501", sql, code)
		}
	}
}

// TestExecuteImmediateQuotedInto (#1037): a quoted INTO variable that
// need not be quoted is SET unquoted.
func TestExecuteImmediateQuotedInto(t *testing.T) {
	got, changed, code, msg := expandExecuteImmediate("DECLARE a, b INT64; EXECUTE IMMEDIATE 'SELECT 1, 2' INTO `a`, `b`", nil)
	if !changed || code != 0 || !strings.Contains(got, "SET a = ") || !strings.Contains(got, "SET b = ") || strings.Contains(got, "`") {
		t.Fatalf("%q %v %d %s", got, changed, code, msg)
	}
}

// TestExecuteImmediateEvaluated (#1037): SQL from a computed expression is
// evaluated first, after the statements before it, when that is faithful.
func TestExecuteImmediateEvaluated(t *testing.T) {
	var probes []string
	eval := func(value string, null bool, typ string, code int) immediateEval {
		return func(probe string) (string, string, string, bool, int, string) {
			probes = append(probes, probe)
			return "_cloudburrow_sql", typ, value, null, code, "boom"
		}
	}
	sql := "DECLARE n INT64 DEFAULT 2; SET n = n + 1; EXECUTE IMMEDIATE FORMAT('SELECT %d AS x', n) INTO n; SELECT n"
	got, changed, code, msg := expandExecuteImmediate(sql, eval("SELECT 3 AS x", false, "STRING", 0))
	if !changed || code != 0 || !strings.Contains(got, "SET n = IF((SELECT COUNT(*) FROM (SELECT 3 AS x)) > 1") {
		t.Fatalf("%q %v %d %s", got, changed, code, msg)
	}
	if want := "DECLARE _cloudburrow_nop INT64; DECLARE n INT64 DEFAULT 2; SET n = n + 1; SELECT (FORMAT('SELECT %d AS x', n)) AS _cloudburrow_sql"; len(probes) != 1 || probes[0] != want {
		t.Fatalf("probes %q, want %q", probes, want)
	}
	// A variable set by an expression is evaluated too.
	if got, _, code, _ := expandExecuteImmediate("DECLARE q STRING DEFAULT CAST(1 AS STRING); EXECUTE IMMEDIATE q",
		eval("SELECT 1", false, "STRING", 0)); code != 0 || got != "DECLARE q STRING DEFAULT CAST(1 AS STRING); SELECT 1" {
		t.Fatalf("%q %d", got, code)
	}
	for _, c := range []struct {
		sql   string
		value string
		null  bool
		typ   string
		ecode int
		want  int
	}{
		{"EXECUTE IMMEDIATE CAST(NULL AS STRING)", "", true, "STRING", 0, 400},
		{"EXECUTE IMMEDIATE 1 + 1", "2", false, "INTEGER", 0, 400},
		{"EXECUTE IMMEDIATE CONCAT('SELECT ', nope)", "", false, "", 400, 400},
		// Not faithful to evaluate first: 501, nothing evaluated.
		{"INSERT ds.t VALUES (1); SET n = (SELECT COUNT(*) FROM ds.t); EXECUTE IMMEDIATE FORMAT('SELECT %d', n)", "SELECT 1", false, "STRING", 0, 501},
		{"INSERT ds.t VALUES (1); EXECUTE IMMEDIATE (SELECT MAX(s) FROM ds.t)", "SELECT 1", false, "STRING", 0, 501},
		{"BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN SELECT 2; END; EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)", "x", false, "STRING", 0, 501},
		{"BEGIN EXECUTE IMMEDIATE FORMAT('SELECT %d', 1); EXCEPTION WHEN ERROR THEN SELECT 2; END", "x", false, "STRING", 0, 501},
		{"DECLARE i INT64 DEFAULT 0; WHILE i < 2 DO SET i = i + 1; EXECUTE IMMEDIATE FORMAT('SELECT %d', i); END WHILE", "x", false, "STRING", 0, 501},
		{"LOOP EXECUTE IMMEDIATE (SELECT MAX(s) FROM ds.t); BREAK; END LOOP", "x", false, "STRING", 0, 501},
		{"DECLARE n INT64; LOOP EXECUTE IMMEDIATE FORMAT('SELECT %d', 1) INTO n; BREAK; END LOOP", "x", false, "STRING", 0, 501},
		{"CALL ds.p(); EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)", "x", false, "STRING", 0, 501},
		{"FOR r IN (SELECT 1 AS a) DO EXECUTE IMMEDIATE FORMAT('SELECT %d', r.a); END FOR", "x", false, "STRING", 0, 501},
		{"DECLARE t DEFAULT CURRENT_TIMESTAMP(); EXECUTE IMMEDIATE FORMAT('SELECT %t', t)", "x", false, "STRING", 0, 501},
		{"EXECUTE IMMEDIATE FORMAT('SELECT %d', CAST(RAND() * 10 AS INT64))", "x", false, "STRING", 0, 501},
		{"EXECUTE IMMEDIATE ds.make_sql()", "x", false, "STRING", 0, 501},
		{"EXECUTE IMMEDIATE FORMAT('SELECT %s', @@dataset_id)", "x", false, "STRING", 0, 501},
	} {
		probes = nil
		_, _, code, _ := expandExecuteImmediate(c.sql, eval(c.value, c.null, c.typ, c.ecode))
		if code != c.want {
			t.Errorf("%s: %d, want %d", c.sql, code, c.want)
		}
		if c.want == 501 && len(probes) > 0 {
			t.Errorf("%s: evaluated %q", c.sql, probes)
		}
	}
}

// TestExecuteImmediateHidesVariables (#1037): BigQuery does not show the
// script's variables to the statement EXECUTE IMMEDIATE runs, so a
// statement that names one is 501; a name only as an alias, a field, a
// parameter or a function, or a variable passed with USING, is run.
func TestExecuteImmediateHidesVariables(t *testing.T) {
	for _, c := range []struct {
		sql string
		run bool
	}{
		{"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT x'", false},
		{"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT `x` + 1'", false},
		{"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT @x' USING x AS x", true},
		{"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT 1 AS x'", true},
		{"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT s.x FROM (SELECT STRUCT(1 AS x) AS s)'", true},
		{"DECLARE abs INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT abs(-2)'", true},
		{"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT \"x\"'", true},
		{"EXECUTE IMMEDIATE 'SELECT x FROM t'", true},
	} {
		_, changed, code, msg := expandExecuteImmediate(c.sql, nil)
		if c.run && (code != 0 || !changed) || !c.run && code != 501 {
			t.Errorf("%s: changed %v, %d %s", c.sql, changed, code, msg)
		}
	}
}

// TestExecuteImmediateEvaluatedInBlocks (#1037): computed SQL in BEGIN ...
// END, and after DML and DDL, is evaluated by a probe that runs what is
// faithful to run again; IF and loops, which the emulator does not run,
// are 501.
func TestExecuteImmediateEvaluatedInBlocks(t *testing.T) {
	const nop = "DECLARE _cloudburrow_nop INT64; "
	for _, c := range []struct{ sql, probe, want string }{
		{"DECLARE n INT64 DEFAULT 1; BEGIN SELECT 0; BEGIN EXECUTE IMMEDIATE FORMAT('SELECT %d', n); END; SELECT 2; END",
			nop + "DECLARE n INT64 DEFAULT 1; SET _cloudburrow_nop = 0; SELECT (FORMAT('SELECT %d', n)) AS _cloudburrow_sql",
			"DECLARE n INT64 DEFAULT 1; BEGIN SELECT 0; BEGIN SELECT 1; END; SELECT 2; END"},
		{"DECLARE n INT64 DEFAULT 1; BEGIN SELECT 0; END; EXECUTE IMMEDIATE FORMAT('SELECT %d', n)",
			nop + "DECLARE n INT64 DEFAULT 1; SET _cloudburrow_nop = 0; SELECT (FORMAT('SELECT %d', n)) AS _cloudburrow_sql",
			"DECLARE n INT64 DEFAULT 1; BEGIN SELECT 0; END; SELECT 1"},
		{"DECLARE n INT64 DEFAULT 1; INSERT ds.t VALUES (1); CREATE TABLE ds.u (x INT64); EXECUTE IMMEDIATE FORMAT('SELECT %d', n)",
			nop + "DECLARE n INT64 DEFAULT 1; SET _cloudburrow_nop = 0; SET _cloudburrow_nop = 0; SELECT (FORMAT('SELECT %d', n)) AS _cloudburrow_sql",
			"DECLARE n INT64 DEFAULT 1; INSERT ds.t VALUES (1); CREATE TABLE ds.u (x INT64); SELECT 1"},
	} {
		var probes []string
		eval := func(p string) (string, string, string, bool, int, string) {
			probes = append(probes, p)
			return "_cloudburrow_sql", "STRING", "SELECT 1", false, 0, ""
		}
		got, _, code, msg := expandExecuteImmediate(c.sql, eval)
		if code != 0 || got != c.want {
			t.Errorf("%s:\n got %q %d %s\nwant %q", c.sql, got, code, msg, c.want)
		}
		if len(probes) != 1 || probes[0] != c.probe {
			t.Errorf("%s: probes\n %q\nwant %q", c.sql, probes, c.probe)
		}
	}
	any := func(string) (string, string, string, bool, int, string) {
		return "_cloudburrow_sql", "STRING", "SELECT 1", false, 0, ""
	}
	for _, sql := range []string{
		"IF TRUE THEN EXECUTE IMMEDIATE FORMAT('SELECT %d', 1); END IF",
		"DECLARE i INT64 DEFAULT 0; WHILE i < 2 DO SET i = i + 1; EXECUTE IMMEDIATE FORMAT('SELECT %d', 1); END WHILE",
		"LOOP EXECUTE IMMEDIATE FORMAT('SELECT %d', 1); END LOOP",
		"IF TRUE THEN SELECT 1; END IF; EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)",
		"INSERT ds.t VALUES (1); SET n = (SELECT COUNT(*) FROM ds.t); EXECUTE IMMEDIATE FORMAT('SELECT %d', n)",
		"INSERT ds.t VALUES (1); EXECUTE IMMEDIATE (SELECT MAX(s) FROM ds.t)",
		"BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN SELECT 2; END; EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)",
		"CALL ds.p(); EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)",
		"BEGIN TRANSACTION; EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)",
	} {
		if _, _, code, _ := expandExecuteImmediate(sql, any); code != 501 {
			t.Errorf("%s: %d, want 501", sql, code)
		}
	}
	// A probe that fails after DDL it left out is 501, not its error.
	fail := func(string) (string, string, string, bool, int, string) { return "", "", "", false, 400, "no f" }
	if _, _, code, _ := expandExecuteImmediate("CREATE TEMP FUNCTION f() AS ('SELECT 1'); EXECUTE IMMEDIATE f()", fail); code != 501 {
		t.Errorf("a failed probe after DDL: %d", code)
	}
}

// TestExecuteImmediateIntoCountsRows (#1037): an INTO with no later
// statement to put the guard before has its rows counted first: more
// than one fails the script at the statement, and otherwise nothing
// changes.
func TestExecuteImmediateIntoCountsRows(t *testing.T) {
	count := func(n string, probes *[]string) immediateEval {
		return func(p string) (string, string, string, bool, int, string) {
			*probes = append(*probes, p)
			return "_cloudburrow_sql", "INTEGER", n, false, 0, ""
		}
	}
	sql := "DECLARE x INT64; SELECT 7; EXECUTE IMMEDIATE 'SELECT n FROM UNNEST([1, 2]) AS n' INTO x"
	var probes []string
	got, _, code, _ := expandExecuteImmediate(sql, count("2", &probes))
	if code != 0 || got != "DECLARE x INT64; SELECT 7; SELECT ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row')" {
		t.Errorf("two rows: %q %d", got, code)
	}
	if want := "DECLARE _cloudburrow_nop INT64; DECLARE x INT64; SET _cloudburrow_nop = 0; " +
		"SELECT ((SELECT COUNT(*) FROM (SELECT n FROM UNNEST([1, 2]) AS n))) AS _cloudburrow_sql"; len(probes) != 1 || probes[0] != want {
		t.Errorf("probes %q, want %q", probes, want)
	}
	got, _, code, _ = expandExecuteImmediate(sql, count("1", &probes))
	if code != 0 || strings.Contains(got, "; SELECT ERROR(") ||
		!strings.Contains(got, "SET x = ") {
		t.Errorf("one row: %q %d", got, code)
	}
	// Counting it first would not be faithful: 501.
	if _, _, code, _ := expandExecuteImmediate("DECLARE x INT64; INSERT ds.t VALUES (1); EXECUTE IMMEDIATE 'SELECT x FROM ds.t' INTO x",
		count("1", &probes)); code != 501 {
		t.Errorf("after DML: %d, want 501", code)
	}
	// A later statement: the guard goes before it, and nothing is counted.
	probes = nil
	if got, _, code, _ := expandExecuteImmediate(sql+"; SELECT x", count("2", &probes)); code != 0 || len(probes) != 0 ||
		!strings.Contains(got, "CREATE TEMP TABLE _cloudburrow_into_1") {
		t.Errorf("a later statement: %q %d %q", got, code, probes)
	}
}
