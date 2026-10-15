//go:build compat

package compat

import (
	"errors"
	"math/big"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryAlterTableSetDataTypeAndKeys (#1334): ALTER COLUMN SET DATA
// TYPE with BigQuery's documented coercions (INT64 to NUMERIC and FLOAT64,
// NUMERIC to BIGNUMERIC, a longer STRING, a wider NUMERIC(P, S)), and ADD
// PRIMARY KEY, ADD CONSTRAINT … FOREIGN KEY, DROP CONSTRAINT and DROP
// PRIMARY KEY, with another action in the same statement, as
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_data_type_statement
// and the key statements document them, through jobs.query and jobs.insert.
// Each change is read back through Table.Metadata (schema, table
// constraints) and the table's rows. Refused 400: a type that is not
// assignable, a shorter STRING, a key without NOT ENFORCED, a second
// primary key, a key's missing column, a DROP CONSTRAINT of a name the
// table lacks; a REFERENCES of a missing table is 404. SET DEFAULT and
// DROP DEFAULT are TestBigQueryColumnDefaults's (#1340).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tabledata.list
func TestBigQueryAlterTableSetDataTypeAndKeys(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name, parent := "typed_q", "parent_q"
		if insert {
			name, parent = "typed_j", "parent_j"
		}
		if err := ds.Table(parent).Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
			{Name: "pid", Type: bigquery.IntegerFieldType, Required: true},
		}}); err != nil {
			t.Fatal(err)
		}
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{
			Schema: bigquery.Schema{
				{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
				{Name: "n", Type: bigquery.IntegerFieldType},
				{Name: "m", Type: bigquery.NumericFieldType},
				{Name: "s", Type: bigquery.StringFieldType, MaxLength: 5, Description: "short"},
				{Name: "d", Type: bigquery.NumericFieldType, Precision: 10, Scale: 2},
				{Name: "f", Type: bigquery.IntegerFieldType},
			},
			Description: "kept",
		}); err != nil {
			t.Fatal(err)
		}
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		meta := func() *bigquery.TableMetadata {
			t.Helper()
			md, err := tbl.Metadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return md
		}
		run("INSERT INTO " + ref + " (id, n, m, s, d, f) VALUES (1, 7, 1.5, 'abc', 12.34, 3), (2, NULL, NULL, NULL, NULL, NULL)")

		// Keys, with ADD COLUMN in the same statement.
		run("ALTER TABLE " + ref + " ADD PRIMARY KEY (id) NOT ENFORCED, ADD CONSTRAINT fk_parent FOREIGN KEY (n) REFERENCES `" +
			ds.DatasetID + "." + parent + "` (pid) NOT ENFORCED, ADD COLUMN extra STRING")
		md := meta()
		tc := md.TableConstraints
		if tc == nil || tc.PrimaryKey == nil || len(tc.PrimaryKey.Columns) != 1 || tc.PrimaryKey.Columns[0] != "id" ||
			len(tc.ForeignKeys) != 1 || tc.ForeignKeys[0].Name != "fk_parent" || tc.ForeignKeys[0].ReferencedTable == nil ||
			tc.ForeignKeys[0].ReferencedTable.TableID != parent || len(tc.ForeignKeys[0].ColumnReferences) != 1 ||
			tc.ForeignKeys[0].ColumnReferences[0].ReferencingColumn != "n" || tc.ForeignKeys[0].ColumnReferences[0].ReferencedColumn != "pid" {
			t.Errorf("insert %v: table constraints %+v", insert, tc)
		}
		if len(md.Schema) != 7 || md.Schema[6].Name != "extra" {
			t.Errorf("insert %v: ADD COLUMN with the keys: %d columns", insert, len(md.Schema))
		}
		// ADD CONSTRAINT IF NOT EXISTS of a name the table has does nothing.
		run("ALTER TABLE " + ref + " ADD CONSTRAINT IF NOT EXISTS fk_parent FOREIGN KEY (f) REFERENCES `" +
			ds.DatasetID + "." + parent + "` (pid) NOT ENFORCED")

		// SET DATA TYPE, several in one statement; the keys are kept.
		run("ALTER TABLE " + ref + " ALTER COLUMN n SET DATA TYPE NUMERIC, ALTER COLUMN m SET DATA TYPE BIGNUMERIC, " +
			"ALTER COLUMN s SET DATA TYPE STRING(10), ALTER COLUMN d SET DATA TYPE NUMERIC(12, 3), ALTER COLUMN f SET DATA TYPE FLOAT64")
		md = meta()
		byName := map[string]*bigquery.FieldSchema{}
		for _, f := range md.Schema {
			byName[f.Name] = f
		}
		if f := byName["id"]; f == nil || f.Type != bigquery.IntegerFieldType || !f.Required {
			t.Errorf("insert %v: id %+v", insert, f)
		}
		if f := byName["n"]; f == nil || f.Type != bigquery.NumericFieldType {
			t.Errorf("insert %v: n %+v, want NUMERIC", insert, f)
		}
		if f := byName["m"]; f == nil || f.Type != bigquery.BigNumericFieldType {
			t.Errorf("insert %v: m %+v, want BIGNUMERIC", insert, f)
		}
		if f := byName["s"]; f == nil || f.Type != bigquery.StringFieldType || f.MaxLength != 10 || f.Description != "short" {
			t.Errorf("insert %v: s %+v, want STRING(10) described", insert, f)
		}
		if f := byName["d"]; f == nil || f.Type != bigquery.NumericFieldType || f.Precision != 12 || f.Scale != 3 {
			t.Errorf("insert %v: d %+v, want NUMERIC(12, 3)", insert, f)
		}
		if f := byName["f"]; f == nil || f.Type != bigquery.FloatFieldType {
			t.Errorf("insert %v: f %+v, want FLOAT", insert, f)
		}
		if len(md.Schema) != 7 || md.Schema[1].Name != "n" || md.Description != "kept" {
			t.Errorf("insert %v: the columns' places or the description changed: %v %q", insert, md.Schema, md.Description)
		}
		if tc := md.TableConstraints; tc == nil || tc.PrimaryKey == nil || len(tc.ForeignKeys) != 1 {
			t.Errorf("insert %v: the keys were not kept by SET DATA TYPE: %+v", insert, tc)
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
			if row["id"] != int64(1) {
				if row["n"] != nil || row["f"] != nil {
					t.Errorf("insert %v: row 2 %v, want NULLs", insert, row)
				}
				continue
			}
			n, _ := row["n"].(*big.Rat)
			m, _ := row["m"].(*big.Rat)
			d, _ := row["d"].(*big.Rat)
			if n == nil || n.Cmp(big.NewRat(7, 1)) != 0 || m == nil || m.Cmp(big.NewRat(3, 2)) != 0 || row["s"] != "abc" ||
				d == nil || d.Cmp(big.NewRat(1234, 100)) != 0 || row["f"] != float64(3) {
				t.Errorf("insert %v: row 1 %v", insert, row)
			}
		}
		if rows != 2 {
			t.Errorf("insert %v: %d rows, want 2", insert, rows)
		}

		for sql, code := range map[string]int{
			"ALTER TABLE " + ref + " ALTER COLUMN n SET DATA TYPE INT64":                                         http.StatusBadRequest,
			"ALTER TABLE " + ref + " ALTER COLUMN id SET DATA TYPE STRING":                                       http.StatusBadRequest,
			"ALTER TABLE " + ref + " ALTER COLUMN s SET DATA TYPE STRING(5)":                                     http.StatusBadRequest,
			"ALTER TABLE " + ref + " ALTER COLUMN nope SET DATA TYPE NUMERIC":                                    http.StatusBadRequest,
			"ALTER TABLE " + ref + " ADD PRIMARY KEY (n) NOT ENFORCED":                                           http.StatusBadRequest,
			"ALTER TABLE " + ref + " DROP PRIMARY KEY, ADD PRIMARY KEY (nope) NOT ENFORCED":                      http.StatusBadRequest,
			"ALTER TABLE " + ref + " ADD FOREIGN KEY (n) REFERENCES `" + ds.DatasetID + "." + parent + "` (pid)": http.StatusBadRequest,
			"ALTER TABLE " + ref + " DROP CONSTRAINT nope":                                                       http.StatusBadRequest,
		} {
			err := bqRun(ctx, c, sql, insert)
			wantHTTPStatus(t, sql, err, code)
			var e *googleapi.Error
			if errors.As(err, &e) && code == http.StatusNotImplemented && (len(e.Errors) != 1 || e.Errors[0].Reason != "notImplemented") {
				t.Errorf("%s: %+v, want reason notImplemented", sql, e.Errors)
			}
		}
		if !insert {
			sql := "ALTER TABLE " + ref + " ADD FOREIGN KEY (n) REFERENCES `" + ds.DatasetID + ".missing` (pid) NOT ENFORCED"
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), http.StatusNotFound)
		}
		if tc := meta().TableConstraints; tc == nil || tc.PrimaryKey == nil || len(tc.ForeignKeys) != 1 {
			t.Errorf("insert %v: a refused statement changed the keys: %+v", insert, tc)
		}

		// Every key dropped: tableConstraints is cleared.
		run("ALTER TABLE " + ref + " DROP CONSTRAINT fk_parent, DROP PRIMARY KEY, DROP CONSTRAINT IF EXISTS nope, " +
			"DROP PRIMARY KEY IF EXISTS")
		if tc := meta().TableConstraints; tc != nil && (tc.PrimaryKey != nil || len(tc.ForeignKeys) != 0) {
			t.Errorf("insert %v: after DROP CONSTRAINT and DROP PRIMARY KEY: %+v", insert, tc)
		}
		// An unnamed foreign key.
		run("ALTER TABLE " + ref + " ADD FOREIGN KEY (id) REFERENCES `" + ds.DatasetID + "." + parent + "` (pid) NOT ENFORCED")
		if tc := meta().TableConstraints; tc == nil || tc.PrimaryKey != nil || len(tc.ForeignKeys) != 1 || tc.ForeignKeys[0].Name != "" {
			t.Errorf("insert %v: unnamed foreign key: %+v", insert, tc)
		}
	}
}
