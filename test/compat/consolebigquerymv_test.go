//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestConsoleBigQueryMaterializedViews (#1390): a materialized view is in
// its dataset's Tables with its type; its page shows its definition (the
// query, automatic refresh, last refreshed) and offers Refresh, which runs
// CALL BQ.REFRESH_MATERIALIZED_VIEW: a row inserted into the base table is
// in the view after it, read through the official Go client. Create table
// makes one with the MATERIALIZED_VIEW type.
func TestConsoleBigQueryMaterializedViews(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for _, sql := range []string{
		"CREATE TABLE " + p("base") + " (k STRING, v INT64)",
		"INSERT INTO " + p("base") + " (k, v) VALUES ('a', 1)",
		"CREATE MATERIALIZED VIEW " + p("mv") + " AS SELECT k, SUM(v) AS s FROM " + p("base") + " GROUP BY k",
		"INSERT INTO " + p("base") + " (k, v) VALUES ('a', 10)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	page := func(path ...string) consoleJobPage {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/bigquery?"+q.Encode(), "")
		var pg consoleJobPage
		if code != http.StatusOK || json.Unmarshal([]byte(body), &pg) != nil || pg.Unavailable != "" {
			t.Fatalf("detail %v = %d: %s", path, code, body)
		}
		return pg
	}
	_, tables := page(ds.DatasetID).section("tables")
	found := false
	for _, r := range tables {
		if r["#"] == "mv" || strings.Contains(strings.Join(mapValues(r), " "), "MATERIALIZED_VIEW") {
			found = true
		}
	}
	if !found {
		t.Errorf("the dataset's tables do not show the materialized view: %v", tables)
	}
	mv := page(ds.DatasetID, "mv")
	if q := mv.property("Table", "Materialized view query"); !strings.Contains(q, "SUM(v)") {
		t.Errorf("Materialized view query %q", q)
	}
	if r := mv.property("Table", "Automatic refresh"); !strings.HasPrefix(r, "On, every 30m") {
		t.Errorf("Automatic refresh %q", r)
	}
	if mv.property("Table", "Last refreshed") == "-" || mv.property("Table", "Type") != "MATERIALIZED_VIEW" {
		t.Errorf("Last refreshed %q, Type %q", mv.property("Table", "Last refreshed"), mv.property("Table", "Type"))
	}
	offered := false
	for _, a := range mv.Actions {
		offered = offered || a.ID == "refreshmv"
	}
	if !offered {
		t.Fatalf("the page offers no Refresh: %+v", mv.Actions)
	}
	sum := func(table string) int64 {
		t.Helper()
		got := rows(t, ctx, c, "SELECT SUM(s) FROM "+p(table))
		if len(got) != 1 || got[0][0] == nil {
			t.Fatalf("SUM of %s: %v", table, got)
		}
		return got[0][0].(int64)
	}
	if got := sum("mv"); got != 1 {
		t.Fatalf("before Refresh: %d, want 1", got)
	}
	if code, out := consoleAct(t, addr, "bigquery", project, []string{ds.DatasetID, "mv"}, "refreshmv", nil); code != http.StatusOK {
		t.Fatalf("Refresh = %d: %s", code, out)
	}
	if got := sum("mv"); got != 11 {
		t.Errorf("after Refresh: %d, want 11", got)
	}

	// Create table with the MATERIALIZED_VIEW type.
	if code, out := consoleAct(t, addr, "bigquery", project, []string{ds.DatasetID}, "createtable", map[string]string{
		"tableId": "made", "tableType": "MATERIALIZED_VIEW", "viewQuery": "SELECT k, SUM(v) AS s FROM " + p("base") + " GROUP BY k",
	}); code != http.StatusOK {
		t.Fatalf("Create table = %d: %s", code, out)
	}
	md, err := ds.Table("made").Metadata(ctx)
	if err != nil || md.Type != bigquery.MaterializedView {
		t.Fatalf("Create table made %v %+v", err, md)
	}
	if got := sum("made"); got != 11 {
		t.Errorf("SUM of the view Create table made: %d, want 11", got)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
