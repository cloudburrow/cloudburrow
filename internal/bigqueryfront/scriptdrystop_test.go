package bigqueryfront

import "testing"

// TestDryRunStop: a script's dry run stops at its first DDL statement and
// before an IF, FOR or WHILE (#1456, scriptdrystop.go).
func TestDryRunStop(t *testing.T) {
	for _, c := range []struct {
		sql, prefix string
		alter, ok   bool
	}{
		{"SELECT 1; ALTER TABLE d.t ADD COLUMN c INT64; SELECT 2", "SELECT 1", true, true},
		{"ALTER TABLE d.t ADD COLUMN c INT64; SELECT nope", "", true, true},
		{"DECLARE x INT64 DEFAULT 1; IF x = 1 THEN ALTER PROJECT p SET OPTIONS (`region-us.default_time_zone` = 'UTC'); END IF",
			"DECLARE x INT64 DEFAULT 1", false, true},
		{"SELECT 1; CREATE TABLE d.n (i INT64); ALTER TABLE d.n ADD COLUMN c INT64", "SELECT 1; CREATE TABLE d.n (i INT64)", false, true},
		{"SELECT 1; SELECT 2", "", false, false},                                                // nothing to cure
		{"SELECT 1; IF TRUE THEN SELECT 2; END IF", "", false, false},                           // no ALTER after the stop
		{"CREATE TEMP TABLE x (i INT64); ALTER TABLE d.t ADD COLUMN c INT64", "", false, false}, // not supported
		{"BEGIN ALTER TABLE d.t ADD COLUMN c INT64; END; SELECT 1", "", false, false},           // a block: as before
		{"ALTER TABLE d.t ADD COLUMN c INT64", "", false, false},                                // lone: altertabledry.go
	} {
		stop, ok := dryRunStop(c.sql)
		if ok != c.ok || ok && (stop.prefix != c.prefix || (stop.alter != nil) != c.alter) {
			t.Errorf("%q: %q %v %v, want %q %v %v", c.sql, stop.prefix, stop.alter != nil, ok, c.prefix, c.alter, c.ok)
		}
	}
	// A CALL and an EXECUTE IMMEDIATE stop it too (#1612).
	for _, c := range []struct {
		sql, prefix string
		call, exec  bool
		ok          bool
	}{
		{"SELECT 1; CALL d.p(1); ALTER TABLE d.t ADD COLUMN c INT64", "SELECT 1", true, false, true},
		{"DECLARE s STRING DEFAULT 'SELECT 1'; EXECUTE IMMEDIATE s; ALTER TABLE d.t ADD COLUMN c INT64",
			"DECLARE s STRING DEFAULT 'SELECT 1'", false, true, true},
		{"CALL d.p(); SELECT 1", "", false, false, false},                                                          // no ALTER after it
		{"CALL BQ.REFRESH_MATERIALIZED_VIEW('d.mv'); ALTER TABLE d.t ADD COLUMN c INT64", "", false, false, false}, // a system procedure
	} {
		stop, ok := dryRunStop(c.sql)
		if ok != c.ok || ok && (stop.prefix != c.prefix || (stop.call != nil) != c.call || (stop.exec != nil) != c.exec) {
			t.Errorf("%q: %+v %v", c.sql, stop, ok)
		}
	}
	if !legacyAlterTable("SELECT 1; ALTER TABLE [d.t] ADD COLUMN c INT64") || legacyAlterTable("SELECT * FROM [d.alter]") {
		t.Error("legacyAlterTable")
	}
}
