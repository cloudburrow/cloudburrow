//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryInformationSchemaDatasetViews (#1286): a dataset's
// INFORMATION_SCHEMA TABLES, COLUMNS, COLUMN_FIELD_PATHS, TABLE_OPTIONS,
// ROUTINES and PARAMETERS, through jobs.query and jobs.insert, have
// BigQuery's documented columns and types and a row for what the dataset
// holds: a partitioned, clustered table with a description, labels and a
// nested field, a view and a function. COLUMNS' async_generation_status
// is NULL, as of every column not a generated embedding one (#1458).
// SCHEMATA_LINKS dataset-qualified is 400 (#1458). Measured first:
// TABLES had 7 columns, creation_time a STRING; TABLE_OPTIONS was empty;
// ROUTINES, PARAMETERS and COLUMN_FIELD_PATHS were 400 "Table not found".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInformationSchemaDatasetViews(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	if err := ds.Table("t").Create(ctx, &bigquery.TableMetadata{
		Description: "the table",
		Labels:      map[string]string{"team": "data"},
		Schema: bigquery.Schema{
			{Name: "d", Type: bigquery.DateFieldType, Required: true},
			{Name: "s", Type: bigquery.StringFieldType, Description: "a string"},
			{Name: "r", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{
				{Name: "x", Type: bigquery.IntegerFieldType, Description: "nested"},
			}},
		},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"},
		Clustering:       &bigquery.Clustering{Fields: []string{"s"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ds.Table("v").Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT 1 AS one"}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "CREATE FUNCTION `"+ds.DatasetID+".f`(x INT64) RETURNS INT64 AS (x + 1)", false); err != nil {
		t.Fatal(err)
	}
	read := func(sql string, insert bool) (bigquery.Schema, [][]bigquery.Value, error) {
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		q := c.Query(sql)
		var it *bigquery.RowIterator
		var err error
		if insert {
			job, err := q.Run(qctx)
			if err != nil {
				return nil, nil, err
			}
			if it, err = job.Read(qctx); err != nil {
				return nil, nil, err
			}
		} else if it, err = q.Read(qctx); err != nil {
			return nil, nil, err
		}
		var rows [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return it.Schema, rows, nil
			}
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, row)
		}
	}
	// columns checks a view's column names and types, as BigQuery documents them.
	columns := func(view string, schema bigquery.Schema, want string) {
		t.Helper()
		var got []string
		for _, f := range schema {
			typ := string(f.Type)
			if f.Repeated {
				typ = "ARRAY<" + typ + ">"
			}
			got = append(got, f.Name+" "+typ)
		}
		if strings.Join(got, ", ") != want {
			t.Errorf("%s columns:\n got %s\nwant %s", view, strings.Join(got, ", "), want)
		}
	}
	from := func(view string) string {
		return "`" + project + "." + ds.DatasetID + ".INFORMATION_SCHEMA." + view + "`"
	}
	for _, insert := range []bool{false, true} {
		schema, rows, err := read("SELECT * FROM "+from("TABLES")+" ORDER BY table_name", insert)
		if err != nil {
			t.Fatalf("insert=%v: TABLES: %v", insert, err)
		}
		columns("TABLES", schema, "table_catalog STRING, table_schema STRING, table_name STRING, table_type STRING, "+
			"managed_table_type STRING, is_insertable_into STRING, is_fine_grained_mutations_enabled STRING, is_typed STRING, "+
			"is_change_history_enabled STRING, creation_time TIMESTAMP, base_table_catalog STRING, base_table_schema STRING, "+
			"base_table_name STRING, snapshot_time_ms TIMESTAMP, replica_source_catalog STRING, replica_source_schema STRING, "+
			"replica_source_name STRING, replication_status STRING, replication_error STRING, ddl STRING, "+
			"default_collation_name STRING, sync_status JSON, upsert_stream_apply_watermark TIMESTAMP")
		if len(rows) != 2 || rows[0][2] != "t" || rows[0][3] != "BASE TABLE" || rows[1][2] != "v" || rows[1][3] != "VIEW" ||
			rows[0][0] != project || rows[0][1] != ds.DatasetID || rows[0][5] != "YES" || rows[1][5] != "NO" {
			t.Fatalf("insert=%v: TABLES rows %v", insert, rows)
		}
		if ct, ok := rows[0][9].(time.Time); !ok || advanceBigQueryClock(t, h, 0).Sub(ct) > time.Hour || advanceBigQueryClock(t, h, 0).Sub(ct) < -time.Hour { // #1487
			t.Errorf("insert=%v: TABLES creation_time %v", insert, rows[0][9])
		}
		if ddl, _ := rows[0][19].(string); !strings.HasPrefix(ddl, "CREATE TABLE `"+project+"."+ds.DatasetID+".t`") ||
			!strings.Contains(ddl, "PARTITION BY d") || !strings.Contains(ddl, "CLUSTER BY s") {
			t.Errorf("insert=%v: TABLES ddl %q", insert, rows[0][19])
		}

		schema, rows, err = read("SELECT * FROM "+from("COLUMNS")+" WHERE table_name = 't' ORDER BY ordinal_position", insert)
		if err != nil {
			t.Fatalf("insert=%v: COLUMNS: %v", insert, err)
		}
		columns("COLUMNS", schema, "table_catalog STRING, table_schema STRING, table_name STRING, column_name STRING, "+
			"ordinal_position INTEGER, is_nullable STRING, data_type STRING, is_generated STRING, generation_expression STRING, "+
			"is_stored STRING, async_generation_status RECORD, is_hidden STRING, is_updatable STRING, is_system_defined STRING, "+
			"is_partitioning_column STRING, "+
			"clustering_ordinal_position INTEGER, collation_name STRING, column_default STRING, rounding_mode STRING, "+
			"data_policies ARRAY<RECORD>, policy_tags ARRAY<STRING>, is_identity STRING, identity_generation STRING, "+
			"identity_start INTEGER, identity_increment INTEGER, identity_maximum INTEGER, identity_minimum INTEGER, identity_cycle STRING")
		if len(rows) != 3 {
			t.Fatalf("insert=%v: COLUMNS rows %v", insert, rows)
		}
		for i, want := range []struct {
			name, nullable, typ, part string
			cluster                   bigquery.Value
		}{
			{"d", "NO", "DATE", "YES", nil}, {"s", "YES", "STRING", "NO", int64(1)}, {"r", "YES", "ARRAY<STRUCT<x INT64>>", "NO", nil},
		} {
			r := rows[i]
			if r[3] != want.name || r[4] != int64(i+1) || r[5] != want.nullable || r[6] != want.typ || r[14] != want.part || r[15] != want.cluster ||
				r[10] != nil {
				t.Errorf("insert=%v: COLUMNS row %d = %v", insert, i, r)
			}
		}

		schema, rows, err = read("SELECT * FROM "+from("COLUMN_FIELD_PATHS")+" WHERE table_name = 't' ORDER BY field_path", insert)
		if err != nil {
			t.Fatalf("insert=%v: COLUMN_FIELD_PATHS: %v", insert, err)
		}
		columns("COLUMN_FIELD_PATHS", schema, "table_catalog STRING, table_schema STRING, table_name STRING, column_name STRING, "+
			"field_path STRING, data_type STRING, description STRING, collation_name STRING, rounding_mode STRING, "+
			"data_policies ARRAY<RECORD>, policy_tags ARRAY<STRING>")
		if len(rows) != 4 || rows[2][4] != "r.x" || rows[2][3] != "r" || rows[2][5] != "INT64" || rows[2][6] != "nested" ||
			rows[3][4] != "s" || rows[3][6] != "a string" {
			t.Errorf("insert=%v: COLUMN_FIELD_PATHS rows %v", insert, rows)
		}

		schema, rows, err = read("SELECT * FROM "+from("TABLE_OPTIONS")+" ORDER BY table_name, option_name", insert)
		if err != nil {
			t.Fatalf("insert=%v: TABLE_OPTIONS: %v", insert, err)
		}
		columns("TABLE_OPTIONS", schema, "table_catalog STRING, table_schema STRING, table_name STRING, option_name STRING, "+
			"option_type STRING, option_value STRING")
		if len(rows) != 2 || rows[0][3] != "description" || rows[0][4] != "STRING" || rows[0][5] != `"the table"` ||
			rows[1][3] != "labels" || rows[1][4] != "ARRAY<STRUCT<STRING, STRING>>" || rows[1][5] != `[STRUCT("team", "data")]` {
			t.Errorf("insert=%v: TABLE_OPTIONS rows %v", insert, rows)
		}

		schema, rows, err = read("SELECT * FROM "+ds.DatasetID+".INFORMATION_SCHEMA.ROUTINES", insert)
		if err != nil {
			t.Fatalf("insert=%v: ROUTINES: %v", insert, err)
		}
		columns("ROUTINES", schema, "specific_catalog STRING, specific_schema STRING, specific_name STRING, routine_catalog STRING, "+
			"routine_schema STRING, routine_name STRING, routine_type STRING, data_type STRING, routine_body STRING, "+
			"routine_definition STRING, external_language STRING, is_deterministic STRING, security_type STRING, "+
			"created TIMESTAMP, last_altered TIMESTAMP, ddl STRING, connection STRING")
		if len(rows) != 1 || rows[0][5] != "f" || rows[0][6] != "FUNCTION" || rows[0][7] != "INT64" || rows[0][8] != "SQL" ||
			rows[0][9] != "x + 1" {
			t.Errorf("insert=%v: ROUTINES rows %v", insert, rows)
		}

		schema, rows, err = read("SELECT * FROM "+ds.DatasetID+".INFORMATION_SCHEMA.PARAMETERS ORDER BY ordinal_position", insert)
		if err != nil {
			t.Fatalf("insert=%v: PARAMETERS: %v", insert, err)
		}
		columns("PARAMETERS", schema, "specific_catalog STRING, specific_schema STRING, specific_name STRING, ordinal_position INTEGER, "+
			"parameter_mode STRING, is_result STRING, parameter_name STRING, data_type STRING, parameter_default STRING, "+
			"is_aggregate STRING")
		if len(rows) != 2 || rows[0][3] != int64(0) || rows[0][5] != "YES" || rows[0][6] != nil || rows[0][7] != "INT64" ||
			rows[1][3] != int64(1) || rows[1][5] != "NO" || rows[1][6] != "x" || rows[1][7] != "INT64" ||
			rows[0][4] != nil || rows[1][4] != nil { // a function's parameter_mode is NULL (#1595)
			t.Errorf("insert=%v: PARAMETERS rows %v", insert, rows)
		}

		// Two views of one dataset, joined.
		_, rows, err = read("SELECT t.table_name, COUNT(c.column_name) FROM "+ds.DatasetID+".INFORMATION_SCHEMA.TABLES t JOIN "+
			ds.DatasetID+".INFORMATION_SCHEMA.COLUMNS c USING (table_name) GROUP BY 1 ORDER BY 1", insert)
		if err != nil || len(rows) != 2 || rows[0][1] != int64(3) {
			t.Errorf("insert=%v: a join gave %v, %v", insert, rows, err)
		}

		// BigQuery documents SCHEMATA_LINKS region-qualified only (#1458).
		_, _, err = read("SELECT * FROM "+ds.DatasetID+".INFORMATION_SCHEMA.SCHEMATA_LINKS", insert)
		var e *googleapi.Error
		if err == nil || !strings.Contains(err.Error(), "must include a region qualifier") ||
			!insert && (!errors.As(err, &e) || e.Code != http.StatusBadRequest) {
			t.Errorf("insert=%v: dataset-qualified SCHEMATA_LINKS gave %v, want 400 naming the region qualifier", insert, err)
		}
		_, _, err = read("SELECT * FROM no_such_dataset_1286.INFORMATION_SCHEMA.TABLES", insert)
		if !errors.As(err, &e) || e.Code != http.StatusNotFound {
			t.Errorf("insert=%v: a dataset that does not exist gave %v, want 404", insert, err)
		}
	}
}
