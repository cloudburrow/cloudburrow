package bigqueryfront

import (
	"net/http"
	"testing"
)

// TestQueryCollationRefusal (#1508): a collation other than 'und:ci' and
// ” is refused 400, in DDL and in the COLLATE function, inside a STRUCT
// or ARRAY type too; the rest runs, a nested COLLATE 'und:ci' since #1516.
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
		{"CREATE TABLE d.t (r STRUCT<s STRING COLLATE 'und:ci'>)", 0},
		{"CREATE TABLE d.t (a ARRAY<STRING COLLATE 'und:ci'>)", 0},
		{"CREATE TABLE d.t (a ARRAY<STRUCT<s STRING COLLATE 'und:cs'>>)", http.StatusBadRequest},
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
		{`{"schema":{"fields":[{"name":"r","type":"RECORD","fields":[{"name":"s","type":"STRING","collation":"und:ci"}]}]}}`, 0},
		{`{"schema":{"fields":[{"name":"a","type":"STRING","mode":"REPEATED","collation":"und:ci"}]}}`, 0},
		{`{"schema":{"fields":[{"name":"r","type":"RECORD","fields":[{"name":"s","type":"STRING","collation":"x"}]}]}}`, http.StatusBadRequest},
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

// TestNestedCollationDDL (#1516): a nested COLLATE is read into the
// field's collation; ALTER TABLE SET DEFAULT COLLATE is an action; SET
// DATA TYPE cannot change a collation; a script's CREATE SCHEMA names its
// default.
func TestNestedCollationDDL(t *testing.T) {
	toks, _ := lex("STRUCT<a STRING COLLATE 'und:ci', b ARRAY<STRING(4) COLLATE ''>, c INT64>")
	f, next, what := fieldType(toks, 0)
	if what != "" || next != len(toks) {
		t.Fatalf("fieldType: %v %d %q", f, next, what)
	}
	fields, _ := f["fields"].([]any)
	if len(fields) != 3 || fields[0].(map[string]any)["collation"] != "und:ci" ||
		fields[1].(map[string]any)["collation"] != "" || fields[1].(map[string]any)["mode"] != "REPEATED" {
		t.Errorf("fieldType fields: %v", fields)
	}
	toks, _ = lex("ALTER TABLE d.t SET DEFAULT COLLATE 'und:ci', ADD COLUMN s STRING")
	st, unsupported, msg := parseAlter(toks)
	if unsupported != "" || msg != "" || len(st.actions) != 2 || st.actions[0].kind != "default collate" ||
		st.actions[0].newName != "und:ci" || actionGroup(st.actions[0].kind) != "patch" {
		t.Errorf("parseAlter SET DEFAULT COLLATE: %+v %q %q", st, unsupported, msg)
	}
	if toks, _ = lex("ALTER TABLE d.t SET DEFAULT COLLATE x"); true {
		if _, _, msg := parseAlter(toks); msg == "" {
			t.Error("SET DEFAULT COLLATE without a literal: no 400")
		}
	}
	old := map[string]any{"type": "STRING", "collation": "und:ci"}
	for _, c := range []struct {
		next    map[string]any
		refused bool
	}{
		{map[string]any{"type": "STRING"}, false},
		{map[string]any{"type": "STRING", "collation": "und:ci"}, false},
		{map[string]any{"type": "STRING", "collation": ""}, true},
	} {
		if got := collationRetypeRefused(old, c.next, "s"); (got != "") != c.refused {
			t.Errorf("collationRetypeRefused(%v): %q", c.next, got)
		}
	}
	oldRec := map[string]any{"type": "RECORD", "fields": []any{map[string]any{"name": "a", "type": "STRING"}}}
	nextRec := map[string]any{"type": "RECORD", "fields": []any{map[string]any{"name": "a", "type": "STRING", "collation": "und:ci"}}}
	if collationRetypeRefused(oldRec, nextRec, "r") == "" {
		t.Error("a nested field's new collation: not refused")
	}
	toks, _ = lex("CREATE SCHEMA IF NOT EXISTS p.d DEFAULT COLLATE 'und:ci' OPTIONS (description = 'x')")
	if ds, c, ok := schemaDefaultCollate(toks); !ok || ds != "d" || c != "und:ci" {
		t.Errorf("schemaDefaultCollate: %q %q %v", ds, c, ok)
	}
}
