//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryCreateViewWithAColumnList (#1240): CREATE VIEW v (c, d) AS
// SELECT ..., through jobs.query and jobs.insert, makes a view whose
// columns are named by the list in order, whatever the query calls them
// (named, unnamed or named twice): Table.Metadata (tables.get) reads the
// listed names in the schema and the query as written, the generated
// client reads view.useExplicitColumnNames true, a query of the view
// reads its rows by the listed names, and jobs.get shows the client's
// text. OR REPLACE, IF NOT EXISTS and OPTIONS are kept. A list of another
// length than the query's, a name BigQuery refuses and a name listed twice
// are 400 invalidQuery, nothing made. Measured first: the emulator refused
// the statement, 400 "CREATE VIEW with explicit column list is not
// supported".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryCreateViewWithAColumnList(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	svc := generatedBigQuery(t, h)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("base")+" AS SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, 'y'", false); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		view, query string
		insert      bool
	}{
		{"named", "SELECT a, b FROM " + path("base"), false},
		{"unnamed", "SELECT a + 0, UPPER(b) FROM " + path("base"), true},
		{"twice", "SELECT a AS z, b AS z FROM " + path("base"), false},
	} {
		sql := "CREATE VIEW " + path(tc.view) + " (id, `the name`) AS " + tc.query
		if tc.insert {
			job, err := c.Query(sql).Run(ctx)
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
				t.Fatalf("%s: %v %v", sql, err, st.Err())
			}
			again, err := c.JobFromID(ctx, job.ID())
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := again.Config()
			if qc, ok := cfg.(*bigquery.QueryConfig); err != nil || !ok || qc.Q != sql {
				t.Errorf("jobs.get of %s shows %+v (%v)", tc.view, cfg, err)
			}
		} else if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		md, err := ds.Table(tc.view).Metadata(ctx)
		if err != nil {
			t.Fatalf("Table.Metadata of %s: %v", tc.view, err)
		}
		if md.Type != bigquery.ViewTable || md.ViewQuery != tc.query || len(md.Schema) != 2 || md.Schema[0].Name != "id" ||
			md.Schema[1].Name != "the name" || md.Schema[0].Type != bigquery.IntegerFieldType || md.Schema[1].Type != bigquery.StringFieldType {
			t.Errorf("%s reads back type %s, query %q, schema %+v %+v", tc.view, md.Type, md.ViewQuery, md.Schema[0], md.Schema[1])
		}
		gen, err := svc.Tables.Get(project, ds.DatasetID, tc.view).Context(ctx).Do()
		if err != nil || gen.View == nil || !gen.View.UseExplicitColumnNames || gen.View.Query != tc.query {
			t.Errorf("the generated client's tables.get of %s: %+v (%v); want useExplicitColumnNames and the query as written", tc.view, gen.View, err)
		}
		got := rows(t, ctx, c, "SELECT id, `the name` FROM "+path(tc.view)+" ORDER BY id")
		if want := "[[1 x] [2 y]]"; tc.view == "unnamed" {
			want = "[[1 X] [2 Y]]"
			if fmt.Sprint(got) != want {
				t.Errorf("the view %s reads %v, want %s", tc.view, got, want)
			}
		} else if fmt.Sprint(got) != want {
			t.Errorf("the view %s reads %v, want %s", tc.view, got, want)
		}
	}

	// OR REPLACE, IF NOT EXISTS and OPTIONS are kept. (The emulator keeps
	// no description from a CREATE VIEW's OPTIONS, with or without a column
	// list, so it is not read back here.)
	sql := "CREATE OR REPLACE VIEW " + path("named") + " (only) OPTIONS(description = 'one column') AS SELECT b FROM " + path("base")
	if err := bqRun(ctx, c, sql, true); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if md, err := ds.Table("named").Metadata(ctx); err != nil || len(md.Schema) != 1 || md.Schema[0].Name != "only" {
		t.Errorf("the replaced view reads %+v (%v)", md, err)
	}
	if err := bqRun(ctx, c, "CREATE VIEW IF NOT EXISTS "+path("named")+" (x, y) AS SELECT a, b FROM "+path("base"), false); err != nil {
		t.Fatalf("CREATE VIEW IF NOT EXISTS: %v", err)
	}
	if md, err := ds.Table("named").Metadata(ctx); err != nil || len(md.Schema) != 1 || md.Schema[0].Name != "only" {
		t.Errorf("CREATE VIEW IF NOT EXISTS of an existing view changed it: %+v (%v)", md, err)
	}

	for _, insert := range []bool{false, true} {
		for view, sql := range map[string]string{
			"short":  "CREATE VIEW " + path("short") + " (c) AS SELECT a, b FROM " + path("base"),
			"long":   "CREATE VIEW " + path("long") + " (c, d, e) AS SELECT a, b FROM " + path("base"),
			"bad":    "CREATE VIEW " + path("bad") + " (`c!`, d) AS SELECT a, b FROM " + path("base"),
			"dup":    "CREATE VIEW " + path("dup") + " (c, C) AS SELECT a, b FROM " + path("base"),
			"noread": "CREATE VIEW " + path("noread") + " (c) AS SELECT a FROM " + path("missing"),
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery")
			if tableExists(t, ctx, ds.Table(view)) {
				t.Errorf("the refused %s made %s", sql, view)
			}
		}
	}
}

// TestBigQueryViewColumnListInAScriptAndColumnOptions (#1323): a view's
// column list inside a multi-statement script, through jobs.query and
// jobs.insert, makes the view as a lone one does (#1240): the listed
// names in Table.Metadata's schema, the rows by them, and the script's
// statements after it read the view. A listed column's OPTIONS(description
// = '...'), alone and in a script, reads back as that column's description
// in tables.get, the other columns none. A name listed twice in a script
// is 400 invalidQuery before anything runs (the script's earlier CREATE
// TABLE makes nothing). A column option other than description is 400
// invalidQuery since #1416, as BigQuery takes description alone (a script with a TEMP table is TestBigQueryViewColumnListInATempScriptAndADryRun,
// #1335). Measured first: the emulator refused a column list
// in a script (400 "CREATE VIEW with explicit column list is not
// supported", through the front 501), and kept no column description from
// DDL.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryViewColumnListInAScriptAndColumnOptions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("base")+" AS SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, 'y'", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		view := fmt.Sprintf("script%v", insert)
		sql := "SELECT 1; CREATE VIEW " + path(view) + " (id, name) AS SELECT a, b FROM " + path("base") +
			"; SELECT name FROM " + path(view) + " WHERE id = 2"
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
				if err := it.Next(&row); err != nil {
					break
				}
				got = append(got, row)
			}
		} else {
			got = rows(t, ctx, c, sql)
		}
		if fmt.Sprint(got) != "[[y]]" {
			t.Errorf("the script's last statement read %v, want [[y]]", got)
		}
		md, err := ds.Table(view).Metadata(ctx)
		if err != nil || md.Type != bigquery.ViewTable || len(md.Schema) != 2 || md.Schema[0].Name != "id" || md.Schema[1].Name != "name" {
			t.Fatalf("the view the script made reads %+v (%v)", md, err)
		}
		if got := rows(t, ctx, c, "SELECT id, name FROM "+path(view)+" ORDER BY id"); fmt.Sprint(got) != "[[1 x] [2 y]]" {
			t.Errorf("the view %s reads %v", view, got)
		}
	}

	for view, sql := range map[string]string{
		"described":       "CREATE VIEW " + path("described") + " (id OPTIONS(description = 'the id'), name) AS SELECT a, b FROM " + path("base"),
		"scriptdescribed": "DECLARE n INT64 DEFAULT 1; CREATE VIEW " + path("scriptdescribed") + " (id, name OPTIONS(description = \"the name\")) AS SELECT a, b FROM " + path("base"),
	} {
		if err := bqRun(ctx, c, sql, view == "described"); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		md, err := ds.Table(view).Metadata(ctx)
		if err != nil || len(md.Schema) != 2 {
			t.Fatalf("Table.Metadata of %s: %+v (%v)", view, md, err)
		}
		want := [2]string{"the id", ""}
		if view == "scriptdescribed" {
			want = [2]string{"", "the name"}
		}
		if md.Schema[0].Name != "id" || md.Schema[0].Description != want[0] || md.Schema[1].Description != want[1] {
			t.Errorf("%s reads descriptions %q, %q, want %q", view, md.Schema[0].Description, md.Schema[1].Description, want)
		}
	}

	for _, insert := range []bool{false, true} {
		sql := "CREATE TABLE " + path("made") + " AS SELECT 1 AS a; CREATE VIEW " + path("dup") + " (c, C) AS SELECT a, b FROM " + path("base")
		wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery")
		for _, table := range []string{"made", "dup"} {
			if tableExists(t, ctx, ds.Table(table)) {
				t.Errorf("the refused script made %s", table)
			}
		}
		// #1416: BigQuery takes description alone for a view's column.
		sql = "CREATE VIEW " + path("opts") + " (c OPTIONS(rounding_mode = 'ROUND_HALF_EVEN'), d) AS SELECT a, b FROM " + path("base")
		wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery")
	}
}

// TestBigQueryViewColumnListInATempScriptAndADryRun (#1335): a view's
// column list in a script with a TEMP table, which the front sends to the
// emulator as one text, through jobs.query and jobs.insert, makes the view
// as a lone one does: the listed names and the query's types in
// Table.Metadata's schema, a listed column's description, the query as
// written, the rows by the listed names, and the script's last statement
// reads the view. So too when the view's query reads a table the script
// makes before it, which cannot be run beforehand. A name listed twice is
// 400 invalidQuery and so (#1416) is a column option other than
// description, nothing run. A dry run of a lone one (Query.DryRun, jobs.insert, and jobs.query
// through the generated client) answers statementType CREATE_VIEW,
// totalBytesProcessed 0 and no schema, and makes nothing; a list of
// another length than the query's is 400 invalidQuery, a dataset that
// does not exist 404. Measured first: both were 501 through the front.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryViewColumnListInATempScriptAndADryRun(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	svc := generatedBigQuery(t, h)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("base")+" AS SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, 'y'", false); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		view := fmt.Sprintf("temp%v", insert)
		query := "SELECT a, b FROM " + path("base")
		sql := "CREATE TEMP TABLE tt AS SELECT 2 AS k; CREATE VIEW " + path(view) + " (id OPTIONS(description = 'the id'), name) AS " +
			query + "; SELECT name FROM " + path(view) + " WHERE id = (SELECT k FROM tt)"
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
				if err := it.Next(&row); err != nil {
					break
				}
				got = append(got, row)
			}
		} else {
			got = rows(t, ctx, c, sql)
		}
		if fmt.Sprint(got) != "[[y]]" {
			t.Errorf("the script's last statement read %v, want [[y]]", got)
		}
		md, err := ds.Table(view).Metadata(ctx)
		if err != nil || md.Type != bigquery.ViewTable || len(md.Schema) != 2 {
			t.Fatalf("the view the script made reads %+v (%v)", md, err)
		}
		if md.Schema[0].Name != "id" || md.Schema[0].Type != bigquery.IntegerFieldType || md.Schema[0].Description != "the id" ||
			md.Schema[1].Name != "name" || md.Schema[1].Type != bigquery.StringFieldType || md.Schema[1].Description != "" ||
			md.ViewQuery != query {
			t.Errorf("%s reads query %q, schema %+v %+v", view, md.ViewQuery, md.Schema[0], md.Schema[1])
		}
		if got := rows(t, ctx, c, "SELECT id, name FROM "+path(view)+" ORDER BY id"); fmt.Sprint(got) != "[[1 x] [2 y]]" {
			t.Errorf("the view %s reads %v", view, got)
		}

		// The view reads a table the script makes first.
		later, made := fmt.Sprintf("later%v", insert), fmt.Sprintf("made%v", insert)
		sql = "CREATE TEMP TABLE tt AS SELECT 1 AS k; CREATE TABLE " + path(made) + " AS SELECT 3 AS a, 'z' AS b, [1.5] AS r; " +
			"CREATE VIEW " + path(later) + " (n, s, rs) AS SELECT a, b, r FROM " + path(made)
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		md, err = ds.Table(later).Metadata(ctx)
		if err != nil || len(md.Schema) != 3 || md.Schema[0].Name != "n" || md.Schema[0].Type != bigquery.IntegerFieldType ||
			md.Schema[1].Name != "s" || md.Schema[1].Type != bigquery.StringFieldType || md.Schema[2].Name != "rs" ||
			md.Schema[2].Type != bigquery.FloatFieldType || !md.Schema[2].Repeated {
			t.Fatalf("the view %s reads %+v (%v)", later, md, err)
		}
		if got := rows(t, ctx, c, "SELECT n, s, rs FROM "+path(later)); fmt.Sprint(got) != "[[3 z [1.5]]]" {
			t.Errorf("the view %s reads %v", later, got)
		}

		sql = "CREATE TEMP TABLE tt AS SELECT 1 AS k; CREATE VIEW " + path("dup") + " (c, C) AS SELECT a, b FROM " + path("base")
		wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery")
		sql = "CREATE TEMP TABLE tt AS SELECT 1 AS k; CREATE VIEW " + path("opts") + " (c OPTIONS(rounding_mode = 'ROUND_HALF_EVEN'), d) AS SELECT a, b FROM " + path("base")
		wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest, "invalidQuery") // #1416
		for _, table := range []string{"dup", "opts"} {
			if tableExists(t, ctx, ds.Table(table)) {
				t.Errorf("the refused script made %s", table)
			}
		}
	}

	// A dry run.
	sql := "CREATE VIEW " + path("dry") + " (id, name) AS SELECT a, b FROM " + path("base")
	q := c.Query(sql)
	q.DryRun = true
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("the dry run: %v", err)
	}
	st := job.LastStatus()
	if st == nil || st.Statistics == nil {
		t.Fatalf("the dry run's status: %+v", st)
	}
	qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics)
	if !ok || qs.StatementType != "CREATE_VIEW" || qs.TotalBytesProcessed != 0 || len(qs.Schema) != 0 {
		t.Errorf("the dry run's statistics: %+v", st.Statistics.Details)
	}
	gq, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: sql, DryRun: true, UseLegacySql: new(bool)}).Context(ctx).Do()
	if err != nil || gq.TotalBytesProcessed != 0 || gq.Schema != nil || len(gq.Rows) != 0 {
		t.Errorf("the generated client's dry run through jobs.query: %+v (%v)", gq, err)
	}
	if tableExists(t, ctx, ds.Table("dry")) {
		t.Error("the dry run made the view")
	}
	for sql, code := range map[string]int{
		"CREATE VIEW " + path("dry") + " (id) AS SELECT a, b FROM " + path("base"):  http.StatusBadRequest,
		"CREATE VIEW `nosuchds.dry` (id, name) AS SELECT a, b FROM " + path("base"): http.StatusNotFound,
	} {
		q := c.Query(sql)
		q.DryRun = true
		_, err := q.Run(ctx)
		reason := "invalidQuery"
		if code == http.StatusNotFound {
			reason = "notFound"
		}
		wantReason(t, sql, err, code, reason)
	}
}
