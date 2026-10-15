//go:build compat

package compat

import (
	"testing"
)

// TestBigQueryInformationSchemaRegionViews (#1329): the region-qualified
// forms of a dataset's views list every dataset's rows, and
// `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE gives each table's row
// count (its byte counts left out). Measured first: each was 400 "Table
// not found".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInformationSchemaRegionViews(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	for _, sql := range []string{"CREATE TABLE " + d + ".t (n INT64)", "INSERT INTO " + d + ".t (n) VALUES (1), (2), (3)",
		"CREATE VIEW " + d + ".v AS SELECT 1 AS one"} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, insert := range []bool{false, true} {
		_, rows, err := infoSchemaRead(ctx, c, "SELECT table_name, table_type FROM `region-us`.INFORMATION_SCHEMA.TABLES "+
			"WHERE table_schema = '"+ds.DatasetID+"' ORDER BY table_name", insert)
		if err != nil || len(rows) != 2 || rows[0][0] != "t" || rows[0][1] != "BASE TABLE" || rows[1][0] != "v" || rows[1][1] != "VIEW" {
			t.Errorf("insert=%v: region TABLES gave %v, %v", insert, rows, err)
		}
		_, rows, err = infoSchemaRead(ctx, c, "SELECT column_name, data_type FROM `"+project+"`.`region-us`.INFORMATION_SCHEMA.COLUMNS "+
			"WHERE table_schema = '"+ds.DatasetID+"' AND table_name = 't'", insert)
		if err != nil || len(rows) != 1 || rows[0][0] != "n" || rows[0][1] != "INT64" {
			t.Errorf("insert=%v: region COLUMNS gave %v, %v", insert, rows, err)
		}
		schema, rows, err := infoSchemaRead(ctx, c, "SELECT * FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE "+
			"WHERE table_schema = '"+ds.DatasetID+"'", insert)
		if err != nil {
			t.Fatalf("insert=%v: TABLE_STORAGE: %v", insert, err)
		}
		if want := "project_id STRING, project_number INTEGER, table_catalog STRING, table_schema STRING, table_name STRING, " +
			"creation_time TIMESTAMP, total_rows INTEGER, deleted BOOLEAN, table_type STRING, managed_table_type STRING, " +
			"last_metadata_index_refresh_time TIMESTAMP, table_deletion_reason STRING, table_deletion_time TIMESTAMP"; infoSchemaColumns(schema) != want {
			t.Errorf("TABLE_STORAGE columns:\n got %s\nwant %s", infoSchemaColumns(schema), want)
		}
		if len(rows) != 1 || rows[0][4] != "t" || rows[0][6] != int64(3) || rows[0][7] != false || rows[0][8] != "BASE TABLE" {
			t.Errorf("insert=%v: TABLE_STORAGE rows %v", insert, rows)
		}
	}
}
