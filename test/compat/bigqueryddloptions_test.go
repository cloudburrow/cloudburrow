//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryDDLOptionsReadBack (#1251): a table's or a view's
// description, friendly_name, labels and expiration_timestamp given in the
// OPTIONS of CREATE TABLE, CREATE TABLE ... AS SELECT, CREATE TABLE ...
// LIKE, CREATE VIEW (with and without a column list) and ALTER TABLE or
// ALTER VIEW ... SET OPTIONS, through jobs.query and jobs.insert, read
// back through Table.Metadata (tables.get) as BigQuery keeps them.
// Measured first: the emulator kept none of the four from CREATE, failed
// ALTER VIEW 400 "Statement not supported: AlterViewStatement", and the
// front answered ALTER TABLE 501. IF NOT EXISTS of a table that exists
// changes nothing; ALTER changes only what it names, NULL clearing it; an
// option the front does not set is 501, nothing made.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDDLOptionsReadBack(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	const opts = "OPTIONS(description = 'the desc', friendly_name = 'Friendly', labels = [('team', 'data'), ('env', 'test')], " +
		"expiration_timestamp = TIMESTAMP '2031-02-03 04:05:06 UTC')"
	want := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := bqRun(ctx, c, "CREATE TABLE "+path("src")+" (a INT64)", false); err != nil {
		t.Fatal(err)
	}
	check := func(sql, table string, typ bigquery.TableType) {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if md.Type != typ || md.Description != "the desc" || md.Name != "Friendly" || len(md.Labels) != 2 ||
			md.Labels["team"] != "data" || md.Labels["env"] != "test" || !md.ExpirationTime.Equal(want) {
			t.Errorf("%s: read back type %s, description %q, name %q, labels %v, expiration %v", sql, md.Type,
				md.Description, md.Name, md.Labels, md.ExpirationTime)
		}
	}
	for _, insert := range []bool{false, true} {
		sfx := "_q"
		if insert {
			sfx = "_j"
		}
		for _, tc := range []struct {
			table, sql string
			typ        bigquery.TableType
		}{
			{"cols", "CREATE TABLE %s (a INT64) " + opts, bigquery.RegularTable},
			{"ctas", "CREATE TABLE %s " + opts + " AS SELECT 1 AS a", bigquery.RegularTable},
			{"like", "CREATE TABLE %s LIKE " + path("src") + " " + opts, bigquery.RegularTable},
			{"view", "CREATE VIEW %s " + opts + " AS SELECT 1 AS a", bigquery.ViewTable},
			{"listed", "CREATE VIEW %s (x) " + opts + " AS SELECT 1 AS a", bigquery.ViewTable},
			{"view", "CREATE OR REPLACE VIEW %s " + opts + " AS SELECT 2 AS b", bigquery.ViewTable},
		} {
			sql := fmt.Sprintf(tc.sql, path(tc.table+sfx))
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			check(sql, tc.table+sfx, tc.typ)
		}

		// IF NOT EXISTS of one that exists: nothing changes.
		sql := "CREATE TABLE IF NOT EXISTS " + path("cols"+sfx) + " (a INT64) OPTIONS(description = 'other')"
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		check(sql, "cols"+sfx, bigquery.RegularTable)

		// ALTER: only what it names; NULL clears.
		for _, kind := range []string{"TABLE", "VIEW"} {
			table := "cols" + sfx
			if kind == "VIEW" {
				table = "view" + sfx
			}
			sql := "ALTER " + kind + " " + path(table) + " SET OPTIONS(description = 'altered', labels = [('k', 'v')], " +
				"expiration_timestamp = NULL)"
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			md, err := ds.Table(table).Metadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if md.Description != "altered" || md.Name != "Friendly" || len(md.Labels) != 1 || md.Labels["k"] != "v" ||
				!md.ExpirationTime.IsZero() {
				t.Errorf("%s: read back description %q, name %q, labels %v, expiration %v", sql, md.Description,
					md.Name, md.Labels, md.ExpirationTime)
			}
			sql = "ALTER " + kind + " " + path(table) + " SET OPTIONS(description = NULL)"
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if md, err := ds.Table(table).Metadata(ctx); err != nil || md.Description != "" || md.Name != "Friendly" {
				t.Errorf("%s: read back %v %+v", sql, err, md)
			}
		}

		// An option the front does not set: 501, nothing made.
		sql = "CREATE TABLE " + path("part"+sfx) + " (a INT64) OPTIONS(description = 'd', enable_fine_grained_mutations = TRUE)"
		wantReason(t, sql, bqRun(ctx, c, sql, insert), http.StatusNotImplemented, "notImplemented")
		if tableExists(t, ctx, ds.Table("part"+sfx)) {
			t.Errorf("the refused %s made the table", sql)
		}
	}
}
