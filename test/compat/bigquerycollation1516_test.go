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

// collationsOf lists schema's fields at any depth as name=collation, a
// RECORD's as parent.field.
func collationsOf(schema bigquery.Schema, prefix string) []string {
	var out []string
	for _, f := range schema {
		if f.Type == bigquery.RecordFieldType {
			out = append(out, collationsOf(f.Schema, prefix+f.Name+".")...)
			continue
		}
		out = append(out, prefix+f.Name+"="+f.Collation)
	}
	return out
}

// TestBigQueryNestedCollation (#1516): a STRING inside a STRUCT or an
// ARRAY with the collation 'und:ci', given by COLLATE in CREATE TABLE
// (STRUCT<a STRING COLLATE 'und:ci'>, ARRAY<STRING COLLATE 'und:ci'>) or
// by tables.insert (a RECORD's field, a REPEATED STRING), compares
// case-insensitively through a field access, UNNEST, IN UNNEST, GROUP BY
// and a STRUCT's =, and reads back from tables.get and
// INFORMATION_SCHEMA.COLUMN_FIELD_PATHS, as BigQuery's
// collation reference documents ("A STRING field in a STRUCT", "A STRING
// element in an ARRAY",
// https://cloud.google.com/bigquery/docs/reference/standard-sql/collation-concepts).
// Measured first: both were refused 501 (#1508), as the engine's analyzer
// failed "GOOGLESQL_RET_CHECK failure ... annotations != nullptr".
//
// covers: bigquery.jobs.query, bigquery.tables.insert, bigquery.tables.get
func TestBigQueryNestedCollation(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	id := ds.DatasetID
	if err := ds.Table("api").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "st", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "a", Type: bigquery.StringFieldType, Collation: "und:ci"},
			{Name: "b", Type: bigquery.StringFieldType},
		}},
		{Name: "arr", Type: bigquery.StringFieldType, Repeated: true, Collation: "und:ci"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE `" + id + ".ddl` (st STRUCT<a STRING COLLATE 'und:ci', b STRING>, arr ARRAY<STRING COLLATE 'und:ci'>)",
		"INSERT INTO `" + id + ".api` (st, arr) VALUES (STRUCT('Abc', 'Abc'), ['Abc', 'x']), (STRUCT('aBC', 'aBC'), ['aBC'])",
		"INSERT INTO `" + id + ".ddl` (st, arr) VALUES (STRUCT('Abc', 'Abc'), ['Abc', 'x']), (STRUCT('aBC', 'aBC'), ['aBC'])",
	} {
		if err := bqRun(ctx, c, q, false); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, table := range []string{"api", "ddl"} {
		tb := "`" + id + "." + table + "`"
		for _, tc := range []struct{ q, want string }{
			{"SELECT COUNT(*) FROM " + tb + " WHERE st.a = 'ABC'", "2"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE st.b = 'ABC'", "0"},
			{"SELECT COUNT(*) FROM " + tb + ", UNNEST(arr) e WHERE e = 'ABC'", "2"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE 'ABC' IN UNNEST(arr)", "2"},
			{"SELECT COUNT(*) FROM (SELECT st.a FROM " + tb + " GROUP BY st.a)", "1"},
			{"SELECT COUNT(*) FROM " + tb + " WHERE st = STRUCT('ABC' AS a, 'Abc' AS b)", "1"},
		} {
			if got := bqValue(ctx, c, tc.q); got != "["+tc.want+"]" {
				t.Errorf("%s = %s, want %s", tc.q, got, tc.want)
			}
		}
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(collationsOf(md.Schema, ""), " "), "st.a=und:ci st.b= arr=und:ci"; got != want {
			t.Errorf("%s: collations %q, want %q", table, got, want)
		}
	}
	// INFORMATION_SCHEMA.COLUMN_FIELD_PATHS: "If a STRING, ARRAY<STRING>,
	// or STRING field in a STRUCT is passed in, the collation
	// specification is returned if it exists".
	q := "SELECT STRING_AGG(CONCAT(field_path, ':', IFNULL(collation_name, '-')), ',' ORDER BY field_path) FROM `" + id +
		"`.INFORMATION_SCHEMA.COLUMN_FIELD_PATHS WHERE table_name = 'ddl'"
	if got, want := bqValue(ctx, c, q), "[arr:und:ci,st:-,st.a:und:ci,st.b:-]"; got != want {
		t.Errorf("COLUMN_FIELD_PATHS collation_name: %s, want %s", got, want)
	}
	// A collation BigQuery does not support is refused inside a type too.
	var gerr *googleapi.Error
	q = "CREATE TABLE `" + id + ".cs` (st STRUCT<a STRING COLLATE 'und:cs'>)"
	if err := bqRun(ctx, c, q, false); !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("%s: %v, want 400", q, err)
	}
}

// TestBigQueryCollationInScriptsAndAlter (#1516): a dataset's DEFAULT
// COLLATE reaches the STRINGs inside a STRUCT or an ARRAY of a new table
// ("A column that contains a collation-supported type ... is a
// collation-supported column"), and a CREATE TABLE inside a script of
// several statements, also after the script's own CREATE SCHEMA; ALTER
// TABLE SET DEFAULT COLLATE sets the table's default, which only the
// columns added after it take ("The updated collation specification only
// applies to columns added afterwards"); ALTER COLUMN SET DATA TYPE keeps
// a column's collation and cannot change it ("You cannot update an
// existing collation specification on a column",
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_collate_statement).
// Measured first: the script's table compared case-sensitively and read
// back no defaultCollation, and SET DEFAULT COLLATE was 501.
//
// covers: bigquery.jobs.query, bigquery.tables.get
func TestBigQueryCollationInScriptsAndAlter(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_coll1516"
	id2 := id + "_script"
	t.Cleanup(func() {
		_ = c.Dataset(id).DeleteWithContents(h.Context())
		_ = c.Dataset(id2).DeleteWithContents(h.Context())
	})
	tb := func(name string) string { return "`" + id + "." + name + "`" }
	for _, q := range []string{
		"CREATE SCHEMA `" + id + "` DEFAULT COLLATE 'und:ci'",
		"CREATE TABLE " + tb("plain") + " (s STRING, st STRUCT<a STRING, b STRING COLLATE ''>, arr ARRAY<STRING>)",
		"INSERT INTO " + tb("plain") + " VALUES ('Abc', STRUCT('Abc', 'Abc'), ['Abc'])",
		"CREATE TABLE " + tb("script") + " (s STRING, st STRUCT<a STRING>); INSERT INTO " + tb("script") +
			" VALUES ('Abc', STRUCT('Abc'))",
		"CREATE SCHEMA `" + id2 + "` DEFAULT COLLATE 'und:ci'; CREATE TABLE `" + id2 + ".t` (s STRING); INSERT INTO `" + id2 +
			".t` VALUES ('Abc')",
		"ALTER TABLE " + tb("plain") + " SET DEFAULT COLLATE ''",
		"ALTER TABLE " + tb("plain") + " ADD COLUMN w STRING",
		"ALTER TABLE " + tb("plain") + " SET DEFAULT COLLATE 'und:ci', ADD COLUMN x STRING, ADD COLUMN y STRUCT<a STRING>",
		"UPDATE " + tb("plain") + " SET w = 'Abc', x = 'Abc', y = STRUCT('Abc') WHERE TRUE",
		"CREATE TABLE " + tb("retype") + " (s STRING(5) COLLATE 'und:ci', n INT64) DEFAULT COLLATE ''",
		"INSERT INTO " + tb("retype") + " VALUES ('Abc', 1)",
		"ALTER TABLE " + tb("retype") + " ALTER COLUMN s SET DATA TYPE STRING(10)",
		"ALTER TABLE " + tb("retype") + " ALTER COLUMN s SET DATA TYPE STRING(12) COLLATE 'und:ci'",
	} {
		if err := bqRun(ctx, c, q, false); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tc := range []struct{ q, want string }{
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE s = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE st.a = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE st.b = 'aBC'", "0"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE 'aBC' IN UNNEST(arr)", "1"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE w = 'aBC'", "0"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE x = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("plain") + " WHERE y.a = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("script") + " WHERE s = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("script") + " WHERE st.a = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM `" + id2 + ".t` WHERE s = 'aBC'", "1"},
		{"SELECT COUNT(*) FROM " + tb("retype") + " WHERE s = 'aBC'", "1"},
	} {
		if got := bqValue(ctx, c, tc.q); got != "["+tc.want+"]" {
			t.Errorf("%s = %s, want %s", tc.q, got, tc.want)
		}
	}
	for _, tc := range []struct{ ds, table, def, cols string }{
		{id, "plain", "und:ci", "s=und:ci st.a=und:ci st.b= arr=und:ci w= x=und:ci y.a=und:ci"},
		{id, "script", "und:ci", "s=und:ci st.a=und:ci"},
		{id2, "t", "und:ci", "s=und:ci"},
		{id, "retype", "", "s=und:ci n="},
	} {
		md, err := c.Dataset(tc.ds).Table(tc.table).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(collationsOf(md.Schema, ""), " "); md.DefaultCollation != tc.def || got != tc.cols {
			t.Errorf("%s: defaultCollation %q, columns %q, want %q, %q", tc.table, md.DefaultCollation, got, tc.def, tc.cols)
		}
	}
	var gerr *googleapi.Error
	q := "ALTER TABLE " + tb("retype") + " ALTER COLUMN s SET DATA TYPE STRING(20) COLLATE ''"
	if err := bqRun(ctx, c, q, false); !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("%s: %v, want 400", q, err)
	}
}
