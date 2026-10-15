//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// infoSchemaRead runs sql through jobs.query or (insert) jobs.insert and
// returns its schema and rows.
func infoSchemaRead(ctx context.Context, c *bigquery.Client, sql string, insert bool) (bigquery.Schema, [][]bigquery.Value, error) {
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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

// infoSchemaColumns is a schema's columns as "name TYPE, ...".
func infoSchemaColumns(schema bigquery.Schema) string {
	var got []string
	for _, f := range schema {
		typ := string(f.Type)
		if f.Repeated {
			typ = "ARRAY<" + typ + ">"
		}
		got = append(got, f.Name+" "+typ)
	}
	return strings.Join(got, ", ")
}

// TestBigQueryInformationSchemaPartitionsAndConstraints (#1328): a
// dataset's INFORMATION_SCHEMA PARTITIONS, TABLE_CONSTRAINTS,
// KEY_COLUMN_USAGE, CONSTRAINT_COLUMN_USAGE, TABLE_SNAPSHOTS,
// ROUTINE_OPTIONS and SEARCH_INDEXES, through jobs.query and jobs.insert,
// have BigQuery's documented columns (PARTITIONS but its byte counts and
// last_modified_time, which the emulator keeps none of) and a row for
// what the dataset holds: a DATE-partitioned table, an integer-range
// partitioned one, a table with a primary key, one with a foreign key, a
// snapshot and a function with a description. Measured first: each was
// 400 "Table not found" in the engine and 501 in the front.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInformationSchemaPartitionsAndConstraints(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	for _, sql := range []string{
		"CREATE TABLE " + d + ".p (d DATE, n INT64) PARTITION BY d",
		"INSERT INTO " + d + ".p (d, n) VALUES (DATE '2024-01-01', 1), (DATE '2024-01-01', 2), (DATE '2024-01-02', 3), (NULL, 4)",
		"CREATE TABLE " + d + ".r (n INT64) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10))",
		"INSERT INTO " + d + ".r (n) VALUES (5), (15), (200)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := ds.Table("u").Create(ctx, &bigquery.TableMetadata{
		Schema:           bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "k", Type: bigquery.StringFieldType}},
		TableConstraints: &bigquery.TableConstraints{PrimaryKey: &bigquery.PrimaryKey{Columns: []string{"id"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+d+".u (id, k) VALUES (1, 'a'), (2, 'b')", false); err != nil {
		t.Fatal(err)
	}
	if err := ds.Table("o").Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{{Name: "uid", Type: bigquery.IntegerFieldType}},
		TableConstraints: &bigquery.TableConstraints{ForeignKeys: []*bigquery.ForeignKey{{
			Name: "fk_u", ReferencedTable: ds.Table("u"),
			ColumnReferences: []*bigquery.ColumnReference{{ReferencingColumn: "uid", ReferencedColumn: "id"}},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "CREATE SNAPSHOT TABLE "+d+".snap CLONE "+d+".u", false); err != nil {
		t.Fatal(err)
	}
	if err := ds.Routine("f").Create(ctx, &bigquery.RoutineMetadata{
		Type: "SCALAR_FUNCTION", Language: "SQL", Body: "x + 1", Description: "adds one",
		Arguments: []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
	}); err != nil {
		t.Fatal(err)
	}
	from := func(view string) string { return d + ".INFORMATION_SCHEMA." + view }
	str := func(row []bigquery.Value) string {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprint(v)
		}
		return strings.Join(parts, "|")
	}
	check := func(insert bool, view, sql, columns string, want ...string) {
		t.Helper()
		schema, rows, err := infoSchemaRead(ctx, c, sql, insert)
		if err != nil {
			t.Errorf("insert=%v: %s: %v", insert, view, err)
			return
		}
		if columns != "" && infoSchemaColumns(schema) != columns {
			t.Errorf("insert=%v: %s columns:\n got %s\nwant %s", insert, view, infoSchemaColumns(schema), columns)
		}
		var got []string
		for _, r := range rows {
			got = append(got, str(r))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("insert=%v: %s rows:\n got %s\nwant %s", insert, view, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	for _, insert := range []bool{false, true} {
		check(insert, "PARTITIONS", "SELECT * FROM "+from("PARTITIONS")+" ORDER BY table_name, partition_id",
			"table_catalog STRING, table_schema STRING, table_name STRING, partition_id STRING, total_rows INTEGER, storage_tier STRING",
			project+"|"+ds.DatasetID+"|o|<nil>|0|ACTIVE",
			project+"|"+ds.DatasetID+"|p|20240101|2|ACTIVE",
			project+"|"+ds.DatasetID+"|p|20240102|1|ACTIVE",
			project+"|"+ds.DatasetID+"|p|__NULL__|1|ACTIVE",
			project+"|"+ds.DatasetID+"|r|0|1|ACTIVE",
			project+"|"+ds.DatasetID+"|r|10|1|ACTIVE",
			project+"|"+ds.DatasetID+"|r|__UNPARTITIONED__|1|ACTIVE",
			project+"|"+ds.DatasetID+"|snap|<nil>|2|ACTIVE",
			project+"|"+ds.DatasetID+"|u|<nil>|2|ACTIVE")
		check(insert, "TABLE_CONSTRAINTS", "SELECT constraint_name, table_name, constraint_type, is_deferrable, initially_deferred, enforced FROM "+
			from("TABLE_CONSTRAINTS")+" ORDER BY table_name", "",
			"fk_u|o|FOREIGN KEY|NO|NO|NO", "u.pk$|u|PRIMARY KEY|NO|NO|NO")
		check(insert, "KEY_COLUMN_USAGE", "SELECT * FROM "+from("KEY_COLUMN_USAGE")+" ORDER BY table_name",
			"constraint_catalog STRING, constraint_schema STRING, constraint_name STRING, table_catalog STRING, table_schema STRING, "+
				"table_name STRING, column_name STRING, ordinal_position INTEGER, position_in_unique_constraint INTEGER",
			project+"|"+ds.DatasetID+"|fk_u|"+project+"|"+ds.DatasetID+"|o|uid|1|1",
			project+"|"+ds.DatasetID+"|u.pk$|"+project+"|"+ds.DatasetID+"|u|id|1|<nil>")
		check(insert, "CONSTRAINT_COLUMN_USAGE", "SELECT table_name, column_name, constraint_name FROM "+from("CONSTRAINT_COLUMN_USAGE")+
			" ORDER BY constraint_name", "", "u|id|fk_u", "u|id|u.pk$")
		check(insert, "TABLE_SNAPSHOTS", "SELECT table_name, base_table_catalog, base_table_schema, base_table_name, snapshot_time IS NOT NULL FROM "+
			from("TABLE_SNAPSHOTS"), "", "snap|"+project+"|"+ds.DatasetID+"|u|true")
		check(insert, "ROUTINE_OPTIONS", "SELECT * FROM "+from("ROUTINE_OPTIONS"),
			"specific_catalog STRING, specific_schema STRING, specific_name STRING, option_name STRING, option_type STRING, option_value STRING",
			project+"|"+ds.DatasetID+"|f|description|STRING|\"adds one\"")
		check(insert, "SEARCH_INDEXES", "SELECT index_name FROM "+from("SEARCH_INDEXES"), "")
		check(insert, "VECTOR_INDEXES", "SELECT index_name FROM "+from("VECTOR_INDEXES"), "")
	}
	// The instance makes no search index, so SEARCH_INDEXES is empty.
	if err := bqRun(ctx, c, "CREATE SEARCH INDEX ix ON "+d+".u (ALL COLUMNS)", false); err == nil {
		t.Errorf("CREATE SEARCH INDEX succeeded; SEARCH_INDEXES would not list it")
	}
}
