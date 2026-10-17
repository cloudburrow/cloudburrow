package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Change history across a table's rebuilds (#1466).
//
// BigQuery keeps a table's history across ALTER TABLE: time travel reads
// the table as it was before an ADD COLUMN, a DROP COLUMN or a RENAME,
// and APPENDS and CHANGES read its changes "limited to the table's current
// schema" (https://cloud.google.com/bigquery/docs/reference/standard-sql/time-series-functions).
// The front carries out those statements, and tables.patch's added
// columns, by making the table again (alterremake.go, schemaupdate.go),
// which in the engine drops its change table (googlesqlite patch 0036)
// and starts a new one, whose INSERTs are the rows copied back. So the
// front keeps the old change table (saveHistory) and carries it into the
// new one (carryHistory):
//
//   - the copied rows' INSERTs become CARRY: the rows are in the table
//     from then on, for time travel, but they are no change, and APPENDS
//     and CHANGES leave them out;
//   - the old changes are added with each column of the new schema the
//     value of its old column (renamed, or CAST to its new type as the
//     rebuild casts it), a column added NULL and a column dropped left
//     out; their row identities are moved below every rowid, so that
//     none is taken for a row of the new table;
//   - each old row still there gets an END at the rebuild's time, which
//     ends it for time travel (the CARRY of its copy follows) and which
//     APPENDS and CHANGES leave out;
//   - the old START row, the start of the history, takes the new one's
//     place.
//
// CREATE OR REPLACE TABLE of a table that exists, which the front carries
// out by deleting the table and sending the statement (script.go's
// replace), keeps the history the same way, but as BigQuery keeps "the
// data that is replaced": the new rows are INSERTs, the replaced rows
// still there DELETEs at the replace, with their values in each column
// the new table has of the same name and type, as a WRITE_TRUNCATE
// records them. (The engine does the same for a CREATE OR REPLACE TABLE
// it carries out itself, googlesqlite patch 0044.)

// Change types of a carried history, besides the engine's INSERT, DELETE,
// UPDATE and START.
const (
	changeCarry = "CARRY"
	changeEnd   = "END"
)

// carriedColumns are the columns a history carried from a change table
// has besides the table's: its change time, row identity and order.
const carriedColumns = "googlesqlite_change_micros AS cloudburrow_micros, googlesqlite_change_rowid AS cloudburrow_row, rowid AS cloudburrow_order"

// historySource is the history of the table ds.table, as carryHistory
// reads it.
func (f front) historySource(ds, table string) string {
	return "(SELECT *, " + carriedColumns + " FROM " + quotePath([]string{projectOf(f.base), ds, table + changeTableSuffix}) + ")"
}

// hasHistory reports whether ds.table has a change table (googlesqlite
// patch 0036 gives none to a table of a column of a change column's
// name, _CHANGE_IS_FOR_UPDATE say, patch 0063).
func (f front) hasHistory(r *http.Request, ds, table string) bool {
	return f.runDML(r, "SELECT COUNT(*) FROM "+quotePath([]string{projectOf(f.base), ds, table + changeTableSuffix})) == ""
}

// saveHistory copies the change table of ds.table into a scratch table
// (scratch.go), before the table is made again, and returns the scratch
// table, which the caller deletes, and true; false when the table has no
// change table to save.
func (f front) saveHistory(r *http.Request, ds, table string) (tableRef, bool) {
	scratch := tableRef{DatasetID: resultsDataset, TableID: scratchTable()}
	if msg := f.runDML(r, "CREATE TABLE "+quotePath([]string{scratch.DatasetID, scratch.TableID})+" AS SELECT * FROM "+
		f.historySource(ds, table)); msg != "" {
		return tableRef{}, false
	}
	return scratch, true
}

// dropSavedHistory deletes the scratch table of saveHistory.
func (f front) dropSavedHistory(r *http.Request, saved tableRef) {
	f.runDML(r, "DROP TABLE IF EXISTS "+quotePath([]string{saved.DatasetID, saved.TableID}))
}

// carriedColumn is a column of the table made again, and its value in
// the old history, an expression of the old change table's columns.
type carriedColumn struct{ name, expr string }

// carryHistory carries src, a history saveHistory saved or historySource
// reads, into the change table of ds.table, just made again with its rows
// copied (above), or says why it could not. replaced is whether the table
// was replaced (CREATE OR REPLACE TABLE, below): its new rows are then
// INSERTs, and its old ones are DELETEs at the replace, with their values.
func (f front) carryHistory(r *http.Request, src, ds, table string, cols []carriedColumn, replaced bool) string {
	changes := quotePath([]string{projectOf(f.base), ds, table + changeTableSuffix})
	names, exprs := make([]string, 0, len(cols)), make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, quoteName(c.name))
		exprs = append(exprs, c.expr)
	}
	list, vals := strings.Join(names, ", "), strings.Join(exprs, ", ")
	if len(cols) > 0 {
		list += ", "
		vals += ", "
	}
	shift := "(SELECT 2 * (COALESCE(MAX(ABS(cloudburrow_row)), 0) + 1) FROM " + src + ")"
	target := "INSERT INTO " + changes + " (" + list + "_CHANGE_TYPE, _CHANGE_TIMESTAMP, _CHANGE_IS_FOR_UPDATE, " +
		"googlesqlite_change_micros, googlesqlite_change_rowid) "
	end, endVals := changeEnd, ""
	if replaced {
		end, endVals = "DELETE", vals
	}
	var qs []string
	if !replaced {
		qs = append(qs, fmt.Sprintf("UPDATE %s SET _CHANGE_TYPE = '%s' WHERE _CHANGE_TYPE = 'INSERT'", changes, changeCarry))
	}
	endTarget := target
	if !replaced {
		endTarget = "INSERT INTO " + changes + " (_CHANGE_TYPE, _CHANGE_TIMESTAMP, _CHANGE_IS_FOR_UPDATE, " +
			"googlesqlite_change_micros, googlesqlite_change_rowid) "
	}
	qs = append(qs,
		fmt.Sprintf("%sSELECT %s_CHANGE_TYPE, _CHANGE_TIMESTAMP, _CHANGE_IS_FOR_UPDATE, cloudburrow_micros, cloudburrow_row - %s "+
			"FROM %s ORDER BY cloudburrow_order", target, vals, shift, src),
		// The rebuild's time is the new START row's, the latest.
		fmt.Sprintf("%sSELECT %s'%s', TIMESTAMP_MICROS(cloudburrow_at), FALSE, cloudburrow_at, cloudburrow_row - %s FROM "+
			"(SELECT *, ROW_NUMBER() OVER (PARTITION BY cloudburrow_row ORDER BY cloudburrow_order DESC) AS cloudburrow_last "+
			"FROM %s WHERE _CHANGE_TYPE != 'START'), (SELECT MAX(googlesqlite_change_micros) AS cloudburrow_at FROM %s WHERE _CHANGE_TYPE = 'START') "+
			"WHERE cloudburrow_last = 1 AND _CHANGE_TYPE NOT IN ('DELETE', '%s') ORDER BY cloudburrow_row",
			endTarget, endVals, end, shift, src, changes, changeEnd),
		fmt.Sprintf("DELETE FROM %s WHERE _CHANGE_TYPE = 'START' AND googlesqlite_change_micros > "+
			"(SELECT MIN(googlesqlite_change_micros) FROM %s WHERE _CHANGE_TYPE = 'START')", changes, changes),
	)
	for _, q := range qs {
		if msg := f.runDML(r, q); msg != "" {
			return msg
		}
	}
	return ""
}

// replacedColumns are the columns of next, a table's new schema fields, that
// old, its schema's before a CREATE OR REPLACE TABLE, has of the same
// name, type and mode: the columns of the replaced history kept.
func replacedColumns(old, next []any) []carriedColumn {
	var out []carriedColumn
	for _, n := range next {
		nm, _ := n.(map[string]any)
		for _, o := range old {
			om, _ := o.(map[string]any)
			if nm != nil && om != nil && str(nm, "name") == str(om, "name") && canonicalType(str(nm, "type")) == canonicalType(str(om, "type")) &&
				str(nm, "mode") == str(om, "mode") && nm["fields"] == nil && om["fields"] == nil {
				out = append(out, carriedColumn{name: str(nm, "name"), expr: quoteName(str(nm, "name"))})
				break
			}
		}
	}
	return out
}

// historyKept is the failure of a rebuild of name whose history could not
// be carried (above): the table is made again, its history before not.
func historyKept(name, msg string) string {
	return "cloudburrow: " + name + " is changed, but its change history before the change could not be kept: " + msg
}

// historyStart is when the change history of ds.table starts, in
// microseconds since the epoch: its START row's time, which a table made
// again keeps (above); false when it has none.
func (f front) historyStart(r *http.Request, ds, table string) (int64, bool) {
	legacy := false
	body, err := json.Marshal(queryOptions{UseLegacySQL: &legacy, Query: "SELECT MIN(googlesqlite_change_micros) FROM " +
		quotePath([]string{projectOf(f.base), ds, table + changeTableSuffix}) + " WHERE _CHANGE_TYPE = 'START'"})
	if err != nil {
		return 0, false
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return 0, false
	}
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 || res.Rows[0].F[0].V == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(*res.Rows[0].F[0].V, 10, 64)
	return n, err == nil
}
