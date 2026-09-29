package bigqueryfront

import "testing"

// TestExecuteImmediateOfAConstant (#1037): SQL given by a constant
// expression (CONCAT or || of literals and known variables) is carried
// out; one that reads a value the script computes stays 501.
func TestExecuteImmediateOfAConstant(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"EXECUTE IMMEDIATE CONCAT('SELECT ', '1 AS a')", "SELECT 1 AS a"},
		{"EXECUTE IMMEDIATE 'SELECT ' || '2'", "SELECT 2"},
		{"DECLARE t STRING DEFAULT 'ds.t'; EXECUTE IMMEDIATE ('SELECT * FROM ' || t)", "DECLARE t STRING DEFAULT 'ds.t'; SELECT * FROM ds.t"},
		{"DECLARE q STRING DEFAULT CONCAT('SELECT ', \"3\"); EXECUTE IMMEDIATE q", "DECLARE q STRING DEFAULT CONCAT('SELECT ', \"3\"); SELECT 3"},
	} {
		got, changed, code, msg := expandExecuteImmediate(c.sql)
		if !changed || code != 0 || got != c.want {
			t.Errorf("%s: %q %v %d %s", c.sql, got, changed, code, msg)
		}
	}
	for _, sql := range []string{
		"EXECUTE IMMEDIATE CONCAT('SELECT ', CAST(1 AS STRING))",
		"DECLARE x INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT ' || x",
		"EXECUTE IMMEDIATE 'SELECT 1' INTO a",
	} {
		if _, _, code, _ := expandExecuteImmediate(sql); code != 501 {
			t.Errorf("%s: %d, want 501", sql, code)
		}
	}
}
