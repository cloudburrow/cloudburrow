//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryAlterTableSetDataTypeArraysAndKeyedColumns (#1416): ALTER
// COLUMN SET DATA TYPE of an ARRAY to an ARRAY of a type its elements take
// (INT64 to NUMERIC), of a STRUCT with a REPEATED field to one whose field
// is wider (ARRAY<INT64> to ARRAY<FLOAT64>), of INT64 to a parameterized
// NUMERIC and BIGNUMERIC that hold every INT64 (NUMERIC(20),
// BIGNUMERIC(40, 20)), and of a clustering column to a type that clusters,
// in one statement, through jobs.query and jobs.insert, as
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_data_type_statement
// documents them. Read back through Table.Metadata (types, modes,
// parameters, clustering and range partitioning kept) and the rows, each
// value converted. Refused 400 invalidQuery, nothing changed: a clustering
// column to FLOAT64, the range-partitioning column to NUMERIC, INT64 to a
// NUMERIC(18), a column made an ARRAY or no longer one, and a STRUCT's
// REPEATED field made a scalar. Measured first: the ARRAY and STRUCT forms,
// INT64 to NUMERIC(20) and the clustering column's were 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tables.insert
func TestBigQueryAlterTableSetDataTypeArraysAndKeyedColumns(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name := fmt.Sprintf("arrays%v", insert)
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{
			Schema: bigquery.Schema{
				{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
				{Name: "arr", Type: bigquery.IntegerFieldType, Repeated: true},
				{Name: "s", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
					{Name: "a", Type: bigquery.IntegerFieldType, Repeated: true},
					{Name: "b", Type: bigquery.StringFieldType, Description: "kept"},
				}},
				{Name: "c", Type: bigquery.IntegerFieldType},
				{Name: "r", Type: bigquery.IntegerFieldType},
			},
			Clustering: &bigquery.Clustering{Fields: []string{"c"}},
			RangePartitioning: &bigquery.RangePartitioning{Field: "r",
				Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}},
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
		run("INSERT INTO " + ref + " (id, arr, s, c, r) VALUES (1, [1, 2], STRUCT([3, 4], 'x'), 5, 15), (2, [], NULL, NULL, NULL)")
		run("ALTER TABLE " + ref + " ALTER COLUMN arr SET DATA TYPE ARRAY<NUMERIC>, " +
			"ALTER COLUMN s SET DATA TYPE STRUCT<a ARRAY<FLOAT64>, b STRING>, ALTER COLUMN c SET DATA TYPE NUMERIC(20), " +
			"ALTER COLUMN id SET DATA TYPE BIGNUMERIC(40, 20)")
		md, err := tbl.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f := md.Schema
		if len(f) != 5 || f[0].Type != bigquery.BigNumericFieldType || f[0].Precision != 40 || f[0].Scale != 20 || !f[0].Required ||
			f[1].Type != bigquery.NumericFieldType || !f[1].Repeated ||
			f[2].Type != bigquery.RecordFieldType || len(f[2].Schema) != 2 || f[2].Schema[0].Type != bigquery.FloatFieldType ||
			!f[2].Schema[0].Repeated || f[2].Schema[1].Description != "kept" ||
			f[3].Type != bigquery.NumericFieldType || f[3].Precision != 20 || f[4].Type != bigquery.IntegerFieldType {
			t.Errorf("insert %v: the schema after SET DATA TYPE: %v", insert, schemaString(f))
		}
		if md.Clustering == nil || len(md.Clustering.Fields) != 1 || md.Clustering.Fields[0] != "c" ||
			md.RangePartitioning == nil || md.RangePartitioning.Field != "r" {
			t.Errorf("insert %v: clustering %+v, range partitioning %+v", insert, md.Clustering, md.RangePartitioning)
		}
		got := rows(t, ctx, c, "SELECT CAST(id AS STRING), "+
			"ARRAY_TO_STRING(ARRAY(SELECT CAST(x AS STRING) FROM UNNEST(arr) x WITH OFFSET o ORDER BY o), ','), "+
			"ARRAY_TO_STRING(ARRAY(SELECT CAST(x AS STRING) FROM UNNEST(s.a) x WITH OFFSET o ORDER BY o), ','), s.b, "+
			"CAST(c AS STRING), r FROM "+ref+" ORDER BY id")
		if want := "[[1 1,2 3,4 x 5 15] [2   <nil> <nil> <nil>]]"; fmt.Sprint(got) != want {
			t.Errorf("insert %v: rows %v, want %s", insert, got, want)
		}
		for _, sql := range []string{
			"ALTER TABLE " + ref + " ALTER COLUMN c SET DATA TYPE FLOAT64",
			"ALTER TABLE " + ref + " ALTER COLUMN r SET DATA TYPE NUMERIC",
			"ALTER TABLE " + ref + " ALTER COLUMN r SET DATA TYPE NUMERIC(18)",
			"ALTER TABLE " + ref + " ALTER COLUMN r SET DATA TYPE ARRAY<INT64>",
			"ALTER TABLE " + ref + " ALTER COLUMN arr SET DATA TYPE BIGNUMERIC",
			"ALTER TABLE " + ref + " ALTER COLUMN s SET DATA TYPE STRUCT<a FLOAT64, b STRING>",
		} {
			err := bqRun(ctx, c, sql, insert)
			wantHTTPStatus(t, sql, err, http.StatusBadRequest)
		}
		if md, err := tbl.Metadata(ctx); err != nil || len(md.Schema) != 5 || md.Schema[4].Type != bigquery.IntegerFieldType ||
			md.Schema[3].Type != bigquery.NumericFieldType || !md.Schema[1].Repeated {
			t.Errorf("insert %v: a refused SET DATA TYPE changed the table: %v (%v)", insert, md, err)
		}
	}
}

// schemaString is a schema's names, types and modes, for a test's message.
func schemaString(s bigquery.Schema) string {
	out := ""
	for _, f := range s {
		out += fmt.Sprintf("%s %s(%d,%d) req=%v rep=%v %s; ", f.Name, f.Type, f.Precision, f.Scale, f.Required, f.Repeated,
			schemaString(f.Schema))
	}
	return out
}

// TestBigQueryAlterColumnSetOptionsRoundingMode (#1416): ALTER COLUMN SET
// OPTIONS (rounding_mode = ..., description = a constant expression), as
// the column_set_options_list of
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_options_statement
// documents it, through jobs.query and jobs.insert: Table.Metadata reads
// the column's RoundingMode and description, and a value written after it
// is rounded by it ("ROUND_HALF_EVEN": 2.25 to 2.2, -2.25 to -2.2), the
// other column still away from zero; rounding_mode = NULL clears it
// ("Setting the VALUE to NULL clears the column's value for that
// option"), and a value written then rounds away from zero. Refused 400
// invalidQuery: a rounding_mode of a STRING column, a mode BigQuery does
// not have. data_policies stays 501. Measured first: rounding_mode there
// was 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryAlterColumnSetOptionsRoundingMode(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name := fmt.Sprintf("rounded%v", insert)
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
			{Name: "k", Type: bigquery.IntegerFieldType},
			{Name: "x", Type: bigquery.NumericFieldType, Precision: 5, Scale: 1},
			{Name: "y", Type: bigquery.NumericFieldType, Precision: 5, Scale: 1},
			{Name: "note", Type: bigquery.StringFieldType},
		}}); err != nil {
			t.Fatal(err)
		}
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN x SET OPTIONS (rounding_mode = 'ROUND_HALF_EVEN', description = CONCAT('half', ' even'))")
		md, err := tbl.Metadata(ctx)
		if err != nil || md.Schema[1].RoundingMode != bigquery.RoundHalfEven || md.Schema[1].Description != "half even" ||
			md.Schema[2].RoundingMode != "" {
			t.Fatalf("insert %v: after SET OPTIONS: %+v %+v (%v)", insert, md.Schema[1], md.Schema[2], err)
		}
		run("INSERT INTO " + ref + " (k, x, y) VALUES (1, 2.25, 2.25), (2, -2.25, -2.25)")
		if got := rows(t, ctx, c, "SELECT CAST(x AS STRING), CAST(y AS STRING) FROM "+ref+" ORDER BY k"); fmt.Sprint(got) != "[[2.2 2.3] [-2.2 -2.3]]" {
			t.Errorf("insert %v: rounded %v, want [[2.2 2.3] [-2.2 -2.3]]", insert, got)
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN x SET OPTIONS (rounding_mode = NULL)")
		if md, err := tbl.Metadata(ctx); err != nil || md.Schema[1].RoundingMode != "" || md.Schema[1].Description != "half even" {
			t.Errorf("insert %v: rounding_mode = NULL: %+v (%v)", insert, md.Schema[1], err)
		}
		run("INSERT INTO " + ref + " (k, x) VALUES (3, 3.25)")
		if got := rows(t, ctx, c, "SELECT CAST(x AS STRING) FROM "+ref+" WHERE k = 3"); fmt.Sprint(got) != "[[3.3]]" {
			t.Errorf("insert %v: after the mode was cleared %v, want [[3.3]]", insert, got)
		}
		for sql, code := range map[string]int{
			"ALTER TABLE " + ref + " ALTER COLUMN note SET OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')": http.StatusBadRequest,
			"ALTER TABLE " + ref + " ALTER COLUMN y SET OPTIONS (rounding_mode = 'ROUND_DOWN')":         http.StatusBadRequest,
			"ALTER TABLE " + ref + " ALTER COLUMN y SET OPTIONS (data_policies = ['p'])":                http.StatusNotImplemented,
		} {
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), code)
		}
		if md, err := tbl.Metadata(ctx); err != nil || md.Schema[2].RoundingMode != "" || md.Schema[3].RoundingMode != "" {
			t.Errorf("insert %v: a refused SET OPTIONS changed the table: %+v (%v)", insert, md, err)
		}
	}
}

// TestBigQueryAlterTableDryRun (#1416): a dry run of a lone ALTER TABLE
// (Query.DryRun through jobs.insert, and jobs.query through the generated
// client) answers statistics.query.statementType ALTER_TABLE,
// totalBytesProcessed 0 and no schema, and changes nothing: ADD COLUMN,
// SET OPTIONS, SET DATA TYPE and RENAME TO each leave the table as it was.
// It is checked as the statement is: a missing table is 404 notFound (and
// nothing with IF EXISTS), a column the table has 400 invalidQuery, a
// coercion SET DATA TYPE does not make 400, and a table option CloudBurrow
// does not carry out 501. Measured first: every ALTER TABLE in a dry run
// was 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryAlterTableDryRun(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc := generatedBigQuery(t, h)
	tbl := ds.Table("dryalter")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "a", Type: bigquery.IntegerFieldType},
		{Name: "s", Type: bigquery.StringFieldType, MaxLength: 5},
	}, Description: "before"}); err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".dryalter`"
	dry := func(sql string) (*bigquery.QueryStatistics, error) {
		q := c.Query(sql)
		q.DryRun = true
		job, err := q.Run(ctx)
		if err != nil {
			return nil, err
		}
		st := job.LastStatus()
		if st == nil || st.Statistics == nil {
			return nil, fmt.Errorf("no statistics: %+v", st)
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		return qs, nil
	}
	for _, sql := range []string{
		"ALTER TABLE " + ref + " ADD COLUMN b STRING, SET OPTIONS (description = 'after')",
		"ALTER TABLE " + ref + " ALTER COLUMN a SET DATA TYPE NUMERIC, ALTER COLUMN s SET DATA TYPE STRING(10)",
		"ALTER TABLE " + ref + " RENAME TO dryrenamed",
		"ALTER TABLE IF EXISTS `" + ds.DatasetID + ".nosuch` ADD COLUMN b STRING",
	} {
		qs, err := dry(sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		if qs == nil || qs.StatementType != "ALTER_TABLE" || qs.TotalBytesProcessed != 0 || len(qs.Schema) != 0 {
			t.Errorf("%s: statistics %+v", sql, qs)
		}
		gq, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: sql, DryRun: true, UseLegacySql: new(bool)}).Context(ctx).Do()
		if err != nil || gq.TotalBytesProcessed != 0 || gq.Schema != nil || len(gq.Rows) != 0 {
			t.Errorf("%s through jobs.query: %+v (%v)", sql, gq, err)
		}
	}
	md, err := tbl.Metadata(ctx)
	if err != nil || len(md.Schema) != 2 || md.Schema[0].Type != bigquery.IntegerFieldType || md.Schema[1].MaxLength != 5 ||
		md.Description != "before" {
		t.Errorf("a dry run changed the table: %+v (%v)", md, err)
	}
	if tableExists(t, ctx, ds.Table("dryrenamed")) {
		t.Error("a dry run renamed the table")
	}
	for sql, want := range map[string]struct {
		code   int
		reason string
	}{
		"ALTER TABLE `" + ds.DatasetID + ".nosuch` ADD COLUMN b STRING":              {http.StatusNotFound, "notFound"},
		"ALTER TABLE " + ref + " ADD COLUMN a STRING":                                {http.StatusBadRequest, "invalidQuery"},
		"ALTER TABLE " + ref + " ALTER COLUMN a SET DATA TYPE STRING":                {http.StatusBadRequest, "invalidQuery"},
		"ALTER TABLE " + ref + " SET OPTIONS (enable_fine_grained_mutations = TRUE)": {http.StatusNotImplemented, "notImplemented"},
	} {
		_, err := dry(sql)
		wantReason(t, sql, err, want.code, want.reason)
	}
}

// TestBigQueryDDLThroughExecuteImmediate (#1416): DDL that EXECUTE
// IMMEDIATE gives is carried out as the statement alone is: a view with a
// column list and a column's description in a script with a TEMP table,
// which the front sends as one text (the view's names, types and
// description read back, the script's last statement reading the view
// with the TEMP table), and ALTER TABLE's SET DATA TYPE of an ARRAY and
// ALTER COLUMN SET OPTIONS (rounding_mode), each through jobs.query and
// jobs.insert. An ALTER TABLE in a script with a TEMP table, given by
// EXECUTE IMMEDIATE or not, is 501 and changes nothing: CloudBurrow runs
// an ALTER TABLE's script a statement at a time, which a TEMP table does
// not survive. Measured first: the view's column list given by EXECUTE
// IMMEDIATE in a TEMP script was 501, and a plain ALTER TABLE in a TEMP
// script changed the table and then failed the script.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDDLThroughExecuteImmediate(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return ds.DatasetID + "." + table }
	if err := bqRun(ctx, c, "CREATE TABLE `"+path("base")+"` AS SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, 'y'", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		view := fmt.Sprintf("eiview%v", insert)
		sql := "CREATE TEMP TABLE tt AS SELECT 2 AS k; EXECUTE IMMEDIATE 'CREATE VIEW " + path(view) +
			" (id OPTIONS(description = \"the id\"), name) AS SELECT a, b FROM " + path("base") + "'; " +
			"SELECT name FROM `" + path(view) + "` WHERE id = (SELECT k FROM tt)"
		var got [][]bigquery.Value
		if insert {
			job, err := c.Query(sql).Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			it, err := job.Read(ctx)
			if err != nil {
				t.Fatalf("the script through jobs.insert: %v", err)
			}
			for {
				var row []bigquery.Value
				if it.Next(&row) != nil {
					break
				}
				got = append(got, row)
			}
		} else {
			got = rows(t, ctx, c, sql)
		}
		if fmt.Sprint(got) != "[[y]]" {
			t.Errorf("insert %v: the script's last statement read %v, want [[y]]", insert, got)
		}
		md, err := ds.Table(view).Metadata(ctx)
		if err != nil || md.Type != bigquery.ViewTable || len(md.Schema) != 2 || md.Schema[0].Name != "id" ||
			md.Schema[0].Description != "the id" || md.Schema[1].Name != "name" || md.Schema[1].Type != bigquery.StringFieldType {
			t.Errorf("insert %v: the view reads %+v (%v)", insert, md, err)
		}

		name := fmt.Sprintf("eialter%v", insert)
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
			{Name: "arr", Type: bigquery.IntegerFieldType, Repeated: true},
			{Name: "n", Type: bigquery.NumericFieldType},
		}}); err != nil {
			t.Fatal(err)
		}
		sql = "EXECUTE IMMEDIATE 'ALTER TABLE " + path(name) + " ALTER COLUMN arr SET DATA TYPE ARRAY<FLOAT64>'; " +
			"EXECUTE IMMEDIATE 'ALTER TABLE " + path(name) + " ALTER COLUMN n SET OPTIONS (rounding_mode = \"ROUND_HALF_EVEN\")'"
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s (insert %v): %v", sql, insert, err)
		}
		md, err = tbl.Metadata(ctx)
		if err != nil || md.Schema[0].Type != bigquery.FloatFieldType || !md.Schema[0].Repeated || md.Schema[1].RoundingMode != bigquery.RoundHalfEven {
			t.Errorf("insert %v: the ALTER TABLEs EXECUTE IMMEDIATE gave: %v (%v)", insert, md, err)
		}
		for _, sql := range []string{
			"CREATE TEMP TABLE tt AS SELECT 1 AS k; EXECUTE IMMEDIATE 'ALTER TABLE " + path(name) + " ADD COLUMN late STRING'; SELECT k FROM tt",
			"CREATE TEMP TABLE tt AS SELECT 1 AS k; ALTER TABLE `" + path(name) + "` ADD COLUMN late STRING; SELECT k FROM tt",
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusNotImplemented, "notImplemented")
		}
		if md, err := tbl.Metadata(ctx); err != nil || len(md.Schema) != 2 {
			t.Errorf("insert %v: a refused script changed the table: %v (%v)", insert, md, err)
		}
	}
}

// TestBigQueryViewColumnOptionsAndScriptDryRun (#1416): a view's column
// list takes a description that is a constant expression (CONCAT and
// UPPER of literals), read back as the column's description;
// description is the one option of a view's column ("Column options for a
// view have the same syntax and requirements as for a table, but with a
// different list", view_column_option_list), so rounding_mode is 400
// invalidQuery, and so is a value that is not a constant expression (a
// subquery, REPEAT, which the reference bars), nothing made. A script with
// a column list in a dry run (Query.DryRun, jobs.insert) is answered as
// the script without the list is, and makes nothing; a list of another
// length than its query's is 400. Measured first: each was 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryViewColumnOptionsAndScriptDryRun(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("base")+" AS SELECT 1 AS a, 'x' AS b", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		view := fmt.Sprintf("described%v", insert)
		q := c.Query("CREATE VIEW " + path(view) + " (id OPTIONS(description = CONCAT('the ', 'id')), " +
			"name OPTIONS(description = UPPER('the name'))) AS SELECT a, b FROM " + path("base"))
		var err error
		if insert {
			var job *bigquery.Job
			if job, err = q.Run(ctx); err == nil {
				var st *bigquery.JobStatus
				if st, err = job.Wait(ctx); err == nil {
					err = st.Err()
				}
			}
		} else {
			_, err = q.Read(ctx)
		}
		if err != nil {
			t.Fatalf("insert %v: %v", insert, err)
		}
		md, err := ds.Table(view).Metadata(ctx)
		if err != nil || len(md.Schema) != 2 || md.Schema[0].Description != "the id" || md.Schema[1].Description != "THE NAME" {
			t.Errorf("insert %v: the view reads %+v (%v)", insert, md, err)
		}
		for i, opt := range []string{"rounding_mode = 'ROUND_HALF_EVEN'", "description = (SELECT 'x')", "description = REPEAT('x', 2)"} {
			sql := "CREATE VIEW " + path(fmt.Sprintf("refused%d", i)) + " (id OPTIONS(" + opt + "), name) AS SELECT a, b FROM " + path("base")
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery")
			if tableExists(t, ctx, ds.Table(fmt.Sprintf("refused%d", i))) {
				t.Errorf("%s made the view", sql)
			}
		}
	}

	// A script with a column list in a dry run.
	sql := "CREATE VIEW " + path("drylist") + " (id, name) AS SELECT a, b FROM " + path("base") + "; SELECT 1 AS one"
	q := c.Query(sql)
	q.DryRun = true
	if _, err := q.Run(ctx); err != nil {
		t.Errorf("the script's dry run: %v", err)
	}
	if tableExists(t, ctx, ds.Table("drylist")) {
		t.Error("the script's dry run made the view")
	}
	q = c.Query("CREATE VIEW " + path("drylist") + " (id) AS SELECT a, b FROM " + path("base") + "; SELECT 1 AS one")
	q.DryRun = true
	_, err := q.Run(ctx)
	wantReason(t, "a dry run of a script with a list of another length", err, http.StatusBadRequest, "invalidQuery")
}
