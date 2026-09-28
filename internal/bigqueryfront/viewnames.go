package bigqueryfront

import "strings"

// A view's table names without a dataset (#1035).
//
// BigQuery: "A reference inside of a view must be qualified with a
// dataset. The default dataset doesn't affect a view body."
// (https://cloud.google.com/bigquery/docs/views, View limitations.)
//
// The emulator makes such a view and reads its bare table ID as the
// first table of that ID made in any dataset (qualify.go). Measured
// against the pinned image through the front, with m1.t (s STRING,
// three rows) made first and m2.t (a INT64, two rows) after: a view m2.v
// made through tables.insert (the Go client's Table.Create with
// ViewQuery "SELECT * FROM t") was made, and Table.Read of it and
// `SELECT * FROM m2.v` gave m1.t's three rows.
//
// So tables.insert, tables.update and tables.patch of a view, or a
// materialized view, whose GoogleSQL query names a table without a
// dataset are refused 400 invalid, as the view's query would be refused
// with no default dataset (qualifyTables's message; BigQuery's own wording
// for a view is UNVERIFIED). A legacy SQL view is not read (insertTable).

// unqualifiedViewTable returns why a view query that names a table
// without a dataset is refused, or "".
func unqualifiedViewTable(query string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}
	_, _, msg := qualifyTables(query, "")
	if msg == "" {
		return ""
	}
	return msg + " A view's query is not read in any default dataset, not even the view's own."
}
