package bigqueryfront

import (
	"strings"
	"testing"
)

// TestParseCloneDDL (#1002): a lone CREATE [SNAPSHOT] TABLE ... CLONE or DROP
// SNAPSHOT TABLE is read; what the front does not carry out is named; any
// other query, or one of them in a script, is left alone.
func TestParseCloneDDL(t *testing.T) {
	for _, c := range []struct {
		sql, kind, dest, src, unsupported string
		asOf, expiration                  string
		ifClause, ok                      bool
	}{
		{sql: "CREATE TABLE ds.c CLONE ds.t", kind: "clone", dest: "ds.c", src: "ds.t", ok: true},
		{sql: "create table if not exists `p.ds.c` clone p.ds.t;", kind: "clone", dest: "p.ds.c", src: "p.ds.t",
			ifClause: true, ok: true},
		{sql: "CREATE SNAPSHOT TABLE ds.s CLONE ds.t", kind: "snapshot", dest: "ds.s", src: "ds.t", ok: true},
		{sql: "DROP SNAPSHOT TABLE IF EXISTS ds.s", kind: "drop", dest: "ds.s", ifClause: true, ok: true},
		{sql: "CREATE TABLE ds.c CLONE ds.t FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP()", kind: "clone", dest: "ds.c",
			src: "ds.t", asOf: "CURRENT_TIMESTAMP()", ok: true},
		{sql: "CREATE SNAPSHOT TABLE ds.s CLONE ds.t FOR SYSTEM_TIME AS OF TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR) " +
			"OPTIONS(expiration_timestamp = TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 48 HOUR), description = 'd')",
			kind: "snapshot", dest: "ds.s", src: "ds.t", asOf: "TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR)",
			expiration: "TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 48 HOUR)", ok: true},
		{sql: "CREATE TABLE ds.c CLONE ds.t OPTIONS(not_an_option = true)", kind: "clone", dest: "ds.c", src: "ds.t",
			unsupported: "not_an_option", ok: true},
		{sql: "CREATE TABLE ds.c CLONE ds.t FOR SYSTEM_TIME AS OF"},
		{sql: "CREATE TABLE ds.c CLONE ds.t OPTIONS(description = 'd') extra"},
		{sql: "CREATE OR REPLACE TABLE ds.c CLONE ds.t", kind: "clone", dest: "ds.c", src: "ds.t",
			unsupported: "OR REPLACE", ok: true},
		{sql: "SELECT 1; CREATE TABLE ds.c CLONE ds.t"},
		{sql: "CREATE TABLE ds.c COPY ds.t"},
		{sql: "CREATE TABLE ds.c AS SELECT 1 AS clone"},
		{sql: "DROP TABLE ds.s"},
		{sql: "DROP SNAPSHOT TABLE ds.s extra"},
	} {
		st, unsupported, ok := parseCloneDDL(c.sql)
		if ok != c.ok {
			t.Errorf("%q: ok %v, want %v", c.sql, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if st.kind != c.kind || strings.Join(st.dest, ".") != c.dest || strings.Join(st.src, ".") != c.src ||
			st.ifClause != c.ifClause {
			t.Errorf("%q: %+v, want %s %s %s %v", c.sql, st, c.kind, c.dest, c.src, c.ifClause)
		}
		exp := ""
		if st.options != nil {
			exp = st.options.expiration
		}
		if st.asOf != c.asOf || exp != c.expiration {
			t.Errorf("%q: asOf %q expiration %q, want %q %q", c.sql, st.asOf, exp, c.asOf, c.expiration)
		}
		if c.unsupported == "" && unsupported != "" || !strings.Contains(unsupported, c.unsupported) {
			t.Errorf("%q: unsupported %q, want %q", c.sql, unsupported, c.unsupported)
		}
	}
}

// TestOwnDDLIsKeptAndPatched (#1002): what a front-run DDL statement did is
// put in its job's statistics.query, and outlives the job texts' file.
func TestOwnDDLIsKeptAndPatched(t *testing.T) {
	in := jobText{query: "CREATE TABLE d.c CLONE d.t", ddl: &ownDDL{statementType: "CREATE_TABLE", operation: "CREATE",
		target: tableRef{ProjectID: "p", DatasetID: "d", TableID: "c"}}}
	out := saveJobText("p/j", in).jobText()
	if out.ddl == nil || *out.ddl != *in.ddl || out.empty() {
		t.Fatalf("kept %+v, want %+v", out.ddl, in.ddl)
	}
	job := map[string]any{"configuration": map[string]any{"query": map[string]any{"query": "DROP TABLE IF EXISTS x"}},
		"statistics": map[string]any{"query": map[string]any{"statementType": "DROP_TABLE"}}}
	out.patch(job)
	q := job["statistics"].(map[string]any)["query"].(map[string]any)
	target, _ := q["ddlTargetTable"].(map[string]any)
	if q["statementType"] != "CREATE_TABLE" || q["ddlOperationPerformed"] != "CREATE" || target["tableId"] != "c" {
		t.Errorf("patched statistics.query = %v", q)
	}
	if got := job["configuration"].(map[string]any)["query"].(map[string]any)["query"]; got != in.query {
		t.Errorf("the job's query = %v, want the client's", got)
	}
}
