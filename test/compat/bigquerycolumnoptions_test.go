//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryCreateTableColumnOptions (#1438): a CREATE TABLE column's
// OPTIONS (description = ...), a STRING literal or a constant expression,
// of a column and of a STRUCT's field, with a NUMERIC column's
// rounding_mode beside it, as the column_option_list of
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#column_option_list
// documents it, through jobs.query and jobs.insert, alone, in a
// multi-statement script and through EXECUTE IMMEDIATE. Read back through
// Table.Metadata (each field's Description, the NUMERIC's precision, scale
// and RoundingMode, the table's description) and
// INFORMATION_SCHEMA.COLUMN_FIELD_PATHS. Refused, nothing made: a column
// data_governance_tags of two tags 400 (#1456, TestBigQueryDataGovernanceTags), a data_policies entry not a policy's 400 (#1456), a description that
// is a subquery 400, a rounding_mode of an INT64 column 400. Measured
// first: the table was made and no column read a description; a STRUCT
// field's description failed 400 "Nested column options are unsupported".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryCreateTableColumnOptions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	columns := `(a INT64 OPTIONS (description = "col"), b STRING OPTIONS (description = CONCAT('con', 'cat')), ` +
		`d NUMERIC(5, 2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN', description = 'num'), ` +
		`s STRUCT<f INT64 OPTIONS (description = 'field'), g STRING OPTIONS (description = UPPER('g'))>, e STRING) ` +
		`OPTIONS (description = "d")`
	for _, insert := range []bool{false, true} {
		for _, form := range []string{"lone", "script", "execute"} {
			name := fmt.Sprintf("colopts_%s_%v", form, insert)
			ref := "`" + ds.DatasetID + "." + name + "`"
			sql := "CREATE TABLE " + ref + " " + columns
			switch form {
			case "script":
				sql = "DECLARE x INT64 DEFAULT 1;\n" + sql + ";\nSELECT x"
			case "execute":
				sql = `EXECUTE IMMEDIATE """` + sql + `"""`
			}
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
			md, err := ds.Table(name).Metadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f := md.Schema
			if md.Description != "d" || len(f) != 5 || f[0].Description != "col" || f[1].Description != "concat" ||
				f[2].Description != "num" || f[2].Precision != 5 || f[2].Scale != 2 || f[2].RoundingMode != bigquery.RoundHalfEven ||
				len(f[3].Schema) != 2 || f[3].Schema[0].Description != "field" || f[3].Schema[1].Description != "G" ||
				f[3].Description != "" || f[4].Description != "" {
				t.Errorf("%s (insert %v): description %q, schema %v", form, insert, md.Description, describedSchema(f))
			}
			got := rows(t, ctx, c, "SELECT field_path, description, rounding_mode FROM `"+ds.DatasetID+
				"`.INFORMATION_SCHEMA.COLUMN_FIELD_PATHS WHERE table_name = '"+name+"' ORDER BY field_path")
			if want := "[[a col <nil>] [b concat <nil>] [d num ROUND_HALF_EVEN] [e <nil> <nil>] [s <nil> <nil>] " +
				"[s.f field <nil>] [s.g G <nil>]]"; fmt.Sprint(got) != want {
				t.Errorf("%s (insert %v): COLUMN_FIELD_PATHS %v, want %s", form, insert, got, want)
			}
		}
		for i, bad := range []struct {
			cols string
			code int
		}{
			{"(a STRING OPTIONS (data_policies = ['p']))", http.StatusBadRequest},                               // not a policy's {'name': ...}, #1456
			{"(a STRING OPTIONS (data_governance_tags = [('p/k', 'v'), ('p/l', 'w')]))", http.StatusBadRequest}, // one tag a field, #1456
			{"(a STRING OPTIONS (description = (SELECT 'x')))", http.StatusBadRequest},
			{"(a INT64 OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'))", http.StatusBadRequest},
		} {
			name := fmt.Sprintf("colopts_bad%d_%v", i, insert)
			sql := "CREATE TABLE `" + ds.DatasetID + "." + name + "` " + bad.cols
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), bad.code)
			if _, err := ds.Table(name).Metadata(ctx); err == nil {
				t.Errorf("%s (insert %v): the table was made", sql, insert)
			}
		}
	}
}

// describedSchema is a schema's names and descriptions, for a test's
// message.
func describedSchema(s bigquery.Schema) string {
	out := ""
	for _, f := range s {
		out += fmt.Sprintf("%s %q %s(%d,%d,%s) {%s}; ", f.Name, f.Description, f.Type, f.Precision, f.Scale, f.RoundingMode,
			describedSchema(f.Schema))
	}
	return out
}
