package bigqueryfront

import (
	"net/http"
	"testing"
)

// TestQueryCollationRefusal (#1508): a collation other than 'und:ci' and
// ” is refused 400, in DDL and in the COLLATE function; a COLLATE inside
// a STRUCT or ARRAY type 501; the rest runs.
func TestQueryCollationRefusal(t *testing.T) {
	for _, c := range []struct {
		sql  string
		code int
	}{
		{"CREATE TABLE d.t (s STRING COLLATE 'und:ci', u STRING COLLATE '')", 0},
		{"CREATE TABLE d.t (s STRING) DEFAULT COLLATE 'und:ci'", 0},
		{"SELECT COLLATE('a', 'und:ci') = 'A', s FROM t WHERE x < 3 AND y > 2", 0},
		{"SELECT 'COLLATE und:cs'", 0},
		{"CREATE TABLE d.t (s STRING COLLATE 'und:cs')", http.StatusBadRequest},
		{"CREATE TABLE d.t (s STRING) DEFAULT COLLATE 'binary'", http.StatusBadRequest},
		{"CREATE SCHEMA d DEFAULT COLLATE 'und:cs'", http.StatusBadRequest},
		{"SELECT COLLATE(CONCAT('a', 'b'), 'und:cs')", http.StatusBadRequest},
		{"SELECT COLLATE(s, '') FROM t", 0},
		{"CREATE TABLE d.t (r STRUCT<s STRING COLLATE 'und:ci'>)", http.StatusNotImplemented},
		{"CREATE TABLE d.t (a ARRAY<STRING COLLATE 'und:ci'>)", http.StatusNotImplemented},
		{"CREATE TABLE d.t (r STRUCT<n INT64>, s STRING COLLATE 'und:ci')", 0},
	} {
		if code, _, msg := queryCollationRefusal(c.sql); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.sql, code, msg, c.code)
		}
	}
}

// TestTableCollationRefusal (#1508): a Table resource's collations.
func TestTableCollationRefusal(t *testing.T) {
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"defaultCollation":"und:ci","schema":{"fields":[{"name":"s","type":"STRING","collation":""}]}}`, 0},
		{`{"defaultCollation":"und:cs"}`, http.StatusBadRequest},
		{`{"schema":{"fields":[{"name":"s","type":"STRING","collation":"und:cs"}]}}`, http.StatusBadRequest},
		{`{"schema":{"fields":[{"name":"r","type":"RECORD","fields":[{"name":"s","type":"STRING","collation":"und:ci"}]}]}}`, http.StatusNotImplemented},
		{`{"schema":{"fields":[{"name":"a","type":"STRING","mode":"REPEATED","collation":"und:ci"}]}}`, http.StatusNotImplemented},
	} {
		if code, _, msg := tableCollationRefusal([]byte(c.body)); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.body, code, msg, c.code)
		}
	}
	if datasetCollationRefused([]byte(`{"defaultCollation":"und:ci"}`)) != "" || datasetCollationRefused([]byte(`{"defaultCollation":"x"}`)) == "" {
		t.Error("datasetCollationRefused")
	}
}

// TestTableDefaultCollate (#1508): where a CREATE TABLE's DEFAULT COLLATE
// goes, and the one it names.
func TestTableDefaultCollate(t *testing.T) {
	for _, c := range []struct {
		sql, insertAt, spec string
		named               bool
	}{
		{"CREATE TABLE d.t (s STRING, n INT64)", "CREATE TABLE d.t (s STRING, n INT64)", "", false},
		{"CREATE OR REPLACE TABLE `p.d.t` (s STRING) OPTIONS (description = 'x')", "CREATE OR REPLACE TABLE `p.d.t` (s STRING)", "", false},
		{"CREATE TABLE IF NOT EXISTS d.t AS SELECT 'a' AS s", "CREATE TABLE IF NOT EXISTS d.t", "", false},
		{"CREATE TABLE d.t (s STRING) DEFAULT COLLATE '' AS SELECT 'a' AS s", "CREATE TABLE d.t (s STRING)", "", true},
		{"CREATE TABLE d.t DEFAULT COLLATE 'und:ci' AS SELECT COLLATE('a', '') AS s", "CREATE TABLE d.t", "und:ci", true},
		{"CREATE TABLE d.t AS SELECT x FROM u WHERE DEFAULT COLLATE 'x'", "CREATE TABLE d.t", "", false},
	} {
		spec, named, at, ok := tableDefaultCollate(c.sql)
		if !ok || c.sql[:at] != c.insertAt || spec != c.spec || named != c.named {
			t.Errorf("%s: %q %v at %q (ok %v), want %q %v at %q", c.sql, spec, named, c.sql[:at], ok, c.spec, c.named, c.insertAt)
		}
	}
}
