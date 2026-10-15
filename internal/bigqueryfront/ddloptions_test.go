package bigqueryfront

import (
	"strings"
	"testing"
)

// TestParseDDLOptions (#1251): the table OPTIONS of a lone CREATE TABLE,
// CREATE VIEW, or ALTER TABLE or VIEW ... SET OPTIONS, and nothing else.
func TestParseDDLOptions(t *testing.T) {
	for _, tc := range []struct {
		sql                 string
		ok                  bool
		desc, friendly, exp string
		labels              string
		msg                 string
		alter, view, ifne   bool
	}{
		{sql: "CREATE TABLE ds.t (a INT64 OPTIONS(description='col')) OPTIONS(description='d', friendly_name=\"F\")", ok: true, desc: "d", friendly: "F"},
		{sql: "CREATE OR REPLACE TABLE ds.t OPTIONS(labels=[('a','b'),('c','d')]) AS SELECT 1 AS x;", ok: true, labels: "a=b,c=d"},
		{sql: "CREATE VIEW IF NOT EXISTS ds.v (c OPTIONS(description='x')) OPTIONS(description='v') AS SELECT 1", ok: true, desc: "v", view: true, ifne: true},
		{sql: "CREATE VIEW ds.v OPTIONS(expiration_timestamp=TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 1 DAY)) AS SELECT 1", ok: true, exp: "TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 1 DAY)", view: true},
		{sql: "CREATE TABLE ds.t LIKE ds.src OPTIONS(description='liked')", ok: true, desc: "liked"},
		{sql: "ALTER TABLE ds.t SET OPTIONS(description=NULL, labels=[])", ok: true, alter: true},
		{sql: "ALTER VIEW IF EXISTS ds.v SET OPTIONS(friendly_name='n')", ok: true, friendly: "n", alter: true, view: true},
		{sql: "CREATE TABLE ds.t OPTIONS(partition_expiration_days=1, description='d')", ok: true, desc: "d", msg: "partition_expiration_days"},
		{sql: "CREATE TABLE ds.t OPTIONS(description=CONCAT('a','b'))", ok: true, msg: "string literal"},
		{sql: "CREATE TABLE ds.t AS SELECT 1 AS x", ok: false},
		{sql: "CREATE TABLE ds.t AS SELECT * FROM ML.PREDICT(MODEL m, TABLE t, STRUCT() AS OPTIONS)", ok: false},
		{sql: "CREATE TEMP TABLE t OPTIONS(description='d') AS SELECT 1", ok: false},
		{sql: "CREATE TABLE ds.t OPTIONS(description='d'); SELECT 1", ok: false},
		{sql: "ALTER TABLE ds.t ADD COLUMN c INT64", ok: false},
		{sql: "ALTER TABLE ds.t SET OPTIONS(description='d'), ADD COLUMN c INT64", ok: false},
		{sql: "SELECT 1", ok: false},
		{sql: "CREATE TABLE FUNCTION ds.f(x INT64) OPTIONS(description='f') AS SELECT x", ok: false},
	} {
		d, msg, ok := parseDDLOptions(tc.sql)
		if ok != tc.ok {
			t.Errorf("%s: ok %v, want %v", tc.sql, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if tc.msg != "" || msg != "" {
			if !strings.Contains(msg, tc.msg) || tc.msg == "" {
				t.Errorf("%s: msg %q, want one with %q", tc.sql, msg, tc.msg)
			}
			continue
		}
		str := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		var labels []string
		for _, k := range []string{"a", "c"} {
			if v, ok := d.labels[k]; ok {
				labels = append(labels, k+"="+v)
			}
		}
		if str(d.description) != tc.desc || str(d.friendlyName) != tc.friendly || d.expiration != tc.exp ||
			strings.Join(labels, ",") != tc.labels || d.alter != tc.alter || d.view != tc.view || d.ifNotExists != tc.ifne {
			t.Errorf("%s: got %+v", tc.sql, d)
		}
		if !d.sets() {
			t.Errorf("%s: sets nothing", tc.sql)
		}
	}
	d, _, _ := parseDDLOptions("ALTER TABLE ds.t SET OPTIONS(description=NULL, labels=[])")
	if !d.clearDescription || !d.hasLabels || len(d.labels) != 0 {
		t.Errorf("ALTER clearing: got %+v", d)
	}
}
