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
	for _, other := range []string{
		"CREATE TEMP TABLE t AS SELECT 1 AS a; SELECT * FROM t",
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 AS a FROM ds.t; SELECT 1",
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 AS a FROM ds.t",
	} {
		if _, ok := tempSessionScript(other); ok {
			t.Errorf("%s: read as a session's script", other)
		}
	}
}
