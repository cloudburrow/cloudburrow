//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryColumnsOfEveryType (#1490): a dataset's and the region's
// INFORMATION_SCHEMA.COLUMNS and COLUMN_FIELD_PATHS, through jobs.query
// and jobs.insert, list every column of a table of nested, GEOGRAPHY,
// JSON, RANGE, INTERVAL, parameterized and collated columns and of an
// ingestion-time partitioned table, the RANGE and parameterized columns
// made both by tables.insert and by DDL (#1492, #1493), with the
// data_type strings BigQuery
// documents (https://cloud.google.com/bigquery/docs/information-schema-columns,
// .../information-schema-column-field-paths). Measured first: COLUMNS of
// a dataset (or region) holding the ingestion-time partitioned table
// failed 400 "Array element type STRUCT<STRING, STRING, STRING, ...> does
// not coerce to STRUCT<table_catalog STRING, ...>", whatever its other
// tables' column types: the pseudo-columns' rows were a value short.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryColumnsOfEveryType(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	if err := ds.Table("wide").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "arr", Type: bigquery.IntegerFieldType, Repeated: true},
		{Name: "st", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
			{Name: "a", Type: bigquery.StringFieldType},
			{Name: "b", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{
				{Name: "c", Type: bigquery.IntegerFieldType},
			}},
		}},
		{Name: "geo", Type: bigquery.GeographyFieldType},
		{Name: "js", Type: bigquery.JSONFieldType},
		{Name: "iv", Type: bigquery.IntervalFieldType},
		{Name: "ps", Type: bigquery.StringFieldType, MaxLength: 10},
		{Name: "pb", Type: bigquery.BytesFieldType, MaxLength: 5},
		{Name: "pn", Type: bigquery.NumericFieldType, Precision: 5, Scale: 2},
		{Name: "pbn", Type: bigquery.BigNumericFieldType, Precision: 40, Scale: 10},
		{Name: "coll", Type: bigquery.StringFieldType, Collation: "und:ci"},
	}}); err != nil {
		t.Fatal(err)
	}
	// RANGE columns made both ways: by DDL, and by tables.insert (which
	// failed 500 "Type not found: TYPE_UNKNOWN" before #1492).
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".rng` (r RANGE<DATE>, ts RANGE<TIMESTAMP>)", false); err != nil {
		t.Fatal(err)
	}
	if err := ds.Table("rngapi").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "r", Type: bigquery.RangeFieldType, RangeElementType: &bigquery.RangeElementType{Type: bigquery.DateFieldType}},
		{Name: "ts", Type: bigquery.RangeFieldType, RangeElementType: &bigquery.RangeElementType{Type: bigquery.TimestampFieldType}},
	}}); err != nil {
		t.Fatal(err)
	}
	// Parameterized columns made by DDL too, as by tables.insert above
	// (CREATE TABLE dropped a STRING's or BYTES's maxLength before #1493).
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".wideddl` (ps STRING(10), pb BYTES(5), pn NUMERIC(5, 2), "+
		"pbn BIGNUMERIC(40, 10))", false); err != nil {
		t.Fatal(err)
	}
	if err := ds.Table("ing").Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{
			{Name: "g", Type: bigquery.GeographyFieldType},
			{Name: "j", Type: bigquery.JSONFieldType},
		},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType},
	}); err != nil {
		t.Fatal(err)
	}
	wantColumns := []string{
		"ing._PARTITIONDATE DATE", "ing._PARTITIONTIME TIMESTAMP", "ing.g GEOGRAPHY", "ing.j JSON",
		"wide.arr ARRAY<INT64>", "wide.coll STRING", "wide.geo GEOGRAPHY", "wide.iv INTERVAL", "wide.js JSON",
		"wide.pb BYTES(5)", "wide.pbn BIGNUMERIC(40, 10)", "wide.pn NUMERIC(5, 2)", "wide.ps STRING(10)",
		"rng.r RANGE<DATE>", "rng.ts RANGE<TIMESTAMP>", "wide.st STRUCT<a STRING, b ARRAY<STRUCT<c INT64>>>",
		"rngapi.r RANGE<DATE>", "rngapi.ts RANGE<TIMESTAMP>", "wideddl.pb BYTES(5)", "wideddl.pbn BIGNUMERIC(40, 10)",
		"wideddl.pn NUMERIC(5, 2)", "wideddl.ps STRING(10)",
	}
	wantPaths := []string{
		"ing.g GEOGRAPHY", "ing.j JSON",
		"wide.arr ARRAY<INT64>", "wide.coll STRING", "wide.geo GEOGRAPHY", "wide.iv INTERVAL", "wide.js JSON",
		"wide.pb BYTES(5)", "wide.pbn BIGNUMERIC(40, 10)", "wide.pn NUMERIC(5, 2)", "wide.ps STRING(10)",
		"rng.r RANGE<DATE>", "rng.ts RANGE<TIMESTAMP>", "wide.st STRUCT<a STRING, b ARRAY<STRUCT<c INT64>>>", "wide.st.a STRING",
		"wide.st.b ARRAY<STRUCT<c INT64>>", "wide.st.b.c INT64",
		"rngapi.r RANGE<DATE>", "rngapi.ts RANGE<TIMESTAMP>", "wideddl.pb BYTES(5)", "wideddl.pbn BIGNUMERIC(40, 10)",
		"wideddl.pn NUMERIC(5, 2)", "wideddl.ps STRING(10)",
	}
	sort.Strings(wantColumns)
	sort.Strings(wantPaths)
	for _, from := range []string{"`" + ds.DatasetID + "`", "`region-us`", "`" + project + "`.`region-us`"} {
		for _, insert := range []bool{false, true} {
			for _, v := range []struct {
				view, name string
				want       []string
			}{{"COLUMNS", "column_name", wantColumns}, {"COLUMN_FIELD_PATHS", "field_path", wantPaths}} {
				// SELECT * reads every column of the row type, as a client
				// listing a schema does; the projection reads the strings.
				what := fmt.Sprintf("%s.INFORMATION_SCHEMA.%s (insert %v)", from, v.view, insert)
				where := " WHERE table_schema = '" + ds.DatasetID + "'"
				if _, _, err := infoSchemaRead(ctx, c, "SELECT * FROM "+from+".INFORMATION_SCHEMA."+v.view+where, insert); err != nil {
					t.Errorf("SELECT * of %s: %v", what, err)
					continue
				}
				_, rows, err := infoSchemaRead(ctx, c, "SELECT table_name, "+v.name+", data_type, collation_name FROM "+
					from+".INFORMATION_SCHEMA."+v.view+where, insert)
				if err != nil {
					t.Errorf("%s: %v", what, err)
					continue
				}
				var got []string
				for _, r := range rows {
					got = append(got, fmt.Sprintf("%v.%v %v", r[0], r[1], r[2]))
					if coll := fmt.Sprint(r[3]); (r[1] == "coll") != (coll == "und:ci") {
						t.Errorf("%s: collation_name of %v.%v is %v, want und:ci for coll alone", what, r[0], r[1], r[3])
					}
				}
				sort.Strings(got)
				if strings.Join(got, "\n") != strings.Join(v.want, "\n") {
					t.Errorf("%s:\n got %q\nwant %q", what, got, v.want)
				}
			}
		}
	}
}
