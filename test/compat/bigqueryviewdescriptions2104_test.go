//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryViewColumnDescriptions (#2104): a view's column
// descriptions are set through tables.patch of its schema, the official Go
// client's Table.Update, and by ALTER VIEW ... ALTER COLUMN ... SET
// OPTIONS (description = ...), which the DDL reference documents as
// "equivalent to the schema.fields[].description table resource property"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_options_statement);
// both are read back by Table.Metadata, and the view still answers its
// query. A patch that changes a view's columns otherwise (a name), which
// Google documents no answer for, stays 501.
//
// covers: bigquery.tables.patch, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryViewColumnDescriptions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := "compat_viewdesc_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(h.Context()) })
	if err := mlRun(h, c, fmt.Sprintf("CREATE VIEW `%s.v` AS SELECT 1 AS id, 'a' AS name", ds)); err != nil {
		t.Fatalf("CREATE VIEW: %v", err)
	}
	v := c.Dataset(ds).Table("v")
	md, err := v.Metadata(ctx)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	schema := bigquery.Schema{}
	for _, f := range md.Schema {
		cp := *f
		if cp.Name == "id" {
			cp.Description = "the row's id"
		}
		schema = append(schema, &cp)
	}
	if _, err := v.Update(ctx, bigquery.TableMetadataToUpdate{Schema: schema}, md.ETag); err != nil {
		t.Fatalf("Table.Update of the view's descriptions: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("ALTER VIEW `%s.v` ALTER COLUMN name SET OPTIONS (description = \"the row's name\")", ds)); err != nil {
		t.Fatalf("ALTER VIEW ALTER COLUMN SET OPTIONS: %v", err)
	}
	md, err = v.Metadata(ctx)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	got := map[string]string{}
	for _, f := range md.Schema {
		got[f.Name] = f.Description
	}
	if got["id"] != "the row's id" || got["name"] != "the row's name" || md.Type != bigquery.ViewTable {
		t.Errorf("the view's columns %v (type %s)", got, md.Type)
	}
	it, err := c.Query(fmt.Sprintf("SELECT id, name FROM `%s.v`", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("query the view: %v", err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || row[0] != int64(1) || row[1] != "a" {
		t.Errorf("the view's row %v %v", row, err)
	}
	renamed := bigquery.Schema{{Name: "ident", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}}
	if _, err := v.Update(ctx, bigquery.TableMetadataToUpdate{Schema: renamed}, ""); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("a patch renaming a view's column: %v, want 501", err)
	}
}
