package bigqueryfront

import "testing"

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
