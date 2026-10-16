package bigqueryfront

import (
	"regexp"
	"strings"
	"testing"
)

// TestTempSessionScript (#1434): a script with a TEMP table or function
// and a materialized view statement is the text it amounts to, each TEMP
// table and function one of the hidden dataset, named where a table is
// named (AS its own name) and where the function is called; the child
// jobs' texts are restored.
func TestTempSessionScript(t *testing.T) {
	sql := "CREATE TEMP TABLE t AS SELECT 1 AS a;\n" +
		"CREATE TEMP FUNCTION twice(x INT64) AS (x * 2);\n" +
		"INSERT INTO t (a) VALUES (2);\n" +
		"INSERT INTO ds.base SELECT twice(t.a) AS a FROM t WHERE a > 0;\n" +
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT SUM(a) AS s FROM ds.base;\n" +
		"UPDATE t SET a = 3 WHERE TRUE;\n" +
		"SELECT x.a, t FROM ds.base x JOIN _SESSION.t USING (a), t AS u;\n" +
		"DROP TABLE t"
	s, ok := tempSessionScript(sql)
	if !ok {
		t.Fatal("not read as a session's script")
	}
	h := regexp.MustCompile("`_cloudburrow_query_results\\._cloudburrow_session_[0-9a-f]+_t`").FindString(s.text)
	fn := strings.Replace(h, "_t`", "_twice`", 1)
	want := "CREATE TABLE " + h + " AS SELECT 1 AS a;\n" +
		"CREATE FUNCTION " + fn + "(x INT64) AS (x * 2);\n" +
		"INSERT INTO " + h + " (a) VALUES (2);\n" +
		"INSERT INTO ds.base SELECT " + fn + "(t.a) AS a FROM " + h + " AS `t` WHERE a > 0;\n" +
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT SUM(a) AS s FROM ds.base;\n" +
		"UPDATE " + h + " AS `t` SET a = 3 WHERE TRUE;\n" +
		"SELECT x.a, t FROM ds.base x JOIN " + h + " AS `t` USING (a), " + h + " AS u;\n" +
		"DROP TABLE " + h
	if h == "" || s.text != want {
		t.Errorf("text\n%s\nwant\n%s", s.text, want)
	}
	if len(s.tables) != 1 || len(s.funcs) != 1 || s.readsTempFromMV {
		t.Errorf("tables %v, funcs %v, readsTempFromMV %v", s.tables, s.funcs, s.readsTempFromMV)
	}
	if got := s.restore("UPDATE " + h + " AS `t` SET a = 3 WHERE TRUE"); got != "UPDATE t SET a = 3 WHERE TRUE" {
		t.Errorf("restore: %q", got)
	}
	if got := s.restore("CREATE TABLE " + h + " AS SELECT 1 AS a"); got != "CREATE TEMP TABLE t AS SELECT 1 AS a" {
		t.Errorf("restore: %q", got)
	}
	if s, _ := tempSessionScript("CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE MATERIALIZED VIEW ds.mv AS SELECT a FROM t"); s == nil || !s.readsTempFromMV {
		t.Error("a materialized view of a TEMP table is not refused")
	}
	if s, ok := tempSessionScript("CREATE TEMP TABLE t AS SELECT 1 AS a; SELECT * FROM t"); !ok || s.useSession() {
		t.Error("a script the emulator runs whole is run as a session's script")
	}
	// #1459: what the emulator cannot run whole with a TEMP table is.
	for _, sql := range []string{
		"CREATE TEMP TABLE t AS SELECT 1 AS a; ALTER TABLE ds.x ADD COLUMN b INT64; SELECT * FROM t",
		"CREATE TEMP TABLE t AS SELECT 1 AS a; SELECT * FROM t; SELECT 2",
		"CREATE TEMP TABLE t AS SELECT 1 AS a; ASSERT (SELECT COUNT(*) FROM t) = 1",
		"CREATE TEMP TABLE t AS SELECT 1 AS a; IF TRUE THEN INSERT INTO t VALUES (2); END IF",
		"CREATE TEMP TABLE t AS SELECT 1 AS a; CREATE OR REPLACE TEMP TABLE t AS SELECT 2 AS a",
		"CREATE TEMP TABLE t AS SELECT 1 AS a; DROP TABLE t",
	} {
		if s, ok := tempSessionScript(sql); !ok || !s.useSession() {
			t.Errorf("%s: not run as a session's script", sql)
		}
	}
	// A table named before the script makes its TEMP table of that name
	// is another.
	if s, _ := tempSessionScript("DROP TABLE t; CREATE TEMP TABLE t AS SELECT 1 AS a; SELECT * FROM t"); s == nil ||
		!strings.HasPrefix(s.text, "DROP TABLE t; CREATE TABLE `") || strings.HasSuffix(s.text, "FROM t") {
		t.Errorf("before its CREATE: %v", s)
	}
	if s, _ := tempSessionScript("BEGIN TRANSACTION; CREATE TEMP TABLE t AS SELECT 1 AS a; COMMIT TRANSACTION"); s != nil {
		t.Error("a transaction's TEMP table read as a session's")
	}
	if s, _ := tempSessionScript("CREATE TEMP TABLE t AS SELECT 1 AS a; SELECT 1"); s == nil ||
		!strings.Contains(s.rename("INSERT INTO t VALUES (2)"), "_cloudburrow_session_") {
		t.Error("EXECUTE IMMEDIATE's statement is not renamed")
	}
	for _, other := range []string{
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 AS a FROM ds.t; SELECT 1",
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 AS a FROM ds.t",
	} {
		if _, ok := tempSessionScript(other); ok {
			t.Errorf("%s: read as a session's script", other)
		}
	}
}
