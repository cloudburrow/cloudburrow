//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryCollationUndCI (#1508): a STRING column with the collation
// 'und:ci', given by tables.insert or by COLLATE in CREATE TABLE, and the
// COLLATE function, compare case-insensitively in comparisons, IN, LIKE,
// joins, GROUP BY, DISTINCT, ORDER BY, MIN/MAX and COUNT(DISTINCT), as
// BigQuery's collation reference documents
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/collation-concepts);
// COLLATE(x, ”) compares by code point again, and comparing two
// different collations is refused. Measured first: CREATE TABLE ...
// COLLATE failed 400 "COLLATE is not supported", and a tables.insert
// collated column's `s = 'aBC'` did not match 'Abc'.
//
// covers: bigquery.jobs.query, bigquery.tables.insert
func TestBigQueryCollationUndCI(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	id := ds.DatasetID
	if err := ds.Table("api").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "s", Type: bigquery.StringFieldType, Collation: "und:ci"},
		{Name: "n", Type: bigquery.IntegerFieldType},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE `" + id + ".ddl` (s STRING COLLATE 'und:ci', n INT64)",
		"INSERT INTO `" + id + ".api` (s, n) VALUES ('Abc', 1), ('aBC', 2), ('b', 3), ('B', 4)",
		"INSERT INTO `" + id + ".ddl` (s, n) VALUES ('Abc', 1), ('aBC', 2), ('b', 3), ('B', 4)",
	} {
		if err := bqRun(ctx, c, q, false); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, table := range []string{"api", "ddl"} {
		tb := "`" + id + "." + table + "`"
		for _, tc := range []struct{ q, want string }{
			{"SELECT COUNT(*) FROM " + tb + " WHERE s = 'ABC'", "2"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE s IN ('ABC', 'x')", "2"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE s LIKE 'ab%'", "2"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE s < 'b'", "2"},
			{"SELECT COUNT(*) FROM (SELECT s FROM " + tb + " GROUP BY s)", "2"},
			{"SELECT COUNT(*) FROM (SELECT DISTINCT s FROM " + tb + ")", "2"},
			{"SELECT COUNT(DISTINCT s) FROM " + tb, "2"},
			{"SELECT STRING_AGG(CAST(n AS STRING), ',' ORDER BY s, n) FROM " + tb, "1,2,3,4"},
			{"SELECT COUNT(*) FROM " + tb + " a JOIN " + tb + " b ON a.s = b.s", "8"},
			{"SELECT LOWER(MIN(s)) FROM " + tb, "abc"},
			{"SELECT LOWER(MAX(s)) FROM " + tb, "b"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE COLLATE(s, '') = 'Abc'", "1"},
		} {
			if got := bqValue(ctx, c, tc.q); got != "["+tc.want+"]" {
				t.Errorf("%s = %s, want %s", tc.q, got, tc.want)
			}
		}
	}
	if got := bqValue(ctx, c, "SELECT COLLATE('a', 'und:ci') = 'A'"); got != "[true]" {
		t.Errorf("COLLATE('a', 'und:ci') = 'A' is %s, want true", got)
	}
	if got := bqValue(ctx, c, "SELECT COLLATE('a', 'und:ci') = COLLATE('A', 'binary')"); len(got) < 6 || got[:6] != "error:" {
		t.Errorf("comparing two collations: %s, want an error", got)
	}
}

// TestBigQueryCollationDefaults (#1508): in a dataset with DEFAULT
// COLLATE 'und:ci', a plain CREATE TABLE, a CREATE TABLE AS SELECT and
// ALTER TABLE ADD COLUMN give each new STRING column that names no
// COLLATE the default, which the engine compares by; COLLATE ” and a
// table's DEFAULT COLLATE ” keep it off ("A column with empty collation
// doesn't inherit the table's default collation"), as BigQuery's DDL
// reference documents
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_table_statement).
// A collation other than 'und:ci' and ” is refused 400 ("und:ci and
// empty collation are supported, but not other collation
// specifications"); a collation on a STRING inside a STRUCT is served
// since #1516 (bigquerycollation1516_test.go). Measured first: the plain CREATE
// TABLE's `s = 'aBC'` matched no row of 'Abc' (BigQuery: 1), and
// COLLATE 'und:cs' was taken and compared by code point.
//
// covers: bigquery.jobs.query, bigquery.tables.insert, bigquery.tables.get
func TestBigQueryCollationDefaults(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_coll1508"
	t.Cleanup(func() { _ = c.Dataset(id).DeleteWithContents(h.Context()) })
	tb := func(name string) string { return "`" + id + "." + name + "`" }
	for _, q := range []string{
		"CREATE SCHEMA `" + id + "` DEFAULT COLLATE 'und:ci'",
		"CREATE TABLE " + tb("plain") + " (s STRING, e STRING COLLATE '', n INT64)",
		"INSERT INTO " + tb("plain") + " (s, e, n) VALUES ('Abc', 'Abc', 1)",
		"CREATE TABLE " + tb("ctas") + " AS SELECT 'Abc' AS s",
		"CREATE TABLE " + tb("own") + " (s STRING) DEFAULT COLLATE ''",
		"INSERT INTO " + tb("own") + " (s) VALUES ('Abc')",
		"ALTER TABLE " + tb("plain") + " ADD COLUMN u STRING",
		"ALTER TABLE " + tb("plain") + " ADD COLUMN v STRING COLLATE ''",
		"UPDATE " + tb("plain") + " SET u = 'Abc', v = 'Abc' WHERE TRUE",
	} {
		if err := bqRun(ctx, c, q, false); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tc := range []struct{ q, want string }{
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE s = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE e = 'aBC'", "0"},
		{"SELECT COUNT(*) FROM " + tb("ctas") + " WHERE s = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("own") + " WHERE s = 'aBC'", "0"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE u = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE v = 'aBC'", "0"},
	} {
		if got := bqValue(ctx, c, tc.q); got != "["+tc.want+"]" {
			t.Errorf("%s = %s, want %s", tc.q, got, tc.want)
		}
	}
	md, err := c.Dataset(id).Table("plain").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, f := range md.Schema {
		cols = append(cols, f.Name+"="+f.Collation)
	}
	if got, want := strings.Join(cols, " "), "s=und:ci e= n= u=und:ci v="; md.DefaultCollation != "und:ci" || got != want {
		t.Errorf("plain: defaultCollation %q, columns %q, want und:ci, %q", md.DefaultCollation, got, want)
	}

	var gerr *googleapi.Error
	for _, tc := range []struct {
		q, table string
		code     int
	}{
		{"CREATE TABLE " + tb("cs") + " (s STRING COLLATE 'und:cs')", "cs", http.StatusBadRequest},
		{"CREATE TABLE " + tb("cs2") + " (s STRING) DEFAULT COLLATE 'binary'", "cs2", http.StatusBadRequest},
		{"SELECT COLLATE('a', 'und:cs') = 'A'", "", http.StatusBadRequest},
	} {
		err := bqRun(ctx, c, tc.q, false)
		if !errors.As(err, &gerr) || gerr.Code != tc.code {
			t.Errorf("%s: %v, want %d", tc.q, err, tc.code)
		}
		if tc.table != "" {
			if _, err := c.Dataset(id).Table(tc.table).Metadata(ctx); err == nil {
				t.Errorf("%s made the table", tc.q)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		schema bigquery.Schema
		code   int
	}{
		{"apics", bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType, Collation: "und:cs"}}, http.StatusBadRequest},
	} {
		err := c.Dataset(id).Table(tc.name).Create(ctx, &bigquery.TableMetadata{Schema: tc.schema})
		if !errors.As(err, &gerr) || gerr.Code != tc.code {
			t.Errorf("tables.insert %s: %v, want %d", tc.name, err, tc.code)
		}
	}
}
