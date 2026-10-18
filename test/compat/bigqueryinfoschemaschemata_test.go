//go:build compat

package compat

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryInformationSchemaSchemataOptions (#1351): a dataset's
// options read back through INFORMATION_SCHEMA.SCHEMATA_OPTIONS, region-
// qualified and unqualified (the US region's), with BigQuery's documented
// columns; dataset-qualified it is 400 invalidQuery, since its page says
// "Queries against this view must include a region qualifier" and lists
// the project and region scopes only (#1595, read 2026-10-17); and
// MATERIALIZED_VIEWS is empty for a dataset with no materialized view
// (TestBigQueryMaterializedViews lists one, #1390).
// Measured first: each was 400 "Table not found", refused 501 by the front.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInformationSchemaSchemataOptions(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	var u bigquery.DatasetMetadataToUpdate
	u.Description = "the dataset"
	u.DefaultTableExpiration = 36 * time.Hour
	u.SetLabel("team", "data")
	if _, err := ds.Update(ctx, u, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := [][]string{
		{"default_table_expiration_days", "FLOAT64", "1.5"},
		{"description", "STRING", `"the dataset"`},
		{"labels", "ARRAY<STRUCT<STRING, STRING>>", `[STRUCT("team", "data")]`},
	}
	for _, insert := range []bool{false, true} {
		_, _, err := infoSchemaRead(ctx, c, "SELECT * FROM "+ds.DatasetID+".INFORMATION_SCHEMA.SCHEMATA_OPTIONS", insert)
		if errReason(err) != "invalidQuery" || !strings.Contains(err.Error(), "region qualifier") {
			t.Errorf("insert=%v: dataset-qualified SCHEMATA_OPTIONS: %v, want invalidQuery region qualifier", insert, err)
		}
		for _, from := range []string{"INFORMATION_SCHEMA.SCHEMATA_OPTIONS", "`" + project + "`.INFORMATION_SCHEMA.SCHEMATA_OPTIONS",
			"`" + project + "`.`region-us`.INFORMATION_SCHEMA.SCHEMATA_OPTIONS"} {
			schema, rows, err := infoSchemaRead(ctx, c, "SELECT * FROM "+from+
				" WHERE schema_name = '"+ds.DatasetID+"' ORDER BY option_name", insert)
			if err != nil {
				t.Fatalf("insert=%v %s: %v", insert, from, err)
			}
			if got, w := infoSchemaColumns(schema), "catalog_name STRING, schema_name STRING, option_name STRING, "+
				"option_type STRING, option_value STRING"; got != w {
				t.Errorf("%s columns:\n got %s\nwant %s", from, got, w)
			}
			if len(rows) != len(want) {
				t.Fatalf("insert=%v %s: rows %v, want %v", insert, from, rows, want)
			}
			for i, w := range want {
				if rows[i][0] != project || rows[i][1] != ds.DatasetID || rows[i][2] != w[0] || rows[i][3] != w[1] || rows[i][4] != w[2] {
					t.Errorf("insert=%v %s: row %d %v, want %v", insert, from, i, rows[i], w)
				}
			}
		}
		for _, from := range []string{ds.DatasetID + ".INFORMATION_SCHEMA.MATERIALIZED_VIEWS",
			"`region-us`.INFORMATION_SCHEMA.MATERIALIZED_VIEWS"} {
			schema, rows, err := infoSchemaRead(ctx, c, "SELECT * FROM "+from, insert)
			if err != nil || len(rows) != 0 {
				t.Errorf("insert=%v %s: %v, %v; want no rows", insert, from, rows, err)
				continue
			}
			if got, w := infoSchemaColumns(schema), "table_catalog STRING, table_schema STRING, table_name STRING, "+
				"last_refresh_time TIMESTAMP, refresh_watermark TIMESTAMP, last_refresh_status RECORD"; got != w {
				t.Errorf("%s columns:\n got %s\nwant %s", from, got, w)
			}
		}
	}
}
