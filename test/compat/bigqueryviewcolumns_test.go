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
// TABLE makes nothing). Still 501: a column option other than description,
// and a column list in a script with a TEMP table, which cannot be run a
// statement at a time. Measured first: the emulator refused a column list
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
		for _, sql := range []string{
			"CREATE VIEW " + path("opts") + " (c OPTIONS(rounding_mode = 'ROUND_HALF_EVEN'), d) AS SELECT a, b FROM " + path("base"),
			"CREATE TEMP TABLE tt AS SELECT 1 AS a; CREATE VIEW " + path("temp") + " (c, d) AS SELECT a, b FROM " + path("base"),
		} {
			wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusNotImplemented, "notImplemented")
		}
	}
}
