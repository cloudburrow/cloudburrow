//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
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
// are 400 invalidQuery, nothing made; a column's OPTIONS in the list and a
// column list in a multi-statement script stay 501. Measured first: the
// emulator refused the statement, 400 "CREATE VIEW with explicit column
// list is not supported".
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
		for _, sql := range []string{
			"CREATE VIEW " + path("opts") + " (c OPTIONS(description = 'x'), d) AS SELECT a, b FROM " + path("base"),
			"SELECT 1; CREATE VIEW " + path("script") + " (c, d) AS SELECT a, b FROM " + path("base"),
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusNotImplemented, "notImplemented")
		}
	}
}
