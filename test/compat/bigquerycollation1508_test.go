//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/bigquery"
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
