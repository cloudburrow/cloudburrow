package bigqueryfront

import "testing"

// TestRewriteTableLikeCopy (#1239): a lone CREATE TABLE ... LIKE or COPY is
// the CREATE TABLE AS SELECT it amounts to; anything else is left alone.
func TestRewriteTableLikeCopy(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"CREATE TABLE ds.t LIKE ds.src":                                        "CREATE TABLE ds.t AS SELECT * FROM ds.src LIMIT 0",
		"CREATE TABLE ds.t COPY ds.src;":                                       "CREATE TABLE ds.t AS SELECT * FROM ds.src",
		"create or replace table `p-1.ds.t` like `p-1.ds.src`":                 "create or replace table `p-1.ds.t` AS SELECT * FROM `p-1.ds.src` LIMIT 0",
		"CREATE TABLE IF NOT EXISTS ds.t COPY ds.src OPTIONS(description='d')": "CREATE TABLE IF NOT EXISTS ds.t OPTIONS(description='d') AS SELECT * FROM ds.src",
		"CREATE TEMP TABLE t LIKE ds.src CLUSTER BY id":                        "CREATE TEMP TABLE t CLUSTER BY id AS SELECT * FROM ds.src LIMIT 0",
	} {
		if got, ok := rewriteTableLikeCopy(in); !ok || got != want {
			t.Errorf("rewriteTableLikeCopy(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"CREATE TABLE ds.t CLONE ds.src",
		"CREATE TABLE ds.t (x INT64)",
		"CREATE TABLE ds.t AS SELECT 1 AS x",
		"CREATE TABLE ds.t LIKE ds.src AS SELECT 1 AS x",
		"CREATE TABLE ds.t LIKE ds.src; SELECT 1",
		"SELECT 'CREATE TABLE ds.t LIKE ds.src'",
		"CREATE VIEW ds.v LIKE ds.src",
	} {
		if got, ok := rewriteTableLikeCopy(in); ok {
			t.Errorf("rewriteTableLikeCopy(%q) = %q; want it left alone", in, got)
		}
	}
}
