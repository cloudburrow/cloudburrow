//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryNestedDefaultsAndTruncateDataDefaults (#1456): a default is
// set for a whole STRUCT column, not for its fields ("you must set the
// default value for the entire STRUCT field. You cannot set the default
// value for a subset of the fields", default-values), so a field's
// defaultValueExpression is 400 in tables.insert and in tables.patch
// (Table.Update), as is ALTER COLUMN s.x SET DEFAULT, while a STRUCT
// column's own default is kept and applied. And a WRITE_TRUNCATE_DATA query
// job whose result lacks a column with a default writes the default ("If
// the query omits a column with a default value, the default value is
// assigned"), keeping the table's schema. Before, both were 501.
//
// covers: bigquery.tables.insert, bigquery.tables.patch, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryNestedDefaultsAndTruncateDataDefaults(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()

	nested := bigquery.Schema{{Name: "s", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
		{Name: "x", Type: bigquery.IntegerFieldType, DefaultValueExpression: "1"}}}}
	err := ds.Table("nesteddefault").Create(ctx, &bigquery.TableMetadata{Schema: nested})
	if httpStatus(err) != http.StatusBadRequest || !strings.Contains(fmt.Sprint(err), "entire STRUCT field s") {
		t.Errorf("tables.insert of a field's default: %v, want 400", err)
	}

	whole := bigquery.Schema{{Name: "k", Type: bigquery.IntegerFieldType},
		{Name: "s", Type: bigquery.RecordFieldType, DefaultValueExpression: "STRUCT(7 AS x)",
			Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}}}
	tbl := ds.Table("structdefault")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: whole}); err != nil {
		t.Fatalf("tables.insert of a STRUCT column's default: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Schema[1].DefaultValueExpression != "STRUCT(7 AS x)" {
		t.Errorf("STRUCT column's default %q", md.Schema[1].DefaultValueExpression)
	}
	ref := "`" + ds.DatasetID + ".structdefault`"
	if err := bqRun(ctx, c, "INSERT INTO "+ref+" (k) VALUES (1)", false); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT s.x FROM "+ref)); got != "[[7]]" {
		t.Errorf("STRUCT default applied: %s, want [[7]]", got)
	}
	update := bigquery.Schema{whole[0], {Name: "s", Type: bigquery.RecordFieldType, DefaultValueExpression: "STRUCT(7 AS x)",
		Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType, DefaultValueExpression: "2"}}}}
	_, err = tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: update}, md.ETag)
	if httpStatus(err) != http.StatusBadRequest {
		t.Errorf("tables.patch of a field's default: %v, want 400", err)
	}
	for _, insert := range []bool{false, true} {
		err := bqRun(ctx, c, "ALTER TABLE "+ref+" ALTER COLUMN s.x SET DEFAULT 3", insert)
		if err == nil || !strings.Contains(err.Error(), "subfields") {
			t.Errorf("insert %v: ALTER COLUMN s.x SET DEFAULT: %v, want a 400 refusal", insert, err)
		}
	}

	// WRITE_TRUNCATE_DATA of a result without the defaulted column b.
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".truncdefault` (a INT64, b STRING DEFAULT 'dflt', c INT64)", false); err != nil {
		t.Fatal(err)
	}
	dest := ds.Table("truncdefault")
	if err := bqRun(ctx, c, "INSERT INTO `"+ds.DatasetID+".truncdefault` (a, b, c) VALUES (1, 'old', 1), (2, 'old', 2)", false); err != nil {
		t.Fatal(err)
	}
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	q := c.Query("SELECT 5 AS a, 6 AS c UNION ALL SELECT 7, 8")
	q.Dst = dest
	q.WriteDisposition = bigquery.WriteTruncateData
	job, err := q.Run(qctx)
	if err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA: %v", err)
	}
	st, err := job.Wait(qctx)
	if err == nil {
		err = st.Err()
	}
	if err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA of a result without b: %v", err)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT a, b, c FROM `"+ds.DatasetID+".truncdefault` ORDER BY a")); got != "[[5 dflt 6] [7 dflt 8]]" {
		t.Errorf("after WRITE_TRUNCATE_DATA: %s, want [[5 dflt 6] [7 dflt 8]]", got)
	}
	md, err = dest.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != 3 || md.Schema[1].Name != "b" || md.Schema[1].DefaultValueExpression != "'dflt'" {
		t.Errorf("schema kept: %+v", md.Schema)
	}
}

// TestBigQueryPartitionFilterThatPrunes (#1456): with
// require_partition_filter, "There must be at least one predicate that
// only references a partitioning column" (querying-partitioned-tables):
// `day = … AND f = …` is enough, `day = … OR f = …` is 400 ("Cannot query
// over table … without a filter that can be used for partition
// elimination"), as is a filter that cannot prune: the column compared
// with another column or a subquery, in arithmetic, or in a function
// other than those the page lists; EXTRACT(YEAR …), DATE_SUB of a
// constant and a parenthesized OR of the column alone do. Before, any
// WHERE naming the column was enough. And a STRUCT field's data_policies
// stays 501: Google documents data policies on a column only.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryPartitionFilterThatPrunes(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".pruned`"
	if err := bqRun(ctx, c, "CREATE TABLE "+ref+" (day DATE, f STRING, other DATE) PARTITION BY day "+
		"OPTIONS (require_partition_filter = TRUE)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ref+" (day, f, other) VALUES (DATE '2024-01-01', 'a', DATE '2024-01-01'), "+
		"(DATE '2024-01-02', 'b', DATE '2024-01-01')", false); err != nil {
		t.Fatal(err)
	}
	refusal := "Cannot query over table '" + project + "." + ds.DatasetID + ".pruned' without a filter"
	for where, refused := range map[string]bool{
		"day = DATE '2024-01-01' AND f = 'a'":                              false,
		"day = DATE '2024-01-01' OR f = 'a'":                               true,
		"f = 'a' AND (day = DATE '2024-01-01' OR day = DATE '2024-01-02')": false,
		"day >= other": true,
		"day = (SELECT MAX(other) FROM " + ref + ")":        true,
		"DATE_ADD(day, INTERVAL 1 DAY) > DATE '2024-01-01'": false,
		"day > DATE_SUB(DATE '2024-01-02', INTERVAL 1 DAY)": false,
		"EXTRACT(YEAR FROM day) = 2024":                     false,
		"EXTRACT(MONTH FROM day) = 1":                       true,
		"ABS(DATE_DIFF(day, DATE '2024-01-01', DAY)) < 1":   true,
	} {
		for _, insert := range []bool{false, true} {
			err := bqRun(ctx, c, "SELECT f FROM "+ref+" WHERE "+where, insert)
			switch {
			case refused && (err == nil || !strings.Contains(err.Error(), refusal)):
				t.Errorf("insert %v: WHERE %s: %v, want 400 %q", insert, where, err, refusal)
			case !refused && err != nil:
				t.Errorf("insert %v: WHERE %s: %v, want rows", insert, where, err)
			}
			if refused && !insert && httpStatus(err) != http.StatusBadRequest {
				t.Errorf("WHERE %s: HTTP %d, want 400", where, httpStatus(err))
			}
		}
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT f FROM "+ref+" WHERE day = DATE '2024-01-01' AND f = 'a'")); got != "[[a]]" {
		t.Errorf("rows: %s", got)
	}

	sql := "CREATE TABLE `" + ds.DatasetID + ".fieldpolicy` (s STRUCT<x STRING OPTIONS (data_policies = " +
		"[\"{'name':'" + project + ".region-us.p0'}\"])>)"
	for _, insert := range []bool{false, true} {
		err := bqRun(ctx, c, sql, insert)
		if err == nil || !strings.Contains(err.Error(), "data_policies") || !insert && httpStatus(err) != http.StatusNotImplemented {
			t.Errorf("insert %v: a STRUCT field's data_policies: %v, want 501", insert, err)
		}
	}
}
