//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryInformationSchemaSchemataOptions (#1351): a dataset's
// options read back through INFORMATION_SCHEMA.SCHEMATA_OPTIONS, dataset-
// and region-qualified, with BigQuery's documented columns, and
// MATERIALIZED_VIEWS is empty (the engine makes no materialized view).
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
		for _, from := range []string{ds.DatasetID + ".INFORMATION_SCHEMA.SCHEMATA_OPTIONS",
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
