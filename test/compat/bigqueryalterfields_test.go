//go:build compat

package compat

import (
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryAlterTableFieldsKeysAndPolicies (#1389): the ALTER TABLE
// forms that were 501, as BigQuery's DDL reference documents them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_table_rename_column_statement
// and the statements around it), through jobs.query and jobs.insert.
//
// Refused 400, as the reference says BigQuery refuses them: an action on
// a nested field ("Modifying subfields ... is not supported"), RENAME TO a
// path ("must only include the name of the table"), renaming a clustering
// column or a primary key's column, dropping a primary key's column, a
// default on a primary key's column, RENAME COLUMN with another action,
// and renaming or dropping a column of a table with row access policies.
//
// Carried out: actions of different kinds in one statement (ADD COLUMN,
// DROP COLUMN of a column outside the key, SET DATA TYPE of a STRUCT to
// one of wider fields, NUMERIC to BIGNUMERIC(40, 20), SET OPTIONS), the
// key kept; SET DATA TYPE of a table with a row access policy, which keeps
// filtering its rows; and RENAME TO with SET OPTIONS, the renamed table
// keeping its rows, key and policy. Each read back through Table.Metadata
// and the table's rows.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tabledata.list
func TestBigQueryAlterTableFieldsKeysAndPolicies(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := ds.Table("fields_t")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{
			{Name: "id", Type: bigquery.IntegerFieldType},
			{Name: "amount", Type: bigquery.IntegerFieldType},
			{Name: "s", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
				{Name: "a", Type: bigquery.IntegerFieldType, Description: "kept"},
				{Name: "b", Type: bigquery.StringFieldType},
			}},
			{Name: "price", Type: bigquery.NumericFieldType},
			{Name: "note", Type: bigquery.StringFieldType},
			{Name: "gone", Type: bigquery.StringFieldType},
		},
		Clustering: &bigquery.Clustering{Fields: []string{"note"}},
	}); err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".fields_t`"
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	count := func(table string) int64 {
		t.Helper()
		it, err := c.Query("SELECT COUNT(*) FROM `" + ds.DatasetID + "." + table + "`").Read(ctx)
		if err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			t.Fatal(err)
		}
		return row[0].(int64)
	}
	run("INSERT INTO "+ref+" (id, amount, s, price, note, gone) VALUES "+
		"(1, 5, STRUCT(1, 'x'), 1.5, 'n1', 'g'), (2, 9, STRUCT(2, 'y'), 2.25, 'n2', 'h')", false)
	run("ALTER TABLE "+ref+" ADD PRIMARY KEY (id) NOT ENFORCED", false)

	for sql, code := range map[string]int{
		"ALTER TABLE " + ref + " DROP COLUMN s.a":                                         http.StatusBadRequest,
		"ALTER TABLE " + ref + " RENAME COLUMN s.a TO c":                                  http.StatusBadRequest,
		"ALTER TABLE " + ref + " ALTER COLUMN s.a SET DATA TYPE NUMERIC":                  http.StatusBadRequest,
		"ALTER TABLE " + ref + " ALTER COLUMN s.b SET OPTIONS (description = 'd')":        http.StatusBadRequest,
		"ALTER TABLE " + ref + " ALTER COLUMN s.b DROP NOT NULL":                          http.StatusBadRequest,
		"ALTER TABLE " + ref + " RENAME TO " + ds.DatasetID + ".elsewhere":                http.StatusBadRequest,
		"ALTER TABLE " + ref + " RENAME COLUMN note TO n":                                 http.StatusBadRequest,
		"ALTER TABLE " + ref + " RENAME COLUMN id TO k":                                   http.StatusBadRequest,
		"ALTER TABLE " + ref + " DROP COLUMN id":                                          http.StatusBadRequest,
		"ALTER TABLE " + ref + " ALTER COLUMN id SET DEFAULT 1":                           http.StatusBadRequest,
		"ALTER TABLE " + ref + " RENAME COLUMN amount TO amt, ADD COLUMN z INT64":         http.StatusBadRequest,
		"ALTER TABLE " + ref + " ALTER COLUMN s SET DATA TYPE STRUCT<a STRING, b STRING>": http.StatusBadRequest,
	} {
		wantHTTPStatus(t, sql, bqRun(ctx, c, sql, false), code)
	}

	// Actions of different kinds in one statement, through jobs.insert.
	run("ALTER TABLE "+ref+" ADD COLUMN extra STRING, DROP COLUMN gone, "+
		"ALTER COLUMN s SET DATA TYPE STRUCT<a NUMERIC, b STRING>, ALTER COLUMN price SET DATA TYPE BIGNUMERIC(40, 20), "+
		"SET OPTIONS (description = 'mixed')", true)
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*bigquery.FieldSchema{}
	for _, f := range md.Schema {
		byName[f.Name] = f
	}
	if byName["gone"] != nil || byName["extra"] == nil || byName["s"] == nil || len(byName["s"].Schema) != 2 ||
		byName["s"].Schema[0].Type != bigquery.NumericFieldType || byName["s"].Schema[0].Description != "kept" ||
		byName["price"] == nil || byName["price"].Type != bigquery.BigNumericFieldType || byName["price"].Precision != 40 ||
		byName["price"].Scale != 20 || md.Description != "mixed" {
		t.Errorf("after the mixed ALTER TABLE: %+v, description %q", md.Schema, md.Description)
	}
	if tc := md.TableConstraints; tc == nil || tc.PrimaryKey == nil || len(tc.PrimaryKey.Columns) != 1 {
		t.Errorf("the primary key was not kept: %+v", md.TableConstraints)
	}
	if md.Clustering == nil || len(md.Clustering.Fields) != 1 || md.Clustering.Fields[0] != "note" {
		t.Errorf("the clustering was not kept: %+v", md.Clustering)
	}
	it := tbl.Read(ctx)
	rows := 0
	for {
		var row map[string]bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows++
		if s, _ := row["s"].(map[string]bigquery.Value); s == nil || s["a"] == nil || row["price"] == nil {
			t.Errorf("row after SET DATA TYPE: %v", row)
		}
	}
	if rows != 2 {
		t.Errorf("%d rows after the mixed ALTER TABLE, want 2", rows)
	}

	// A row access policy: renaming or dropping a column is refused; SET
	// DATA TYPE and RENAME TO keep the policy.
	run("CREATE ROW ACCESS POLICY big ON "+ref+" GRANT TO ('allUsers') FILTER USING (amount > 6)", false)
	if n := count("fields_t"); n != 1 {
		t.Fatalf("the policy keeps %d rows, want 1", n)
	}
	for _, sql := range []string{
		"ALTER TABLE " + ref + " RENAME COLUMN extra TO more",
		"ALTER TABLE " + ref + " DROP COLUMN extra",
	} {
		wantHTTPStatus(t, sql, bqRun(ctx, c, sql, false), http.StatusBadRequest)
	}
	run("ALTER TABLE "+ref+" ALTER COLUMN s SET DATA TYPE STRUCT<a BIGNUMERIC, b STRING>", false)
	if n := count("fields_t"); n != 1 {
		t.Errorf("after SET DATA TYPE the policy keeps %d rows, want 1", n)
	}
	run("ALTER TABLE "+ref+" RENAME TO fields_renamed, SET OPTIONS (description = 'renamed')", true)
	if tableExists(t, ctx, tbl) {
		t.Error("fields_t is still there after RENAME TO")
	}
	md, err = ds.Table("fields_renamed").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Description != "renamed" || md.TableConstraints == nil || md.TableConstraints.PrimaryKey == nil || len(md.Schema) != 6 {
		t.Errorf("fields_renamed: description %q, constraints %+v, %d columns", md.Description, md.TableConstraints, len(md.Schema))
	}
	if n := count("fields_renamed"); n != 1 {
		t.Errorf("after RENAME TO the policy keeps %d rows, want 1", n)
	}
}
