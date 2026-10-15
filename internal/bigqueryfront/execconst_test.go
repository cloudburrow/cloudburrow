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
		return func(probe string) (string, string, bool, int, string) {
			probes = append(probes, probe)
			return typ, value, null, code, "boom"
		}
	}
	sql := "DECLARE n INT64 DEFAULT 2; SET n = n + 1; EXECUTE IMMEDIATE FORMAT('SELECT %d AS x', n) INTO n; SELECT n"
	got, changed, code, msg := expandExecuteImmediate(sql, eval("SELECT 3 AS x", false, "STRING", 0))
	if !changed || code != 0 || !strings.Contains(got, "SET n = IF((SELECT COUNT(*) FROM (SELECT 3 AS x)) > 1") {
		t.Fatalf("%q %v %d %s", got, changed, code, msg)
	}
	if want := "DECLARE n INT64 DEFAULT 2; SET n = n + 1; SELECT (FORMAT('SELECT %d AS x', n))"; len(probes) != 1 || probes[0] != want {
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
		{"INSERT ds.t VALUES (1); EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)", "SELECT 1", false, "STRING", 0, 501},
		{"BEGIN EXECUTE IMMEDIATE FORMAT('SELECT %d', 1); END", "SELECT 1", false, "STRING", 0, 501},
		{"IF TRUE THEN EXECUTE IMMEDIATE FORMAT('SELECT %d', 1); END IF", "SELECT 1", false, "STRING", 0, 501},
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
