package bigqueryfront

import (
	"strings"
	"testing"
)

func TestChangeFuncs(t *testing.T) {
	sql := "SELECT * FROM APPENDS(TABLE ds.t, NULL, NULL) a JOIN CHANGES(TABLE `p.ds.u`, TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR), NULL) c ON TRUE, APPENDS(TABLE v)"
	calls, ok := changeFuncs(sql, "p", "dd")
	if !ok || len(calls) != 3 {
		t.Fatalf("calls %+v, ok %v", calls, ok)
	}
	if c := calls[0]; c.name != "APPENDS" || c.dataset != "ds" || c.table != "t" || c.start != "NULL" || c.stop != "NULL" ||
		sql[c.pos:c.end] != "APPENDS(TABLE ds.t, NULL, NULL)" {
		t.Errorf("first %+v", c)
	}
	if c := calls[1]; c.name != "CHANGES" || c.dataset != "ds" || c.table != "u" ||
		c.start != "TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR)" || c.stop != "NULL" {
		t.Errorf("second %+v", c)
	}
	if c := calls[2]; c.dataset != "dd" || c.table != "v" || c.start != "" || c.stop != "" {
		t.Errorf("third %+v", c)
	}
	if calls, ok := changeFuncs("SELECT 'CHANGES' AS x", "p", ""); !ok || len(calls) != 0 {
		t.Errorf("a string: %+v %v", calls, ok)
	}
	if _, ok := changeFuncs("SELECT * FROM CHANGES(TABLE ds.t, , NULL)", "p", ""); ok {
		t.Error("an empty argument was read")
	}
}

// TestChangesInTransaction: APPENDS and CHANGES inside a multi-statement
// transaction are found (#1466).
func TestChangesInTransaction(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want bool
	}{
		{"SELECT * FROM APPENDS(TABLE d.t)", false},
		{"BEGIN TRANSACTION; SELECT * FROM APPENDS(TABLE d.t); COMMIT TRANSACTION;", true},
		{"BEGIN TRANSACTION; COMMIT TRANSACTION; SELECT * FROM APPENDS(TABLE d.t)", false},
		{"BEGIN TRAN; ROLLBACK; SELECT * FROM APPENDS(TABLE d.t)", false},
		{"BEGIN SELECT * FROM APPENDS(TABLE d.t); END", false},
		{"BEGIN; BEGIN TRANSACTION; SELECT * FROM CHANGES(TABLE d.t, NULL, NULL); COMMIT; END", true},
	} {
		pos := strings.Index(c.sql, "APPENDS(")
		if pos < 0 {
			pos = strings.Index(c.sql, "CHANGES(")
		}
		if got := inTransaction(c.sql, pos); got != c.want {
			t.Errorf("inTransaction(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
	cdc := map[string]any{"maxStaleness": "0-0 0 0:10:0", "tableConstraints": map[string]any{"primaryKey": map[string]any{"columns": []any{"id"}}}}
	if !changeDataCapture(cdc) {
		t.Error("a table with a primary key and max_staleness is not taken for change data capture")
	}
	delete(cdc, "maxStaleness")
	if changeDataCapture(cdc) {
		t.Error("a table with no max_staleness is taken for change data capture")
	}
}
